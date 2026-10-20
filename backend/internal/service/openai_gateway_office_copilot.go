package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Both the admin test and public gateway exercise the same inference endpoint.
func officeCopilotInference(ctx context.Context, repo AccountRepository, account *Account, request office_copilot.InboundRequest, model string) (*http.Response, error) {
	request, err := office_copilot.ExtractImages(request)
	if err != nil {
		return nil, err
	}
	payload, err := office_copilot.BuildCopilotRequestPayload(request, model)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(account.GetCredential("access_token"))
	expires := officeCopilotAccountExpiresAt(account)
	if token == "" || (expires != nil && time.Until(*expires) < 2*time.Minute) {
		creds, err := NewOfficeCopilotTokenRefresher().Refresh(ctx, account)
		if err != nil {
			return nil, fmt.Errorf("Office Copilot token refresh failed: %w", err)
		}
		if err := persistAccountCredentials(ctx, repo, account, creds); err != nil {
			return nil, fmt.Errorf("persist Office Copilot credentials: %w", err)
		}
		token, _ = creds["access_token"].(string)
	}
	if token == "" {
		return nil, fmt.Errorf("Office Copilot access token is missing")
	}
	client := office_copilot.NewClient(token, resolveAccountProxyURL(account), account.GetCredential("scenario"))
	if err := client.AttachImages(ctx, payload, request.Images); err != nil {
		return nil, err
	}
	resolved, _, _ := office_copilot.ResolveModel(model)
	client.OnQuota = func(snapshot office_copilot.QuotaSnapshot) {
		saveOfficeCopilotUsage(ctx, repo, account.ID, resolved, snapshot)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := client.SendStreamRequest(ctx, "ChatHub", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if strings.TrimSpace(string(errorBody)) == "Connection ID required" {
			return nil, fmt.Errorf("Office Copilot inference requires a negotiated ChatHub connection (HTTP %d: Connection ID required); account connectivity alone does not verify inference", resp.StatusCode)
		}
		return nil, fmt.Errorf("Office Copilot inference returned HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("Office Copilot inference returned an unsupported response format")
	}
	return resp, nil
}

func (s *AccountTestService) testOfficeCopilotAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = "copilot-auto"
	}
	if strings.TrimSpace(prompt) == "" {
		// WordDraftingAgent can finish a bare greeting with Success but no
		// assistant message when its document tools are unavailable. Ask for
		// actual text so this probe verifies inference, not just completion.
		prompt = "Reply only with: connection test successful. This is a text-only API connection test; no Office document is connected. Do not call tools or edit a document."
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	upstreamModel := resolveOpenAIForwardModel(account, model, "")
	resp, err := officeCopilotInference(c.Request.Context(), s.accountRepo, account, office_copilot.InboundRequest{Messages: []office_copilot.InboundMessage{{Role: "user", Content: prompt}}}, upstreamModel)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	defer resp.Body.Close()
	err = office_copilot.ReadTextStream(resp.Body, func(text string) error {
		s.sendEvent(c, TestEvent{Type: "content", Text: text})
		return nil
	})
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// Preserve supported content blocks and their order, IDs and error status.
// Unsupported content is rejected rather than silently flattened or discarded.
func officeCopilotContent(content gjson.Result) (any, error) {
	if content.Type == gjson.String {
		return content.String(), nil
	}
	if !content.Exists() || content.Type == gjson.Null {
		return nil, nil
	}
	if !content.IsArray() {
		return "", fmt.Errorf("Office Copilot content must be text or text blocks")
	}
	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "tool_result":
			if _, err := officeCopilotContent(block.Get("content")); err != nil {
				return nil, err
			}
		case "tool_use":
		case "text", "thinking", "redacted_thinking":
		case "image", "image_url":
			// Validate through the same parser used to extract attachments. The
			// complete request is extracted below to enforce aggregate limits.
			if _, err := office_copilot.ExtractImages(office_copilot.InboundRequest{Messages: []office_copilot.InboundMessage{{Role: "user", Content: json.RawMessage("[" + block.Raw + "]")}}}); err != nil {
				return nil, err
			}
		default:
			return "", fmt.Errorf("Office Copilot does not yet support %s content blocks", block.Get("type").String())
		}
	}
	return json.RawMessage(content.Raw), nil
}

