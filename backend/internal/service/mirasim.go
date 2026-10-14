package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/tidwall/gjson"
)

// Mirasim 是 Mirasim 平台的端点与会话协议适配器。
// 支持自适应协议分流（Claude 模型走 Anthropic Messages，其他默认走 Responses），
// 原生设备票据换发与签名校验，以及 5h 窗口额度监控。

const (
	// DefaultMirasimBaseURL 是 Mirasim 默认中继服务地址。
	DefaultMirasimBaseURL = "https://relay.mirasim.ai"
	// DefaultMirasimAuthBaseURL 是 Mirasim 默认鉴权服务地址。
	DefaultMirasimAuthBaseURL = "https://auth.mirasim.ai"
	// DefaultMirasimClientVersion 是 Mirasim 客户端默认版本号，对齐当前官方 App。
	// 0.0.383 的签名核心与当前嵌入的 WASM 一致，换票和推理请求使用同一版本。
	DefaultMirasimClientVersion = "0.0.383"
	// DefaultMirasimTestModel 是管理员测试连接时的回退模型。
	DefaultMirasimTestModel = "claude-haiku-4-5-20251001"

	// 接口端点
	mirasimDeviceSessionPath = "/v1/device/session"
	mirasimLimitsPath        = "/v1/limits"

	// 请求头常量
	headerMirasimDevice = "x-mirasim-device"
	headerMirasimTS     = "x-mirasim-ts"
	headerMirasimNonce  = "x-mirasim-nonce"
	headerMirasimSig    = "x-mirasim-sig"
	headerMirasimClient = "x-mirasim-client"
	headerMirasimEnc    = "x-mirasim-enc"
	headerMirasimProbe  = "x-mirasim-probe"

	mirasimProtocolRulesKey         = "protocol_rules"
	maxMirasimProtocolRules         = 64
	maxMirasimProtocolPatternLength = 128
)

// MirasimTicketSession 缓存的设备票据状态。
type MirasimTicketSession struct {
	// Ticket 换发出的短期设备票据
	Ticket string
	// ExpiresAt 票据到期时间
	ExpiresAt time.Time
	// DeviceID 绑定的设备 ID
	DeviceID string
	// ProxyURL 换票时使用的代理，代理变更后不复用旧出口的票据。
	ProxyURL string
	// BaseURL 签发票据的中继地址，避免跨端点复用。
	BaseURL string
	// Fingerprint 绑定凭据、设备密钥、版本与出口的摘要，不保存明文凭据。
	Fingerprint [32]byte
}

// MirasimTicketManager 管理 Mirasim 设备票据的换发与内存缓存。
type MirasimTicketManager struct {
	mu      sync.RWMutex
	cache   map[int64]*MirasimTicketSession
	signer  *MirasimSigner
	// flights 保存每个账号当前有效的换票任务，删除后旧任务不能回填。
	flights map[int64]*mirasimTicketFlight
}

var (
	defaultMirasimTicketManager     *MirasimTicketManager
	defaultMirasimTicketManagerOnce sync.Once
)

// GetMirasimTicketManager 获取全局单例票据管理器。
func GetMirasimTicketManager() *MirasimTicketManager {
	defaultMirasimTicketManagerOnce.Do(func() {
		signer, _ := GetMirasimSigner()
		defaultMirasimTicketManager = &MirasimTicketManager{
			cache: make(map[int64]*MirasimTicketSession),
			signer: signer,
		}
	})
	return defaultMirasimTicketManager
}

// DefaultMirasimModelIDs 返回 Mirasim 平台常见模型目录，供目录未同步时回退。
// Claude 部分复用现有目录，同时包含 GPT 对话、图片模型与 Kimi K3，供测试连接与模型同步使用。
func DefaultMirasimModelIDs() []string {
	ids := make([]string, 0, len(claude.DefaultModels)+8)
	for _, m := range claude.DefaultModels {
		ids = append(ids, m.ID)
	}
	ids = append(ids,
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"kimi-k3",
	)
	return append(ids, DefaultMirasimImageModelIDs()...)
}

