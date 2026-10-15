package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Copilot's text endpoint does not expose Anthropic tool calling. This explicit
// JSON bridge preserves tool definitions and history, and only returns calls for
// names declared by the client. The gateway never executes a generated tool.
func officeCopilotToolInstruction(body []byte) string {
	tools := gjson.GetBytes(body, "tools")
	if len(tools.Array()) == 0 {
		return ""
	}
	return `Respond ONLY with a single valid JSON object of the form {"text":"assistant response", "tool_calls":[{"name":"declared tool name","arguments":{}}]}. Put all user-facing explanations and progress updates in text, never before or after the object. Use an empty tool_calls array for final answers. Use the tools and tool_choice fields in this conversation envelope. When tools are needed, return their name and argument object; do not claim to have executed them. Tool execution results will arrive in subsequent conversation messages. Do not wrap the JSON in markdown. JSON-escape quotes, backslashes and newlines inside every string, especially shell commands and embedded scripts. Preserve the user's task and system/developer instructions.`
}

func parseOfficeCopilotToolResponse(output string, body []byte) (*apicompat.ChatCompletionsResponse, error) {
	envelope, err := extractOfficeCopilotToolEnvelope(output)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		name := tool.Get("name").String()
		if name == "" {
			name = tool.Get("function.name").String()
		}
		if name != "" {
			allowed[name] = true
		}
	}
	text, _ := json.Marshal(envelope.Text)
	message := apicompat.ChatMessage{Role: "assistant", Content: text}
	for _, call := range envelope.Calls {
		if !allowed[call.Name] {
			return nil, fmt.Errorf("Office Copilot returned an undeclared tool")
		}
		if len(call.Arguments) == 0 || !gjson.ParseBytes(call.Arguments).IsObject() {
			return nil, fmt.Errorf("Office Copilot returned invalid tool arguments")
		}
		message.ToolCalls = append(message.ToolCalls, apicompat.ChatToolCall{ID: "call_" + office_copilot.GenerateUUID(), Type: "function", Function: apicompat.ChatFunctionCall{Name: call.Name, Arguments: string(call.Arguments)}})
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if (choice.String() == "none" || choice.Get("type").String() == "none") && len(message.ToolCalls) > 0 {
		return nil, fmt.Errorf("Office Copilot returned tools when tool_choice is none")
	}
	forcedName := choice.Get("name").String()
	if choice.Get("type").String() == "function" {
		forcedName = choice.Get("function.name").String()
	}
	if forcedName != "" {
		for _, call := range message.ToolCalls {
			if call.Function.Name != forcedName {
				return nil, fmt.Errorf("Office Copilot returned a tool different from tool_choice")
			}
		}
	}
	if (choice.String() == "required" || choice.Get("type").String() == "any" || forcedName != "") && len(message.ToolCalls) == 0 {
		return nil, fmt.Errorf("Office Copilot did not return the required tool call")
	}
	if envelope.Text == "" && len(message.ToolCalls) == 0 {
		return nil, fmt.Errorf("Office Copilot returned an empty tool response")
	}
	reason := "stop"
	if len(message.ToolCalls) > 0 {
		reason = "tool_calls"
	}
	return &apicompat.ChatCompletionsResponse{ID: "chatcmpl-" + office_copilot.GenerateUUID(), Object: "chat.completion", Created: time.Now().Unix(), Choices: []apicompat.ChatChoice{{Message: message, FinishReason: reason}}}, nil
}

type officeCopilotToolRepair func(invalidOutput string) (*http.Response, int, error)

func writeOfficeCopilotToolResponse(c *gin.Context, resp *http.Response, body []byte, model, billingModel string, anthropic bool, start time.Time, inputEstimate ...int) (*OpenAIForwardResult, error) {
	input := 0
	if len(inputEstimate) > 0 {
		input = inputEstimate[0]
	}
	return writeOfficeCopilotToolResponseWithRepair(c, resp, body, model, billingModel, anthropic, start, input, nil)
}

