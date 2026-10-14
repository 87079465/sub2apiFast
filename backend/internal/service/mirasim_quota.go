package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// mirasimQuotaRemaining 读取全模型共享的 30 天额度，到期时间以 Mirasim 首次调用起算的当期快照为准。
func (a *Account) mirasimQuotaRemaining(now time.Time) time.Duration {
	if !a.IsMirasim() {
		return 0
	}
	if schedulingPercentValue(a.Extra["mirasim_30d_used_percent"]) < 100 {
		return 0
	}
	reset := parseSchedulingResetAt(a.Extra["mirasim_30d_reset_at"])
	if reset != nil && reset.After(now) {
		return reset.Sub(now)
	}
	return 0
}

// persistMirasimExhaustedWindow 处理明确的 30 天额度耗尽错误，通过共享限流范围暂停所有模型。
// 重置时间仅使用对应窗口快照或上游 Retry-After；缺失时告警，不猜测冷却时长。
func (s *RateLimitService) persistMirasimExhaustedWindow(ctx context.Context, account *Account, headers http.Header, body []byte) bool {
	if !account.IsMirasim() {
		return false
	}
	code := extractUpstreamErrorCode(body)
	window, exhausted := strings.CutPrefix(code, "credit_exhausted_")
	if !exhausted {
		return false
	}
	if window != "30d" {
		return false
	}
	now := time.Now()
	reset := parseSchedulingResetAt(account.Extra["mirasim_"+window+"_reset_at"])
	if reset == nil || !reset.After(now) {
		reset = parseRetryAfterResetTime(headers, now)
	}
	if reset == nil || !reset.After(now) {
		slog.Warn("mirasim_quota_reset_missing", "account_id", account.ID, "window", window, "error_code", code)
		return true
	}
	scope := "mirasim:"+window
	// 同一窗口的较早响应不能缩短已经确认的冷却。
	if current := account.modelRateLimitResetAt(scope); current != nil && current.After(*reset) {
		reset = current
	}
	setAccountModelRateLimitSnapshot(account, scope, *reset, code, now)
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, scope, *reset, code); err != nil {
		slog.Error("mirasim_quota_limit_persist_failed", "account_id", account.ID, "window", window, "error", err)
		return true
	}
	slog.Warn("mirasim_quota_window_exhausted", "account_id", account.ID, "window", window, "reset_at", reset.UTC())
	return true
}