func officeCopilotMessages(body []byte, anthropic bool) ([]office_copilot.InboundMessage, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid JSON request")
	}
	var messages []office_copilot.InboundMessage
	if anthropic {
		system, err := officeCopilotContent(gjson.GetBytes(body, "system"))
		if err != nil {
			return nil, err
		}
		if system != nil {
			messages = append(messages, office_copilot.InboundMessage{Role: "system", Content: system})
		}
	}
	input := gjson.GetBytes(body, "messages")
	if !input.IsArray() || len(input.Array()) == 0 {
		return nil, fmt.Errorf("messages must be a nonempty array")
	}
	for _, m := range input.Array() {
		role := m.Get("role").String()
		if role != "user" && role != "assistant" && role != "system" && role != "developer" && role != "tool" {
			return nil, fmt.Errorf("Office Copilot does not support message role %s", role)
		}
		content, err := officeCopilotContent(m.Get("content"))
		if err != nil {
			return nil, err
		}
		messages = append(messages, office_copilot.InboundMessage{
			Role: role, Content: content,
			ToolCalls:  json.RawMessage(m.Get("tool_calls").Raw),
			ToolCallID: m.Get("tool_call_id").String(), Name: m.Get("name").String(),
			FunctionCall: json.RawMessage(m.Get("function_call").Raw),
		})
	}
	return messages, nil
}

func officeCopilotRequest(body []byte, anthropic bool) (office_copilot.InboundRequest, error) {
	body, err := filterOfficeCopilotTools(body)
	if err != nil {
		return office_copilot.InboundRequest{}, err
	}
	messages, err := officeCopilotMessages(body, anthropic)
	if err != nil {
		return office_copilot.InboundRequest{}, err
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.Type != gjson.Null && !tools.IsArray() {
		return office_copilot.InboundRequest{}, fmt.Errorf("Office Copilot tools must be an array")
	}
	var toolDefinitions json.RawMessage
	if tools.IsArray() && len(tools.Array()) > 0 {
		toolDefinitions = json.RawMessage(tools.Raw)
	}
	return office_copilot.ExtractImages(office_copilot.InboundRequest{
		Messages: messages, Tools: toolDefinitions,
		ToolChoice:       json.RawMessage(gjson.GetBytes(body, "tool_choice").Raw),
		ToolInstructions: officeCopilotToolInstruction(body),
	})
}

func (s *OpenAIGatewayService) forwardOfficeCopilotMessages(ctx context.Context, c *gin.Context, account *Account, body []byte, mapped string) (*OpenAIForwardResult, error) {
	return s.forwardOfficeCopilot(ctx, c, account, body, mapped, true)
}

func (s *OpenAIGatewayService) forwardOfficeCopilotChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte, mapped string) (*OpenAIForwardResult, error) {
	return s.forwardOfficeCopilot(ctx, c, account, body, mapped, false)
}

func (s *OpenAIGatewayService) forwardOfficeCopilot(ctx context.Context, c *gin.Context, account *Account, body []byte, mapped string, anthropic bool) (*OpenAIForwardResult, error) {
	start := time.Now()
	writeError := writeChatCompletionsError
	if anthropic {
		writeError = writeAnthropicError
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		writeError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("model is required")
	}
	originalBody := body
	body, err := filterOfficeCopilotTools(body)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	request, err := officeCopilotRequest(body, anthropic)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	billingModel := resolveOpenAIForwardModel(account, model, mapped)
	if _, _, err := office_copilot.ResolveModel(billingModel); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	if len(request.Images) > 0 {
		_, tone, _ := office_copilot.ResolveModel(billingModel)
		if tone == "" {
			err := fmt.Errorf("Office Copilot images require claude-opus-5-5, claude-sonnet-5 or gpt-6-sol; copilot-auto uses an unverified Excel image route")
			writeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return nil, err
		}
	}
	SetActualOpenAIUpstreamEndpoint(c, "/m365Copilot/ChatHub")
	ctx = office_copilot.BeginDiagnostics(ctx, account.ID, originalBody, anthropic)
	resp, err := officeCopilotInference(ctx, s.accountRepo, account, request, billingModel)
	if err != nil {
		writeError(c, http.StatusBadGateway, "api_error", "Office Copilot inference failed; check the account inference test")
		return nil, err
	}
	defer resp.Body.Close()
	if officeCopilotToolInstruction(body) != "" {
		repair := func(invalidOutput string) (*http.Response, int, error) {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			corrected := officeCopilotToolRepairRequest(request, invalidOutput)
			retry, err := officeCopilotInference(ctx, s.accountRepo, account, corrected, billingModel)
			return retry, office_copilot.EstimateInput(corrected), err
		}
		return writeOfficeCopilotToolResponseWithRepair(c, resp, body, model, billingModel, anthropic, start, office_copilot.EstimateInput(request), repair)
	}
	return writeOfficeCopilotResponse(c, resp, model, billingModel, gjson.GetBytes(body, "stream").Bool(), anthropic, start, office_copilot.EstimateInput(request))
}

