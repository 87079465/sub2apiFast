package office_copilot

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiagnosticFieldsDoNotExposeSecretsOrText(t *testing.T) {
	u, err := url.Parse("wss://user:password@substrate.office.com/m365Copilot/ChatHub?access_token=SECRET_TOKEN&source=word&scenario=office&unknown=SECRET_QUERY&ClientRequestId=PRIVATE_ID")
	require.NoError(t, err)
	body := []byte(`{"source":"word","tone":"Claude_Opus","isStartOfSession":true,"gpts":[{"id":"WordDraftingAgent","source":"BuiltInAgents","version":"helix"}],"message":{"text":"你好 SECRET_CONTENT","messageId":"PRIVATE_ID"},"unknown":"SECRET_FIELD"}`)
	fields := diagnosticFields(u, "https://fa000000125.resources.office.net", body)
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	for _, secret := range []string{"SECRET_", "PRIVATE_ID", "password", "access_token", "你好"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Equal(t, "wss://substrate.office.com/m365Copilot/ChatHub", fields["endpoint"])
	require.Equal(t, "Claude_Opus", fields["tone"])
	require.Equal(t, diagnosticHash([]byte("你好 SECRET_CONTENT")), fields["text_sha256"])
}

func TestDiagnosticsRequireMatchingAccountAndUnexpiredConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.json")
	t.Setenv("OFFICE_COPILOT_DIAGNOSTICS_FILE", path)
	ctx := context.Background()
	require.Equal(t, ctx, BeginDiagnostics(ctx, 7, nil, true))
	for _, tc := range []struct {
		account int64
		until   time.Time
		enabled bool
	}{
		{7, time.Now().Add(time.Hour), true}, {8, time.Now().Add(time.Hour), false}, {7, time.Now().Add(-time.Hour), false},
	} {
		b, err := json.Marshal(map[string]any{"account_id": tc.account, "until": tc.until})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, b, 0600))
		_, enabled := BeginDiagnostics(ctx, 7, []byte(`{"messages":[]}`), true).Value(diagnosticContextKey{}).(diagnosticContext)
		require.Equal(t, tc.enabled, enabled)
	}
}
