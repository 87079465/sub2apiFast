//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMirasimRelayMetadataSession 校验真实会话来源、无会话兜底及每次调用的独立标识。
func TestMirasimRelayMetadataSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		headers http.Header
		body string
		session string
		fallback bool // 无真实会话，断言兜底生成 mirasim_ 前缀会话
		agent string
	}{
		{name: "Claude 会话头", path: "/v1/messages", headers: http.Header{"X-Claude-Code-Session-Id": {"session-header"}}, body: `{}`, session: "session-header", agent: "claude"},
		{name: "Claude 新元数据", path: "/v1/messages", body: `{"metadata":{"user_id":"{\"device_id\":\"device\",\"session_id\":\"session-json\"}"}}`, session: "session-json", agent: "claude"},
		{name: "Claude 旧元数据", path: "/v1/messages", body: `{"metadata":{"user_id":"user_` + strings.Repeat("a", 64) + `_account__session_12345678-1234-1234-1234-123456789012"}}`, session: "12345678-1234-1234-1234-123456789012", agent: "claude"},
		{name: "Codex 会话头", path: "/gpt/v1/responses", headers: http.Header{"session_id": {"codex-session"}}, body: `{"prompt_cache_key":"body-session"}`, session: "codex-session", agent: "codex"},
		{name: "Codex 缓存键", path: "/v1/responses", body: `{"prompt_cache_key":"body-session"}`, session: "body-session", agent: "codex"},
		{name: "普通用户 ID 不是会话则兜底", path: "/v1/messages", body: `{"metadata":{"user_id":"customer-1"}}`, fallback: true, agent: "claude"},
		{name: "无会话兜底", path: "/v1/messages", body: `{}`, fallback: true, agent: "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "https://relay.invalid"+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			for key, values := range tc.headers { req.Header[key] = values }
			require.NoError(t, prepareMirasimRelayMetadata(req, []byte(tc.body)))
			gotSession := req.Header.Get("x-mirasim-session")
			if tc.fallback {
				require.True(t, strings.HasPrefix(gotSession, "mirasim_"))
			} else {
				require.Equal(t, tc.session, gotSession)
			}
			require.Equal(t, tc.agent, req.Header.Get("x-mirasim-agent"))
			require.Empty(t, req.Header.Get("x-mirasim-account"))
			callID := req.Header.Get("x-mirasim-call")
			_, err = uuid.Parse(callID)
			require.NoError(t, err)
			require.NoError(t, prepareMirasimRelayMetadata(req, []byte(tc.body)))
			// 兜底会话首次已写入请求头，二次调用应读回同一值，保持跨调用稳定。
			require.Equal(t, gotSession, req.Header.Get("x-mirasim-session"))
			require.NotEqual(t, callID, req.Header.Get("x-mirasim-call"))
		})
	}
}

// TestMirasimRelayMetadataBeta 验证多值与大小写变体，防止删除其他 beta 能力。
func TestMirasimRelayMetadataBeta(t *testing.T) {
	for _, tc := range []struct { name string; values []string; want string }{
		{name: "混合能力", values: []string{"oauth-2025-04-20, context-management-2025-06-27", "effort-2025-11-24"}, want: "context-management-2025-06-27,effort-2025-11-24"},
		{name: "仅 OAuth", values: []string{" oauth-2025-04-20 "}},
		{name: "无 beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "https://relay.invalid/v1/messages", nil)
			require.NoError(t, err)
			req.Header["aNtHrOpIc-BeTa"] = tc.values
			require.NoError(t, prepareMirasimRelayMetadata(req, nil))
			require.Equal(t, tc.want, req.Header.Get("anthropic-beta"))
			require.NotContains(t, req.Header, "aNtHrOpIc-BeTa")
		})
	}
	req, err := http.NewRequest(http.MethodGet, "https://relay.invalid/v1/limits", nil)
	require.NoError(t, err)
	require.NoError(t, prepareMirasimRelayMetadata(req, nil))
	require.Empty(t, req.Header)
}

