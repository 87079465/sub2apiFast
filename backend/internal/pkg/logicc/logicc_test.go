package logicc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func testToken(sid string, expiry int64) string {
	b, _ := json.Marshal(map[string]any{"sid": sid, "exp": expiry})
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}

func TestLogiccRefreshRotationAndInference(t *testing.T) {
	token := testToken("sess_test", time.Now().Add(time.Hour).Unix())
	refreshed := 0
	saved := Credentials{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/client/sessions/sess_test/tokens":
			refreshed++
			require.Equal(t, "POST", r.Method)
			require.Equal(t, "__client=old-cookie", r.Header.Get("Cookie"))
			require.Equal(t, "https://app.logicc.com", r.Header.Get("Origin"))
			http.SetCookie(w, &http.Cookie{Name: "__client", Value: "rotated-cookie"})
			_ = json.NewEncoder(w).Encode(map[string]string{"jwt": token})
		case "/api/models":
			require.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
			fmt.Fprint(w, `{"models":[{"id":"model-a"},{"modelId":"model-b","isDefault":true}]}`)
		case "/api/chats":
			var b map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			require.Equal(t, "assistant-1", b["assistantId"])
			fmt.Fprint(w, `{"id":"chat-1"}`)
		case "/api/chats/chat-1":
			require.Equal(t, "ai-sdk-v6", r.Header.Get("Stream-Response-Protocol"))
			var b map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			require.Equal(t, "model-b", b["modelId"])
			require.Equal(t, "hello", b["prompt"])
			require.Equal(t, "high", b["reasoningEffort"])
			fmt.Fprint(w, "data: {\"type\":\"text-delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"finish\",\"totalUsage\":{\"inputTokens\":12,\"outputTokens\":3}}\n\n")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := NewClient(Credentials{ClientCookie: "old-cookie", SessionID: "sess_test"}, "")
	require.NoError(t, err)
	client.BaseURL = server.URL + "/api"
	client.ClerkURL = server.URL
	client.Save = func(c Credentials) error { saved = c; return nil }
	resp, model, err := client.Infer(context.Background(), "default", "hello", "assistant-1", "high")
	require.NoError(t, err)
	defer resp.Body.Close()
	var text string
	result, err := ReadStream(resp.Body, func(s, r string) error { text += s; return nil })
	require.NoError(t, err)
	require.Equal(t, "hello", text)
	require.Equal(t, "model-b", model)
	require.Equal(t, 1, refreshed)
	require.Equal(t, "rotated-cookie", saved.ClientCookie)
	require.Equal(t, 15, result.Usage.TotalTokens)
}

func TestLogiccRefreshRejectsWrongSessionAndRedirect(t *testing.T) {
	for _, status := range []int{200, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.com")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]string{"jwt": testToken("sess_other", time.Now().Add(time.Hour).Unix())})
			}))
			defer srv.Close()
			c, err := NewClient(Credentials{ClientCookie: "secret-cookie", SessionID: "sess_test"}, "")
			require.NoError(t, err)
			c.ClerkURL = srv.URL
			err = c.Refresh(context.Background())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-cookie")
			require.Empty(t, c.Credentials.Token)
		})
	}
}

func TestLogiccRetries401OnlyOnce(t *testing.T) {
	calls, refreshes := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tokens") {
			refreshes++
			_ = json.NewEncoder(w).Encode(map[string]string{"jwt": testToken("sess_test", time.Now().Add(time.Hour).Unix())})
			return
		}
		calls++
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c, err := NewClient(Credentials{Token: testToken("sess_test", time.Now().Add(time.Hour).Unix()), SessionID: "sess_test", ClientCookie: "cookie"}, "")
	require.NoError(t, err)
	c.BaseURL = srv.URL
	c.ClerkURL = srv.URL
	_, err = c.Models(context.Background())
	require.Error(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, refreshes)
}

func TestLogiccStreamCompletionAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		fail        bool
	}{
		{"complete", "data: {\"type\":\"text-delta\",\"delta\":\"hi\"}\r\n\r\ndata: {\"type\":\"finish\"}\r\n\r\ndata: [DONE]\n\n", false},
		{"truncated", "data: {\"type\":\"text-delta\",\"delta\":\"hi\"}\n\n", true},
		{"done only", "data: [DONE]\n\n", true},
		{"invalid", "data: nope\n\n", true},
		{"error", "data: {\"type\":\"error\",\"errorText\":\"secret\"}\n\n", true},
		{"abort", "data: {\"type\":\"abort\"}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadStream(strings.NewReader(tc.input), func(string, string) error { return nil })
			if tc.fail {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLogiccPromptAndTools(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{Model: "model", Messages: []apicompat.ChatMessage{{Role: "system", Content: json.RawMessage(`"be precise"`)}, {Role: "user", Content: json.RawMessage(`"read a file"`)}, {Role: "tool", ToolCallID: "call_old", Content: json.RawMessage(`"file contents"`)}}, Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "Read", Parameters: json.RawMessage(`{"type":"object"}`)}}}, ToolChoice: json.RawMessage(`"required"`)}
	prompt, err := BuildPrompt(req)
	require.NoError(t, err)
	require.Contains(t, prompt, "file contents")
	require.Contains(t, prompt, "call_old")
	require.Contains(t, prompt, "be precise")
	_, calls, err := ParseToolOutput(`{"content":null,"tool_calls":[{"name":"Read","arguments":{"path":"/tmp/a"}}]}`, req)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.JSONEq(t, `{"path":"/tmp/a"}`, calls[0].Function.Arguments)
	for _, output := range []string{`{"content":"no tool","tool_calls":[]}`, `{"tool_calls":[{"name":"Delete","arguments":{}}]}`, `{"tool_calls":[{"name":"Read","arguments":[]}]}`} {
		_, _, err := ParseToolOutput(output, req)
		require.Error(t, err)
	}
	req.Messages[0].Content = json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`)
	_, err = BuildPrompt(req)
	require.ErrorContains(t, err, "text content only")
}

func TestLogiccNestedUsageAndFailedFinish(t *testing.T) {
	result, err := ReadStream(strings.NewReader("data: {\"type\":\"finish\",\"totalUsage\":{\"inputTokens\":{\"total\":20},\"outputTokens\":{\"total\":4}}}\n\n"), func(string, string) error { return nil })
	require.NoError(t, err)
	require.Equal(t, 24, result.Usage.TotalTokens)
	_, err = ReadStream(strings.NewReader("data: {\"type\":\"finish\",\"finishReason\":\"error\"}\n\n"), func(string, string) error { return nil })
	require.Error(t, err)
}
