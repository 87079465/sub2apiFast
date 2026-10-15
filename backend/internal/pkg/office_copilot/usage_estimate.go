package office_copilot

// TokenEstimate is a local heuristic for visible text, never upstream billing
// usage. It excludes image processing, hidden reasoning and Microsoft context.
type TokenEstimate struct {
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
	Source       string `json:"source"`
	Estimated    bool   `json:"estimated"`
	Scope        string `json:"scope"`
}

// TextCounter is chunk-boundary invariant, including for Chinese text.
type TextCounter struct{ ascii, other int }

func (c *TextCounter) Add(text string) {
	for _, r := range text {
		if r <= 127 {
			c.ascii++
		} else {
			c.other++
		}
	}
}

func (c TextCounter) Tokens() int { return (c.ascii+3)/4 + c.other }

func EstimateInput(request InboundRequest) int {
	// The extracted image bytes are excluded by construction. Estimate exactly
	// the text envelope sent to ChatHub, including roles, tools and adapter text.
	text, err := buildConversationPrompt(request)
	if err != nil {
		return 0
	}
	var counter TextCounter
	counter.Add(text)
	return counter.Tokens()
}

func NewTokenEstimate(input, output int) TokenEstimate {
	return TokenEstimate{InputTokens: input, OutputTokens: output, TotalTokens: input + output,
		Source: "local_character_estimate", Estimated: true,
		Scope: "visible_text_and_tool_envelope_excludes_images_hidden_reasoning_and_microsoft_context"}
}