// DefaultMirasimImageModelIDs 返回本地生图入口支持的固定模型，中继对话目录可能不包含这些型号。
func DefaultMirasimImageModelIDs() []string {
	return []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}
}

// MirasimProtocolRule 表示模型匹配模式与目标协议的映射规则。
type MirasimProtocolRule struct {
	// Pattern 模型名称通配表达式（支持末尾 * 匹配）
	Pattern string `json:"pattern"`
	// Protocol 目标协议（chat_completions、anthropic 或 responses）
	Protocol string `json:"protocol"`
}

// DefaultMirasimProtocolRules 返回 Mirasim 内置自适应分流路由表。
// Claude 模型走 Anthropic Messages，其余（GPT/Kimi K3 等）默认走原生 OpenAI Responses 协议。
// relay 对 gpt-* 仅提供 /v1/responses 端点（实测 chat_completions 返回 404 use /v1/responses）。
func DefaultMirasimProtocolRules() []MirasimProtocolRule {
	return []MirasimProtocolRule{
		{Pattern: "claude-*", Protocol: APIProtocolAnthropic},
		{Pattern: "*", Protocol: APIProtocolResponses},
	}
}

func normalizeMirasimModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"mirasim/", "mirasim-"} {
		model = strings.TrimPrefix(model, prefix)
	}
	return model
}

func isNativeMirasimProtocol(protocol string) bool {
	switch protocol {
	case APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolResponses:
		return true
	default:
		return false
	}
}

func mirasimPatternMatches(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = normalizeMirasimModelID(model)
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return model == pattern
}

func matchMirasimProtocolRules(model string, rules []MirasimProtocolRule) string {
	for _, rule := range rules {
		if !isNativeMirasimProtocol(rule.Protocol) {
			continue
		}
		if mirasimPatternMatches(rule.Pattern, model) {
			return rule.Protocol
		}
	}
	return APIProtocolResponses
}

// MirasimModelProtocol 返回默认规则下指定模型的上游协议。
func MirasimModelProtocol(model string) string {
	return matchMirasimProtocolRules(model, DefaultMirasimProtocolRules())
}

func (a *Account) mirasimProtocolRules() ([]MirasimProtocolRule, bool) {
	if a == nil || a.Credentials == nil {
		return nil, false
	}
	raw, ok := a.Credentials[mirasimProtocolRulesKey]
	if !ok || raw == nil {
		return nil, false
	}
	rules, err := parseMirasimProtocolRules(raw)
	if err != nil {
		return nil, false
	}
	return rules, true
}

func parseMirasimProtocolRules(raw any) ([]MirasimProtocolRule, error) {
	items, err := mirasimProtocolRuleItems(raw)
	if err != nil {
		return nil, err
	}
	if len(items) > maxMirasimProtocolRules {
		return nil, fmt.Errorf("protocol_rules supports at most %d entries", maxMirasimProtocolRules)
	}
	rules := make([]MirasimProtocolRule, 0, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("protocol_rules[%d] must be an object", i)
		}
		pattern, _ := entry["pattern"].(string)
		protocol, _ := entry["protocol"].(string)
		pattern, err := normalizeMirasimProtocolPattern(pattern)
		if err != nil {
			return nil, fmt.Errorf("protocol_rules[%d]: %w", i, err)
		}
		protocol = strings.TrimSpace(protocol)
		if !isNativeMirasimProtocol(protocol) {
			return nil, fmt.Errorf("protocol_rules[%d]: 协议必须为 chat_completions、anthropic 或 responses", i)
		}
		rules = append(rules, MirasimProtocolRule{Pattern: pattern, Protocol: protocol})
	}
	return rules, nil
}

func mirasimProtocolRuleItems(raw any) ([]any, error) {
	switch items := raw.(type) {
	case []any:
		return items, nil
	case []map[string]any:
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, item)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("protocol_rules must be an array")
	}
}

