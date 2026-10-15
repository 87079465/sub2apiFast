package office_copilot

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatHubWebSocketProtocol(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/m365Copilot/ChatHub" || r.URL.Query().Get("access_token") != "test-token" {
			w.WriteHeader(400)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, handshake, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if string(handshake) != "{\"protocol\":\"json\",\"version\":1}\x1e" {
			return
		}
		conn.WriteMessage(websocket.TextMessage, []byte("{}\x1e"))
		_, request, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var invocation map[string]any
		json.Unmarshal([]byte(strings.TrimSuffix(string(request), "\x1e")), &invocation)
		received <- invocation
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1,"target":"update","arguments":[{"streamingMode":"Full","messages":[{"author":"bot","messageId":"m","text":"Hello"}]}]}`+"\x1e"+`{"type":1,"target":"update","arguments":[{"streamingMode":"Full","messages":[{"author":"bot","messageId":"m","text":"Hello world"}]}]}`+"\x1e"))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":2,"invocationId":"0","item":{"throttling":{"metering":{"ClaudeOpusQueryDailyWord":{"remainingAllowance":0}}},"messages":[{"author":"bot","messageId":"m","text":"Hello world"}],"result":{"value":"Success"}}}`+"\x1e"+`{"type":3,"invocationId":"0"}`+"\x1e"))
	}))
	defer server.Close()
	client := NewClient("test-token", "", "")
	quotas := make(chan QuotaSnapshot, 2)
	client.OnQuota = func(snapshot QuotaSnapshot) { quotas <- snapshot }
	client.BaseURL = server.URL
	payload, err := BuildCopilotPayload([]InboundMessage{{Role: "user", Content: "hi"}}, "copilot-auto")
	require.NoError(t, err)
	body, _ := json.Marshal(payload)
	resp, err := client.SendStreamRequest(context.Background(), "ChatHub", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	var output strings.Builder
	require.NoError(t, ReadTextStream(resp.Body, func(text string) error { output.WriteString(text); return nil }))
	require.Equal(t, "Hello world", output.String())
	select {
	case snapshot := <-quotas:
		require.Equal(t, 0, *snapshot.Meters["ClaudeOpusQueryDailyWord"].Remaining)
		require.False(t, snapshot.ObservedAt.IsZero())
	case <-time.After(time.Second):
		t.Fatal("quota observation not delivered")
	}
	require.Empty(t, quotas)
	invocation := <-received
	require.Equal(t, "chat", invocation["target"])
	require.Equal(t, float64(4), invocation["type"])
	args := invocation["arguments"].([]any)[0].(map[string]any)
	require.Equal(t, "hi", args["message"].(map[string]any)["text"])
}

func TestChatHubErrorAndPrematureCompletion(t *testing.T) {
	for _, frame := range []string{
		`{"type":2,"invocationId":"0","item":{"result":{"value":"Unauthorized"}}}`,
		`{"type":3,"invocationId":"0"}`,
		`{"type":7}`,
		`{"type":3,"invocationId":"0","error":"denied"}`,
	} {
		state := chatHubStream{previous: map[string]string{}}
		done, err := state.process([]byte(frame), func(string) error { return nil })
		require.Error(t, err)
		require.False(t, done)
	}
}

func TestChatHubHandshakeErrorDoesNotExposeToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	client := NewClient("secret-never-log", "", "")
	client.BaseURL = server.URL
	_, err := client.SendStreamRequest(context.Background(), "ChatHub", []byte(`{}`))
	require.ErrorContains(t, err, "401")
	require.NotContains(t, err.Error(), "secret-never-log")
}

