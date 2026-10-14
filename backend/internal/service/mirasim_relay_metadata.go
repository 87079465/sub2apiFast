package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// applyMirasimSessionHeader 在请求头白名单过滤后保留真实客户端会话标识。
// 此处只回填真实会话（客户端会话头、Claude Code 元数据等），不使用用户 ID 或账号 ID 冒充；
// 找不到真实会话时不在此处兜底，随机会话 ID 由签名前的 prepareMirasimRelayMetadata 统一生成，避免两处各生成不同值。
func applyMirasimSessionHeader(c *gin.Context, account *Account, headers http.Header, bodies ...[]byte) {
	if !account.IsMirasim() {
		return
	}
	sessionID := ExtractClientSessionID(c)
	if sessionID == "" {
		sessionID = mirasimSessionID(headers, bodies...)
	}
	if sessionID != "" {
		setHeaderRaw(headers, "x-mirasim-session", sessionID)
	}
}

// mirasimSessionID 复用现有会话头和 Claude Code 元数据解析规则。
func mirasimSessionID(headers http.Header, bodies ...[]byte) string {
	for _, key := range append([]string{"x-mirasim-session"}, clientSessionIDHeaders...) {
		if sessionID := sanitizeSessionID(getHeaderRaw(headers, key)); sessionID != "" {
			return sessionID
		}
	}
	for _, body := range bodies {
		if sessionID := sanitizeSessionID(gjson.GetBytes(body, "prompt_cache_key").String()); sessionID != "" {
			return sessionID
		}
		if metadata := ParseMetadataUserID(gjson.GetBytes(body, "metadata.user_id").String()); metadata != nil {
			if sessionID := sanitizeSessionID(metadata.SessionID); sessionID != "" {
				return sessionID
			}
		}
	}
	return ""
}

// prepareMirasimRelayMetadata 对齐官方 App 0.0.383 的 relay 推理请求。
// 会话、语言与调用标识在设备签名前写入请求头，随后随设备认证字段一起封装；额度查询不添加推理元数据。
// 参数：
//   - req: 发往上游的 HTTP 请求对象
//   - body: 请求体字节切片，用于回溯真实会话标识
// 返回值：
//   - error: 处理过程中的错误
func prepareMirasimRelayMetadata(req *http.Request, body []byte) error {
	if req.Method != http.MethodPost {
		return nil
	}
	var agent string
	switch {
	case strings.HasSuffix(req.URL.Path, "/v1/messages"):
		agent = "claude"
	case strings.HasSuffix(req.URL.Path, "/v1/responses"):
		agent = "codex"
	default:
		return nil
	}
	callID, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate mirasim call ID: %w", err)
	}
	// 步骤 1: 解析真实会话；找不到时兜底生成 mirasim_<uuid>，对齐 App 的会话 ID 前缀，避免封套缺失会话字段。
	sessionID := mirasimSessionID(req.Header, body)
	if sessionID == "" {
		sessionID = "mirasim_" + uuid.NewString()
	}
	// 步骤 2: 使用客户端提供的语言；网关没有 App 的 identity.accountId，不用登录用户 sub 冒充。
	locale := firstAcceptLanguage(req.Header.Get("Accept-Language"))
	// 步骤 3: 先移除所有大小写变体，避免入站/重试残留的同名元数据参与签名时产生不确定值。
	for key := range req.Header {
		switch strings.ToLower(key) {
		case "x-mirasim-session", "x-mirasim-agent", "x-mirasim-call", "x-mirasim-account", "x-mirasim-locale":
			delete(req.Header, key)
		}
	}
	// 步骤 4: 写入本次会话元数据；缺少语言或 App 本地账号标识时省略对应字段。
	req.Header.Set("x-mirasim-session", sessionID)
	req.Header.Set("x-mirasim-agent", agent)
	req.Header.Set("x-mirasim-call", callID.String())
	if locale != "" {
		req.Header.Set("x-mirasim-locale", locale)
	}

	// 官方 relay 使用设备票据鉴权，只删除 OAuth 专用 beta，保留其他能力声明。
	var betaTokens []string
	for key, values := range req.Header {
		if !strings.EqualFold(key, "anthropic-beta") {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				token = strings.TrimSpace(token)
				if token != "" && token != "oauth-2025-04-20" {
					betaTokens = append(betaTokens, token)
				}
			}
		}
		delete(req.Header, key)
	}
	if len(betaTokens) > 0 {
		req.Header.Set("anthropic-beta", strings.Join(betaTokens, ","))
	}
	return nil
}

// firstAcceptLanguage 从 Accept-Language 头解析首个语言标签，对齐 App 的 locale（如 zh-CN）。
// 参数：
//   - header: 原始 Accept-Language 头值，形如 "zh-CN,zh;q=0.9,en;q=0.8"
// 返回值：
//   - string: 首个语言标签（去除 q 权重）；为空或通配符 "*" 时返回空串
func firstAcceptLanguage(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	// 取第一段（逗号分隔），再去掉分号后的权重参数。
	first := strings.SplitN(header, ",", 2)[0]
	first = strings.SplitN(first, ";", 2)[0]
	first = strings.TrimSpace(first)
	if first == "" || first == "*" {
		return ""
	}
	return first
}
