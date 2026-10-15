package service

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOfficeCopilotToolBridge(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object"}}],"stream":true}`)
	for _, stream := range []bool{false, true} {
		request := body
		if !stream {
			request = []byte(strings.ReplaceAll(string(body), `"stream":true`, `"stream":false`))
		}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"text\":\"{\\\"text\\\":\\\"\\\",\\\"tool_calls\\\":[{\\\"name\\\":\\\"Read\\\",\\\"arguments\\\":{\\\"file_path\\\":\\\"/tmp/example.txt\\\"}}]}\"}\n\ndata: [DONE]\n\n"))}
		result, err := writeOfficeCopilotToolResponse(c, resp, request, "claude-opus-5-5", "claude-opus-5-5", true, time.Now())
		require.NoError(t, err)
		require.Equal(t, stream, result.Stream)
		if stream {
			require.Contains(t, rec.Body.String(), `"type":"tool_use"`)
			require.Contains(t, rec.Body.String(), `"type":"input_json_delta"`)
			require.Contains(t, rec.Body.String(), `"stop_reason":"tool_use"`)
		} else {
			require.Equal(t, "tool_use", gjson.Get(rec.Body.String(), "stop_reason").String())
			require.Equal(t, "Read", gjson.Get(rec.Body.String(), "content.0.name").String())
		}
	}
	for _, output := range []string{`{"text":"","tool_calls":[{"name":"Delete","arguments":{}}]}`, `{"text":"","tool_calls":[{"name":"Read","arguments":"bad"}]}`, `{}`, `{"malformed`} {
		_, err := parseOfficeCopilotToolResponse(output, body)
		require.Error(t, err)
	}
	refusal, err := parseOfficeCopilotToolResponse("I cannot perform this action.", body)
	require.NoError(t, err)
	require.Empty(t, refusal.Choices[0].Message.ToolCalls)
	require.Equal(t, "stop", refusal.Choices[0].FinishReason)
	messages, err := officeCopilotMessages([]byte(`{"tools":[{"name":"Read"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"Read","input":{"file_path":"/tmp/example.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"file contents"}]}]}`), true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Contains(t, string(messages[0].Content.(json.RawMessage)), "call1")
	require.Contains(t, string(messages[1].Content.(json.RawMessage)), "file contents")
}

func TestOfficeCopilotMixedToolEnvelope(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Read"}]}`)
	completion, err := parseOfficeCopilotToolResponse("我先检查模板。\n\n"+`{"text":"","tool_calls":[{"name":"Read","arguments":{"path":"example.xlsx"}}]}`, body)
	require.NoError(t, err)
	require.Equal(t, "tool_calls", completion.Choices[0].FinishReason)
	require.Len(t, completion.Choices[0].Message.ToolCalls, 1)
	require.Equal(t, "我先检查模板。", gjson.ParseBytes(completion.Choices[0].Message.Content).String())
}

func TestOfficeCopilotMalformedMixedToolEnvelopeIsNotPlainText(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Bash"}]}`)
	_, err := parseOfficeCopilotToolResponse("接下来检查两项。\n"+`{"text":"","tool_calls":[{"name":"Bash","arguments":{"command":"cd "$TMPDIR/demo" && pwd"}}]}`, body)
	require.Error(t, err, "malformed commands must never be returned as successful prose")
}
