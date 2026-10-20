package office_copilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

type diagnosticContextKey struct{}
type diagnosticContext struct {
	ID        string
	AccountID int64
	Until     time.Time
}

func diagnosticHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BeginDiagnostics is opt-in, account-scoped and expires without a restart.
// Never log request bodies, credentials, headers or arbitrary URL query fields.
func BeginDiagnostics(ctx context.Context, accountID int64, body []byte, anthropic bool) context.Context {
	path := os.Getenv("OFFICE_COPILOT_DIAGNOSTICS_FILE")
	if path == "" {
		path = "/app/data/office-copilot-diagnostics.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ctx
	}
	var config struct {
		AccountID int64     `json:"account_id"`
		Until     time.Time `json:"until"`
	}
	if json.Unmarshal(raw, &config) != nil || config.AccountID != accountID || !time.Now().Before(config.Until) {
		return ctx
	}
	d := diagnosticContext{ID: GenerateUUID(), AccountID: accountID, Until: config.Until}
	ctx = context.WithValue(ctx, diagnosticContextKey{}, d)
	logger.FromContext(ctx).Info("office_copilot.diagnostic.inbound",
		zap.String("diagnostic_id", d.ID), zap.Int64("account_id", accountID),
		zap.Bool("anthropic", anthropic), zap.Int("body_bytes", len(body)),
		zap.String("body_sha256", diagnosticHash(body)), zap.Bool(logger.OpsSystemLogSkipField, true))
	return ctx
}

// diagnosticFields uses an explicit allowlist instead of redacting an entire
// URL/payload. Text is represented only by its size and SHA256 fingerprint.
func diagnosticFields(endpoint *url.URL, origin string, body []byte) map[string]any {
	var payload struct {
		Source string `json:"source"`
		Tone   string `json:"tone"`
		Start  bool   `json:"isStartOfSession"`
		GPTs   []struct {
			ID      string `json:"id"`
			Source  string `json:"source"`
			Version string `json:"version"`
		} `json:"gpts"`
		Message struct {
			Text       string `json:"text"`
			Author     string `json:"author"`
			Type       string `json:"messageType"`
			Locale     string `json:"locale"`
			Experience string `json:"experienceType"`
		} `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	query := endpoint.Query()
	safeURL := url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host, Path: endpoint.Path}
	return map[string]any{
		"endpoint": safeURL.String(), "origin": origin,
		"query_source": query.Get("source"), "query_scenario": query.Get("scenario"),
		"query_variants": query.Get("variants"), "image_conversation": query.Get("ConversationId") != "",
		"source": payload.Source, "tone": payload.Tone, "gpts": payload.GPTs,
		"is_start_of_session": payload.Start, "author": payload.Message.Author,
		"message_type": payload.Message.Type, "locale": payload.Message.Locale,
		"experience_type": payload.Message.Experience,
		"text_bytes":      len(payload.Message.Text), "text_chars": utf8.RuneCountInString(payload.Message.Text),
		"text_sha256": diagnosticHash([]byte(payload.Message.Text)), "payload_bytes": len(body),
	}
}

func logChatHubDiagnostic(ctx context.Context, endpoint *url.URL, origin string, body []byte) {
	d, ok := ctx.Value(diagnosticContextKey{}).(diagnosticContext)
	if !ok || !time.Now().Before(d.Until) {
		return
	}
	logger.FromContext(ctx).Info("office_copilot.diagnostic.outbound",
		zap.String("diagnostic_id", d.ID), zap.Int64("account_id", d.AccountID),
		zap.Any("upstream", diagnosticFields(endpoint, origin, body)),
		zap.Bool(logger.OpsSystemLogSkipField, true))
}
