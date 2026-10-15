package office_copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

const (
	// DefaultClientID 是 Microsoft 365 Copilot 官方应用客户端 ID（支持 FOCI 家族 Refresh Token 互换）。
	DefaultClientID = "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"

	// DeviceCodeClientID 是 Microsoft Office 官方第一方客户端 ID（用于发起 Device Code 授权获取 FOCI refresh_token）。
	DeviceCodeClientID = "d3590ed6-52b3-4102-aeff-aad2292ab01c"

	// DefaultTenantID 是微软消费者/通用租户 ID。
	DefaultTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"

	// DefaultTokenURL 是微软 OAuth 2.0 凭据刷新端点。
	DefaultTokenURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"

	// DefaultDeviceCodeURL 是微软 OAuth 2.0 设备码请求端点。
	DefaultDeviceCodeURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode"

	// DefaultScope 是访问 Substrate Sydney v2 推理网关的核心作用域。
	DefaultScope = "https://substrate.office.com/sydney/v2/.default offline_access openid profile"

	// DefaultScenario 是 Office Copilot 场景标识。
	DefaultScenario = "ExcelSidepaneWebPremiumConsumerCopilot"

	// SubstrateBaseURL 是底层统一服务基址。
	SubstrateBaseURL = "https://substrate.office.com"
)

// DeviceCodeResponse 表示微软设备码授权初始化响应。
type DeviceCodeResponse struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

func newOAuthHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if proxyURL != "" {
		if parsedProxy, parseErr := url.Parse(proxyURL); parseErr == nil {
			transport := &http.Transport{}
			_ = proxyutil.ConfigureTransportProxy(transport, parsedProxy)
			client.Transport = transport
		}
	}
	return client
}

// RequestDeviceCode 请求微软第一方客户端的设备登录码。
func RequestDeviceCode(ctx context.Context, proxyURL string) (*DeviceCodeResponse, error) {
	data := url.Values{}
	data.Set("client_id", DeviceCodeClientID)
	data.Set("scope", DefaultScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DefaultDeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建设备码请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := newOAuthHTTPClient(proxyURL, 20*time.Second).Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求微软设备码接口失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取设备码响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取微软设备码失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var dcResp DeviceCodeResponse
	if err := json.Unmarshal(bodyBytes, &dcResp); err != nil {
		return nil, fmt.Errorf("解析设备码响应失败: %w", err)
	}
	return &dcResp, nil
}

// ExchangeDeviceCode 轮询兑换设备码并自动通过 FOCI 互换为 Office Copilot (c0ab8ce9) 专属令牌。
func ExchangeDeviceCode(ctx context.Context, deviceCode, proxyURL string) (*TokenResponse, error) {
	if strings.TrimSpace(deviceCode) == "" {
		return nil, fmt.Errorf("device_code 不能为空")
	}

	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	data.Set("client_id", DeviceCodeClientID)
	data.Set("device_code", strings.TrimSpace(deviceCode))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DefaultTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建设备码兑换请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := newOAuthHTTPClient(proxyURL, 20*time.Second).Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求设备码兑换失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取设备码兑换响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if strings.Contains(string(bodyBytes), "authorization_pending") {
			return nil, fmt.Errorf("尚未完成浏览器授权，请先在打开的微软页面中输入设备验证码并允许登录")
		}
		return nil, fmt.Errorf("设备码兑换失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var initialToken TokenResponse
	if err := json.Unmarshal(bodyBytes, &initialToken); err != nil {
		return nil, fmt.Errorf("解析初始 Token 失败: %w", err)
	}

	// 利用 FOCI 家族互换机制，将 d3590ed6 的 refresh_token 互换为 c0ab8ce9 的专属 sydney.readwrite 令牌
	if initialToken.RefreshToken != "" {
		if fociToken, fociErr := RefreshToken(ctx, initialToken.RefreshToken, DefaultClientID, proxyURL); fociErr == nil {
			if fociToken.RefreshToken == "" {
				fociToken.RefreshToken = initialToken.RefreshToken
			}
			return fociToken, nil
		}
	}
	return &initialToken, nil
}

// RefreshToken 使用微软 Refresh Token 换取最新的 Access Token。
func RefreshToken(ctx context.Context, refreshToken, clientID, proxyURL string) (*TokenResponse, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token 不能为空")
	}
	if clientID == "" {
		clientID = DefaultClientID
	}

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("scope", DefaultScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DefaultTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建刷新请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	if proxyURL != "" {
		if parsedProxy, parseErr := url.Parse(proxyURL); parseErr == nil {
			transport := &http.Transport{}
			_ = proxyutil.ConfigureTransportProxy(transport, parsedProxy)
			client.Transport = transport
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求微软 Token 接口失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 Token 响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("微软 Token 刷新失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, fmt.Errorf("解析 Token 响应失败: %w", err)
	}

	return &tokenResp, nil
}