func writeOfficeCopilotToolResponseWithRepair(c *gin.Context, resp *http.Response, body []byte, model, billingModel string, anthropic bool, start time.Time, input int, repair officeCopilotToolRepair) (*OpenAIForwardResult, error) {
	var output, reasoning strings.Builder
	var counter office_copilot.TextCounter
	var firstToken *int
	read := func(response *http.Response) error {
		return office_copilot.ReadEventStream(response.Body, func(event office_copilot.StreamEvent) error {
			if firstToken == nil {
				ms := int(time.Since(start).Milliseconds())
				firstToken = &ms
			}
			counter.Add(event.Text)
			if event.Kind == office_copilot.EventReasoning {
				reasoning.WriteString(event.Text)
			} else {
				output.WriteString(event.Text)
			}
			return nil
		})
	}
	err := read(resp)
	if err != nil {
		officeCopilotResponseError(c, err, anthropic)
		return nil, err
	}
	completion, err := parseOfficeCopilotToolResponse(output.String(), body)
	var formatError *officeCopilotToolFormatError
	if repair != nil && errors.As(err, &formatError) {
		// Nothing has been sent to the client, so this retry cannot duplicate a
		// delivered tool call. One retry only; never infer shell/JSON escapes.
		resp.Body.Close()
		repaired, repairedInput, repairErr := repair(output.String())
		if repairErr != nil {
			err = repairErr
		} else {
			defer repaired.Body.Close()
			output.Reset()
			reasoning.Reset()
			counter = office_copilot.TextCounter{}
			firstToken = nil
			input = repairedInput
			if err = read(repaired); err == nil {
				completion, err = parseOfficeCopilotToolResponse(output.String(), body)
			}
		}
	}
	if err != nil {
		officeCopilotResponseError(c, err, anthropic)
		return nil, err
	}
	completion.Model = model
	completion.Choices[0].Message.ReasoningContent = reasoning.String()
	estimate := office_copilot.NewTokenEstimate(input, counter.Tokens())
	stream := gjson.GetBytes(body, "stream").Bool()
	if !stream {
		officeCopilotWriteJSON(c, completion, anthropic, estimate)
	} else {
		officeCopilotStreamHeaders(c)
		choice := completion.Choices[0]
		text := gjson.ParseBytes(choice.Message.Content).String()
		reasoningText := choice.Message.ReasoningContent
		calls := choice.Message.ToolCalls
		for i := range calls {
			index := i
			calls[i].Index = &index
		}
		chunk := &apicompat.ChatCompletionsChunk{ID: completion.ID, Object: "chat.completion.chunk", Created: completion.Created, Model: model, Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Role: "assistant", Content: &text, ReasoningContent: &reasoningText, ToolCalls: calls}, FinishReason: &choice.FinishReason}}}
		if anthropic {
			state := apicompat.NewChatCompletionsToAnthropicStreamState(model)
			events := apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, state)
			state.InputTokens, state.OutputTokens = estimate.InputTokens, estimate.OutputTokens
			events = append(events, apicompat.FinalizeChatCompletionsAnthropicStream(state)...)
			err = officeCopilotWriteAnthropicEvents(c, events, estimate)
		} else {
			err = officeCopilotWriteChunk(c, chunk, nil)
			if err == nil {
				chunk.Choices = []apicompat.ChatChunkChoice{}
				chunk.Usage = officeCopilotChatUsage(estimate)
				err = officeCopilotWriteChunk(c, chunk, &estimate)
			}
			if err == nil {
				_, err = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
				c.Writer.Flush()
			}
		}
		if err != nil {
			return nil, err
		}
	}
	upstreamModel, _, _ := office_copilot.ResolveModel(billingModel)
	return &OpenAIForwardResult{RequestID: completion.ID, ResponseID: completion.ID, Model: model, BillingModel: billingModel, UpstreamModel: upstreamModel, UpstreamEndpoint: "/m365Copilot/ChatHub", Stream: stream, Duration: time.Since(start), FirstTokenMs: firstToken}, nil
}
