package metadata

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

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
