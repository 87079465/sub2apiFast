package service

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMirasimProtocolRules(t *testing.T) {
	// 测试默认规则：claude-* 走 Anthropic，其他（gpt 等）走原生 Responses
	require.Equal(t, APIProtocolAnthropic, MirasimModelProtocol("claude-opus-5"))
	require.Equal(t, APIProtocolAnthropic, MirasimModelProtocol("claude-haiku-4-5"))
	require.Equal(t, APIProtocolResponses, MirasimModelProtocol("gpt-6-astra"))
	require.Equal(t, APIProtocolResponses, MirasimModelProtocol("gpt-5.6-sol"))

	// 测试自定义规则
	customRules := []MirasimProtocolRule{
		{Pattern: "gpt-*", Protocol: APIProtocolResponses},
		{Pattern: "deepseek-*", Protocol: APIProtocolResponses},
		{Pattern: "*", Protocol: APIProtocolAnthropic},
	}
	require.Equal(t, APIProtocolResponses, matchMirasimProtocolRules("gpt-5", customRules))
	require.Equal(t, APIProtocolAnthropic, matchMirasimProtocolRules("custom-model", customRules))

	// 测试账号协议解析
	account := &Account{
		Platform: PlatformMirasim,
	}
	require.True(t, account.IsMirasim())
	require.Equal(t, APIProtocolAnthropic, account.ResolveMirasimUpstreamProtocol("claude-3-7-sonnet"))
	require.Equal(t, APIProtocolResponses, account.ResolveMirasimUpstreamProtocol("gpt-4o"))
}

func TestParseEd25519Seed(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	// 1. 测试 Base64
	b64 := base64.StdEncoding.EncodeToString(priv.Seed())
	seed, err := ParseEd25519Seed(b64)
	require.NoError(t, err)
	require.Equal(t, []byte(priv.Seed()), seed)

	// 2. 测试 PKCS#8 PEM
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	seedFromPEM, err := ParseEd25519Seed(pemStr)
	require.NoError(t, err)
	require.Equal(t, []byte(priv.Seed()), seedFromPEM)
}

func TestParseMirasimUsageTiers(t *testing.T) {
	respJSON := []byte(`{
		"paid": true,
		"windows": [
			{
				"name": "30d",
				"budget": 100,
				"used": 45.5,
				"reset_at": 1726200000
			}
		]
	}`)

	tiers := parseMirasimUsageTiers(respJSON)
	require.Len(t, tiers, 1)
	require.Equal(t, "30d", tiers[0].Window)
	require.InDelta(t, 45.5, tiers[0].UsedPercent, 0.001)
	require.NotEmpty(t, tiers[0].ResetAt)
}
