package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/office_copilot"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Opt-in: one synthetic request, no document access or tool execution. Replays
// the normalized result locally to verify both protocols, without logging text.
func TestOfficeCopilotLiveEventUsage(t *testing.T) {
	raw := os.Getenv("OFFICE_COPILOT_EVENT_LIVE_CREDENTIALS_JSON")
	if raw == "" {
		t.Skip("live event probe not enabled")
	}
	var credentials map[string]any
	if json.Unmarshal([]byte(raw), &credentials) != nil {
		t.Fatal("invalid live credentials JSON")
	}
	account := &Account{Platform: PlatformOfficeCopilot, Type: AccountTypeOAuth, Credentials: credentials}
	proxyID := int64(1)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 7897}
	request := office_copilot.InboundRequest{Messages: []office_copilot.InboundMessage{{Role: "user", Content: "Compute 137 * 149. Reply with the number only. This is a text-only API test; do not use tools or edit a document."}}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := officeCopilotInference(ctx, nil, account, request, "claude-opus-5-5")
	if err != nil {
		t.Fatalf("synthetic inference failed: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		t.Fatalf("synthetic stream failed: %v", err)
	}
	summaryEvents := 0
	var answer strings.Builder
	err = office_copilot.ReadEventStream(strings.NewReader(string(data)), func(e office_copilot.StreamEvent) error {
		if e.Kind == office_copilot.EventReasoning {
			summaryEvents++
		} else {
			answer.WriteString(e.Text)
		}
		return nil
	})
	require.NoError(t, err)
	if !strings.Contains(answer.String(), "20413") {
		t.Fatal("synthetic answer check failed")
	}
	for _, anthropic := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			local := &http.Response{Body: io.NopCloser(strings.NewReader(string(data)))}
			result, err := writeOfficeCopilotResponse(c, local, "claude-opus-5-5", "claude-opus-5-5", stream, anthropic, time.Now(), office_copilot.EstimateInput(request))
			require.NoError(t, err)
			require.Zero(t, result.Usage.InputTokens)
			if !strings.Contains(rec.Body.String(), `"estimated":true`) {
				t.Fatal("estimate metadata missing")
			}
		}
	}
	t.Logf("synthetic Opus answer verified; public summary events=%d; four protocol modes verified", summaryEvents)
}
