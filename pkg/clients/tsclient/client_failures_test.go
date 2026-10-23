package tsclient

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/metadata"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
)

// newClientWithCloudAPI returns a client whose metadata service names
// cloudAPIURL as the cloud API. Empty leaves it out, as before the agent has
// heard from the metadata service.
func newClientWithCloudAPI(t *testing.T, cloudAPIURL string, maxRetries int) *Client {
	t.Helper()
	metadataService := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(metadata.TokenResponse{Token: "token", CloudAPIUrl: cloudAPIURL})
	}))
	t.Cleanup(metadataService.Close)
	authMgr := metadata.NewAuthManager(&config.Config{
		MetadataServiceEndpoint: metadataService.URL,
		HTTPTimeout:             time.Second,
		VMID:                    "vm",
	}, zap.NewNop())
	t.Cleanup(authMgr.Close)
	err := authMgr.EnsureValidToken(context.Background())
	if cloudAPIURL != "" {
		require.NoError(t, err)
	}
	return NewClient(ClientConfig{AuthMgr: authMgr, Timeout: time.Second, MaxRetries: maxRetries, RetryDelay: 10 * time.Millisecond}, zap.NewNop())
}

func newClientFor(t *testing.T, ingest http.HandlerFunc, maxRetries int) *Client {
	t.Helper()
	server := httptest.NewServer(ingest)
	t.Cleanup(server.Close)
	return newClientWithCloudAPI(t, server.URL, maxRetries)
}

var oneBatch = [][]byte{[]byte("batch")}

// The ingestor's Retry-After is how it spreads out many agents retrying at
// once, so a retry waits that long instead of the client's own delay.
func TestARetryWaitsAsLongAsTheIngestorAsks(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	client := newClientFor(t, func(response http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			response.Header().Set("Retry-After", "1")
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		response.WriteHeader(http.StatusOK)
	}, 1)
	started := time.Now()

	response, err := client.SendCompressedBatches(context.Background(), oneBatch, "token")

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.GreaterOrEqual(t, time.Since(started), time.Second, "the retry came before the ingestor's Retry-After")
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 2*time.Second, parseRetryAfter("2"))
	for _, unusable := range []string{"", "0", "-1", "soon", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)} {
		assert.Zero(t, parseRetryAfter(unusable), "Retry-After %q", unusable)
	}
	inAMinute := parseRetryAfter(time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
	assert.InDelta(t, time.Minute.Seconds(), inAMinute.Seconds(), 2)
}

func TestARequestThatFailsIsRetriedThenReported(t *testing.T) {
	t.Parallel()
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	for name, testCase := range map[string]struct {
		client *Client
		want   string
	}{
		"no connection": {newClientWithCloudAPI(t, unreachable.URL, 1), "HTTP request failed"},
		"a response cut short": {newClientFor(t, func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Length", "100")
			_, _ = response.Write([]byte("{"))
		}, 1), "failed to read response body"},
		"a malformed cloud API address": {newClientWithCloudAPI(t, "http://[::1", 1), "failed to create request"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := testCase.client.SendCompressedBatches(context.Background(), oneBatch, "token")

			require.Error(t, err)
			assert.Contains(t, err.Error(), "request failed after 2 attempts")
			assert.Contains(t, err.Error(), testCase.want)
		})
	}
}

// Shutting down cancels the context; a send must stop waiting then.
func TestASendStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	client := newClientFor(t, func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "3600")
		response.WriteHeader(http.StatusServiceUnavailable)
	}, 1)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.SendCompressedBatches(cancelled, oneBatch, "token")
	assert.ErrorIs(t, err, context.Canceled)

	waiting, stopWaiting := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stopWaiting()
	_, err = client.SendCompressedBatches(waiting, oneBatch, "token")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the send kept waiting out the hour-long Retry-After")
}

// Until the metadata service names the cloud API, there is nowhere to send to.
func TestNothingIsSentBeforeTheCloudAPIIsKnown(t *testing.T) {
	t.Parallel()
	client := newClientWithCloudAPI(t, "", 1)

	_, err := client.SendCompressedBatches(context.Background(), oneBatch, "token")
	assert.ErrorContains(t, err, "failed to resolve ingestor endpoint")

	_, err = client.SendHeartbeat(context.Background(), "token", "1.0.0")
	assert.ErrorContains(t, err, "empty CloudAPI URL")

	err = NewAuxiliaryWriter(client, zap.NewNop()).SendHeartbeat(context.Background(), "token", "1.0.0")
	assert.ErrorContains(t, err, "failed to send heartbeat")
}

// JSON has no NaN or infinity, so a batch holding one can't be encoded. No
// collector produces one today: their values are counters and sizes.
func TestABatchWithAValueJSONCantHoldIsAnError(t *testing.T) {
	t.Parallel()
	client := newClientWithCloudAPI(t, "", 1)

	_, err := client.CompressMetrics([]aggregate.MetricWithValue{{Name: "node_load1", Value: math.NaN()}})

	assert.ErrorContains(t, err, "failed to marshal metrics")
}
