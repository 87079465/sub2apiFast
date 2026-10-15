package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOfficeCopilotToolEnvelopeBoundariesAndArguments(t *testing.T) {
	// Command strings are opaque test data; none are executed.
	command := "cd \"$TMPDIR/demo\" && python3 -c 'print({\"x\": \"} {\"})'\nprintf '%s' \"中文\""
	raw, err := json.Marshal(map[string]any{"text": "检查模板", "tool_calls": []any{
		map[string]any{"name": "Bash", "arguments": map[string]any{"command": command}},
		map[string]any{"name": "Read", "arguments": map[string]any{"path": "demo.xlsx"}},
	}})
	require.NoError(t, err)
	body := []byte(`{"tools":[{"type":"function","function":{"name":"Bash"}},{"type":"function","function":{"name":"Read"}}]}`)
	for _, output := range []string{string(raw), "```json\n" + string(raw) + "\n```", "先核对字段。\n\n" + string(raw), "先核对字段。\n```JSON\n" + string(raw) + "\n```", "先核对字段。\r\n```\r\n" + string(raw) + "\r\n```"} {
		completion, err := parseOfficeCopilotToolResponse(output, body)
		require.NoError(t, err)
		message := completion.Choices[0].Message
		require.Len(t, message.ToolCalls, 2)
		require.Equal(t, command, gjson.Get(message.ToolCalls[0].Function.Arguments, "command").String())
		require.NotEqual(t, message.ToolCalls[0].ID, message.ToolCalls[1].ID)
		require.NotContains(t, gjson.ParseBytes(message.Content).String(), `"tool_calls"`)
		require.Contains(t, gjson.ParseBytes(message.Content).String(), "检查模板")
	}
}

func TestOfficeCopilotToolEnvelopeRejectsAmbiguousOrBrokenObjects(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Read"}]}`)
	valid := `{"text":"","tool_calls":[{"name":"Read","arguments":{"path":"demo"}}]}`
	for _, output := range []string{
		"prefix\n" + valid + "\n" + valid,
		"prefix\n" + valid + " trailing text",
		"prefix\n" + `{"wrapper":` + valid + `}`,
		"prefix\n" + `{"wrapper":` + "\n" + valid,
		"prefix\n```json\n" + valid,
		"prefix\n" + `{"text":"","tool_calls":"invalid"}`,
		"prefix\n" + `{"text":"","tool_calls":[`,
	} {
		_, err := parseOfficeCopilotToolResponse(output, body)
		var formatError *officeCopilotToolFormatError
		require.ErrorAs(t, err, &formatError, output)
	}
	// An example safely inside the text field must remain prose.
	raw, _ := json.Marshal(map[string]any{"text": "Example: " + valid, "tool_calls": []any{}})
	completion, err := parseOfficeCopilotToolResponse(string(raw), body)
	require.NoError(t, err)
	require.Empty(t, completion.Choices[0].Message.ToolCalls)
	require.Contains(t, gjson.ParseBytes(completion.Choices[0].Message.Content).String(), valid)
}

func TestOfficeCopilotMixedEnvelopeStillValidatesToolChoice(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"name":"Read"}],"tool_choice":"none"}`,
		`{"tools":[{"name":"Read"},{"name":"Bash"}],"tool_choice":{"type":"tool","name":"Bash"}}`,
		`{"tools":[{"type":"function","function":{"name":"Read"}},{"type":"function","function":{"name":"Bash"}}],"tool_choice":{"type":"function","function":{"name":"Bash"}}}`,
		`{"tools":[{"name":"Bash"}]}`,
	} {
		_, err := parseOfficeCopilotToolResponse("检查\n"+`{"text":"","tool_calls":[{"name":"Read","arguments":{"path":"demo"}}]}`, []byte(body))
		require.Error(t, err)
		var formatError *officeCopilotToolFormatError
		require.False(t, errors.As(err, &formatError), "validation failures must not trigger format repair")
	}
}

func officeCopilotTestToolStream(output string) *http.Response {
	raw, _ := json.Marshal(map[string]string{"kind": "text", "text": output})
	return &http.Response{Body: io.NopCloser(strings.NewReader("data: " + string(raw) + "\n\ndata: [DONE]\n\n"))}
}

func TestOfficeCopilotMixedToolResponseAcrossProtocols(t *testing.T) {
	output := "先检查文件。\n" + `{"text":"","tool_calls":[{"name":"Read","arguments":{"path":"demo"}}]}`
	for _, anthropic := range []bool{true, false} {
		for _, stream := range []bool{true, false} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body, _ := json.Marshal(map[string]any{"stream": stream, "tools": []any{map[string]any{"name": "Read"}}})
			result, err := writeOfficeCopilotToolResponse(c, officeCopilotTestToolStream(output), body, "claude-opus-5-5", "claude-opus-5-5", anthropic, time.Now(), 10)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, rec.Body.String(), "先检查文件。")
			require.NotContains(t, rec.Body.String(), `\"tool_calls\"`)
			if anthropic {
				require.Contains(t, rec.Body.String(), `"type":"tool_use"`)
			} else {
				require.Contains(t, rec.Body.String(), `"finish_reason":"tool_calls"`)
			}
		}
	}
}

