package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOfficeCopilotTokenRefresher_CanRefresh(t *testing.T) {
	refresher := NewOfficeCopilotTokenRefresher()

	// 1. nil account
	require.False(t, refresher.CanRefresh(nil))

	// 2. 错误平台
	wrongPlatform := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{"refresh_token": "token"}}
	require.False(t, refresher.CanRefresh(wrongPlatform))

	// 3. 缺少 refresh_token
	missingToken := &Account{Platform: PlatformOfficeCopilot, Credentials: map[string]any{}}
	require.False(t, refresher.CanRefresh(missingToken))

	// 4. 正确情况
	valid := &Account{Platform: PlatformOfficeCopilot, Credentials: map[string]any{"refresh_token": "valid_token"}}
	require.True(t, refresher.CanRefresh(valid))
}

func TestOfficeCopilotTokenRefresher_NeedsRefresh(t *testing.T) {
	refresher := NewOfficeCopilotTokenRefresher()

	// 1. 刚签发，还有 2 小时才过期 -> 不需要刷新
	fresh := &Account{
		Platform: PlatformOfficeCopilot,
		Credentials: map[string]any{
			"refresh_token": "rt",
			"expires_at":    time.Now().Add(2 * time.Hour).Unix(),
		},
	}
	require.False(t, refresher.NeedsRefresh(fresh, 0))

	// 2. 还有 5 分钟过期 (< 15 分钟窗口) -> 需要刷新
	expiring := &Account{
		Platform: PlatformOfficeCopilot,
		Credentials: map[string]any{
			"refresh_token": "rt",
			"expires_at":    time.Now().Add(5 * time.Minute).Unix(),
		},
	}
	require.True(t, refresher.NeedsRefresh(expiring, 0))
}
