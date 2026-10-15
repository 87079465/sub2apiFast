package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/stretchr/testify/require"
)

type officeUsageRepo struct {
	AccountRepository
	account *Account
	updates map[string]any
}

func (r *officeUsageRepo) GetByID(context.Context, int64) (*Account, error) { return r.account, nil }
func (r *officeUsageRepo) UpdateExtra(ctx context.Context, _ int64, updates map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.updates = updates
	return nil
}

func TestOfficeCopilotUsageReadNeverCallsAnthropicOrInference(t *testing.T) {
	account := &Account{ID: 63, Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth}
	repo := &officeUsageRepo{account: account}
	s := &AccountUsageService{accountRepo: repo} // No upstream fetchers configured.
	for _, force := range []bool{false, true} {
		info, err := s.GetUsage(context.Background(), account.ID, force)
		require.NoError(t, err)
		require.Equal(t, "passive", info.Source)
		require.Nil(t, info.OfficeCopilotQuota)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	zero := 0
	snapshot := office_copilot.QuotaSnapshot{ObservedAt: time.Now().UTC(), Meters: map[string]office_copilot.QuotaMeter{"ClaudeOpusQueryDailyWord": {Remaining: &zero}}}
	saveOfficeCopilotUsage(ctx, repo, account.ID, "claude-opus-5-5", snapshot)
	require.NotNil(t, repo.updates)
	// Exercise the JSON types returned by a database round trip.
	raw, err := json.Marshal(repo.updates)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &account.Extra))
	info, err := s.GetPassiveUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "claude-opus-5-5", info.OfficeCopilotQuota.Model)
	require.Equal(t, 0, *info.OfficeCopilotQuota.Meters["ClaudeOpusQueryDailyWord"].Remaining)
	require.NotNil(t, info.UpdatedAt)
	active, err := s.GetUsage(context.Background(), account.ID, true)
	require.NoError(t, err)
	require.Equal(t, info, active)
}

func TestOfficeCopilotUsageIgnoresEmptyFailedStream(t *testing.T) {
	old := map[string]any{"observed_at": "2026-10-13T00:00:00Z", "model": "claude-opus-5-5", "conversation_limit": 600}
	account := &Account{ID: 63, Extra: map[string]any{officeCopilotUsageKey: old}}
	repo := &officeUsageRepo{account: account}
	saveOfficeCopilotUsage(context.Background(), repo, account.ID, "gpt-6-sol", office_copilot.QuotaSnapshot{ObservedAt: time.Now().UTC()})
	require.Nil(t, repo.updates, "an empty failure must not replace the last observation")
	require.Equal(t, "claude-opus-5-5", officeCopilotUsage(account).OfficeCopilotQuota.Model)
	account.Extra[officeCopilotUsageKey] = office_copilot.QuotaSnapshot{ObservedAt: time.Now().UTC()}
	require.Nil(t, officeCopilotUsage(account).OfficeCopilotQuota, "ignore empty snapshots from older versions")
}

func TestOfficeCopilotRestrictionOnlySnapshotPersists(t *testing.T) {
	account := &Account{ID: 63, Platform: PlatformOfficeCopilot}
	repo := &officeUsageRepo{account: account}
	snapshot := office_copilot.QuotaSnapshot{ObservedAt: time.Now().UTC(), Restrictions: []office_copilot.MeteringRestriction{{Code: "ImageGenInsufficientTokensThrottled", Category: "quota", Scope: "image_generation"}}}
	saveOfficeCopilotUsage(context.Background(), repo, account.ID, "claude-opus-5-5", snapshot)
	raw, err := json.Marshal(repo.updates)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &account.Extra))
	info := officeCopilotUsage(account)
	require.NotNil(t, info.OfficeCopilotQuota)
	require.Len(t, info.OfficeCopilotQuota.Restrictions, 1)
	require.Empty(t, info.OfficeCopilotQuota.Meters)
	require.Nil(t, info.OfficeCopilotQuota.ConversationLimit)
}
