//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMirasimQuotaScopes 验证 30 天额度限制所有模型及别名，旧窗口不再阻断调度，到期后恢复。
func TestMirasimQuotaScopes(t *testing.T) {
	for _, window := range []string{"30d", "5h", "7d", "7d_claude", "7d_fable"} {
		for _, model := range []string{"gpt-6-astra", "claude-sonnet-5", "claude-fable-5-1", "fable-alias"} {
			t.Run(window+"/"+model, func(t *testing.T) {
				reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
				account := &Account{ID: 1103, Platform: PlatformMirasim, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"model_mapping": map[string]any{"fable-alias": "claude-fable-5-1"}},
					Extra: map[string]any{"mirasim_"+window+"_used_percent": 100.0, "mirasim_"+window+"_reset_at": reset}}
				blocked := window == "30d"
				require.Equal(t, !blocked, account.IsSchedulableForModel(model))
				require.Equal(t, blocked, account.GetModelRateLimitRemainingTime(model) > 0)
				require.True(t, account.IsSchedulable(), "模型窗口不能关闭账号调度开关")
				account.Extra["mirasim_"+window+"_reset_at"] = time.Now().Add(-time.Second).Format(time.RFC3339)
				require.True(t, account.IsSchedulableForModel(model), "重置后应恢复")
			})
		}
	}
}

// TestMirasimQuotaReactive429 验证快照尚未满但上游明确耗尽时，只冷却对应额度范围。
func TestMirasimQuotaReactive429(t *testing.T) {
	for _, window := range []string{"30d"} {
		t.Run(window, func(t *testing.T) {
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			account := &Account{ID: 1103, Platform: PlatformMirasim, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Extra: map[string]any{"mirasim_"+window+"_used_percent": 59.7, "mirasim_"+window+"_reset_at": reset.Format(time.RFC3339)}}
			repo := &anthropicWindowLimitRepo{}
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			body := []byte(fmt.Sprintf(`{"error":{"code":"credit_exhausted_%s","type":"rate_limit_error","message":"quota exhausted"}}`, window))
			svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body, "claude-fable-5-1")
			require.Equal(t, 1, repo.modelRateLimitCalls)
			require.Equal(t, "mirasim:"+window, repo.lastModelRateLimitScope)
			require.True(t, reset.Equal(repo.lastModelRateLimitReset))
			require.Zero(t, repo.rateLimitCalls, "不能误停整个账号")
			require.Zero(t, repo.tempUnschedCalls)
			require.False(t, account.IsSchedulableForModel("claude-fable-5-1"))
			require.False(t, account.IsSchedulableForModel("gpt-6-astra"))
			require.False(t, account.IsSchedulableForModel("claude-sonnet-5"))
		})
	}
}

// TestMirasimQuotaAccountSwitch 验证优先账号 30 天额度耗尽时，Claude 和 GPT 均切换到 1104。
func TestMirasimQuotaAccountSwitch(t *testing.T) {
	for _, window := range []string{"30d"} {
		t.Run(window, func(t *testing.T) {
			first := Account{ID: 1103, Platform: PlatformMirasim, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Extra: map[string]any{"mirasim_"+window+"_used_percent": 100.0, "mirasim_"+window+"_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
			second := Account{ID: 1104, Platform: PlatformMirasim, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1}
			svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{first, second}}}
			for _, model := range []string{"claude-fable-5-1", "gpt-6-astra"} {
				selected, err := svc.selectAccountForModelWithExclusions(context.Background(), nil, PlatformMirasim, "", model, nil, false, 0, "", false)
				require.NoError(t, err)
				want := int64(1104)
				require.Equal(t, want, selected.ID)
				// 粘性会话也必须重新检查额度，不能继续命中已耗尽账号。
				svc.cache = &stubGatewayCache{sessionBindings: map[string]int64{"openai:quota-session": 1103}}
				selected, err = svc.selectAccountForModelWithExclusions(context.Background(), nil, PlatformMirasim, "quota-session", model, nil, false, 1103, "", false)
				require.NoError(t, err)
				require.Equal(t, want, selected.ID)
				svc.concurrencyService = NewConcurrencyService(stubConcurrencyCache{})
				selection, err := svc.selectAccountWithLoadAwareness(context.Background(), nil, PlatformMirasim, "", model, nil, false, "", false)
				require.NoError(t, err)
				require.Equal(t, want, selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			}
		})
	}
}

// TestMirasimQuotaLegacyWindows 验证旧快照和旧限流不会误停新套餐，阈值仅使用当期 30 天额度。
func TestMirasimQuotaLegacyWindows(t *testing.T) {
	now := time.Now()
	reset := now.Add(30 * 24 * time.Hour)
	account := &Account{Platform: PlatformMirasim, Status: StatusActive, Schedulable: true, Extra: map[string]any{
		"mirasim_30d_used_percent": 80.0,
		"mirasim_30d_reset_at": reset.Format(time.RFC3339),
	}}
	for _, window := range []string{"5h", "7d", "7d_claude", "7d_fable"} {
		account.Extra["mirasim_"+window+"_used_percent"] = 100.0
		account.Extra["mirasim_"+window+"_reset_at"] = reset.Format(time.RFC3339)
		setAccountModelRateLimitSnapshot(account, "mirasim:"+window, reset, "credit_exhausted_"+window, now)
	}
	for _, model := range []string{"claude-fable-5-1", "claude-sonnet-5", "gpt-6-astra"} {
		require.True(t, account.IsSchedulableForModel(model))
		require.Zero(t, account.GetModelRateLimitRemainingTime(model))
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformMirasim: 80}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "30d", decision.Window)
	account.Extra["mirasim_30d_used_percent"] = 79.0
	require.False(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformMirasim: 80}, now).ShouldPause)
}

// TestMirasimQuotaReactiveResetEvidence 验证上游给出的重置时间及缺失时的明确处理。
func TestMirasimQuotaReactiveResetEvidence(t *testing.T) {
	for _, retryAfter := range []string{"60", ""} {
		t.Run(retryAfter, func(t *testing.T) {
			account := &Account{ID: 1103, Platform: PlatformMirasim, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
			repo := &anthropicWindowLimitRepo{}
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			header := http.Header{"Retry-After": []string{retryAfter}}
			require.True(t, svc.persistMirasimExhaustedWindow(context.Background(), account, header, []byte(`{"error":{"code":"credit_exhausted_30d"}}`)))
			require.Equal(t, retryAfter != "", repo.modelRateLimitCalls == 1)
			require.Zero(t, repo.rateLimitCalls)
			require.Equal(t, retryAfter == "", account.IsSchedulableForModel("gpt-6-astra"))
			require.False(t, svc.persistMirasimExhaustedWindow(context.Background(), account, header, []byte(`{"error":{"code":"30d"}}`)))
		})
	}
}
