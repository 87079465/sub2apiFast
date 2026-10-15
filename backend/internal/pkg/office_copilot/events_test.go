package office_copilot

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestChatHubSeparatesSummaryProgressAndTools(t *testing.T) {
	var state chatHubStream
	var events []StreamEvent
	emit := func(e StreamEvent) error { events = append(events, e); return nil }
	for _, raw := range []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","messageId":"same","messageType":"Progress","contentOrigin":"ChainOfThoughtSummary","text":"检查"},{"author":"bot","messageId":"same","messageType":"Progress","text":"working"},{"author":"bot","contentType":"ToolCall","text":"dangerous native tool payload"},{"author":"user","text":"echo"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","messageId":"same","addToChainOfThought":true,"text":"检查完成"},{"author":"bot","messageId":"same","text":"answer"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"streamingMode":"Delta","messages":[{"author":"bot","messageId":"same","text":"!"}]}]}`,
		`{"type":2,"invocationId":"0","item":{"result":{"value":"Success"},"messages":[{"author":"bot","messageId":"same","text":"answer!"},{"author":"bot","messageId":"same","contentOrigin":"ChainOfThoughtSummary","text":"检查完成"}]}}`,
	} {
		_, err := state.processEvents([]byte(raw), emit)
		require.NoError(t, err)
	}
	done, err := state.processEvents([]byte(`{"type":3,"invocationId":"0"}`), emit)
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, []StreamEvent{{EventReasoning, "检查"}, {EventReasoning, "完成"}, {EventText, "answer"}, {EventText, "!"}}, events)
}

func TestEventStreamDoesNotTurnSummaryIntoAnswer(t *testing.T) {
	for _, source := range []string{
		"data: {\"kind\":\"reasoning\",\"text\":\"summary\"}\n\ndata: [DONE]\n\n",
		"data: {\"kind\":\"progress\",\"text\":\"working\"}\n\ndata: [DONE]\n\n",
		"data: {\"text\":\"partial\"}\n\n",
	} {
		require.Error(t, ReadEventStream(strings.NewReader(source), func(StreamEvent) error { return nil }))
	}
	var text strings.Builder
	require.NoError(t, ReadTextStream(strings.NewReader("data: {\"kind\":\"reasoning\",\"text\":\"summary\"}\n\ndata: {\"kind\":\"text\",\"text\":\"answer\"}\n\ndata: [DONE]\n\n"), func(s string) error { text.WriteString(s); return nil }))
	require.Equal(t, "answer", text.String())
}

func TestSummaryStageReplacementDoesNotAbortAnswer(t *testing.T) {
	var state chatHubStream
	var summary strings.Builder
	emit := func(e StreamEvent) error {
		if e.Kind == EventReasoning {
			summary.WriteString(e.Text)
		}
		return nil
	}
	for _, stage := range []string{"searching", "verifying", "verifying"} {
		raw, _ := json.Marshal(map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{"author": "bot", "messageId": "stage", "contentOrigin": "ChainOfThoughtSummary", "text": stage}}}}})
		_, err := state.processEvents(raw, emit)
		require.NoError(t, err)
	}
	require.Equal(t, "searching\nverifying", summary.String())
	_, err := state.processEvents([]byte(`{"type":2,"invocationId":"0","item":{"result":{"value":"Success"},"messages":[{"author":"bot","text":"answer"}]}}`), emit)
	require.NoError(t, err)
	done, err := state.processEvents([]byte(`{"type":3,"invocationId":"0"}`), emit)
	require.NoError(t, err)
	require.True(t, done)
}

func TestMeteringDenialWithSuccessAndWithText(t *testing.T) {
	for _, text := range []string{"", "text is available"} {
		var state chatHubStream
		frame, _ := json.Marshal(map[string]any{"type": 2, "invocationId": "0", "item": map[string]any{
			"messages": []any{map[string]any{"author": "bot", "text": text}},
			"result":   map[string]any{"value": "Success", "meteringInformation": []any{map[string]any{"meterError": "ImageGenInsufficientTokensThrottled", "hasAccess": false}}},
		}})
		_, err := state.processEvents(frame, func(StreamEvent) error { return nil })
		require.NoError(t, err)
		done, err := state.processEvents([]byte(`{"type":3,"invocationId":"0"}`), func(StreamEvent) error { return nil })
		if text == "" {
			var denial *MeteringError
			require.ErrorAs(t, err, &denial)
			require.Equal(t, "image_generation", denial.Restriction.Scope)
			require.False(t, done)
		} else {
			require.NoError(t, err)
			require.True(t, done)
		}
		require.True(t, state.quota.HasObservation())
		require.Nil(t, state.quota.ConversationLimit)
	}
}

func TestMeteringSanitizationAndOptionalMalformedMetadata(t *testing.T) {
	var q QuotaSnapshot
	for _, raw := range []string{`null`, `{}`, `[null,1,"invalid",{"hasAccess":"bad"}]`, `[{"hasAccess":true}]`} {
		q.observeMetering(json.RawMessage(raw))
		require.False(t, q.HasObservation())
	}
	q.observeMetering(json.RawMessage(`[{"meterError":"private-token-and-document-content","hasAccess":false}]`))
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-token")
	require.Equal(t, "unknown", q.Restrictions[0].Scope)
	require.Equal(t, "UpstreamMeteringRestriction", q.Restrictions[0].Code)
}

func TestUsageEstimateChunkInvariantAndExcludesImageBytes(t *testing.T) {
	var whole, chunks TextCounter
	whole.Add("abc中文123456")
	for _, part := range []string{"a", "bc中", "文123", "456"} {
		chunks.Add(part)
	}
	require.Equal(t, whole.Tokens(), chunks.Tokens())
	r := InboundRequest{Messages: []InboundMessage{{Role: "user", Content: "hello"}}}
	baseline := EstimateInput(r)
	r.Images = []ImageInput{{Data: make([]byte, 1024*1024)}}
	require.Equal(t, baseline, EstimateInput(r))
	r.Tools = json.RawMessage(`[{"name":"Read","input_schema":{"type":"object"}}]`)
	require.Greater(t, EstimateInput(r), baseline)
	e := NewTokenEstimate(EstimateInput(r), whole.Tokens())
	require.True(t, e.Estimated)
	require.Equal(t, e.InputTokens+e.OutputTokens, e.TotalTokens)
}