func normalizeMirasimProtocolPattern(pattern string) (string, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if len(pattern) > maxMirasimProtocolPatternLength {
		return "", fmt.Errorf("pattern is too long")
	}
	if strings.ContainsAny(pattern, " \t") {
		return "", fmt.Errorf("pattern must not contain whitespace")
	}
	star := strings.Count(pattern, "*")
	if star > 1 || (star == 1 && !strings.HasSuffix(pattern, "*")) {
		return "", fmt.Errorf("pattern may use a single trailing * wildcard")
	}
	return pattern, nil
}

// NormalizeMirasimProtocolRulesCredentials 校验并规范化 credentials.protocol_rules。
func NormalizeMirasimProtocolRulesCredentials(credentials map[string]any) error {
	if credentials == nil {
		return nil
	}
	raw, ok := credentials[mirasimProtocolRulesKey]
	if !ok || raw == nil {
		return nil
	}
	rules, err := parseMirasimProtocolRules(raw)
	if err != nil {
		return infraerrors.New(http.StatusBadRequest, "INVALID_MIRASIM_PROTOCOL_RULES", err.Error())
	}
	encoded := make([]any, 0, len(rules))
	for _, rule := range rules {
		encoded = append(encoded, map[string]any{
			"pattern":  rule.Pattern,
			"protocol": rule.Protocol,
		})
	}
	credentials[mirasimProtocolRulesKey] = encoded
	return nil
}

// IsMirasim 检查账号是否为 Mirasim 平台。
func (a *Account) IsMirasim() bool {
	return a != nil && a.Platform == PlatformMirasim
}

// ResolveMirasimUpstreamProtocol 根据模型和账号规则解析实际路由协议。
func (a *Account) ResolveMirasimUpstreamProtocol(model string) string {
	if a == nil || !a.IsMirasim() {
		return ""
	}
	switch a.GetAPIProtocol() {
	case APIProtocolChatCompletions, APIProtocolAnthropic:
		return a.GetAPIProtocol()
	default:
		if rules, present := a.mirasimProtocolRules(); present {
			return matchMirasimProtocolRules(model, rules)
		}
		return matchMirasimProtocolRules(model, DefaultMirasimProtocolRules())
	}
}

// mirasimNativeProtocol 返回经过自适应解析后的原生协议。
func mirasimNativeProtocol(account *Account, model string) string {
	if account == nil {
		return APIProtocolResponses
	}
	proto := account.ResolveMirasimUpstreamProtocol(model)
	if proto == APIProtocolAnthropic {
		return APIProtocolAnthropic
	}
	if proto == APIProtocolResponses {
		return APIProtocolResponses
	}
	return APIProtocolChatCompletions
}

// mirasimDefaultChatBaseURL 返回 Mirasim 的 OpenAI 协议默认基址。
func (a *Account) mirasimDefaultChatBaseURL() string {
	if a != nil {
		if u := strings.TrimSpace(a.GetCredential("base_url")); u != "" {
			return u
		}
	}
	return DefaultMirasimBaseURL
}

// mirasimDefaultAnthropicBaseURL 返回 Mirasim 的 Anthropic 协议默认基址。
func (a *Account) mirasimDefaultAnthropicBaseURL() string {
	if a != nil {
		if baseURLs, ok := a.Credentials["api_base_urls"].(map[string]any); ok {
			if u, ok := baseURLs[APIProtocolAnthropic].(string); ok && strings.TrimSpace(u) != "" {
				return strings.TrimSpace(u)
			}
		}
		if u := strings.TrimSpace(a.GetCredential("base_url")); u != "" {
			return u
		}
	}
	return DefaultMirasimBaseURL
}

