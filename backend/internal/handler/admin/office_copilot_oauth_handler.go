package admin

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// OfficeCopilotDeviceCodeRequest 请求生成 Office Copilot 设备登录码。
type OfficeCopilotDeviceCodeRequest struct {
	ProxyID *int64 `json:"proxy_id"`
}

// OfficeCopilotExchangeCodeRequest 请求兑换 Office Copilot 设备登录码或刷新令牌。
type OfficeCopilotExchangeCodeRequest struct {
	DeviceCode   string `json:"device_code"`
	RefreshToken string `json:"refresh_token"`
	ProxyID      *int64 `json:"proxy_id"`
}

// GenerateOfficeCopilotDeviceCode 生成微软 Office Copilot 设备码。
// POST /api/v1/admin/office-copilot/oauth/device-code
func (h *AccountHandler) GenerateOfficeCopilotDeviceCode(c *gin.Context) {
	var req OfficeCopilotDeviceCodeRequest
	_ = c.ShouldBindJSON(&req)

	proxyURL := ""
	if req.ProxyID != nil && *req.ProxyID > 0 {
		if proxy, err := h.adminService.GetProxy(c.Request.Context(), *req.ProxyID); err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	dcResp, err := office_copilot.RequestDeviceCode(c.Request.Context(), proxyURL)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	response.Success(c, dcResp)
}

// ExchangeOfficeCopilotToken 兑换设备码或校验 Refresh Token。
// POST /api/v1/admin/office-copilot/oauth/exchange-token
func (h *AccountHandler) ExchangeOfficeCopilotToken(c *gin.Context) {
	var req OfficeCopilotExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	proxyURL := ""
	if req.ProxyID != nil && *req.ProxyID > 0 {
		if proxy, err := h.adminService.GetProxy(c.Request.Context(), *req.ProxyID); err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	var (
		tokenResp *office_copilot.TokenResponse
		err       error
	)
	if req.RefreshToken != "" {
		tokenResp, err = office_copilot.RefreshToken(c.Request.Context(), req.RefreshToken, office_copilot.DefaultClientID, proxyURL)
	} else {
		tokenResp, err = office_copilot.ExchangeDeviceCode(c.Request.Context(), req.DeviceCode, proxyURL)
	}
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 28799
	}

	response.Success(c, gin.H{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"client_id":     office_copilot.DefaultClientID,
		"expires_in":    expiresIn,
		"expires_at":    time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
		"scenario":      office_copilot.DefaultScenario,
	})
}
