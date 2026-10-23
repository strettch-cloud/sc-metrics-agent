package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/metadata"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/tsclient"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
	"github.com/strettch-cloud/sc-metrics-agent/test/helpers"
)

// createTestClient creates a test client for integration testing
func createTestClient(endpoint string, logger *zap.Logger) *tsclient.Client {
	baseURL := endpoint
	if strings.Contains(endpoint, "/metrics/ingest") {
		baseURL = endpoint[:strings.Index(endpoint, "/metrics/ingest")]
	}

	mockMetadataServer := helpers.NewMockMetadataServer(baseURL, logger)
	
	cfg := &config.Config{
		MetadataServiceEndpoint: mockMetadataServer.URL() + "/metadata/v1/auth-token",
		HTTPTimeout:            30 * time.Second,
		VMID:                   "test-vm-id",
	}
	
	authMgr := metadata.NewAuthManager(cfg, logger)
	
	ctx := context.Background()
	if err := authMgr.EnsureValidToken(ctx); err != nil {
		logger.Error("Failed to get auth token from mock metadata service", zap.Error(err))
		return nil
	}
	
	// Create client configuration
	clientConfig := tsclient.ClientConfig{
		AuthMgr:    authMgr,
		Timeout:    30 * time.Second,
		MaxRetries: 3,
		RetryDelay: 5 * time.Second,
	}
	
	return tsclient.NewClient(clientConfig, logger)
}

// sendLikeTheAgent sends metrics the way the agent does: compressed into a
// batch, in a batch-v1 envelope, which is how it drains its disk buffer.
func sendLikeTheAgent(ctx context.Context, client *tsclient.Client, metrics []aggregate.MetricWithValue) (*tsclient.Response, error) {
	compressed, err := client.CompressMetrics(metrics)
	if err != nil {
		return nil, err
	}
	return client.SendCompressedBatches(ctx, [][]byte{compressed}, "")
}

// TestResourceManagerCompatibility_EndToEnd tests complete end-to-end
// compatibility with the resource-manager ingest endpoint
func TestResourceManagerCompatibility_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	
	// Start mock resource-manager server
	mockServer := helpers.NewMockIngestServer(logger)
	defer mockServer.Close()

	// Create client pointing to mock server
	client := createTestClient(mockServer.URL(), logger)
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("Failed to close client: %v", err)
		}
	}()

	// Test with valid supported metrics
	testMetrics := []aggregate.MetricWithValue{
		{
			Name: "node_cpu_seconds_total",
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
				"cpu":   "cpu0",
				"mode":  "user",
			},
			Value:     12345.67,
			Timestamp: time.Now().UnixMilli(),
			Type:      "counter",
		},
		{
			Name: "node_memory_MemTotal_bytes",
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
			},
			Value:     8589934592, // 8GB
			Timestamp: time.Now().UnixMilli(),
			Type:      "gauge",
		},
		{
			Name: "node_load1",
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
			},
			Value:     1.23,
			Timestamp: time.Now().UnixMilli(),
			Type:      "gauge",
		},
	}

	// Send metrics
	ctx := context.Background()
	response, err := sendLikeTheAgent(ctx, client, testMetrics)
	require.NoError(t, err)
	require.NotNil(t, response)

	// Verify response
	assert.Equal(t, 202, response.StatusCode, "Expected 202 Accepted status")

	// Parse response body
	var apiResponse helpers.ApiResponse
	err = json.Unmarshal(response.Body, &apiResponse)
	require.NoError(t, err)

	// Verify all metrics were processed successfully
	assert.Equal(t, "metrics processed", apiResponse.Data.Status)
	assert.Equal(t, 3, apiResponse.Data.Processed, "All 3 metrics should be processed")
	assert.Equal(t, 0, apiResponse.Data.Failed, "No metrics should fail")
	assert.Empty(t, apiResponse.Data.Errors, "Should have no errors")

	// Verify request format
	requests := mockServer.GetRequests()
	require.Len(t, requests, 1, "Should have received exactly one request")

	request := requests[0]
	issues := mockServer.ValidateRequest(request)
	assert.Empty(t, issues, "Request format should be perfect: %v", issues)
}