func writeOfficeCopilotResponse(c *gin.Context, resp *http.Response, model, billingModel string, stream, anthropic bool, start time.Time, inputEstimate ...int) (*OpenAIForwardResult, error) {
	sp := office_copilot.NewStreamProcessor(model)
	state := apicompat.NewChatCompletionsToAnthropicStreamState(model)
	input := 0
	if len(inputEstimate) > 0 {
		input = inputEstimate[0]
	}
	var output, reasoning strings.Builder
	var counter office_copilot.TextCounter
	var firstToken *int
	chunk := func(delta apicompat.ChatDelta) *apicompat.ChatCompletionsChunk {
		return &apicompat.ChatCompletionsChunk{ID: sp.ID, Object: "chat.completion.chunk", Created: sp.Created, Model: model, Choices: []apicompat.ChatChunkChoice{{Delta: delta}}}
	}
	err := office_copilot.ReadEventStream(resp.Body, func(event office_copilot.StreamEvent) error {
		if firstToken == nil {
			ms := int(time.Since(start).Milliseconds())
			firstToken = &ms
		}
		counter.Add(event.Text)
		if !stream {
			if event.Kind == office_copilot.EventReasoning {
				reasoning.WriteString(event.Text)
			} else {
				output.WriteString(event.Text)
			}
			return nil
		}
		officeCopilotStreamHeaders(c)
		delta := apicompat.ChatDelta{}
		if event.Kind == office_copilot.EventReasoning {
			delta.ReasoningContent = &event.Text
		} else {
			delta.Content = &event.Text
		}
		if anthropic {
			return officeCopilotWriteAnthropicEvents(c, apicompat.ChatCompletionsChunkToAnthropicEvents(chunk(delta), state), office_copilot.NewTokenEstimate(input, counter.Tokens()))
		}
		return officeCopilotWriteChunk(c, chunk(delta), nil)
	})
	if err != nil {
		officeCopilotResponseError(c, err, anthropic)
		return nil, err
	}
	estimate := office_copilot.NewTokenEstimate(input, counter.Tokens())
	if stream {
		if anthropic {
			state.InputTokens, state.OutputTokens = estimate.InputTokens, estimate.OutputTokens
			err = officeCopilotWriteAnthropicEvents(c, apicompat.FinalizeChatCompletionsAnthropicStream(state), estimate)
		} else {
			stop := "stop"
			final := chunk(apicompat.ChatDelta{})
			final.Choices[0].FinishReason = &stop
			err = officeCopilotWriteChunk(c, final, nil)
			if err == nil {
				final.Choices = []apicompat.ChatChunkChoice{}
				final.Usage = officeCopilotChatUsage(estimate)
				err = officeCopilotWriteChunk(c, final, &estimate)
			}
			if err == nil {
				_, err = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
				c.Writer.Flush()
			}
		}
		if err != nil {
			return nil, err
		}
	} else {
		text, _ := json.Marshal(output.String())
		completion := &apicompat.ChatCompletionsResponse{ID: sp.ID, Object: "chat.completion", Created: sp.Created, Model: model, Choices: []apicompat.ChatChoice{{Message: apicompat.ChatMessage{Role: "assistant", Content: text, ReasoningContent: reasoning.String()}, FinishReason: "stop"}}}
		officeCopilotWriteJSON(c, completion, anthropic, estimate)
	}
	// Client estimates are deliberately excluded from billable usage.
	upstreamModel, _, _ := office_copilot.ResolveModel(billingModel)
	return &OpenAIForwardResult{RequestID: sp.ID, ResponseID: sp.ID, Model: model, BillingModel: billingModel, UpstreamModel: upstreamModel, UpstreamEndpoint: "/m365Copilot/ChatHub", Stream: stream, Duration: time.Since(start), FirstTokenMs: firstToken}, nil
}
