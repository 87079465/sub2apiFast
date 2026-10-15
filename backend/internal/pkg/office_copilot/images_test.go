package office_copilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func testImageData(t *testing.T, value uint8) string {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{R: value, A: 255})
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, im))
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestOfficeCopilotImagesKeepHistoryAndToolAssociations(t *testing.T) {
	data := testImageData(t, 1)
	first := json.RawMessage(`[{"type":"text","text":"old image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}]`)
	nested := json.RawMessage(`[{"type":"tool_result","tool_use_id":"read_1","is_error":true,"content":[{"type":"text","text":"screenshot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + data + `"}}]},{"type":"text","text":"explain"}]`)
	original := InboundRequest{Messages: []InboundMessage{{Role: "user", Content: first}, {Role: "assistant", Content: "history"}, {Role: "user", Content: nested}}}
	got, err := ExtractImages(original)
	require.NoError(t, err)
	require.Len(t, got.Images, 1, "same image in history must be uploaded only once")
	require.Equal(t, "image-1.png", got.Images[0].FileName)
	require.Equal(t, first, original.Messages[0].Content, "caller input must not be mutated")
	wire, err := BuildCopilotRequestPayload(got, "claude-opus-5-5")
	require.NoError(t, err)
	text := wire["message"].(map[string]any)["text"].(string)
	require.NotContains(t, text, data)
	require.Equal(t, 2, strings.Count(text, "[Attached image: image-1.png]"))
	require.Contains(t, text, `"tool_use_id":"read_1"`)
	require.Contains(t, text, `"is_error":true`)
	require.Contains(t, text, `"role":"assistant","content":"history"`)
}

func TestOfficeCopilotImageValidation(t *testing.T) {
	data := testImageData(t, 2)
	for _, block := range []string{
		`{"type":"image"}`,
		`{"type":"image","source":{"type":"url","url":"https://private.invalid/secret"}}`,
		`{"type":"image_url","image_url":{"url":"http://127.0.0.1/private"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + data + `"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"invalid!"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/svg+xml","data":"PHN2Zy8+"}}`,
	} {
		_, err := ExtractImages(InboundRequest{Messages: []InboundMessage{{Role: "user", Content: json.RawMessage("[" + block + "]")}}})
		require.ErrorContains(t, err, "image")
		require.NotContains(t, err.Error(), "private")
		require.NotContains(t, err.Error(), data)
	}
	var blocks []any
	for i := 0; i <= MaxImages; i++ {
		blocks = append(blocks, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": testImageData(t, uint8(i))}})
	}
	_, err := ExtractImages(InboundRequest{Messages: []InboundMessage{{Role: "user", Content: blocks}}})
	require.ErrorContains(t, err, "at most")
}

func TestOfficeCopilotImageUploadAndChatBinding(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	uploads := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/m365Copilot/UploadFile" {
			require.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
			require.Equal(t, "https://fa000000125.resources.office.net", r.Header.Get("Origin"))
			require.Equal(t, "WordDraftingAgent@BuiltInAgents", r.Header.Get("X-GptId"))
			require.Equal(t, "feature.EnableImageSupportInUploadFile", r.Header.Get("X-Variants"))
			require.NoError(t, r.ParseMultipartForm(1<<20))
			defer r.MultipartForm.RemoveAll()
			require.Equal(t, "UploadImage", r.FormValue("scenario"))
			require.True(t, strings.HasPrefix(r.FormValue("FileBase64"), "data:image/png;base64,"))
			require.Equal(t, []string{"cwcgptvsan", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch", "gptvnorm2048"}, r.MultipartForm.Value["optionsSets"])
			if uploads > 0 {
				require.Equal(t, "upload-session", r.FormValue("conversationId"))
			}
			uploads++
			_ = json.NewEncoder(w).Encode(map[string]any{"docId": fmt.Sprintf("attachment-%d", uploads), "conversationId": "upload-session", "result": map[string]string{"value": "Success"}})
			return
		}
		require.Equal(t, "/m365Copilot/ChatHub", r.URL.Path)
		require.Equal(t, 2, uploads)
		require.Equal(t, "upload-session", r.URL.Query().Get("ConversationId"))
		require.Equal(t, "agt_worddraftingagent_enable_image_rich_response", r.URL.Query().Get("variants"))
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("{}\x1e")))
		_, wire, err := conn.ReadMessage()
		require.NoError(t, err)
		var frame struct {
			Arguments []map[string]any `json:"arguments"`
		}
		require.NoError(t, json.Unmarshal(bytes.TrimSuffix(wire, []byte{0x1e}), &frame))
		p := frame.Arguments[0]
		require.NotContains(t, p, "conversationId")
		require.Equal(t, false, p["isStartOfSession"])
		annotations := p["message"].(map[string]any)["messageAnnotations"].([]any)
		require.Len(t, annotations, 2)
		require.Equal(t, "ImageFile", annotations[0].(map[string]any)["messageAnnotationType"])
		require.Equal(t, "attachment-1", annotations[0].(map[string]any)["id"])
		require.Equal(t, "attachment-2", annotations[1].(map[string]any)["id"])
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":2,"invocationId":"0","item":{"messages":[{"author":"bot","text":"image understood"}],"result":{"value":"Success"}}}`+"\x1e"+`{"type":3,"invocationId":"0"}`+"\x1e")))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := NewClient("synthetic-token", "", "")
	client.HTTPClient = server.Client()
	client.BaseURL = server.URL
	request, err := ExtractImages(InboundRequest{Messages: []InboundMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":"data:image/png;base64,` + testImageData(t, 1) + `"},{"type":"image_url","image_url":"data:image/png;base64,` + testImageData(t, 2) + `"}]`)}}})
	require.NoError(t, err)
	payload, err := BuildCopilotRequestPayload(request, "claude-opus-5-5")
	require.NoError(t, err)
	require.NoError(t, client.AttachImages(context.Background(), payload, request.Images))
	transport := client.HTTPClient.Transport.(*http.Transport)
	transport.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	resp, err := client.SendStreamRequest(context.Background(), "ChatHub", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, ReadTextStream(resp.Body, func(text string) error { require.Equal(t, "image understood", text); return nil }))
	require.Equal(t, []string{"h2", "http/1.1"}, transport.TLSClientConfig.NextProtos, "WS must not mutate the HTTP transport")
}

func TestOfficeCopilotImageUploadFailureAndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTemporaryRedirect, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirected := false
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"docId":"x","result":{"value":"Failure","message":"private upstream detail"}}`))
			}))
			defer server.Close()
			client := NewClient("synthetic-token", "", "")
			client.BaseURL = server.URL
			payload, _ := BuildCopilotPayload([]InboundMessage{{Role: "user", Content: "test"}}, "claude-opus-5-5")
			err := client.AttachImages(context.Background(), payload, []ImageInput{{MediaType: "image/png", Data: []byte("synthetic")}})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private upstream detail")
			require.False(t, redirected)
			require.Empty(t, client.imageConversationID)
			require.NotContains(t, payload["message"], "messageAnnotations")
		})
	}
}
