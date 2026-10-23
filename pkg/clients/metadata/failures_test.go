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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
)

func metadataAnswering(t *testing.T, respond http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(respond)
	t.Cleanup(server.Close)
	return server.URL
}

func TestAMetadataResponseTheAgentCantUseIsAnError(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	for name, testCase := range map[string]struct{ endpoint, want string }{
		// The address comes from agent.yaml, so a typo there must fail
		// cleanly.
		"a malformed address": {"http://[::1", "failed to create request"},
		"no connection":       {unreachable.URL, "failed to fetch auth token"},
		"a response cut short": {metadataAnswering(t, func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Length", "100")
			_, _ = response.Write([]byte("{"))
		}), "failed to read response body"},
		"not JSON": {metadataAnswering(t, func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte("<html>"))
		}), "failed to unmarshal token response"},
		"no token": {metadataAnswering(t, func(response http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(response).Encode(TokenResponse{CloudAPIUrl: "https://api.test.invalid"})
		}), "received empty token"},
	} {
		t.Run(name, func(t *testing.T) {
			client := NewClient(testCase.endpoint, time.Second, zap.NewNop())

			_, err := client.GetAuthToken(t.Context(), "vm")

			assert.ErrorContains(t, err, testCase.want)
		})
	}
}

// A token without a cloud API address is no use: there is nowhere to send
// the metrics.
func TestATokenWithoutACloudAPIAddressIsAnError(t *testing.T) {
	manager := newTestAuthManager(t, metadataAnswering(t, func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(TokenResponse{Token: "token"})
	}))

	assert.ErrorContains(t, manager.EnsureValidToken(t.Context()), "CloudAPI URL not available")
}

func TestTheCloudAPIAddressIsUnknownWhileTheMetadataServiceIsDown(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	client := NewClient(unreachable.URL, time.Second, zap.NewNop())

	_, err := client.GetCloudAPIURL(t.Context(), "vm")

	assert.ErrorContains(t, err, "failed to get CloudAPI URL")
}

// Every token lifetime the auth manager fetches a new token. A refresh that
// fails is reported and keeps the token the agent already has.
func TestTheRefreshLoopReplacesTheTokenAndSurvivesAFailedRefresh(t *testing.T) {
	var metadataServiceDown atomic.Bool
	var issuedTokens atomic.Int64
	endpoint := metadataAnswering(t, func(response http.ResponseWriter, _ *http.Request) {
		if metadataServiceDown.Load() {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(response).Encode(TokenResponse{
			Token:       fmt.Sprintf("token-%d", issuedTokens.Add(1)),
			CloudAPIUrl: "https://api.test.invalid",
		})
	})
	core, logs := observer.New(zap.ErrorLevel)
	manager := NewAuthManager(&config.Config{MetadataServiceEndpoint: endpoint, HTTPTimeout: time.Second, VMID: "vm"}, zap.New(core))
	t.Cleanup(manager.Close)
	// Thirty minutes in the agent.
	manager.refreshTicker.Reset(20 * time.Millisecond)
	require.NoError(t, manager.EnsureValidToken(t.Context()))

	manager.StartRefresh(t.Context())

	assert.Eventually(t, func() bool { return manager.GetCurrentToken() != "token-1" }, 5*time.Second, 10*time.Millisecond,
		"the refresh loop never replaced the first token")
	metadataServiceDown.Store(true)
	assert.Eventually(t, func() bool { return logs.FilterMessage("Background token refresh failed").Len() > 0 }, 5*time.Second, 10*time.Millisecond,
		"a failed refresh wasn't reported")
	assert.NotEmpty(t, manager.GetCurrentToken(), "a failed refresh dropped the token the agent had")
}

// Asked first, the cloud API address is fetched with a token, then cached.
func TestTheCloudAPIAddressIsFetchedWhenNotYetKnown(t *testing.T) {
	client := NewClient(metadataAnswering(t, func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(TokenResponse{Token: "token", CloudAPIUrl: "https://api.test.invalid"})
	}), time.Second, zap.NewNop())

	url, err := client.GetCloudAPIURL(t.Context(), "vm")

	require.NoError(t, err)
	assert.Equal(t, "https://api.test.invalid", url)
}
