package dto

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLogiccCredentialsRedactedAndPreservedOnEdit(t *testing.T) {
	credentials := map[string]any{"client_cookie": "secret-cookie", "access_token": "secret-jwt", "session_id": "sess_test"}
	redacted, _ := RedactCredentials(credentials)
	require.NotContains(t, redacted, "client_cookie")
	require.NotContains(t, redacted, "access_token")
	merged := service.MergePreservingSensitiveCreds(credentials, redacted)
	require.Equal(t, "secret-cookie", merged["client_cookie"])
	require.Equal(t, "secret-jwt", merged["access_token"])
}
