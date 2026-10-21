package tsclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/klauspost/compress/snappy"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/metadata"
	"go.uber.org/zap"
)

const (
	// ContentType for timeseries binary data
	ContentTypeTimeseriesBinary = "application/timeseries-binary-0"
	// ContentTypeTimeseriesBatchV1 carries many already-compressed batches in a JSON array of base64-encoded blobs.
	ContentTypeTimeseriesBatchV1 = "application/timeseries-batch-v1"
	ContentTypeJSON              = "application/json"

	// Headers
	HeaderContentType     = "Content-Type"
	HeaderContentEncoding = "Content-Encoding"
	HeaderUserAgent       = "User-Agent"
	HeaderRetryAfter      = "Retry-After"
	HeaderAuthorization   = "Authorization"
	
	// Values
	ContentEncodingSnappy = "snappy"
	UserAgentValue        = "sc-metrics-agent/1.0"
	
	// Defaults
	DefaultTimeout    = 30 * time.Second
	DefaultMaxRetries = 3
	DefaultRetryDelay = 5 * time.Second

	// Agent Type
	AgentTypeSCMetricsAgent = "SC_METRICS_AGENT"
)

// Client handles HTTP communication with the timeseries ingestor
type Client struct {
	authMgr     *metadata.AuthManager
	httpClient  *http.Client
	logger      *zap.Logger
	maxRetries  int
	retryDelay  time.Duration
}

// ClientConfig holds client configuration
type ClientConfig struct {
	AuthMgr    *metadata.AuthManager
	Timeout    time.Duration
	MaxRetries int
	RetryDelay time.Duration
}

// Response represents the server response
type Response struct {
	StatusCode   int
	Body         []byte
	Headers      http.Header
	RetryAfter   time.Duration
	Error        error
}

// HeartbeatRequest represents the heartbeat payload
type HeartbeatRequest struct {
	AgentType string `json:"agentType"`
	EventTime string `json:"eventTime"`
	Version   string `json:"version"`
}

// NewClient creates a new HTTP client for timeseries data ingestion.
//
// Parameters:
//   - config: ClientConfig struct containing client configuration. The AuthMgr field
//     (a pointer to metadata.AuthManager) is required and must be non-nil, as it is
//     used for authentication on outgoing requests. Timeout, MaxRetries, and RetryDelay
//     are optional and will use defaults if zero-valued.
//   - logger: Logger instance for logging client activity.
//
// Returns:
//   - A pointer to a configured Client instance.
func NewClient(config ClientConfig, logger *zap.Logger) *Client {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	
	maxRetries := config.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	
	retryDelay := config.RetryDelay
	if retryDelay == 0 {
		retryDelay = DefaultRetryDelay
	}

	return &Client{
		authMgr: config.AuthMgr,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger:     logger,
		maxRetries: maxRetries,
		retryDelay: retryDelay,
	}
}


// Compresses metrics then sends them to the ingestor.
func (c *Client) SendMetrics(ctx context.Context, metrics []aggregate.MetricWithValue, authToken string) (*Response, error) {
	compressed, err := c.CompressMetrics(metrics)
	if err != nil {
		return nil, err
	}
	return c.SendCompressedMetrics(ctx, compressed, authToken)
}

// CompressMetrics JSON-encodes and Snappy-compresses metrics for wire transmission.
// The result can be buffered directly without re-serialization.
func (c *Client) CompressMetrics(metrics []aggregate.MetricWithValue) ([]byte, error) {
	if len(metrics) == 0 {
		return nil, fmt.Errorf("no metrics to send")
	}

	payload, err := json.Marshal(metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metrics: %w", err)
	}

	compressed := snappy.Encode(nil, payload)

	c.logger.Debug("Compressed metrics payload",
		zap.Int("metrics_count", len(metrics)),
		zap.Int("original_size", len(payload)),
		zap.Int("compressed_size", len(compressed)),
	)

	return compressed, nil
}

// Sends compressed payload; resolves endpoint each call for updated CloudAPI URL.
func (c *Client) SendCompressedMetrics(ctx context.Context, compressed []byte, authToken string) (*Response, error) {
	endpoint, err := c.getIngestorEndpoint()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ingestor endpoint: %w", err)
	}
	return c.sendWithRetry(ctx, compressed, ContentTypeTimeseriesBinary, authToken, endpoint)
}

// SendCompressedBatches POSTs many already-compressed batches in a single
// envelope. ["<base64(snappy(json(metrics)))>", "<base64(...)>", ...]
func (c *Client) SendCompressedBatches(ctx context.Context, batches [][]byte, authToken string) (*Response, error) {
	if len(batches) == 0 {
		return nil, fmt.Errorf("no batches to send")
	}

	endpoint, err := c.getIngestorEndpoint()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ingestor endpoint: %w", err)
	}

	envelope, err := buildBatchEnvelope(batches)
	if err != nil {
		return nil, fmt.Errorf("failed to build batch envelope: %w", err)
	}

	c.logger.Debug("Sending batch envelope",
		zap.Int("batch_count", len(batches)),
		zap.Int("envelope_bytes", len(envelope)),
	)

	return c.sendWithRetry(ctx, envelope, ContentTypeTimeseriesBatchV1, authToken, endpoint)
}

// buildBatchEnvelope wraps a slice of Snappy blobs into the
// timeseries-batch-v1 wire format.
func buildBatchEnvelope(batches [][]byte) ([]byte, error) {
	encoded := make([]string, len(batches))
	for i, blob := range batches {
		encoded[i] = base64.StdEncoding.EncodeToString(blob)
	}
	return json.Marshal(encoded)
}