// ParseEd25519Seed 解析各种格式（PKCS#8 PEM、Base64、Hex、原始字节）的 Ed25519 私钥，返回 32 字节 seed。
func ParseEd25519Seed(keyStr string) ([]byte, error) {
	keyStr = strings.TrimSpace(keyStr)
	if keyStr == "" {
		return nil, fmt.Errorf("empty private key")
	}

	// 步骤 1: 尝试解析 PKCS#8 PEM 格式
	if strings.Contains(keyStr, "BEGIN PRIVATE KEY") {
		block, _ := pem.Decode([]byte(keyStr))
		if block == nil {
			return nil, fmt.Errorf("invalid pem block")
		}
		priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse pkcs8 failed: %w", err)
		}
		if edPriv, ok := priv.(ed25519.PrivateKey); ok {
			return edPriv.Seed(), nil
		}
		return nil, fmt.Errorf("private key is not ed25519")
	}

	// 步骤 2: 尝试标准 Base64 解码
	if b, err := base64.StdEncoding.DecodeString(keyStr); err == nil && len(b) == 32 {
		return b, nil
	}

	// 步骤 3: 尝试 URL Base64 解码
	if b, err := base64.RawURLEncoding.DecodeString(keyStr); err == nil && len(b) == 32 {
		return b, nil
	}

	// 步骤 4: 尝试十六进制 Hex 解码
	if b, err := hex.DecodeString(keyStr); err == nil && len(b) == 32 {
		return b, nil
	}

	// 步骤 5: 原始字符串正好为 32 字节
	if len(keyStr) == 32 {
		return []byte(keyStr), nil
	}

	return nil, fmt.Errorf("unsupported private key format or invalid length")
}

// DeriveSPKIPublicKey 将 Ed25519 公钥（32 字节）封装为 SPKI DER 格式并 Base64 编码，同时派生 22 字符 Device ID。
// 参数：
//   - pubBytes: 32 字节 Ed25519 公钥切片
// 返回值：
//   - string: SPKI DER 格式 Base64 编码的公钥
//   - string: 22 字符 base64url 设备标识符
//   - error: 错误信息
func DeriveSPKIPublicKey(pubBytes []byte) (string, string, error) {
	if len(pubBytes) != ed25519.PublicKeySize {
		return "", "", fmt.Errorf("invalid ed25519 public key size: %d", len(pubBytes))
	}
	spkiDER, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(pubBytes))
	if err != nil {
		return "", "", fmt.Errorf("marshal spki public key failed: %w", err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(spkiDER)
	deviceID := DeriveDeviceID(pubKeyB64)
	return pubKeyB64, deviceID, nil
}

// SignMirasimRelayRequest 为发送到 Mirasim 中继的 HTTP 请求附加设备签名头（x-mirasim-*）。
// 参数：
//   - ctx: 请求上下文
//   - account: Mirasim 账号对象
//   - req: 待签名的 HTTP 请求指针
//   - bodyBytes: 请求体字节切片（可为空）
//   - credential: 鉴权凭据（ticket 或 token）
// 返回值：
//   - error: 签名失败时返回错误
func SignMirasimRelayRequest(ctx context.Context, account *Account, req *http.Request, bodyBytes []byte, credential string) error {
	return signMirasimRelayRequestWithMetadata(ctx, account, req, bodyBytes, credential, nil)
}

// signMirasimRelayRequestWithMetadata 对最终请求体及明文会话元数据签名，供后续整体封装。
// 参数为上下文、账号、请求、最终请求体、票据及元数据；签名头写入 req，失败时返回错误。
func signMirasimRelayRequestWithMetadata(ctx context.Context, account *Account, req *http.Request, bodyBytes []byte, credential string, metadata map[string]string) error {
	if account == nil || req == nil {
		return nil
	}

	privKeyStr := account.GetMirasimPrivateKey()
	if privKeyStr == "" {
		return nil
	}

	seed, err := ParseEd25519Seed(privKeyStr)
	if err != nil {
		return fmt.Errorf("parse seed for sign: %w", err)
	}

	signer, err := GetMirasimSigner()
	if err != nil {
		return fmt.Errorf("get mirasim signer failed: %w", err)
	}

	pubBytes, err := signer.DerivePublicKey(ctx, seed)
	if err != nil {
		return fmt.Errorf("derive pubkey for sign: %w", err)
	}
	_, deviceID, err := DeriveSPKIPublicKey(pubBytes)
	if err != nil {
		return fmt.Errorf("derive spki pubkey for sign: %w", err)
	}

	clientVersion := strings.TrimSpace(account.GetCredential("client_version"))
	if clientVersion == "" {
		clientVersion = DefaultMirasimClientVersion
	}

	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := GenerateMirasimNonce()

	sig, err := signer.SignRelay(
		ctx,
		seed,
		req.Method,
		req.URL.Path,
		ts,
		nonce,
		deviceID,
		clientVersion,
		credential,
		metadata,
		bodyBytes,
	)
	if err != nil {
		return fmt.Errorf("sign relay request failed: %w", err)
	}

	req.Header.Set(headerMirasimDevice, deviceID)
	req.Header.Set(headerMirasimTS, ts)
	req.Header.Set(headerMirasimNonce, nonce)
	req.Header.Set(headerMirasimSig, sig)
	req.Header.Set(headerMirasimClient, clientVersion)
	return nil
}

// GenerateMirasimNonce 生成 12 字节随机数并转为 base64url 格式。
func GenerateMirasimNonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// mirasimQuotaURL 返回配额探测端点完整地址。
func mirasimQuotaURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultMirasimBaseURL
	}
	return base + mirasimLimitsPath
}

