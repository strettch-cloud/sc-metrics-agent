package helpers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/snappy"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Content types the resource manager accepts. They are spelled out here rather
// than taken from the agent's tsclient: the fake stands for the server, so a
// wrong value in the agent must fail the tests, not be accepted by both.
const (
	contentTypeTimeseriesBinary = "application/timeseries-binary-0"
	contentTypeTimeseriesBatch  = "application/timeseries-batch-v1"
)

// TimeseriesMetric represents the expected format from the agent
type TimeseriesMetric struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
	Type      string            `json:"type"`
}

// TimeseriesMetrics represents an array of timeseries metrics
type TimeseriesMetrics []TimeseriesMetric

// MetricsProcessingResponse matches resource-manager response format
type MetricsProcessingResponse struct {
	Status    string   `json:"status"`
	Processed int      `json:"processed"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
}

// ApiResponse matches resource-manager API response structure
type ApiResponse struct {
	Message string                    `json:"message"`
	Data    MetricsProcessingResponse `json:"data"`
	Errors  interface{}               `json:"errors"`
}

// MockIngestServer simulates the resource-manager /metrics/ingest endpoint
// with exact validation logic matching the real implementation
type MockIngestServer struct {
	server *httptest.Server
	logger *zap.Logger

	// mutex guards the fields below: a running agent sends while a test reads.
	mutex       sync.Mutex
	requests    []IngestRequest
	heartbeats  []HeartbeatRequest
	unavailable     bool
	rejectedIngests int
}

// HeartbeatRequest captures a heartbeat the agent sent.
type HeartbeatRequest struct {
	Authorization string
	Body          []byte
}

// IngestRequest captures details of received requests for testing
type IngestRequest struct {
	Authorization   string
	ContentType     string
	ContentEncoding string
	UserAgent       string
	Body            []byte
	DecompressedBody []byte
	ParsedMetrics   TimeseriesMetrics
	Timestamp       time.Time
}

// ResourceManagerSupportedMetrics is the exact whitelist from resource-manager:
// keep it in sync with resource-manager/internal/dto/metrics_dto.go in
// strettch-cloud/platform, or tests reject metrics production accepts.
var ResourceManagerSupportedMetrics = map[string]bool{
	// CPU Metrics
	"node_cpu_seconds_total": true,

	// Memory Metrics
	"node_memory_MemTotal_bytes":     true,
	"node_memory_MemFree_bytes":      true,
	"node_memory_MemAvailable_bytes": true,
	"node_memory_Buffers_bytes":      true,
	"node_memory_Cached_bytes":       true,
	"node_memory_SwapTotal_bytes":    true,
	"node_memory_SwapFree_bytes":     true,

	// Load Average Metrics
	"node_load1":  true,
	"node_load5":  true,
	"node_load15": true,

	// Disk Stats Metrics
	"node_disk_reads_completed_total":  true,
	"node_disk_writes_completed_total": true,
	"node_disk_read_bytes_total":       true,
	"node_disk_written_bytes_total":    true,

	// Network Metrics
	"node_network_receive_bytes_total":    true,
	"node_network_transmit_bytes_total":   true,
	"node_network_receive_packets_total":  true,
	"node_network_transmit_packets_total": true,

	// Filesystem Metrics
	"node_filesystem_size_bytes":  true,
	"node_filesystem_free_bytes":  true,
	"node_filesystem_avail_bytes": true,
}

// NewMockIngestServer creates a new mock server that behaves exactly like resource-manager
func NewMockIngestServer(logger *zap.Logger) *MockIngestServer {
	mock := &MockIngestServer{
		logger:   logger,
		requests: make([]IngestRequest, 0),
	}

	mock.server = httptest.NewServer(http.HandlerFunc(mock.handleIngest))
	return mock
}

// URL returns the mock server URL
func (m *MockIngestServer) URL() string {
	return m.server.URL + "/metrics/ingest"
}

// BaseURL is the cloud API address the metadata service hands to the agent.
func (m *MockIngestServer) BaseURL() string {
	return m.server.URL
}

// SetUnavailable makes every request fail with 503, as during an outage.
func (m *MockIngestServer) SetUnavailable(unavailable bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.unavailable = unavailable
}

// RejectedIngestCount is how many metric sends were refused while unavailable.
func (m *MockIngestServer) RejectedIngestCount() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.rejectedIngests
}

// GetHeartbeats returns the heartbeats received so far.
func (m *MockIngestServer) GetHeartbeats() []HeartbeatRequest {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]HeartbeatRequest(nil), m.heartbeats...)
}

// Close shuts down the mock server
func (m *MockIngestServer) Close() {
	m.server.Close()
}

// GetRequests returns all received requests for testing
func (m *MockIngestServer) GetRequests() []IngestRequest {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]IngestRequest(nil), m.requests...)
}

// ClearRequests clears the request history
func (m *MockIngestServer) ClearRequests() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.requests = make([]IngestRequest, 0)
}

// handleIngest implements the exact logic from resource-manager metrics handler
func (m *MockIngestServer) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.mutex.Lock()
	unavailable := m.unavailable
	if unavailable && strings.HasSuffix(r.URL.Path, "/metrics/ingest") {
		m.rejectedIngests++
	}
	m.mutex.Unlock()
	if unavailable {
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/compute/agent/heartbeat") {
		m.handleHeartbeat(w, r)
		return
	}

	if !strings.HasSuffix(r.URL.Path, "/metrics/ingest") {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	contentType := r.Header.Get("Content-Type")
	contentEncoding := r.Header.Get("Content-Encoding")
	userAgent := r.Header.Get("User-Agent")

	m.logger.Info("Received metric ingestion request",
		zap.String("content-type", contentType),
		zap.String("content-encoding", contentEncoding),
		zap.String("user-agent", userAgent),
	)

	// Read request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		m.logger.Error("Failed to read request body", zap.Error(err))
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			m.logger.Warn("Failed to close request body", zap.Error(err))
		}
	}()

	request := IngestRequest{
		Authorization:   r.Header.Get("Authorization"),
		ContentType:     contentType,
		ContentEncoding: contentEncoding,
		UserAgent:       userAgent,
		Body:            bodyBytes,
		Timestamp:       time.Now(),
	}

	// Handle Snappy compression (matching resource-manager logic)
	decompressedBody := bodyBytes
	if contentEncoding == "snappy" {
		decoded, err := snappy.Decode(nil, bodyBytes)
		if err != nil {
			m.logger.Error("Failed to decompress snappy request body", zap.Error(err))
			http.Error(w, "Failed to decompress request body", http.StatusBadRequest)
			return
		}
		decompressedBody = decoded
		m.logger.Debug("Successfully decompressed Snappy payload", 
			zap.Int("original_size", len(bodyBytes)),
			zap.Int("decompressed_size", len(decompressedBody)))
	}

	request.DecompressedBody = decompressedBody

	// Handle different content types (matching resource-manager switch statement)
	switch {
	case strings.Contains(contentType, contentTypeTimeseriesBinary):
		response := m.handleTimeseriesMetrics(decompressedBody, &request)
		m.sendResponse(w, response)
	case strings.Contains(contentType, contentTypeTimeseriesBatch):
		// The running agent sends this format: its disk buffer drains batches in one envelope.
		if contentEncoding == "snappy" {
			http.Error(w, "Envelope must not be snappy-encoded", http.StatusBadRequest)
			return
		}
		metrics, err := decodeBatchEnvelope(decompressedBody)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.sendResponse(w, m.processMetrics(metrics, &request))
	default:
		m.logger.Error("Unsupported content type", zap.String("content_type", contentType))
		http.Error(w, fmt.Sprintf("Unsupported content type: %s", contentType), http.StatusBadRequest)
		return
	}

	// Store request for testing
	m.mutex.Lock()
	m.requests = append(m.requests, request)
	m.mutex.Unlock()
}

func (m *MockIngestServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	m.mutex.Lock()
	m.heartbeats = append(m.heartbeats, HeartbeatRequest{Authorization: r.Header.Get("Authorization"), Body: body})
	m.mutex.Unlock()
	w.WriteHeader(http.StatusOK)
}

// handleTimeseriesMetrics processes timeseries metrics with exact resource-manager validation
func (m *MockIngestServer) handleTimeseriesMetrics(bodyBytes []byte, request *IngestRequest) ApiResponse {
	var timeseriesMetrics TimeseriesMetrics
	if err := json.Unmarshal(bodyBytes, &timeseriesMetrics); err != nil {
		m.logger.Error("Failed to parse timeseries metrics", zap.Error(err))
		return ApiResponse{
			Message: "Invalid timeseries payload",
			Data: MetricsProcessingResponse{
				Status:    "error",
				Processed: 0,
				Failed:    1,
				Errors:    []string{fmt.Sprintf("JSON parse error: %s", err.Error())},
			},
		}
	}

	return m.processMetrics(timeseriesMetrics, request)
}

// decodeBatchEnvelope mirrors resource-manager's DecodeTimeseriesBatchEnvelope:
// a JSON array of base64 strings, each a snappy-compressed JSON batch of metrics.
func decodeBatchEnvelope(body []byte) (TimeseriesMetrics, error) {
	var encodedBatches []string
	if err := json.Unmarshal(body, &encodedBatches); err != nil {
		return nil, fmt.Errorf("invalid envelope JSON: %w", err)
	}
	if len(encodedBatches) == 0 {
		return nil, fmt.Errorf("envelope contains no batches")
	}
	var metrics TimeseriesMetrics
	for index, encodedBatch := range encodedBatches {
		snappyBatch, err := base64.StdEncoding.DecodeString(encodedBatch)
		if err != nil {
			return nil, fmt.Errorf("base64 decode failed at batch index %d: %w", index, err)
		}
		jsonBatch, err := snappy.Decode(nil, snappyBatch)
		if err != nil {
			return nil, fmt.Errorf("snappy decode failed at batch index %d: %w", index, err)
		}
		var batch TimeseriesMetrics
		if err := json.Unmarshal(jsonBatch, &batch); err != nil {
			return nil, fmt.Errorf("json decode failed at batch index %d: %w", index, err)
		}
		metrics = append(metrics, batch...)
	}
	return metrics, nil
}

// processMetrics applies resource-manager's per-metric checks.
func (m *MockIngestServer) processMetrics(timeseriesMetrics TimeseriesMetrics, request *IngestRequest) ApiResponse {
	request.ParsedMetrics = timeseriesMetrics

	processed := 0
	failed := 0
	var errors []string

	// Process each metric with exact resource-manager logic
	for i, metric := range timeseriesMetrics {
		// Validate vm_id label (matching resource-manager logic)
		vmIDStr, exists := metric.Labels["vm_id"]
		if !exists {
			failed++
			errors = append(errors, fmt.Sprintf("Metric %d: vm_id label is required", i))
			continue
		}

		// Validate UUID format
		if _, err := uuid.Parse(vmIDStr); err != nil {
			failed++
			errors = append(errors, fmt.Sprintf("Metric %d: vm_id must be a valid UUID", i))
			continue
		}

		// Validate timestamp format (should be Unix milliseconds)
		if metric.Timestamp <= 0 {
			failed++
			errors = append(errors, fmt.Sprintf("Metric %d: invalid timestamp %d", i, metric.Timestamp))
			continue
		}

		// Validate metric name against whitelist (exact resource-manager logic)
		if !ResourceManagerSupportedMetrics[metric.Name] {
			failed++
			m.logger.Warn("Unsupported metric type received",
				zap.Int("index", i),
				zap.String("metric_name", metric.Name))
			errors = append(errors, fmt.Sprintf("Unsupported metric: %s", metric.Name))
			continue
		}

		// If we reach here, metric is valid
		processed++
	}

	responseData := MetricsProcessingResponse{
		Status:    "metrics processed",
		Processed: processed,
		Failed:    failed,
	}

	if len(errors) > 0 {
		// Limit error list to 10 items (matching resource-manager)
		if len(errors) > 10 {
			responseData.Errors = errors[:10]
		} else {
			responseData.Errors = errors
		}
	}

	return ApiResponse{
		Message: "Metrics queued for processing",
		Data:    responseData,
		Errors:  nil,
	}
}

// sendResponse sends API response in resource-manager format
func (m *MockIngestServer) sendResponse(w http.ResponseWriter, response ApiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202 status like resource-manager

	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.logger.Error("Failed to encode response", zap.Error(err))
	}
}

// ValidateRequest validates that a request matches expected format
func (m *MockIngestServer) ValidateRequest(request IngestRequest) []string {
	var issues []string

	// The running agent drains its disk buffer as a plain JSON batch-v1
	// envelope; a direct send is snappy-encoded binary-0.
	switch request.ContentType {
	case contentTypeTimeseriesBatch:
		if request.ContentEncoding != "" {
			issues = append(issues, fmt.Sprintf("Expected no content-encoding on a batch envelope, got '%s'", request.ContentEncoding))
		}
	case contentTypeTimeseriesBinary:
		if request.ContentEncoding != "snappy" {
			issues = append(issues, fmt.Sprintf("Expected content-encoding 'snappy', got '%s'", request.ContentEncoding))
		}
	default:
		issues = append(issues, fmt.Sprintf("Unexpected content-type '%s'", request.ContentType))
	}

	if !strings.Contains(request.UserAgent, "sc-metrics-agent") {
		issues = append(issues, fmt.Sprintf("Expected user-agent to contain 'sc-metrics-agent', got '%s'", request.UserAgent))
	}

	// Validate metrics format
	if len(request.ParsedMetrics) == 0 {
		issues = append(issues, "No metrics found in request")
	}

	for i, metric := range request.ParsedMetrics {
		// Check required fields
		if metric.Name == "" {
			issues = append(issues, fmt.Sprintf("Metric %d: missing name", i))
		}
		if metric.Labels == nil {
			issues = append(issues, fmt.Sprintf("Metric %d: missing labels", i))
		}
		if metric.Timestamp <= 0 {
			issues = append(issues, fmt.Sprintf("Metric %d: invalid timestamp", i))
		}

		// Check vm_id label
		if vmID, exists := metric.Labels["vm_id"]; !exists {
			issues = append(issues, fmt.Sprintf("Metric %d: missing vm_id label", i))
		} else if _, err := uuid.Parse(vmID); err != nil {
			issues = append(issues, fmt.Sprintf("Metric %d: invalid vm_id format", i))
		}

		// Check metric name against whitelist
		if !ResourceManagerSupportedMetrics[metric.Name] {
			issues = append(issues, fmt.Sprintf("Metric %d: unsupported metric name '%s'", i, metric.Name))
		}
	}

	return issues
}

// MockMetadataServer simulates the metadata service for testing
type MockMetadataServer struct {
	server      *httptest.Server
	cloudAPIURL string
	logger      *zap.Logger

	mutex        sync.Mutex
	issuedTokens []string
}

// IssuedTokens returns every token handed out so far.
func (m *MockMetadataServer) IssuedTokens() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]string(nil), m.issuedTokens...)
}

// NewMockMetadataServer creates a new mock metadata server
func NewMockMetadataServer(cloudAPIURL string, logger *zap.Logger) *MockMetadataServer {
	mock := &MockMetadataServer{
		cloudAPIURL: cloudAPIURL,
		logger:      logger,
	}

	// Create the HTTP server
	mock.server = httptest.NewServer(http.HandlerFunc(mock.handleRequest))
	
	return mock
}

// URL returns the mock server URL
func (m *MockMetadataServer) URL() string {
	return m.server.URL
}

// Close shuts down the mock server
func (m *MockMetadataServer) Close() {
	m.server.Close()
}

// handleRequest handles all requests to the mock metadata service
func (m *MockMetadataServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	m.logger.Debug("Mock metadata service received request", 
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
	)

	// The agent builds this path from metadata_service.base_url.
	if r.Method == "GET" && r.URL.Path == "/metadata/v1/auth-token" {
		m.handleTokenRequest(w, r)
		return
	}

	// Return 404 for unhandled endpoints
	http.NotFound(w, r)
}

// handleTokenRequest simulates the metadata service auth token endpoint
func (m *MockMetadataServer) handleTokenRequest(w http.ResponseWriter, r *http.Request) {
	// Simulate the metadata service response format
	token := "mock-test-token-" + time.Now().Format("20060102150405")
	m.mutex.Lock()
	m.issuedTokens = append(m.issuedTokens, token)
	m.mutex.Unlock()
	response := map[string]interface{}{
		"token":       token,
		"cloudAPIUrl": m.cloudAPIURL,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.logger.Error("Failed to encode metadata response", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}

	m.logger.Debug("Returned mock metadata response", 
		zap.String("cloudAPIUrl", m.cloudAPIURL),
	)
}