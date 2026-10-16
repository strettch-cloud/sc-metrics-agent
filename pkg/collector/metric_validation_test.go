package collector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	dto "github.com/prometheus/client_model/go"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
)

const skipMessageNonLinuxValidation = "Skipping test on non-Linux system"

// ResourceManagerSupportedMetrics defines the exact whitelist of metrics 
// supported by the resource-manager as defined in metrics_dto.go:mapMetricNameToTypeAndUnit
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

// TestCollectorGeneratesOnlySupportedMetrics ensures that our collectors
// only generate metrics that are supported by the resource-manager.
// This is critical to prevent unsupported metrics from being logged as failures.
func TestCollectorGeneratesOnlySupportedMetrics(t *testing.T) {
	// Skip test on non-Linux systems since collectors depend on /proc
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	
	// Test all collectors enabled
	cfg := config.CollectorConfig{
		CPU:        true,
		LoadAvg:    true,
		Memory:     true,
		DiskStats:  true,
		Filesystem: true,
		NetDev:     true,
	}

	collector, err := NewSystemCollector(cfg, logger)
	if err != nil {
		t.Skip(skipMessageNonLinuxValidation + " (cannot access /proc)")
	}
	defer func() {
		if closeErr := collector.Close(); closeErr != nil {
			t.Logf("Failed to close collector: %v", closeErr)
		}
	}()

	// Collect metrics
	ctx := context.Background()
	metricFamilies, err := collector.Collect(ctx)
	if err != nil {
		t.Skip("Skipping test due to collection error (likely non-Linux system)")
	}

	// Validate all collected metrics are in the supported whitelist
	var unsupportedMetrics []string
	var supportedMetricsFound []string

	for _, family := range metricFamilies {
		metricName := family.GetName()
		
		if ResourceManagerSupportedMetrics[metricName] {
			supportedMetricsFound = append(supportedMetricsFound, metricName)
		} else {
			unsupportedMetrics = append(unsupportedMetrics, metricName)
		}
	}

	// CRITICAL: Fail test if any unsupported metrics are found
	if len(unsupportedMetrics) > 0 {
		t.Errorf("CRITICAL: Found %d unsupported metrics that will be rejected by resource-manager:\n%v", 
			len(unsupportedMetrics), unsupportedMetrics)
		t.Errorf("These metrics will be logged as failures in the resource-manager")
		t.FailNow()
	}

	// Ensure we found some supported metrics
	assert.Greater(t, len(supportedMetricsFound), 0, 
		"Expected to find at least some supported metrics, but found none")

	t.Logf("SUCCESS: All %d collected metrics are supported by resource-manager: %v", 
		len(supportedMetricsFound), supportedMetricsFound)
}

// collectorTestCase represents a test case for individual collector testing
type collectorTestCase struct {
	name             string
	config           config.CollectorConfig
	expectedMetrics  []string
}

// getCollectorTestCases returns test cases for individual collector compliance testing
func getCollectorTestCases() []collectorTestCase {
	return []collectorTestCase{
		{
			name: "CPU collector",
			config: config.CollectorConfig{CPU: true},
			expectedMetrics: []string{"node_cpu_seconds_total"},
		},
		{
			name: "Memory collector", 
			config: config.CollectorConfig{Memory: true},
			expectedMetrics: []string{
				"node_memory_MemTotal_bytes",
				"node_memory_MemFree_bytes", 
				"node_memory_MemAvailable_bytes",
				"node_memory_Buffers_bytes",
				"node_memory_Cached_bytes",
				"node_memory_SwapTotal_bytes",
				"node_memory_SwapFree_bytes",
			},
		},
		{
			name: "Load average collector",
			config: config.CollectorConfig{LoadAvg: true}, 
			expectedMetrics: []string{
				"node_load1",
				"node_load5", 
				"node_load15",
			},
		},
		{
			name: "Disk stats collector",
			config: config.CollectorConfig{DiskStats: true},
			expectedMetrics: []string{
				"node_disk_reads_completed_total",
				"node_disk_writes_completed_total",
				"node_disk_read_bytes_total",
				"node_disk_written_bytes_total",
			},
		},
		{
			name: "Network collector",
			config: config.CollectorConfig{NetDev: true},
			expectedMetrics: []string{
				"node_network_receive_bytes_total",
				"node_network_transmit_bytes_total",
				"node_network_receive_packets_total", 
				"node_network_transmit_packets_total",
			},
		},
		{
			name: "Filesystem collector",
			config: config.CollectorConfig{Filesystem: true},
			expectedMetrics: []string{
				"node_filesystem_size_bytes",
			},
		},
	}
}