// mirasimAccountProxyURL 从账号读取代理；配置了代理但关联未加载时拒绝直连。
// 参数 account 为待请求的账号；返回代理 URL（未配置时为空）或配置错误。
func mirasimAccountProxyURL(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("nil account")
	}
	if account.ProxyID == nil {
		return "", nil
	}
	if account.Proxy == nil {
		return "", fmt.Errorf("mirasim account %d proxy %d is not loaded", account.ID, *account.ProxyID)
	}
	return account.Proxy.URL(), nil
}

// mirasimTicketFlight 表示同账号共享的换票任务，完成后通过 done 发布结果。
type mirasimTicketFlight struct {
	// fingerprint 为本次换票使用的配置摘要。
	fingerprint [32]byte
	// done 在结果写入后关闭，让等待者安全读取。
	done chan struct{}
	// ticket 和 err 保存一次换票的结果。
	ticket string
	err error
}

// Invalidate 清除账号票据及在途任务；ticket 非空时只清理匹配票据，避免迟到的 401 清除新票据。
// 参数为账号 ID 和被拒绝票据（主动刷新时传空）；无返回值，不取消其他调用者上下文。
func (m *MirasimTicketManager) Invalidate(accountID int64, ticket string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ticket != "" {
		session := m.cache[accountID]
		if session == nil || session.Ticket != ticket {
			return
		}
	}
	delete(m.cache, accountID)
	delete(m.flights, accountID)
}

