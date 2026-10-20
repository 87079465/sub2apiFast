package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type logiccModelsRepo struct{ gatewayModelsAccountRepoStub }

func (r *logiccModelsRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, accounts := range r.byGroup {
		for _, account := range accounts {
			if account.ID == id {
				return &account, nil
			}
		}
	}
	return nil, fmt.Errorf("account not found")
}

// Exercise the public handler through the real Logicc client with a local TLS
// upstream. Do not run in parallel: the cloned default transport is redirected.
func TestLogiccPublicModels(t *testing.T) {
	var calls atomic.Int64
	var fail atomic.Bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/models" || r.URL.Query().Get("language") != "en" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"id":"claude-test"},{"id":"gpt-test"},{"id":"claude-test"}]}`)
	}))
	defer upstream.Close()
	original := http.DefaultTransport
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "app.logicc.com:443" {
			return nil, fmt.Errorf("unexpected host")
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sid":"sess_fixture","exp":%d}`, time.Now().Add(time.Hour).Unix())))
	account := service.Account{ID: 1, Platform: service.PlatformLogicc, Credentials: map[string]any{"access_token": "test." + claims + ".test"}}
	repo := &logiccModelsRepo{gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{1: {account}}}}
	h := newGatewayModelsHandlerForTest(repo)
	group := &service.Group{ID: 1, Platform: service.PlatformLogicc}
	list := func() gatewayModelsResponseForTest {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
		h.Models(c)
		require.Equal(t, http.StatusOK, rec.Code)
		var result gatewayModelsResponseForTest
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		return result
	}
	got := list()
	require.Equal(t, "list", got.Object)
	require.Equal(t, []string{"claude-test", "gpt-test"}, modelIDsForTest(got.Data))
	require.Equal(t, "model", got.Data[0].Object)
	require.Equal(t, "logicc", got.Data[0].OwnedBy)
	require.Positive(t, got.Data[0].Created)
	list()
	require.Equal(t, int64(1), calls.Load(), "subsequent listing uses cache")
	group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-*"}}
	require.Equal(t, []string{"claude-test"}, modelIDsForTest(list().Data))
	group.ModelAllowlist = service.GroupModelAllowlist{}
	account.Credentials["model_mapping"] = map[string]any{"public-alias": "claude-test"}
	h.gatewayService.InvalidateAvailableModelsCache(&group.ID, group.Platform)
	require.Equal(t, []string{"public-alias"}, modelIDsForTest(list().Data))
	require.Equal(t, int64(1), calls.Load(), "explicit mappings do not need discovery")
	delete(account.Credentials, "model_mapping")
	fail.Store(true)
	h.gatewayService.InvalidateAvailableModelsCache(&group.ID, group.Platform)
	require.Equal(t, []string{"default"}, modelIDsForTest(list().Data))
}