// IngestEndpointAvailable reports whether the CloudAPI URL has been resolved
func (c *Client) IngestEndpointAvailable() bool {
	_, err := c.getIngestorEndpoint()
	return err == nil
}


// SendHeartbeat sends agent heartbeat to the backend
func (c *Client) SendHeartbeat(ctx context.Context, authToken, version string) (*Response, error) {
	if c.authMgr == nil {
		return nil, fmt.Errorf("AuthManager is required for heartbeat")
	}

	cloudAPIURL := c.authMgr.GetCloudAPIURL()
	if cloudAPIURL == "" {
		return nil, fmt.Errorf("empty CloudAPI URL from metadata")
	}
	url := fmt.Sprintf("%s/resource-manager/api/v1/compute/agent/heartbeat", cloudAPIURL)

	payload := HeartbeatRequest{
		AgentType: AgentTypeSCMetricsAgent,
		EventTime: time.Now().UTC().Format(time.RFC3339),
		Version:   version,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	c.logger.Debug("Sending heartbeat", zap.String("url", url))

	return c.sendRequest(ctx, body, ContentTypeJSON, authToken, url)
}

// sendWithRetry handles the HTTP request with retry logic
func (c *Client) sendWithRetry(ctx context.Context, data []byte, contentType string, authToken string, endpoint string) (*Response, error) {
	var lastResponse *Response
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if attempt > 0 {
			// Wait before retry
			waitTime := c.retryDelay
			if lastResponse != nil && lastResponse.RetryAfter > 0 {
				waitTime = lastResponse.RetryAfter
			}

			c.logger.Info("Retrying request",
				zap.Int("attempt", attempt),
				zap.Duration("wait_time", waitTime),
			)

			select {
			case <-time.After(waitTime):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		response, err := c.sendRequest(ctx, data, contentType, authToken, endpoint)
		if err != nil {
			lastErr = err
			c.logger.Warn("Request failed", zap.Error(err), zap.Int("attempt", attempt))
			continue
		}

		lastResponse = response

		// Check if we should retry based on status code
		if c.shouldRetry(response.StatusCode) {
			c.logger.Warn("Request failed with retryable status",
				zap.Int("status_code", response.StatusCode),
				zap.Int("attempt", attempt),
			)
			continue
		}

		// Success or non-retryable error
		return response, nil
	}

	// All retries exhausted
	if lastResponse != nil {
		return lastResponse, fmt.Errorf("request failed after %d attempts, last status: %d", c.maxRetries+1, lastResponse.StatusCode)
	}
	return nil, fmt.Errorf("request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

// sendRequest sends a single HTTP request
func (c *Client) sendRequest(ctx context.Context, data []byte, contentType string, authToken string, endpoint string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set(HeaderContentType, contentType)
	// Only set compression header for binary content types (not JSON)
	switch contentType {
	case ContentTypeTimeseriesBinary:
		req.Header.Set(HeaderContentEncoding, ContentEncodingSnappy)
	}
	req.Header.Set(HeaderUserAgent, UserAgentValue)
	
	if authToken != "" {
		req.Header.Set(HeaderAuthorization, "Bearer "+authToken)
	}

	c.logger.Debug("Sending HTTP request",
		zap.String("endpoint", endpoint),
		zap.String("content_type", contentType),
		zap.Int("payload_size", len(data)),
	)

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Warn("Failed to close response body", zap.Error(closeErr))
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse retry-after header if present
	retryAfter := parseRetryAfter(resp.Header.Get(HeaderRetryAfter))

	response := &Response{
		StatusCode: resp.StatusCode,
		Body:       body,
		Headers:    resp.Header,
		RetryAfter: retryAfter,
	}

	c.logger.Debug("Received HTTP response",
		zap.Int("status_code", resp.StatusCode),
		zap.Int("response_size", len(body)),
		zap.Duration("retry_after", retryAfter),
	)

	return response, nil
}

// getIngestorEndpoint resolves the ingestor endpoint from CloudAPI URL
func (c *Client) getIngestorEndpoint() (string, error) {
	if c.authMgr == nil {
		return "", fmt.Errorf("AuthManager is required for endpoint resolution")
	}

	cloudAPIURL := c.authMgr.GetCloudAPIURL()
	if cloudAPIURL == "" {
		return "", fmt.Errorf("empty CloudAPI URL from metadata")
	}
	// Construct the ingestor endpoint
	endpoint := fmt.Sprintf("%s/resource-manager/api/v1/metrics/ingest", cloudAPIURL)

	return endpoint, nil
}

// shouldRetry determines if a request should be retried based on status code
func (c *Client) shouldRetry(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError,     // 500
		http.StatusBadGateway,              // 502
		http.StatusServiceUnavailable,      // 503
		http.StatusGatewayTimeout:          // 504
		return true
	default:
		return false
	}
}

// parseRetryAfter parses the Retry-After header value
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}

	// Try parsing as seconds (integer)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	// Try parsing as HTTP date (not commonly used for this header)
	if t, err := http.ParseTime(value); err == nil {
		duration := time.Until(t)
		if duration > 0 {
			return duration
		}
	}

	return 0
}

// Close closes the HTTP client
func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

// SetMaxRetries updates the maximum retry count
func (c *Client) SetMaxRetries(maxRetries int) {
	if maxRetries >= 0 {
		c.maxRetries = maxRetries
	}
}

// SetRetryDelay updates the retry delay
func (c *Client) SetRetryDelay(delay time.Duration) {
	if delay > 0 {
		c.retryDelay = delay
	}
}