// GetOrMintTicket 按账号及完整配置合并换票；ctx 只控制当前等待者，account 和 baseURL 指定凭据与端点。
// 返回有效票据或错误；换票独立限时，失效中的旧任务不得回填缓存或返回旧票据。
func (m *MirasimTicketManager) GetOrMintTicket(ctx context.Context, account *Account, baseURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	proxyURL, err := mirasimAccountProxyURL(account)
	if err != nil {
		return "", err
	}
	privKeyStr := strings.TrimSpace(account.GetMirasimPrivateKey())
	issuerToken := strings.TrimSpace(account.GetMirasimIssuerToken())
	if issuerToken == "" {
		issuerToken = strings.TrimSpace(account.GetCNAPIKey())
	}
	if privKeyStr == "" || issuerToken == "" {
		return "", fmt.Errorf("mirasim account %d requires private_key and issuer_token for a signed session", account.ID)
	}
	clientVersion := strings.TrimSpace(account.GetCredential("client_version"))
	if clientVersion == "" {
		clientVersion = DefaultMirasimClientVersion
	}
	baseURL = strings.TrimRight(baseURL, "/")
	encoded, _ := json.Marshal([]string{privKeyStr, issuerToken, clientVersion, proxyURL, baseURL})
	fingerprint := sha256.Sum256(encoded)
	accountID := account.ID
	m.mu.Lock()
	if session := m.cache[accountID]; session != nil && session.Fingerprint == fingerprint && session.Ticket != "" && session.ExpiresAt.After(time.Now().Add(time.Minute)) {
		m.mu.Unlock()
		return session.Ticket, nil
	}
	if m.cache == nil {
		m.cache = make(map[int64]*MirasimTicketSession)
	}
	if m.flights == nil {
		m.flights = make(map[int64]*mirasimTicketFlight)
	}
	flight := m.flights[accountID]
	if flight == nil || flight.fingerprint != fingerprint {
		delete(m.cache, accountID)
		flight = &mirasimTicketFlight{fingerprint: fingerprint, done: make(chan struct{})}
		m.flights[accountID] = flight
		// 只传递不可变快照；首个等待者取消不影响同账号其他请求。
		go func(f *mirasimTicketFlight) {
			mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			session, mintErr := m.mintTicket(mintCtx, privKeyStr, issuerToken, clientVersion, proxyURL, baseURL)
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.flights[accountID] != f {
				f.err = fmt.Errorf("mirasim ticket configuration changed or cache invalidated during mint")
			} else {
				delete(m.flights, accountID)
				f.err = mintErr
				if mintErr == nil {
					session.Fingerprint = fingerprint
					m.cache[accountID] = session
					f.ticket = session.Ticket
				}
			}
			close(f.done)
		}(flight)
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-flight.done:
		return flight.ticket, flight.err
	}
}

