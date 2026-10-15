package office_copilot

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatHubQuotaDoesNotConfuseOtherModelsOrMissingWithZero(t *testing.T) {
	state := chatHubStream{previous: map[string]string{}}
	_, err := state.process([]byte(`{"type":1,"target":"update","arguments":[{"throttling":{"maxNumUserMessagesInConversation":600,"numUserMessagesInConversation":1}}]}`), func(string) error { return nil })
	require.NoError(t, err)
	_, err = state.process([]byte(`{"type":2,"invocationId":"0","item":{"throttling":{"metering":{"GPT61SolQueryWeekly":{"remainingAllowance":0},"ClaudeOpusQueryDailyWord":{"remainingAllowance":0},"ClaudeOpusQueryWeeklyWord":{"remainingAllowance":12},"ClaudeOpusQueryDaily":{}}},"result":{"value":"Throttled"}}}`), func(string) error { return nil })
	require.Error(t, err) // Preserve quota even on a rejected inference.
	require.Equal(t, 600, *state.quota.ConversationLimit)
	require.Equal(t, 1, *state.quota.ConversationUsed)
	require.Len(t, state.quota.Meters, 2)
	require.Equal(t, 0, *state.quota.Meters["ClaudeOpusQueryDailyWord"].Remaining)
	require.Equal(t, 12, *state.quota.Meters["ClaudeOpusQueryWeeklyWord"].Remaining)
	state.quota.observe(json.RawMessage(`{"metering":{"GPT61SolQueryWeekly":{"remainingAllowance":0}}}`))
	require.Empty(t, state.quota.Meters, "new observations cannot retain stale Claude counts")
}

func TestChatHubMalformedQuotaDoesNotBreakInference(t *testing.T) {
	state := chatHubStream{previous: map[string]string{}}
	_, err := state.process([]byte(`{"type":2,"invocationId":"0","item":{"throttling":{"metering":{"unexpected":{"remainingAllowance":"invalid"}}},"result":{"value":"Success"},"messages":[{"author":"bot","text":"OK"}]}}`), func(string) error { return nil })
	require.NoError(t, err)
	done, err := state.process([]byte(`{"type":3,"invocationId":"0"}`), func(string) error { return nil })
	require.NoError(t, err)
	require.True(t, done)
}

func TestChatHubQuotaTimestampRequiresValidMetadata(t *testing.T) {
	var snapshot QuotaSnapshot
	for _, raw := range []string{
		`null`, `{}`, `{"metering":{}}`,
		`{"metering":{"GPT61SolQueryWeekly":{"remainingAllowance":0}}}`,
		`{"metering":{"ClaudeOpusQueryDailyWord":{"remainingAllowance":-1}}}`,
		`{"numUserMessagesInConversation":-1,"maxNumUserMessagesInConversation":0}`,
	} {
		snapshot.observe(json.RawMessage(raw))
		require.False(t, snapshot.HasObservation(), raw)
		require.True(t, snapshot.ObservedAt.IsZero(), raw)
	}
	snapshot.observe(json.RawMessage(`{"numUserMessagesInConversation":1,"maxNumUserMessagesInConversation":600}`))
	require.True(t, snapshot.HasObservation())
	observedAt := snapshot.ObservedAt
	snapshot.observe(json.RawMessage(`{}`))
	require.Equal(t, observedAt, snapshot.ObservedAt, "missing metadata must not refresh the observation time")
	require.Empty(t, snapshot.Meters, "conversation counts alone do not establish an account balance")
}
