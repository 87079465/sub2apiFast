package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
)

const officeCopilotUsageKey = "office_copilot_usage"

// A read never performs inference: no verified standalone quota endpoint is
// available. Even force=true returns the last recorded upstream observation.
func officeCopilotUsage(account *Account) *UsageInfo {
	info := &UsageInfo{Source: "passive"}
	raw, err := json.Marshal(account.Extra[officeCopilotUsageKey])
	if err != nil {
		return info
	}
	var snapshot office_copilot.QuotaSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || !snapshot.HasObservation() {
		return info
	}
	info.OfficeCopilotQuota = &snapshot
	info.UpdatedAt = &snapshot.ObservedAt
	return info
}

func saveOfficeCopilotUsage(ctx context.Context, repo AccountRepository, accountID int64, model string, snapshot office_copilot.QuotaSnapshot) {
	if repo == nil || !snapshot.HasObservation() {
		return
	}
	snapshot.Model = model
	// Request cancellation should not discard quota reported in an error frame.
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := repo.UpdateExtra(updateCtx, accountID, map[string]any{officeCopilotUsageKey: snapshot}); err != nil {
		slog.Warn("Office Copilot quota snapshot persistence failed", "account_id", accountID)
	}
}