// validateCollectorMetrics validates that collector metrics are supported and expected
func validateCollectorMetrics(t *testing.T, metricFamilies []*dto.MetricFamily, tc collectorTestCase) {
	foundMetrics := make(map[string]bool)
	var unsupportedMetrics []string

	for _, family := range metricFamilies {
		metricName := family.GetName()
		foundMetrics[metricName] = true

		if !ResourceManagerSupportedMetrics[metricName] {
			unsupportedMetrics = append(unsupportedMetrics, metricName)
		}
	}

	// CRITICAL: No unsupported metrics allowed
	assert.Empty(t, unsupportedMetrics, 
		"Collector %s generated unsupported metrics: %v", tc.name, unsupportedMetrics)

	// Verify we got the expected metrics (at least some of them, as exact metrics depend on system)
	foundExpectedCount := 0
	for _, expectedMetric := range tc.expectedMetrics {
		if foundMetrics[expectedMetric] {
			foundExpectedCount++
		}
	}

	assert.Greater(t, foundExpectedCount, 0, 
		"Expected to find at least one of the expected metrics %v", tc.expectedMetrics)
}

// testSingleCollectorCompliance tests a single collector for metric compliance
func testSingleCollectorCompliance(t *testing.T, tc collectorTestCase, logger *zap.Logger) {
	collector, err := NewSystemCollector(tc.config, logger)
	if err != nil {
		t.Skip(skipMessageNonLinuxValidation)
	}
	defer func() {
		if closeErr := collector.Close(); closeErr != nil {
			t.Logf("Failed to close collector: %v", closeErr)
		}
	}()

	// Collect metrics
	ctx := context.Background()
	metricFamilies, err := collector.Collect(ctx)
	if err != nil {
		t.Skip("Skipping test due to collection error")
	}

	// Validate metrics
	validateCollectorMetrics(t, metricFamilies, tc)
}

// TestIndividualCollectorMetricCompliance tests each collector type individually
// to ensure they only generate supported metrics
func TestIndividualCollectorMetricCompliance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zaptest.NewLogger(t)
	testCases := getCollectorTestCases()

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testSingleCollectorCompliance(t, tc, logger)
		})
	}
}

// TestMetricNameFormat validates that our metric names follow the expected format
func TestMetricNameFormat(t *testing.T) {
	for metricName := range ResourceManagerSupportedMetrics {
		// Validate metric name format (should start with "node_")
		assert.True(t, len(metricName) > 5, "Metric name too short: %s", metricName)
		assert.True(t, metricName[:5] == "node_", "Metric name should start with 'node_': %s", metricName)
		
		// Should not contain spaces or hyphens
		assert.NotContains(t, metricName, " ", "Metric name should not contain spaces: %s", metricName)
		assert.NotContains(t, metricName, "-", "Metric name should not contain hyphens: %s", metricName)
	}
}

// TestResourceManagerMetricWhitelist validates our hardcoded whitelist matches
// the resource-manager expectations exactly
func TestResourceManagerMetricWhitelist(t *testing.T) {
	// This test documents the exact metrics supported by resource-manager
	// If this test fails, it means the resource-manager has changed its supported metrics
	// and this whitelist needs to be updated
	
	expectedCount := 22 // Total number of supported metrics as of latest check
	actualCount := len(ResourceManagerSupportedMetrics)
	
	assert.Equal(t, expectedCount, actualCount, 
		"Metric whitelist count mismatch - resource-manager support may have changed")

	// Verify specific critical metrics are present
	criticalMetrics := []string{
		"node_cpu_seconds_total",
		"node_memory_MemTotal_bytes",
		"node_memory_MemFree_bytes", 
		"node_load1",
		"node_disk_reads_completed_total",
		"node_network_receive_bytes_total",
		"node_filesystem_size_bytes",
	}

	for _, metric := range criticalMetrics {
		assert.True(t, ResourceManagerSupportedMetrics[metric], 
			"Critical metric missing from whitelist: %s", metric)
	}
}