// TestResourceManagerCompatibility_UnsupportedMetrics tests that unsupported
// metrics are properly rejected by the resource-manager
func TestResourceManagerCompatibility_UnsupportedMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	
	// Start mock resource-manager server
	mockServer := helpers.NewMockIngestServer(logger)
	defer mockServer.Close()

	// Create client pointing to mock server
	client := createTestClient(mockServer.URL(), logger)
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("Failed to close client: %v", err)
		}
	}()

	// Test with unsupported metrics that should be rejected
	testMetrics := []aggregate.MetricWithValue{
		{
			Name: "unsupported_metric_name",  // This should be rejected
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
			},
			Value:     123.45,
			Timestamp: time.Now().UnixMilli(),
			Type:      "gauge",
		},
		{
			Name: "node_cpu_seconds_total",  // This should be accepted
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
				"cpu":   "cpu0",
				"mode":  "user",
			},
			Value:     678.90,
			Timestamp: time.Now().UnixMilli(),
			Type:      "counter",
		},
		{
			Name: "another_unsupported_metric",  // This should be rejected
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
			},
			Value:     999.99,
			Timestamp: time.Now().UnixMilli(),
			Type:      "gauge",
		},
	}

	// Send metrics
	ctx := context.Background()
	response, err := sendLikeTheAgent(ctx, client, testMetrics)
	require.NoError(t, err)
	require.NotNil(t, response)

	// Verify response
	assert.Equal(t, 202, response.StatusCode, "Should still return 202 for partial success")

	// Parse response body
	var apiResponse helpers.ApiResponse
	err = json.Unmarshal(response.Body, &apiResponse)
	require.NoError(t, err)

	// Verify processing results
	assert.Equal(t, "metrics processed", apiResponse.Data.Status)
	assert.Equal(t, 1, apiResponse.Data.Processed, "Only 1 metric should be processed")
	assert.Equal(t, 2, apiResponse.Data.Failed, "2 metrics should fail")
	assert.Len(t, apiResponse.Data.Errors, 2, "Should have 2 error messages")

	// Verify error messages contain the unsupported metric names
	errorStr := ""
	for _, err := range apiResponse.Data.Errors {
		errorStr += err + " "
	}
	assert.Contains(t, errorStr, "unsupported_metric_name", "Error should mention first unsupported metric")
	assert.Contains(t, errorStr, "another_unsupported_metric", "Error should mention second unsupported metric")
}

// TestResourceManagerCompatibility_ValidationErrors tests various validation errors
func TestResourceManagerCompatibility_ValidationErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	
	testCases := []struct {
		name           string
		metrics        []aggregate.MetricWithValue
		expectedFailed int
		expectedErrors []string
	}{
		{
			name: "missing vm_id label",
			metrics: []aggregate.MetricWithValue{
				{
					Name: "node_cpu_seconds_total",
					Labels: map[string]string{
						"cpu":  "cpu0",
						"mode": "user",
						// vm_id missing
					},
					Value:     123.45,
					Timestamp: time.Now().UnixMilli(),
					Type:      "counter",
				},
			},
			expectedFailed: 1,
			expectedErrors: []string{"vm_id label is required"},
		},
		{
			name: "invalid vm_id format",
			metrics: []aggregate.MetricWithValue{
				{
					Name: "node_cpu_seconds_total",
					Labels: map[string]string{
						"vm_id": "not-a-valid-uuid",
						"cpu":   "cpu0",
						"mode":  "user",
					},
					Value:     123.45,
					Timestamp: time.Now().UnixMilli(),
					Type:      "counter",
				},
			},
			expectedFailed: 1,
			expectedErrors: []string{"vm_id must be a valid UUID"},
		},
		{
			name: "invalid timestamp",
			metrics: []aggregate.MetricWithValue{
				{
					Name: "node_cpu_seconds_total",
					Labels: map[string]string{
						"vm_id": "123e4567-e89b-12d3-a456-426614174000",
						"cpu":   "cpu0",
						"mode":  "user",
					},
					Value:     123.45,
					Timestamp: 0, // Invalid timestamp
					Type:      "counter",
				},
			},
			expectedFailed: 1,
			expectedErrors: []string{"invalid timestamp"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Start fresh mock server for each test
			mockServer := helpers.NewMockIngestServer(logger)
			defer mockServer.Close()

			client := createTestClient(mockServer.URL(), logger)
			defer func() {
				if err := client.Close(); err != nil {
					t.Logf("Failed to close client: %v", err)
				}
			}()

			// Send metrics
			ctx := context.Background()
			response, err := sendLikeTheAgent(ctx, client, tc.metrics)
			require.NoError(t, err)
			require.NotNil(t, response)

			// Parse response
			var apiResponse helpers.ApiResponse
			err = json.Unmarshal(response.Body, &apiResponse)
			require.NoError(t, err)

			// Verify failure count
			assert.Equal(t, tc.expectedFailed, apiResponse.Data.Failed, 
				"Expected %d failed metrics", tc.expectedFailed)

			// Verify error messages
			assert.NotEmpty(t, apiResponse.Data.Errors, "Should have error messages")
			
			errorStr := ""
			for _, err := range apiResponse.Data.Errors {
				errorStr += err + " "
			}

			for _, expectedError := range tc.expectedErrors {
				assert.Contains(t, errorStr, expectedError, 
					"Error message should contain: %s", expectedError)
			}
		})
	}
}

