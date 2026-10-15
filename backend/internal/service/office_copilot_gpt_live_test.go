package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Opt-in: validates the verified Word GPT-6 Sol route with synthetic text.
// Does not claim native Excel routing, model-version attestation, or tool support.
func TestOfficeCopilotLiveGPTSol(t *testing.T) {
	path := os.Getenv("OFFICE_COPILOT_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("live credentials not configured")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var credentials map[string]any
	require.NoError(t, json.Unmarshal(raw, &credentials))
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	if proxyURL := os.Getenv("OFFICE_COPILOT_LIVE_PROXY_URL"); proxyURL != "" {
		u, err := url.Parse(proxyURL)
		require.NoError(t, err)
		port, err := strconv.Atoi(u.Port())
		require.NoError(t, err)
		id := int64(1)
		account.ProxyID = &id
		account.Proxy = &Proxy{Protocol: u.Scheme, Host: u.Hostname(), Port: port}
		if u.User != nil {
			account.Proxy.Username = u.User.Username()
			account.Proxy.Password, _ = u.User.Password()
		}
	}
	for _, anthropic := range []bool{false, true} {
		name := "chat_completions"
		if anthropic {
			name = "messages"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/"+name, nil).WithContext(ctx)
			body := []byte(`{"model":"gpt-6-sol","max_tokens":128,"messages":[{"role":"user","content":"Compute 137 * 149. Reply only with the number. This is a text-only API test, with no Office document connected. Do not call tools or edit a document."}]}`)
			result, err := (&OpenAIGatewayService{}).forwardOfficeCopilot(ctx, c, account, body, "", anthropic)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "gpt-6-sol", result.UpstreamModel)
			require.Equal(t, 200, rec.Code)
			answerPath := "choices.0.message.content"
			if anthropic {
				answerPath = "content.0.text"
			}
			require.Contains(t, gjson.Get(rec.Body.String(), answerPath).String(), "20413")
		})
	}
}
