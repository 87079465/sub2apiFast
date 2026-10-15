package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/gin-gonic/gin"
)

// Estimates are returned for client context displays, never assigned to the
// billable OpenAIForwardResult.Usage. Keep metadata beside every usage object.
func officeCopilotUsageHeaders(c *gin.Context) {
	c.Header("X-Usage-Source", "local_character_estimate")
	c.Header("X-Usage-Estimated", "true")
}

func officeCopilotChatUsage(e office_copilot.TokenEstimate) *apicompat.ChatUsage {
	return &apicompat.ChatUsage{PromptTokens: e.InputTokens, CompletionTokens: e.OutputTokens, TotalTokens: e.TotalTokens}
}

func officeCopilotWriteJSON(c *gin.Context, completion *apicompat.ChatCompletionsResponse, anthropic bool, estimate office_copilot.TokenEstimate) {
	officeCopilotUsageHeaders(c)
	completion.Usage = officeCopilotChatUsage(estimate)
	if anthropic {
		c.JSON(http.StatusOK, struct {
			*apicompat.AnthropicResponse
			Estimate office_copilot.TokenEstimate `json:"usage_estimate"`
		}{apicompat.ChatCompletionsResponseToAnthropic(completion, completion.Model), estimate})
	} else {
		c.JSON(http.StatusOK, struct {
			*apicompat.ChatCompletionsResponse
			Estimate office_copilot.TokenEstimate `json:"usage_estimate"`
		}{completion, estimate})
	}
}

func officeCopilotWriteAnthropicEvents(c *gin.Context, events []apicompat.AnthropicStreamEvent, estimate office_copilot.TokenEstimate) error {
	for _, event := range events {
		var metadata *office_copilot.TokenEstimate
		if event.Type == "message_start" {
			event.Message.Usage.InputTokens = estimate.InputTokens
			initial := office_copilot.NewTokenEstimate(estimate.InputTokens, 0)
			metadata = &initial
		} else if event.Type == "message_delta" {
			metadata = &estimate
		}
		data, err := json.Marshal(struct {
			apicompat.AnthropicStreamEvent
			Estimate *office_copilot.TokenEstimate `json:"usage_estimate,omitempty"`
		}{event, metadata})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Type, data); err != nil {
			return err
		}
	}
	c.Writer.Flush()
	return nil
}

func officeCopilotWriteChunk(c *gin.Context, chunk *apicompat.ChatCompletionsChunk, estimate *office_copilot.TokenEstimate) error {
	data, err := json.Marshal(struct {
		*apicompat.ChatCompletionsChunk
		Estimate *office_copilot.TokenEstimate `json:"usage_estimate,omitempty"`
	}{chunk, estimate})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
	return err
}

func officeCopilotStreamHeaders(c *gin.Context) {
	officeCopilotUsageHeaders(c)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
}

func officeCopilotResponseError(c *gin.Context, err error, anthropic bool) {
	status, kind := http.StatusBadGateway, "api_error"
	var restriction *office_copilot.MeteringError
	if errors.As(err, &restriction) && (restriction.Restriction.Category == "quota" || restriction.Restriction.Category == "capacity") {
		status, kind = http.StatusTooManyRequests, "rate_limit_error"
	}
	if !c.Writer.Written() {
		if anthropic {
			writeAnthropicError(c, status, kind, err.Error())
		} else {
			writeChatCompletionsError(c, status, kind, err.Error())
		}
		return
	}
	// Errors produced by the adapter contain no raw upstream message text.
	data, _ := json.Marshal(gin.H{"type": "error", "error": gin.H{"type": kind, "message": err.Error()}})
	if anthropic {
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", data)
	} else {
		fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	}
	c.Writer.Flush()
}
