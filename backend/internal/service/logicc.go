package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logicc"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Serialize refresh/create per account, including across independent requests.
// Locks are bounded; streaming occurs after the lock is released.
var logiccAccountLocks [64]sync.Mutex

func withLogiccClient(ctx context.Context, repo AccountRepository, account *Account, run func(*logicc.Client) error) error {
	lock := &logiccAccountLocks[uint64(account.ID)%uint64(len(logiccAccountLocks))]
	lock.Lock()
	defer lock.Unlock()
	// Reload persisted rotating credentials after acquiring the account lock.
	if repo != nil {
		fresh, err := repo.GetByID(ctx, account.ID)
		if err != nil {
			return err
		}
		if fresh != nil {
			copy := *account
			copy.Credentials = fresh.Credentials
			account = &copy
		}
	}
	client, err := logicc.NewClient(logicc.Credentials{Token: account.GetCredential("access_token"), ClientCookie: account.GetCredential("client_cookie"), SessionID: account.GetCredential("session_id")}, resolveAccountProxyURL(account))
	if err != nil {
		return err
	}
	defer client.HTTP.CloseIdleConnections()
	client.Save = func(creds logicc.Credentials) error {
		updated := make(map[string]any, len(account.Credentials)+3)
		for k, v := range account.Credentials {
			updated[k] = v
		}
		updated["access_token"], updated["client_cookie"], updated["session_id"] = creds.Token, creds.ClientCookie, creds.SessionID
		if repo == nil {
			account.Credentials = updated
			return nil
		}
		return persistAccountCredentials(ctx, repo, account, updated)
	}
	return run(client)
}

func (s *AccountTestService) GetLogiccModels(ctx context.Context, account *Account) ([]logicc.Model, error) {
	return fetchLogiccModels(ctx, s.accountRepo, account)
}

func fetchLogiccModels(ctx context.Context, repo AccountRepository, account *Account) ([]logicc.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var models []logicc.Model
	err := withLogiccClient(ctx, repo, account, func(client *logicc.Client) error { var err error; models, err = client.Models(ctx); return err })
	return models, err
}

func logiccInference(ctx context.Context, repo AccountRepository, account *Account, model, prompt, effort string) (*http.Response, string, error) {
	var resp *http.Response
	var resolved string
	err := withLogiccClient(ctx, repo, account, func(client *logicc.Client) error {
		var err error
		resp, resolved, err = client.Infer(ctx, model, prompt, account.GetCredential("assistant_id"), effort)
		return err
	})
	return resp, resolved, err
}

func (s *AccountTestService) testLogiccAccountConnection(c *gin.Context, account *Account, model, prompt string) error {
	if model == "" {
		model = "default"
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Reply only: connection test successful."
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	resp, _, err := logiccInference(c.Request.Context(), s.accountRepo, account, resolveOpenAIForwardModel(account, model, ""), prompt, "")
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	defer resp.Body.Close()
	_, err = logicc.ReadStream(resp.Body, func(text, reasoning string) error {
		if text != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: text})
		}
		return nil
	})
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func decodeLogiccRequest(body []byte, protocol string) (*apicompat.ChatCompletionsRequest, error) {
	switch protocol {
	case "messages":
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "custom" {
				return nil, fmt.Errorf("Logicc supports client function tools only")
			}
		}
		return apicompat.AnthropicToChatCompletionsRequest(&req)
	case "responses":
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		if v := raw["previous_response_id"]; len(v) > 0 && string(v) != "null" && string(v) != `""` {
			return nil, fmt.Errorf("Logicc requires complete input history; previous_response_id is unsupported")
		}
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for _, tool := range req.Tools {
			if tool.Type != "function" {
				return nil, fmt.Errorf("Logicc Responses supports function tools only")
			}
		}
		return apicompat.ResponsesToChatCompletionsRequest(&req)
	default:
		var req apicompat.ChatCompletionsRequest
		err := json.Unmarshal(body, &req)
		return &req, err
	}
}

