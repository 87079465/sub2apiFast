package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logicc"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLogiccProtocolResponses(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				upstream := `data: {"type":"text-delta","delta":"hello"}` + "\n\n" + `data: {"type":"finish","totalUsage":{"inputTokens":7,"outputTokens":2}}` + "\n\n"
				req := &apicompat.ChatCompletionsRequest{Model: "public-model", Stream: stream}
				result, err := writeLogiccResponse(c, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, req, "billing-model", "upstream-model", protocol, time.Now())
				require.NoError(t, err)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
				require.Equal(t, "upstream-model", result.UpstreamModel)
				require.Contains(t, rec.Body.String(), "hello")
				if stream {
					terminal := map[string]string{"chat": "data: [DONE]", "messages": "event: message_stop", "responses": "event: response.completed"}[protocol]
					require.Equal(t, 1, strings.Count(rec.Body.String(), terminal))
				} else {
					require.True(t, json.Valid(rec.Body.Bytes()))
				}
			})
		}
	}
}

func TestLogiccStreamFailureDoesNotComplete(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		_, err := writeLogiccResponse(c, &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"type\":\"text-delta\",\"delta\":\"partial\"}\n\n"))}, &apicompat.ChatCompletionsRequest{Model: "model", Stream: true}, "model", "model", protocol, time.Now())
		require.Error(t, err)
		require.Contains(t, rec.Body.String(), "event: error")
		require.NotContains(t, rec.Body.String(), "[DONE]")
		require.NotContains(t, rec.Body.String(), "event: message_stop")
		require.NotContains(t, rec.Body.String(), "event: response.completed")
	}
}

func TestLogiccToolResponseAndHistory(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			text, _ := json.Marshal(map[string]any{"type": "text-delta", "delta": `{"content":null,"tool_calls":[{"name":"Read","arguments":{"path":"a"}}]}`})
			req := &apicompat.ChatCompletionsRequest{Model: "model", Stream: true, Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "Read"}}}}
			_, err := writeLogiccResponse(c, &http.Response{Body: io.NopCloser(strings.NewReader("data: " + string(text) + "\n\ndata: {\"type\":\"finish\"}\n\n"))}, req, "model", "model", protocol, time.Now())
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), "Read")
			require.Contains(t, rec.Body.String(), "call_")
		})
	}
	request, err := decodeLogiccRequest([]byte(`{"model":"m","max_tokens":100,"system":"instruction","messages":[{"role":"user","content":"hello"}]}`), "messages")
	require.NoError(t, err)
	prompt, err := logicc.BuildPrompt(request)
	require.NoError(t, err)
	require.Contains(t, prompt, "instruction")
	_, err = decodeLogiccRequest([]byte(`{"model":"m","input":"hello","previous_response_id":"resp_old"}`), "responses")
	require.ErrorContains(t, err, "complete input history")
}

func TestLogiccPlatformIntegration(t *testing.T) {
	a := &Account{Platform: PlatformLogicc, Type: AccountTypeOAuth}
	require.True(t, a.IsOpenAICompatible())
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions))
	require.Equal(t, PlatformLogicc, NormalizeOpenAICompatiblePlatform(PlatformLogicc))
	require.Contains(t, AllowedQuotaPlatforms, PlatformLogicc)
	require.True(t, isConcreteRequestPlatform(PlatformLogicc))
	require.True(t, IsSensitiveCredentialKey("client_cookie"))
}
