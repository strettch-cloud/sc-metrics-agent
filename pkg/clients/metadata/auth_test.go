package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
)

// newMetadataServer answers every token request with a new token.
func newMetadataServer(t *testing.T) *httptest.Server {
	t.Helper()
	var issuedTokens atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(TokenResponse{
			Token:       fmt.Sprintf("token-%d", issuedTokens.Add(1)),
			CloudAPIUrl: "https://api.test.invalid",
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestAuthManager(t *testing.T, endpoint string) *AuthManager {
	t.Helper()
	manager := NewAuthManager(&config.Config{
		MetadataServiceEndpoint: endpoint,
		HTTPTimeout:             5 * time.Second,
		VMID:                    "test-vm",
	}, zaptest.NewLogger(t))
	t.Cleanup(manager.Close)
	return manager
}

// The refresh loop replaces the token while the pipeline, the heartbeat and the
// ingest client read it from other goroutines. The race detector, which
// `mise run test` turns on, fails this test if those reads aren't synchronised.
func TestAuthManagerTokenCanBeReadWhileItIsRefreshed(t *testing.T) {
	manager := newTestAuthManager(t, newMetadataServer(t).URL)
	require.NoError(t, manager.EnsureValidToken(t.Context()))

	refreshesDone := make(chan struct{})
	go func() {
		defer close(refreshesDone)
		for range 20 {
			assert.NoError(t, manager.refresh(t.Context()))
		}
	}()
	for reading := true; reading; {
		select {
		case <-refreshesDone:
			reading = false
		default:
			_ = manager.GetCurrentToken()
			_ = manager.GetCloudAPIURL()
		}
	}

	assert.Equal(t, "token-21", manager.GetCurrentToken(), "each refresh must fetch and store a new token")
	assert.Equal(t, "https://api.test.invalid", manager.GetCloudAPIURL())
}

func TestWhenATokenIsDueForRefresh(t *testing.T) {
	client := NewClient("", time.Second, zaptest.NewLogger(t))
	now := time.Now()
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }

	assert.WithinDuration(t, now.Add(time.Hour-TokenRefreshMargin), client.refreshTime(stamp(time.Hour)), 2*time.Second,
		"a token is replaced TokenRefreshMargin before expiresAt")
	assert.WithinDuration(t, now.Add(20*time.Second), client.refreshTime(stamp(40*time.Second)), 2*time.Second,
		"a token with less than the margin left is kept for half of it")
	for _, expiresAt := range []string{"", "tomorrow"} {
		assert.WithinDuration(t, now.Add(TokenCacheLifetime), client.refreshTime(expiresAt), 2*time.Second,
			"a token without a usable expiresAt is kept for TokenCacheLifetime")
	}
}

func TestARefusedTokenIsReplacedOnce(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(TokenResponse{
			Token:       fmt.Sprintf("token-%d", requests.Add(1)),
			CloudAPIUrl: "https://api.test.invalid",
		})
	}))
	t.Cleanup(server.Close)
	manager := newTestAuthManager(t, server.URL)
	require.NoError(t, manager.EnsureValidToken(t.Context()))

	require.NoError(t, manager.RenewRefusedToken(t.Context(), "token-1"))
	require.NoError(t, manager.RenewRefusedToken(t.Context(), "token-1"))

	assert.Equal(t, "token-2", manager.GetCurrentToken())
	assert.EqualValues(t, 2, requests.Load(), "a token already replaced is not replaced again")
}

func TestTheRefreshLoopStopsWithItsContext(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	manager := NewAuthManager(&config.Config{MetadataServiceEndpoint: newMetadataServer(t).URL, HTTPTimeout: time.Second, VMID: "vm"}, zap.New(core))
	t.Cleanup(manager.Close)
	ctx, cancel := context.WithCancel(t.Context())

	manager.StartRefresh(ctx)
	cancel()

	assert.Eventually(t, func() bool { return logs.FilterMessage("Token refresh stopped due to context cancel").Len() == 1 },
		5*time.Second, 10*time.Millisecond)
}