// mintTicket 使用凭据、版本、代理和端点快照完成一次签名换票；ctx 限制总耗时，返回会话或错误。
func (m *MirasimTicketManager) mintTicket(ctx context.Context, privKeyStr, issuerToken, clientVersion, proxyURL, baseURL string) (*MirasimTicketSession, error) {

	seed, err := ParseEd25519Seed(privKeyStr)
	if err != nil {
		return nil, fmt.Errorf("invalid mirasim private_key: %w", err)
	}

	// 步骤 3: 确定公钥与设备标识 Device ID
	pubBytes, err := m.signer.DerivePublicKey(ctx, seed)
	if err != nil {
		return nil, fmt.Errorf("derive ed25519 pubkey failed: %w", err)
	}
	pubKeyB64, derivedDevID, err := DeriveSPKIPublicKey(pubBytes)
	if err != nil {
		return nil, fmt.Errorf("derive spki pubkey failed: %w", err)
	}

	deviceID := derivedDevID

	// 步骤 4: 构造 POST /v1/device/session 请求体
	bodyObj := map[string]string{
		"publicKey": pubKeyB64,
		"deviceId":  deviceID,
	}
	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, err
	}

	mintURL := strings.TrimRight(baseURL, "/") + mirasimDeviceSessionPath
	mintTarget, err := url.Parse(mintURL)
	if err != nil {
		return nil, err
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := GenerateMirasimNonce()

	// 步骤 5: 调用 WASM 签名核心生成签名
	sig, err := m.signer.SignRelay(
		ctx,
		seed,
		http.MethodPost,
		mintTarget.Path,
		ts,
		nonce,
		deviceID,
		clientVersion,
		issuerToken,
		nil,
		bodyBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("sign device session request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mintURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if issuerToken != "" {
		req.Header.Set("Authorization", "Bearer "+issuerToken)
	}
	req.Header.Set(headerMirasimDevice, deviceID)
	req.Header.Set(headerMirasimTS, ts)
	req.Header.Set(headerMirasimNonce, nonce)
	req.Header.Set(headerMirasimSig, sig)
	req.Header.Set(headerMirasimClient, clientVersion)

	// 换票与模型请求使用相同的账号代理，配置错误或代理不可达时不回退直连。
	httpCli, err := httpclient.GetClient(httpclient.Options{
		ProxyURL: proxyURL,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("configure mirasim ticket proxy failed: %w", err)
	}
	resp, err := httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mint ticket request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read mint ticket response failed: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		kind, reason := classifyMirasimAuth(respBytes)
		if kind == mirasimAuthTicket {
			// 换票请求使用 issuer_token，这一阶段的 token_invalid 才代表登录凭据失效。
			kind, reason = mirasimAuthLogin, "Mirasim 登录凭据无法换取设备票据"
		}
		return nil, &mirasimMintAuthError{kind: kind, reason: fmt.Sprintf("mint ticket (HTTP %d): %s", resp.StatusCode, reason)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mint ticket upstream error (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	ticket := gjson.GetBytes(respBytes, "ticket").String()
	if ticket == "" {
		return nil, fmt.Errorf("no ticket returned in mint response: %s", string(respBytes))
	}

	expiresIn := gjson.GetBytes(respBytes, "expiresIn").Int()
	expiresAt := time.Now().Add(5 * time.Minute)
	if expiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	} else if exp := gjson.GetBytes(respBytes, "expiresAt").Int(); exp > 0 {
		if exp > 1e11 {
			expiresAt = time.UnixMilli(exp)
		} else {
			expiresAt = time.Unix(exp, 0)
		}
	}

	if !expiresAt.After(time.Now()) {
		return nil, fmt.Errorf("mirasim returned an expired device ticket")
	}

	// 步骤 6: 返回会话，由共享任务检查失效状态后写入缓存
	newSession := &MirasimTicketSession{
		Ticket:    ticket,
		ExpiresAt: expiresAt,
		DeviceID:  deviceID,
		ProxyURL:  proxyURL,
		BaseURL:   baseURL,
	}
	return newSession, nil
}

// parseMirasimUsageTiers 解析 Mirasim /v1/limits 返回的配额窗口。
func parseMirasimUsageTiers(bodyBytes []byte) []CNQuotaTier {
	var tiers []CNQuotaTier
	windows := gjson.GetBytes(bodyBytes, "windows").Array()
	for _, w := range windows {
		name := w.Get("name").String()
		if name == "" {
			continue
		}
		budget := w.Get("budget").Float()
		used := w.Get("used").Float()
		resetAtSec := w.Get("reset_at").Int()

		usedPercent := 0.0
		if budget > 0 {
			usedPercent = (used / budget) * 100
		}

		var resetAtStr string
		if resetAtSec > 0 {
			resetAtStr = time.Unix(resetAtSec, 0).UTC().Format(time.RFC3339)
		}

		tiers = append(tiers, CNQuotaTier{
			Window:      name,
			UsedPercent: usedPercent,
			ResetAt:     resetAtStr,
		})
	}
	return tiers
}

// signMirasimUpstreamRequest 使用最终出站请求体完成设备签名，保持待发送请求体可读。
// 参数 req 为协议转换后的请求、account 为账号；返回读取或签名错误，非 Mirasim 账号不处理。
func signMirasimUpstreamRequest(req *http.Request, account *Account) error {
	if account == nil || !account.IsMirasim() {
		return nil
	}
	if req == nil {
		return fmt.Errorf("nil mirasim upstream request")
	}
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("copy mirasim request body: %w", err)
			}
			defer body.Close()
			bodyBytes, err = io.ReadAll(body)
			if err != nil {
				return fmt.Errorf("read mirasim request body: %w", err)
			}
		} else {
			// 不支持复制的请求体读取后恢复，避免签名成功却向上游发送空内容。
			var err error
			bodyBytes, err = io.ReadAll(req.Body)
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			if err != nil {
				return fmt.Errorf("read mirasim request body: %w", err)
			}
		}
	}
	return SignAndSealRelayRequest(req.Context(), req, account, bodyBytes)
}

// SignAndSealRelayRequest 以设备票据替换鉴权头，对最终内容签名后整体封装设备签名与会话元数据。
// 参数：
//   - ctx: 上下文
//   - req: 发往上游的 HTTP 请求对象
//   - account: 账号信息
//   - bodyBytes: 请求体字节切片
// 返回值：
//   - error: 处理过程中的错误
func SignAndSealRelayRequest(ctx context.Context, req *http.Request, account *Account, bodyBytes []byte) error {
	return signAndSealMirasimRequest(ctx, req, account, bodyBytes, MirasimSealPublicKey)
}