func TestChatHubSuccessfulCompletionRequiresAssistantText(t *testing.T) {
	// Reproduces Word Opus replying to "hi" with Success but only the echoed
	// user message. A successful transport must not make the account test pass.
	for _, tc := range []struct {
		name     string
		update   string
		wantText string
		wantErr  string
	}{
		{"user_echo_only", `{"result":{"value":"Success"},"messages":[{"author":"user","text":"hi"}]}`, "", "returned no assistant text"},
		{"status_only", `{"result":{"value":"Success"},"messages":[{"author":"bot","messageType":"Progress","text":"Working"}]}`, "", "returned no assistant text"},
		{"assistant_text", `{"result":{"value":"Success"},"messages":[{"author":"user","text":"hi"},{"author":"bot","text":"connection test successful."}]}`, "connection test successful.", ""},
		{"missing_success", `{"messages":[{"author":"bot","text":"partial"}]}`, "partial", "without a successful response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := chatHubStream{previous: map[string]string{}}
			var text strings.Builder
			emit := func(part string) error { text.WriteString(part); return nil }
			done, err := state.process([]byte(`{"type":2,"invocationId":"0","item":`+tc.update+`}`), emit)
			require.NoError(t, err)
			require.False(t, done)
			done, err = state.process([]byte(`{"type":3,"invocationId":"0"}`), emit)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.False(t, done)
			} else {
				require.NoError(t, err)
				require.True(t, done)
			}
			require.Equal(t, tc.wantText, text.String())
		})
	}
}

func TestChatHubWordModelAndUnavailableDocument(t *testing.T) {
	for _, tc := range []struct{ model, tone string }{{"claude-opus-5-5", "Claude_Opus"}, {"claude-sonnet-5", "Claude_Sonnet"}, {"gpt-6-sol", "Gpt_5_5_Reasoning"}} {
		t.Run(tc.model, func(t *testing.T) {
			observed := make(chan map[string]any, 1)
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("source") != "word" || r.Header.Get("Origin") != "https://fa000000125.resources.office.net" {
					w.WriteHeader(400)
					return
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				conn.ReadMessage()
				conn.WriteMessage(websocket.TextMessage, []byte("{}\x1e"))
				_, raw, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]any
				json.Unmarshal([]byte(strings.TrimSuffix(string(raw), "\x1e")), &req)
				arg := req["arguments"].([]any)[0].(map[string]any)
				if arg["tone"] != tc.tone {
					return
				}
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1,"target":"invoke_local_plugin","invocationId":"s1","arguments":[{"schema_version":"test-schema","correlation_id":"c1","invocation":{"local_endpoint":"Microsoft.Office.WordCanvas","payload":"untrusted document command"}}]}`+"\x1e"))
				_, raw, err = conn.ReadMessage()
				if err != nil {
					return
				}
				var reply map[string]any
				json.Unmarshal([]byte(strings.TrimSuffix(string(raw), "\x1e")), &reply)
				observed <- reply
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":2,"invocationId":"0","item":{"messages":[{"author":"bot","messageId":"m","text":"ok"}],"result":{"value":"Success"}}}`+"\x1e"+`{"type":3,"invocationId":"0"}`+"\x1e"))
			}))
			defer server.Close()
			payload, err := BuildCopilotPayload([]InboundMessage{{Role: "user", Content: "hello"}}, tc.model)
			require.NoError(t, err)
			body, _ := json.Marshal(payload)
			client := NewClient("token", "", "")
			client.BaseURL = server.URL
			resp, err := client.SendStreamRequest(context.Background(), "ChatHub", body)
			require.NoError(t, err)
			defer resp.Body.Close()
			var text strings.Builder
			require.NoError(t, ReadTextStream(resp.Body, func(s string) error { text.WriteString(s); return nil }))
			require.Equal(t, "ok", text.String())
			reply := <-observed
			require.Equal(t, float64(3), reply["type"])
			require.Equal(t, "s1", reply["invocationId"])
			result := reply["result"].(map[string]any)
			require.Equal(t, "c1", result["correlation_id"])
			require.Equal(t, "Fail", result["response"].(map[string]any)["status"])
		})
	}
}

func TestChatHubLocalPluginCompletionWireOrder(t *testing.T) {
	reply, handled, err := unavailableLocalPlugin([]byte(`{"type":1,"target":"invoke_local_plugin","invocationId":"s1","arguments":[{"schema_version":"schema","correlation_id":"c1"}]}`))
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, strings.HasPrefix(string(reply), `{"type":3,"invocationId":"s1","result":`), "upstream rejects result before type")
	_, handled, err = unavailableLocalPlugin([]byte(`{"type":1,"target":"invoke_local_plugin","invocationId":"s1","arguments":[]}`))
	require.True(t, handled)
	require.Error(t, err)
}
