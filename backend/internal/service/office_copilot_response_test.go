package service

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
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

func copilotTestEvents(text string) string {
	var source strings.Builder
	for _, event := range []office_copilot.StreamEvent{{Kind: office_copilot.EventReasoning, Text: "公开摘要"}, {Kind: office_copilot.EventText, Text: text}} {
		raw, _ := json.Marshal(event)
		source.WriteString("data: " + string(raw) + "\n\n")
	}
	return source.String() + "data: [DONE]\n\n"
}

func copilotResponseEvents(raw string) []gjson.Result {
	var events []gjson.Result
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "data: {") {
			events = append(events, gjson.Parse(strings.TrimPrefix(line, "data: ")))
		}
	}
	return events
}

func TestOfficeCopilotSummaryUsageAcrossProtocolsAndTools(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tools := range []bool{false, true} {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				answer := "answer"
				if tools {
					answer = `{"text":"","tool_calls":[{"name":"Read","arguments":{"path":"demo"}}]}`
				}
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(copilotTestEvents(answer)))}
				var result *OpenAIForwardResult
				var err error
				if tools {
					body, _ := json.Marshal(map[string]any{"stream": stream, "tools": []any{map[string]any{"name": "Read"}}})
					result, err = writeOfficeCopilotToolResponse(c, resp, body, "claude-opus-5-5", "claude-opus-5-5", anthropic, time.Now(), 123)
				} else {
					result, err = writeOfficeCopilotResponse(c, resp, "claude-opus-5-5", "claude-opus-5-5", stream, anthropic, time.Now(), 123)
				}
				require.NoError(t, err)
				require.Equal(t, "true", rec.Header().Get("X-Usage-Estimated"))
				require.Zero(t, result.Usage.InputTokens, "estimates must never enter billing")
				require.Zero(t, result.Usage.OutputTokens)
				require.Contains(t, rec.Body.String(), "公开摘要")
				require.NotContains(t, rec.Body.String(), "signature")
				var usage gjson.Result
				if stream {
					var initial, final gjson.Result
					for _, event := range copilotResponseEvents(rec.Body.String()) {
						if event.Get("type").String() == "message_start" {
							initial = event
						}
						if event.Get("usage").Exists() {
							final = event
						}
					}
					usage = final
					if anthropic {
						require.Equal(t, int64(123), initial.Get("message.usage.input_tokens").Int())
						require.Contains(t, rec.Body.String(), "thinking_delta")
						require.Contains(t, rec.Body.String(), "message_stop")
						if tools {
							require.Equal(t, "tool_use", final.Get("delta.stop_reason").String())
						}
					} else {
						require.Contains(t, rec.Body.String(), "reasoning_content")
						require.Empty(t, final.Get("choices").Array())
						require.True(t, strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n"))
					}
				} else {
					usage = gjson.Parse(rec.Body.String())
					if anthropic {
						require.Equal(t, "thinking", usage.Get("content.0.type").String())
						if tools {
							require.Equal(t, "tool_use", usage.Get("content.1.type").String())
						}
					} else {
						require.Equal(t, "公开摘要", usage.Get("choices.0.message.reasoning_content").String())
					}
				}
				require.True(t, usage.Get("usage_estimate.estimated").Bool())
				require.Equal(t, int64(123), usage.Get("usage_estimate.input_tokens").Int())
				require.Positive(t, usage.Get("usage_estimate.output_tokens").Int())
				if anthropic {
					require.Equal(t, usage.Get("usage_estimate.output_tokens").Int(), usage.Get("usage.output_tokens").Int())
				} else {
					require.Equal(t, usage.Get("usage_estimate.output_tokens").Int(), usage.Get("usage.completion_tokens").Int())
				}
			}
		}
	}
}

func TestOfficeCopilotSummaryRoundtripWithToolResult(t *testing.T) {
	request, err := officeCopilotRequest([]byte(`{"model":"claude-opus-5-5","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"公开摘要"},{"type":"tool_use","id":"c1","name":"Read","input":{"path":"demo"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"file content"}]}]}`), true)
	require.NoError(t, err)
	require.Len(t, request.Messages, 2)
	raw, err := json.Marshal(request.Messages)
	require.NoError(t, err)
	require.Contains(t, string(raw), "公开摘要")
	require.Contains(t, string(raw), "tool_use_id")
}

func TestOfficeCopilotRestrictionHTTPAndSSE(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, started := range []bool{false, true} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			if started {
				c.Writer.WriteHeaderNow()
			}
			err := &office_copilot.MeteringError{Restriction: office_copilot.MeteringRestriction{Code: "ImageGenInsufficientTokensThrottled", Category: "quota", Scope: "image_generation"}}
			officeCopilotResponseError(c, err, anthropic)
			if !started {
				require.Equal(t, 429, rec.Code)
			}
			require.Contains(t, rec.Body.String(), "rate_limit_error")
			require.Contains(t, rec.Body.String(), "image generation")
			require.NotContains(t, rec.Body.String(), "message_stop")
		}
	}
}