func (s *OpenAIGatewayService) forwardLogicc(ctx context.Context, c *gin.Context, account *Account, body []byte, mapped, protocol string) (*OpenAIForwardResult, error) {
	start := time.Now()
	writeError := func(status int, message string) {
		if protocol == "messages" {
			writeAnthropicError(c, status, "api_error", message)
		} else {
			writeChatCompletionsError(c, status, "api_error", message)
		}
	}
	if protocol == "responses" && c.Request != nil && strings.HasSuffix(c.Request.URL.Path, "/compact") {
		err := fmt.Errorf("Logicc does not support Responses compaction")
		writeError(http.StatusBadRequest, err.Error())
		return nil, err
	}
	req, err := decodeLogiccRequest(body, protocol)
	if err != nil {
		writeError(400, "Invalid Logicc request: "+err.Error())
		return nil, err
	}
	prompt, err := logicc.BuildPrompt(req)
	if err != nil {
		writeError(400, err.Error())
		return nil, err
	}
	billing := resolveOpenAIForwardModel(account, req.Model, mapped)
	SetActualOpenAIUpstreamEndpoint(c, "/api/chats")
	resp, resolved, err := logiccInference(ctx, s.accountRepo, account, billing, prompt, req.ReasoningEffort)
	if err != nil {
		var upstream *logicc.HTTPError
		if errors.As(err, &upstream) {
			writer := writeChatCompletionsError
			if protocol == "messages" {
				writer = writeAnthropicError
			}
			payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": upstream.Error()}})
			return s.handleCompatErrorResponse(&http.Response{StatusCode: upstream.Status, Header: upstream.Header, Body: io.NopCloser(strings.NewReader(string(payload)))}, c, account, writer, billing)
		}
		writeError(502, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	if billing == "default" {
		billing = resolved
	}
	return writeLogiccResponse(c, resp, req, billing, resolved, protocol, start)
}

func writeLogiccResponse(c *gin.Context, resp *http.Response, req *apicompat.ChatCompletionsRequest, billing, resolved, protocol string, start time.Time) (*OpenAIForwardResult, error) {
	writeError := func(status int, message string) {
		if protocol == "messages" {
			writeAnthropicError(c, status, "api_error", message)
		} else {
			writeChatCompletionsError(c, status, "api_error", message)
		}
	}
	var err error
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	if protocol == "responses" {
		id = "resp_" + uuid.NewString()
	}
	var firstToken *int
	var output, reasoning strings.Builder
	anthropicState := apicompat.NewChatCompletionsToAnthropicStreamState(req.Model)
	responsesState := apicompat.NewChatCompletionsToResponsesStreamState(req.Model)
	writeSSE := func(event string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		if event != "" {
			if _, err = fmt.Fprintf(c.Writer, "event: %s\n", event); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
		c.Writer.Flush()
		return err
	}
	chunk := func(delta apicompat.ChatDelta, finish *string, usage *apicompat.ChatUsage) error {
		part := &apicompat.ChatCompletionsChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []apicompat.ChatChunkChoice{{Delta: delta, FinishReason: finish}}, Usage: usage}
		switch protocol {
		case "messages":
			for _, e := range apicompat.ChatCompletionsChunkToAnthropicEvents(part, anthropicState) {
				if err := writeSSE(e.Type, e); err != nil {
					return err
				}
			}
		case "responses":
			for _, e := range apicompat.ChatCompletionsChunkToResponsesEvents(part, responsesState) {
				if err := writeSSE(e.Type, e); err != nil {
					return err
				}
			}
		default:
			return writeSSE("", part)
		}
		return nil
	}
	toolMode := logicc.HasTools(req)
	result, err := logicc.ReadStream(resp.Body, func(text, thought string) error {
		if firstToken == nil && (text != "" || thought != "") {
			ms := int(time.Since(start).Milliseconds())
			firstToken = &ms
		}
		if !req.Stream || toolMode {
			output.WriteString(text)
			reasoning.WriteString(thought)
			return nil
		}
		delta := apicompat.ChatDelta{}
		if text != "" {
			delta.Content = &text
		}
		if thought != "" {
			delta.ReasoningContent = &thought
		}
		return chunk(delta, nil, nil)
	})
	var calls []apicompat.ChatToolCall
	text := output.String()
	if err == nil && toolMode {
		text, calls, err = logicc.ParseToolOutput(text, req)
	}
	if err != nil {
		if c.Writer.Written() {
			_ = writeSSE("error", gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": err.Error()}})
		} else {
			writeError(502, err.Error())
		}
		return nil, err
	}
	if len(calls) > 0 {
		result.FinishReason = "tool_calls"
	}
	if req.Stream {
		if toolMode {
			for i := range calls {
				index := i
				calls[i].Index = &index
			}
			thought := reasoning.String()
			delta := apicompat.ChatDelta{Role: "assistant", Content: &text, ToolCalls: calls}
			if thought != "" {
				delta.ReasoningContent = &thought
			}
			err = chunk(delta, nil, nil)
		}
		if err == nil {
			err = chunk(apicompat.ChatDelta{}, &result.FinishReason, result.Usage)
		}
		if err == nil {
			switch protocol {
			case "messages":
				for _, e := range apicompat.FinalizeChatCompletionsAnthropicStream(anthropicState) {
					if err = writeSSE(e.Type, e); err != nil {
						break
					}
				}
			case "responses":
				for _, e := range apicompat.FinalizeChatCompletionsResponsesStream(responsesState) {
					if err = writeSSE(e.Type, e); err != nil {
						break
					}
				}
			default:
				_, err = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
				c.Writer.Flush()
			}
		}
	} else {
		content, _ := json.Marshal(text)
		completion := &apicompat.ChatCompletionsResponse{ID: id, Object: "chat.completion", Created: created, Model: req.Model, Choices: []apicompat.ChatChoice{{Message: apicompat.ChatMessage{Role: "assistant", Content: content, ToolCalls: calls, ReasoningContent: reasoning.String()}, FinishReason: result.FinishReason}}, Usage: result.Usage}
		switch protocol {
		case "messages":
			c.JSON(200, apicompat.ChatCompletionsResponseToAnthropic(completion, req.Model))
		case "responses":
			c.JSON(200, apicompat.ChatCompletionsResponseToResponses(completion, req.Model, nil, nil, false, nil))
		default:
			c.JSON(200, completion)
		}
	}
	if err != nil {
		return nil, err
	}
	usage := OpenAIUsage{}
	if result.Usage != nil {
		usage.InputTokens = result.Usage.PromptTokens
		usage.OutputTokens = result.Usage.CompletionTokens
	}
	return &OpenAIForwardResult{RequestID: id, ResponseID: id, Model: req.Model, BillingModel: billing, UpstreamModel: resolved, UpstreamEndpoint: "/api/chats", Usage: usage, Stream: req.Stream, Duration: time.Since(start), FirstTokenMs: firstToken}, nil
}
