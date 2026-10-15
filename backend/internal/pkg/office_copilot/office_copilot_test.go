package office_copilot

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildCopilotPayload(t *testing.T) {
	messages := []InboundMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello world"},
	}

	payload, err := BuildCopilotPayload(messages, "copilot-auto")
	if err != nil {
		t.Fatalf("BuildCopilotPayload failed: %v", err)
	}

	if _, ok := payload["requestedModel"]; ok {
		t.Error("must not pretend ChatHub supports public API model selection")
	}

	content, ok := payload["message"].(map[string]any)["text"].(string)
	if !ok || !strings.Contains(content, "Hello world") {
		t.Errorf("content does not contain expected prompt: %v", content)
	}
}

func TestBuildCopilotPayloadModelSelection(t *testing.T) {
	for _, tc := range []struct{ model, tone string }{
		{"claude-opus-5-5", "Claude_Opus"}, {"claude-opus-5.5", "Claude_Opus"},
		{"claude-sonnet-5", "Claude_Sonnet"},
		{"gpt-6-sol", "Gpt_5_5_Reasoning"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			payload, err := BuildCopilotPayload([]InboundMessage{{Role: "user", Content: "hello"}}, tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if payload["tone"] != tc.tone || payload["source"] != "word" {
				t.Fatalf("wrong model route: %v", payload)
			}
			if _, exists := payload["conversationId"]; exists {
				t.Fatal("fresh Word request must not invent a conversation ID")
			}
			gpts := payload["gpts"].([]map[string]string)
			if len(gpts) != 1 || gpts[0]["id"] != "WordDraftingAgent" || gpts[0]["version"] != "helix" {
				t.Fatal("missing Word agent route")
			}
		})
	}
	for _, model := range []string{"claude-opus-4-6", "gpt-4o", "gpt-6.1-sol", "gpt-6-1-sol", "Gpt5Slot4", "Gpt5Slot6", "", "made-up-model"} {
		if _, err := BuildCopilotPayload([]InboundMessage{{Role: "user", Content: "hello"}}, model); err == nil {
			t.Fatalf("silently accepted unknown model %q", model)
		}
	}
}

func TestStreamProcessor(t *testing.T) {
	sp := NewStreamProcessor("copilot-gpt-4o")
	src := strings.NewReader("data: {\"content\": \"Hello\"}\ndata: {\"content\": \" World\"}\ndata: [DONE]\n")
	var dst bytes.Buffer

	err := sp.ProcessStream(src, &dst)
	if err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}

	out := dst.String()
	if !strings.Contains(out, "Hello") || !strings.Contains(out, " World") || !strings.Contains(out, "[DONE]") {
		t.Errorf("stream output missing expected tokens: %s", out)
	}
}

func TestStreamProcessorRejectsIncompleteAndErrorResponses(t *testing.T) {
	for _, input := range []string{
		"<html>upstream error</html>",
		"data: [DONE]\n",
		"data: {\"error\":{\"message\":\"denied\"}}\ndata: [DONE]\n",
		"data: {\"text\":\"partial\"}\n",
		"data: invalid-json\ndata: [DONE]\n",
	} {
		var dst bytes.Buffer
		if err := NewStreamProcessor("test").ProcessStream(strings.NewReader(input), &dst); err == nil {
			t.Errorf("expected failure for %q", input)
		}
		if strings.Contains(dst.String(), "[DONE]") {
			t.Errorf("failed inference emitted success terminator for %q", input)
		}
	}
}