// signAndSealMirasimRequest 执行完整出站签名；参数额外指定接收方公钥以便使用独立接收端验证协议，返回处理错误。
// 正式入口固定使用官方公钥，账号配置不能覆盖接收方公钥。
func signAndSealMirasimRequest(ctx context.Context, req *http.Request, account *Account, bodyBytes []byte, sealPublicKey string) error {
	if account == nil || !account.IsMirasim() {
		return nil
	}
	if req == nil {
		return fmt.Errorf("nil mirasim upstream request")
	}
	if strings.TrimSpace(account.GetMirasimPrivateKey()) == "" {
		return fmt.Errorf("mirasim account %d requires private_key for a signed session", account.ID)
	}

	signer, err := GetMirasimSigner()
	if err != nil {
		return fmt.Errorf("get mirasim signer failed: %w", err)
	}

	mgr := GetMirasimTicketManager()
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = DefaultMirasimBaseURL
	}

	// 步骤 1: 获取有效票据
	ticket, err := mgr.GetOrMintTicket(ctx, account, baseURL)
	if err != nil {
		return fmt.Errorf("mirasim get ticket: %w", err)
	}
	// 正式转发可能保留原始大小写的鉴权头，逐项移除，避免旧 API Key 与设备票据冲突。
	for key := range req.Header {
		if strings.EqualFold(key, "authorization") || strings.EqualFold(key, "x-api-key") || strings.EqualFold(key, "x-goog-api-key") {
			delete(req.Header, key)
		}
	}
	req.Header.Set("Authorization", "Bearer "+ticket)
	if err := prepareMirasimRelayMetadata(req, bodyBytes); err != nil {
		return err
	}

	clientVersion := strings.TrimSpace(account.GetCredential("client_version"))
	if clientVersion == "" {
		clientVersion = DefaultMirasimClientVersion
	}

	// 步骤 2: 丢弃入站或重试残留的设备签名，提取本次需要参与签名的会话元数据。
	metaMap := make(map[string]string)
	for k, v := range req.Header {
		lowerK := strings.ToLower(k)
		switch lowerK {
		case headerMirasimDevice, headerMirasimTS, headerMirasimNonce, headerMirasimSig, headerMirasimEnc:
			delete(req.Header, k)
		case headerMirasimClient:
			delete(req.Header, k)
		default:
			if strings.HasPrefix(lowerK, "x-mirasim-") && len(v) > 0 && v[0] != "" {
				metaMap[lowerK] = v[0]
			}
		}
	}
	req.Header.Set(headerMirasimClient, clientVersion)

	// 步骤 3: 先签名，再把新生成的四个设备认证字段加入封套；无会话元数据时也必须封装。
	if err := signMirasimRelayRequestWithMetadata(ctx, account, req, bodyBytes, ticket, metaMap); err != nil {
		return fmt.Errorf("sign relay request failed: %w", err)
	}
	for _, key := range []string{headerMirasimDevice, headerMirasimTS, headerMirasimNonce, headerMirasimSig} {
		metaMap[key] = req.Header.Get(key)
	}
	metaJsonBytes, err := json.Marshal(metaMap)
	if err != nil {
		return fmt.Errorf("marshal relay metadata failed: %w", err)
	}
	encVal, err := signer.sealMetadataWithKey(ctx, metaJsonBytes, req.Method, req.URL.Path, sealPublicKey)
	if err != nil {
		return fmt.Errorf("seal metadata failed: %w", err)
	}
	// 步骤 4: 只保留版本号和加密封套，不能把设备签名留在明文头中，否则上游返回 client_outdated。
	for key := range req.Header {
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, "x-mirasim-") && lowerKey != headerMirasimClient {
			delete(req.Header, key)
		}
	}
	req.Header.Set(headerMirasimEnc, encVal)

	return nil
}
