package office_copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

// Client 是与 Microsoft Substrate Copilot 通信的 HTTP 客户端。
type Client struct {
	imageConversationID string
	BaseURL             string
	AccessToken         string
	ProxyURL            string
	Scenario            string
	HTTPClient          *http.Client
	// Called once after each established inference stream, including failures.
	OnQuota func(QuotaSnapshot)
}

// NewClient 创建新的 Office Copilot 客户端。
func NewClient(accessToken, proxyURL, scenario string) *Client {
	if scenario == "" {
		scenario = DefaultScenario
	}

	transport := &http.Transport{}
	if proxyURL != "" {
		if parsedProxy, err := url.Parse(proxyURL); err == nil {
			_ = proxyutil.ConfigureTransportProxy(transport, parsedProxy)
		}
	}

	return &Client{
		BaseURL:     SubstrateBaseURL,
		AccessToken: accessToken,
		ProxyURL:    proxyURL,
		Scenario:    scenario,
		HTTPClient:  &http.Client{Transport: transport, Timeout: 120 * time.Second},
	}
}

// applyStandardHeaders 注入微软 Office WebView2 标准环境指纹头。
func (c *Client) applyStandardHeaders(req *http.Request) {
	req.Header.Set("authorization", "Bearer "+c.AccessToken)
	req.Header.Set("x-scenario", c.Scenario)
	req.Header.Set("Referer", "https://fa000000124.resources.office.net/")
	req.Header.Set("Origin", "https://fa000000124.resources.office.net")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36 Edg/154.0.0.0")
	req.Header.Set("sec-ch-ua", `"Microsoft Edge";v="154", "Microsoft Edge WebView2";v="154", "Chromium";v="154"`)
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
}

// GetChats 获取当前用户的 Copilot 对话列表（可用于连通性探测和会话复用）。
func (c *Client) GetChats(ctx context.Context) (*GetChatsResponse, error) {
	reqPayload := map[string]any{
		"source":            "excel",
		"traceId":           "",
		"productThreadType": "Excel",
		"mergeWorkWebChats": true,
		"threadType":        "appchat",
	}
	payloadBytes, _ := json.Marshal(reqPayload)
	reqURL := fmt.Sprintf("%s/m365Copilot/GetChats?request=%s", c.BaseURL, url.QueryEscape(string(payloadBytes)))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构建 GetChats 请求失败: %w", err)
	}
	c.applyStandardHeaders(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GetChats 失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 GetChats 响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GetChats 失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var res GetChatsResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, fmt.Errorf("解析 GetChats 响应失败: %w", err)
	}
	return &res, nil
}

// SendStreamRequest 发起推理请求并返回可读流。
func (c *Client) SendStreamRequest(ctx context.Context, action string, body []byte) (*http.Response, error) {
	if action != "ChatHub" {
		return nil, fmt.Errorf("unsupported Office Copilot action")
	}
	return c.sendChatHub(ctx, body)
}
