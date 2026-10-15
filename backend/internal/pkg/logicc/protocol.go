package logicc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
	"io"
	"strings"
)

// BuildPrompt preserves the complete conversation in each fresh upstream chat.
// Logicc exposes a text prompt, not an OpenAI messages or native client-tools API.
func BuildPrompt(req *apicompat.ChatCompletionsRequest) (string, error) {
	if req.Model == "" || len(req.Messages) == 0 {
		return "", fmt.Errorf("model and nonempty messages are required")
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return "", fmt.Errorf("unsupported Logicc message role")
		}
		if len(m.Content) == 0 || string(m.Content) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			continue
		}
		var parts []apicompat.ChatContentPart
		if json.Unmarshal(m.Content, &parts) != nil {
			return "", fmt.Errorf("invalid message content")
		}
		for _, p := range parts {
			if p.Type != "text" {
				return "", fmt.Errorf("Logicc supports text content only")
			}
		}
	}
	if len(req.Functions) > 0 || len(req.FunctionCall) > 0 {
		return "", fmt.Errorf("use tools and tool_choice instead of legacy functions")
	}
	for _, tool := range req.Tools {
		if tool.Type != "function" || tool.Function == nil || tool.Function.Name == "" {
			return "", fmt.Errorf("Logicc supports named function tools only")
		}
	}
	choice := strings.TrimSpace(string(req.ToolChoice))
	if choice != "" && choice != "null" && choice != `"auto"` && choice != `"none"` && choice != `"required"` {
		var named struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(req.ToolChoice, &named) != nil || named.Type != "function" || !hasTool(req, named.Function.Name) {
			return "", fmt.Errorf("invalid tool_choice")
		}
	}
	if choice == `"required"` && len(req.Tools) == 0 {
		return "", fmt.Errorf("required tool_choice needs tools")
	}
	history, _ := json.Marshal(req.Messages)
	prompt := "Continue this conversation. Return only the next assistant response. Conversation:\n" + string(history)
	if HasTools(req) {
		definitions, _ := json.Marshal(req.Tools)
		prompt = "Return only a JSON object with this format: {\"content\":\"answer or null\",\"tool_calls\":[{\"name\":\"declared function\",\"arguments\":{}}]}. The client executes these functions. Do not execute them yourself or invent results. Use supplied tool results to continue. For a normal answer use an empty tool_calls array. Request a tool and stop when needed. Arguments must match its JSON schema.\nDeclared tools: " + string(definitions) + "\nTool choice: " + choice
		if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
			prompt += "\nReturn at most one tool call."
		}
		prompt += "\nConversation:\n" + string(history)
	}
	limit := req.MaxCompletionTokens
	if limit == nil {
		limit = req.MaxTokens
	}
	if limit != nil && *limit > 0 {
		prompt += fmt.Sprintf("\nKeep the answer within approximately %d output tokens.", *limit)
	}
	if len(prompt) > 2<<20 {
		return "", fmt.Errorf("Logicc prompt exceeds 2 MiB")
	}
	return prompt, nil
}
func HasTools(req *apicompat.ChatCompletionsRequest) bool {
	return len(req.Tools) > 0 && string(req.ToolChoice) != `"none"`
}
func hasTool(req *apicompat.ChatCompletionsRequest, name string) bool {
	for _, t := range req.Tools {
		if t.Function != nil && t.Function.Name == name {
			return true
		}
	}
	return false
}