// TestMirasimRelayMetadataAccountLocale 校验账号与语言字段的写入、缺省、残留清理，并确认不添加 turn。
func TestMirasimRelayMetadataAccountLocale(t *testing.T) {
	// 场景 1: 保留客户端语言，清除并省略不属于 App 本地账号标识的 account。
	req, err := http.NewRequest(http.MethodPost, "https://relay.invalid/v1/messages", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("x-mirasim-account", "stale-account")
	req.Header["X-MiRaSiM-AcCoUnT"] = []string{"stale-variant"}
	req.Header.Set("x-mirasim-locale", "en-US")
	require.NoError(t, prepareMirasimRelayMetadata(req, []byte(`{}`)))
	require.Empty(t, req.Header.Get("x-mirasim-account"))
	require.NotContains(t, req.Header, "X-MiRaSiM-AcCoUnT")
	require.Equal(t, "zh-CN", req.Header.Get("x-mirasim-locale"))
	require.Empty(t, req.Header.Get("x-mirasim-turn"))

	// 场景 2: 无 Accept-Language 时不写入 account/locale（对齐 App “存在才发”）。
	req2, err := http.NewRequest(http.MethodPost, "https://relay.invalid/v1/messages", strings.NewReader(`{}`))
	require.NoError(t, err)
	require.NoError(t, prepareMirasimRelayMetadata(req2, []byte(`{}`)))
	require.Empty(t, req2.Header.Get("x-mirasim-account"))
	require.Empty(t, req2.Header.Get("x-mirasim-locale"))
	require.Empty(t, req2.Header.Get("x-mirasim-turn"))
}

// TestMirasimRelayMetadataSignedGateway 从正式构造器到封套解密及独立验签验证完整链路。
func TestMirasimRelayMetadataSignedGateway(t *testing.T) {
	secret, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	account := newMirasimGatewayTestAccount(t)
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	c := adaptiveProtocolTestContext("/v1/messages", body)
	c.Request.Header.Set("session-id", "client-conversation")
	c.Request.Header.Set("anthropic-beta", "oauth-2025-04-20,effort-2025-11-24")
	c.Request.Header.Set("x-mirasim-device", "untrusted-device")
	svc := &OpenAIGatewayService{}
	req, finalBody, err := svc.buildNativeAnthropicUpstreamRequest(context.Background(), c, account, body, "issuer", account.GetOpenAIBaseURL()+"/v1/messages")
	require.NoError(t, err)
	require.Equal(t, "client-conversation", getHeaderRaw(req.Header, "x-mirasim-session"))
	require.NoError(t, signAndSealMirasimRequest(context.Background(), req, account, finalBody, base64.StdEncoding.EncodeToString(secret.PublicKey().Bytes())))
	require.Equal(t, DefaultMirasimClientVersion, req.Header.Get(headerMirasimClient))
	require.Equal(t, "effort-2025-11-24", req.Header.Get("anthropic-beta"))
	envelope, err := base64.RawURLEncoding.DecodeString(req.Header.Get(headerMirasimEnc))
	require.NoError(t, err)
	plaintext := mirasimTestCore(t, "cc_open", secret.Bytes(), []byte("mrs-seal-v1\nPOST\n/v1/messages"), envelope)
	meta := map[string]string{}
	require.NoError(t, json.Unmarshal(plaintext, &meta))
	require.Equal(t, "client-conversation", meta["x-mirasim-session"])
	require.Equal(t, "claude", meta["x-mirasim-agent"])
	require.NotEmpty(t, meta["x-mirasim-call"])
	require.NotContains(t, meta, "x-mirasim-account")
	require.NotEqual(t, "untrusted-device", meta[headerMirasimDevice])
	seed, err := ParseEd25519Seed(account.GetMirasimPrivateKey())
	require.NoError(t, err)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	require.True(t, mirasimIndependentVerify(t, req, finalBody, meta, "gateway-ticket", public))
	for key := range req.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-mirasim-") { require.Contains(t, []string{headerMirasimClient, headerMirasimEnc}, strings.ToLower(key)) }
	}
	require.True(t, bytes.Equal(body, finalBody))
	otherHeaders := http.Header{}
	applyMirasimSessionHeader(c, &Account{Platform: PlatformAnthropic}, otherHeaders, body)
	require.Empty(t, otherHeaders)
}
