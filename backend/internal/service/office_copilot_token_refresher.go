package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
)

const (
	officeCopilotRefreshWindow = 15 * time.Minute
)

// OfficeCopilotTokenRefresher 实现 Office Copilot 的 Token 自动刷新器。
type OfficeCopilotTokenRefresher struct{}

// NewOfficeCopilotTokenRefresher 创建 Office Copilot 令牌刷新器。
func NewOfficeCopilotTokenRefresher() *OfficeCopilotTokenRefresher {
	return &OfficeCopilotTokenRefresher{}
}

// CacheKey 返回分布式锁缓存键。
func (r *OfficeCopilotTokenRefresher) CacheKey(account *Account) string {
	if account == nil {
		return "token:office_copilot:unknown"
	}
	return fmt.Sprintf("token:office_copilot:%d", account.ID)
}

// CanRefresh 检查账户是否支持刷新。
func (r *OfficeCopilotTokenRefresher) CanRefresh(account *Account) bool {
	if account == nil {
		return false
	}
	if account.Platform != PlatformOfficeCopilot {
		return false
	}
	return account.GetCredential("refresh_token") != ""
}

// NeedsRefresh 检查是否需要刷新（临近过期）。
func (r *OfficeCopilotTokenRefresher) NeedsRefresh(account *Account, globalWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}

	expiresAt := officeCopilotAccountExpiresAt(account)
	if expiresAt == nil {
		return false
	}

	window := officeCopilotRefreshWindow
	if globalWindow > 0 && globalWindow < window {
		window = globalWindow
	}

	return time.Until(*expiresAt) < window
}

// Refresh 调用微软 OAuth 刷新接口换取新令牌。
func (r *OfficeCopilotTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}

	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		return nil, fmt.Errorf("account refresh_token is empty")
	}

	clientID := account.GetCredential("client_id")
	proxyURL := resolveAccountProxyURL(account)

	tokenResp, err := office_copilot.RefreshToken(ctx, refreshToken, clientID, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("office copilot refresh failed: %w", err)
	}

	newCreds := map[string]any{
		"access_token": tokenResp.AccessToken,
		"token_type":   tokenResp.TokenType,
		"expires_in":   tokenResp.ExpiresIn,
		"expires_at":   time.Now().Unix() + tokenResp.ExpiresIn,
	}

	if tokenResp.RefreshToken != "" {
		newCreds["refresh_token"] = tokenResp.RefreshToken
	}

	return MergeCredentials(account.Credentials, newCreds), nil
}

func officeCopilotAccountExpiresAt(account *Account) *time.Time {
	if account == nil || account.Credentials == nil {
		return nil
	}
	switch v := account.Credentials["expires_at"].(type) {
	case int64:
		t := time.Unix(v, 0)
		return &t
	case float64:
		t := time.Unix(int64(v), 0)
		return &t
	default:
		return nil
	}
}