func ParseToolOutput(text string, req *apicompat.ChatCompletionsRequest) (string, []apicompat.ChatToolCall, error) {
	var envelope struct {
		Content *string `json:"content"`
		Calls   []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json\n") && strings.HasSuffix(text, "```") {
		text = strings.TrimSuffix(strings.TrimPrefix(text, "```json\n"), "```")
	}
	if json.Unmarshal([]byte(text), &envelope) != nil {
		return "", nil, fmt.Errorf("Logicc returned an invalid client-tool envelope")
	}
	var calls []apicompat.ChatToolCall
	for _, call := range envelope.Calls {
		var args map[string]any
		if !hasTool(req, call.Name) || json.Unmarshal(call.Arguments, &args) != nil || args == nil {
			return "", nil, fmt.Errorf("Logicc returned an undeclared tool or invalid arguments")
		}
		calls = append(calls, apicompat.ChatToolCall{ID: "call_" + uuid.NewString(), Type: "function", Function: apicompat.ChatFunctionCall{Name: call.Name, Arguments: string(call.Arguments)}})
	}
	choice := string(req.ToolChoice)
	if choice == `"required"` && len(calls) == 0 {
		return "", nil, fmt.Errorf("Logicc did not return the required tool call")
	}
	if strings.HasPrefix(choice, "{") {
		var named struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(req.ToolChoice, &named)
		if len(calls) == 0 {
			return "", nil, fmt.Errorf("Logicc did not return the selected tool")
		}
		for _, call := range calls {
			if call.Function.Name != named.Function.Name {
				return "", nil, fmt.Errorf("Logicc returned a different tool than requested")
			}
		}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls && len(calls) > 1 {
		return "", nil, fmt.Errorf("Logicc returned parallel tools when disabled")
	}
	content := ""
	if envelope.Content != nil {
		content = *envelope.Content
	}
	if content == "" && len(calls) == 0 {
		return "", nil, fmt.Errorf("Logicc returned an empty tool response")
	}
	return content, calls, nil
}

type StreamResult struct {
	Usage        *apicompat.ChatUsage
	FinishReason string
}

// AI SDK v6 may represent a count as either a number or {"total": number}.
type tokenCount int

func (n *tokenCount) UnmarshalJSON(data []byte) error {
	var value int
	if err := json.Unmarshal(data, &value); err == nil {
		*n = tokenCount(value)
		return nil
	}
	var object struct {
		Total *int `json:"total"`
	}
	if err := json.Unmarshal(data, &object); err != nil || object.Total == nil {
		return fmt.Errorf("invalid token count")
	}
	*n = tokenCount(*object.Total)
	return nil
}

// ReadStream requires the AI SDK finish event; EOF alone is not success.
// Tool events executed by Logicc are never sent to a client to execute again.
func ReadStream(reader io.Reader, emit func(text, reasoning string) error) (StreamResult, error) {
	result := StreamResult{FinishReason: "stop"}
	finished := false
	total := 0
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var data []string
	process := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if raw == "[DONE]" {
			return nil
		}
		var event struct {
			Type            string          `json:"type"`
			Delta           string          `json:"delta"`
			FinishReason    string          `json:"finishReason"`
			Usage           json.RawMessage `json:"usage"`
			TotalUsage      json.RawMessage `json:"totalUsage"`
			MessageMetadata struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"messageMetadata"`
		}
		if json.Unmarshal([]byte(raw), &event) != nil {
			return fmt.Errorf("invalid Logicc stream event")
		}
		switch event.Type {
		case "error", "abort":
			return fmt.Errorf("Logicc upstream stream failed")
		case "text-delta", "reasoning-delta":
			total += len(event.Delta)
			if total > 1<<20 {
				return fmt.Errorf("Logicc output exceeds 1 MiB")
			}
			if event.Type == "text-delta" {
				if err := emit(event.Delta, ""); err != nil {
					return err
				}
			} else {
				if err := emit("", event.Delta); err != nil {
					return err
				}
			}
		case "finish":
			if event.FinishReason == "error" || event.FinishReason == "abort" {
				return fmt.Errorf("Logicc inference did not complete successfully")
			}
			finished = true
			if event.FinishReason == "length" || event.FinishReason == "content_filter" {
				result.FinishReason = event.FinishReason
			}
		}
		for _, u := range []json.RawMessage{event.Usage, event.TotalUsage, event.MessageMetadata.Usage} {
			var usage struct {
				Input      *tokenCount `json:"inputTokens"`
				Output     *tokenCount `json:"outputTokens"`
				Prompt     *tokenCount `json:"prompt_tokens"`
				Completion *tokenCount `json:"completion_tokens"`
			}
			if json.Unmarshal(u, &usage) != nil {
				continue
			}
			if usage.Input == nil {
				usage.Input = usage.Prompt
			}
			if usage.Output == nil {
				usage.Output = usage.Completion
			}
			if usage.Input != nil && usage.Output != nil && *usage.Input >= 0 && *usage.Output >= 0 {
				result.Usage = &apicompat.ChatUsage{PromptTokens: int(*usage.Input), CompletionTokens: int(*usage.Output), TotalTokens: int(*usage.Input + *usage.Output)}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				return result, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("Logicc stream read failed")
	}
	if err := process(); err != nil {
		return result, err
	}
	if !finished {
		return result, fmt.Errorf("Logicc stream ended before finish")
	}
	return result, nil
}