func TestOfficeCopilotToolFormatRepairIsBoundedAndBuffered(t *testing.T) {
	broken := "检查后继续。\n" + `{"text":"","tool_calls":[{"name":"Bash","arguments":{"command":"cd "$TMPDIR/demo""}}]}`
	valid := `{"text":"","tool_calls":[{"name":"Bash","arguments":{"command":"pwd"}}]}`
	for _, anthropic := range []bool{true, false} {
		for _, stream := range []bool{true, false} {
			for _, repaired := range []string{valid, broken} {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				body, _ := json.Marshal(map[string]any{"stream": stream, "tools": []any{map[string]any{"name": "Bash"}}})
				attempts := 0
				repair := func(invalid string) (*http.Response, int, error) {
					attempts++
					require.Equal(t, broken, invalid)
					require.False(t, c.Writer.Written(), "no speculative commands or raw JSON may escape")
					return officeCopilotTestToolStream(repaired), 123, nil
				}
				result, err := writeOfficeCopilotToolResponseWithRepair(c, officeCopilotTestToolStream(broken), body, "claude-opus-5-5", "claude-opus-5-5", anthropic, time.Now(), 1, repair)
				require.Equal(t, 1, attempts)
				require.NotContains(t, rec.Body.String(), "TMPDIR")
				if repaired == valid {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Contains(t, rec.Body.String(), `"input_tokens":123`)
					require.Contains(t, rec.Body.String(), "pwd")
				} else {
					require.Error(t, err)
					require.Nil(t, result)
					require.Equal(t, 502, rec.Code)
					require.NotContains(t, rec.Body.String(), "message_stop")
					require.NotContains(t, rec.Body.String(), "[DONE]")
				}
			}
		}
	}
}

func TestOfficeCopilotToolRepairSkipsSemanticErrorsAndTransportFailures(t *testing.T) {
	for _, original := range []string{
		`{"text":"","tool_calls":[{"name":"Delete","arguments":{}}]}`,
		`{"text":"","tool_calls":[{"name":"Read","arguments":"bad"}]}`,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		repair := func(string) (*http.Response, int, error) { t.Fatal("unexpected repair"); return nil, 0, nil }
		_, err := writeOfficeCopilotToolResponseWithRepair(c, officeCopilotTestToolStream(original), []byte(`{"tools":[{"name":"Read"}]}`), "claude-opus-5-5", "claude-opus-5-5", true, time.Now(), 1, repair)
		require.Error(t, err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	original := &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"text\":\"partial\"}\n\n"))}
	repair := func(string) (*http.Response, int, error) {
		t.Fatal("transport failure must not trigger format repair")
		return nil, 0, nil
	}
	_, err := writeOfficeCopilotToolResponseWithRepair(c, original, []byte(`{"tools":[{"name":"Read"}]}`), "claude-opus-5-5", "claude-opus-5-5", true, time.Now(), 1, repair)
	require.Error(t, err)
}

func TestOfficeCopilotToolRepairRequestPreservesEvidence(t *testing.T) {
	request := office_copilot.InboundRequest{
		Messages: []office_copilot.InboundMessage{{Role: "system", Content: "Follow observed results"}, {Role: "user", Content: "Check demo"}},
		Tools:    json.RawMessage(`[{"name":"Read"}]`), ToolChoice: json.RawMessage(`{"type":"tool","name":"Read"}`),
		Images: []office_copilot.ImageInput{{FileName: "image-1.png", Data: []byte{1, 2}}}, ToolInstructions: "format rules",
	}
	repaired := officeCopilotToolRepairRequest(request, "invalid output")
	require.Len(t, request.Messages, 2)
	require.Len(t, repaired.Messages, 4)
	require.Equal(t, request.Messages, repaired.Messages[:2])
	require.Equal(t, "assistant", repaired.Messages[2].Role)
	require.Equal(t, request.Tools, repaired.Tools)
	require.Equal(t, request.ToolChoice, repaired.ToolChoice)
	require.Equal(t, request.Images, repaired.Images)
	require.Equal(t, request.ToolInstructions, repaired.ToolInstructions)
	require.Contains(t, repaired.Messages[3].Content, "No tool calls")
}