// TestResourceManagerCompatibility_CompressionAndHeaders tests that the
// request format exactly matches resource-manager expectations
func TestResourceManagerCompatibility_CompressionAndHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	
	// Start mock resource-manager server
	mockServer := helpers.NewMockIngestServer(logger)
	defer mockServer.Close()

	// Create client pointing to mock server
	client := createTestClient(mockServer.URL(), logger)
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("Failed to close client: %v", err)
		}
	}()

	// Test with minimal valid metric
	testMetrics := []aggregate.MetricWithValue{
		{
			Name: "node_cpu_seconds_total",
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
				"cpu":   "cpu0",
				"mode":  "user",
			},
			Value:     100.0,
			Timestamp: time.Now().UnixMilli(),
			Type:      "counter",
		},
	}

	// Send metrics
	ctx := context.Background()
	response, err := sendLikeTheAgent(ctx, client, testMetrics)
	require.NoError(t, err)
	require.NotNil(t, response)

	// Verify request format
	requests := mockServer.GetRequests()
	require.Len(t, requests, 1, "Should have received exactly one request")

	request := requests[0]

	// The envelope is plain JSON; each batch inside it is snappy-compressed.
	assert.Empty(t, request.ContentEncoding, "The envelope itself should not be compressed")
	assert.Greater(t, len(request.Body), 0, "The envelope should not be empty")

	assert.Equal(t, "application/timeseries-batch-v1", request.ContentType,
		"Should use the batch envelope content type")
	assert.Contains(t, request.UserAgent, "sc-metrics-agent", 
		"Should have correct user agent")

	// Test JSON format
	assert.NotEmpty(t, request.ParsedMetrics, "Should parse metrics successfully")
	assert.Len(t, request.ParsedMetrics, 1, "Should have exactly one metric")

	metric := request.ParsedMetrics[0]
	assert.Equal(t, "node_cpu_seconds_total", metric.Name, "Metric name should match")
	assert.Equal(t, "123e4567-e89b-12d3-a456-426614174000", metric.Labels["vm_id"], 
		"VM ID should match")
	assert.Equal(t, 100.0, metric.Value, "Metric value should match")
	assert.Greater(t, metric.Timestamp, int64(0), "Timestamp should be valid")
}

// TestResourceManagerCompatibility_BatchProcessing tests that large batches
// of metrics are handled correctly
func TestResourceManagerCompatibility_BatchProcessing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	
	// Start mock resource-manager server
	mockServer := helpers.NewMockIngestServer(logger)
	defer mockServer.Close()

	client := createTestClient(mockServer.URL(), logger)
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("Failed to close client: %v", err)
		}
	}()

	// Create a large batch of valid metrics
	batchSize := 100
	testMetrics := make([]aggregate.MetricWithValue, batchSize)
	
	supportedMetrics := []string{
		"node_cpu_seconds_total",
		"node_memory_MemTotal_bytes",
		"node_memory_MemFree_bytes",
		"node_load1",
		"node_disk_reads_completed_total",
		"node_network_receive_bytes_total",
	}

	for i := 0; i < batchSize; i++ {
		testMetrics[i] = aggregate.MetricWithValue{
			Name: supportedMetrics[i%len(supportedMetrics)],
			Labels: map[string]string{
				"vm_id": "123e4567-e89b-12d3-a456-426614174000",
				"instance": "test-instance",
			},
			Value:     float64(i * 10),
			Timestamp: time.Now().UnixMilli(),
			Type:      "gauge",
		}
	}

	// Send batch
	ctx := context.Background()
	response, err := sendLikeTheAgent(ctx, client, testMetrics)
	require.NoError(t, err)
	require.NotNil(t, response)

	// Verify response
	assert.Equal(t, 202, response.StatusCode)

	var apiResponse helpers.ApiResponse
	err = json.Unmarshal(response.Body, &apiResponse)
	require.NoError(t, err)

	// All metrics should be processed successfully
	assert.Equal(t, batchSize, apiResponse.Data.Processed, 
		"All %d metrics should be processed", batchSize)
	assert.Equal(t, 0, apiResponse.Data.Failed, "No metrics should fail")
	assert.Empty(t, apiResponse.Data.Errors, "Should have no errors")
}