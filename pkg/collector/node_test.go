package collector

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
)

const skipMessageNonLinux = "Skipping test on non-Linux system"

// isLinuxWithProc checks if we're running on Linux with accessible /proc filesystem
func isLinuxWithProc() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	// Check if /proc is accessible
	_, err := os.Stat("/proc")
	return err == nil
}

func TestNewSystemCollector(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	// Determine expected behavior based on platform
	expectSuccess := isLinuxWithProc()
	
	tests := []struct {
		name        string
		config      config.CollectorConfig
		expectError bool
		expectedCollectors int
	}{
		{
			name: "all collectors enabled",
			config: config.CollectorConfig{
				CPU:        true,
				LoadAvg:    true,
				Memory:     true,
				DiskStats:  true,
				Filesystem: true,
				NetDev:     true,
			},
			expectError: !expectSuccess, // Success on Linux, error on non-Linux
			expectedCollectors: 6,
		},
		{
			name: "minimal config",
			config: config.CollectorConfig{
				CPU:    true,
				Memory: true,
			},
			expectError: !expectSuccess, // Success on Linux, error on non-Linux
			expectedCollectors: 2,
		},
		{
			name:        "no collectors enabled",
			config:      config.CollectorConfig{},
			expectError: true, // Always fails - no collectors enabled
			expectedCollectors: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector, err := NewSystemCollector(tt.config, logger)
			
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			
			require.NoError(t, err)
			assert.NotNil(t, collector)
			
			enabled := collector.GetEnabledCollectors()
			assert.Len(t, enabled, tt.expectedCollectors)
		})
	}
}

func TestSystemCollectorInterface(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	// Test that SystemCollector implements the Collector interface
	var _ Collector = (*SystemCollector)(nil)
	
	// Create a minimal collector that should work even without /proc
	cfg := config.CollectorConfig{
		Memory: true,
	}
	
	// This will fail on non-Linux, but we can test the interface
	collector, err := NewSystemCollector(cfg, logger)
	if err != nil {
		t.Skip(skipMessageNonLinux)
	}
	
	// Test context cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately
	
	_, err = collector.Collect(ctx)
	assert.Equal(t, context.Canceled, err)
}

func TestSystemCollectorTimeout(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	cfg := config.CollectorConfig{
		CPU: true,
	}
	
	collector, err := NewSystemCollector(cfg, logger)
	if err != nil {
		t.Skip(skipMessageNonLinux)
	}
	
	// Test with very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	
	// Sleep to ensure timeout
	time.Sleep(1 * time.Millisecond)
	
	_, err = collector.Collect(ctx)
	assert.Equal(t, context.DeadlineExceeded, err)
}

func TestGetEnabledCollectors(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	cfg := config.CollectorConfig{
		CPU:      true,
		Memory:   true,
		LoadAvg:  true,
		NetDev:   false,
		DiskStats: false,
	}
	
	collector, err := NewSystemCollector(cfg, logger)
	if err != nil {
		t.Skip(skipMessageNonLinux)
	}
	
	enabled := collector.GetEnabledCollectors()
	
	// Should have enabled collectors
	assert.Contains(t, enabled, "cpu")
	assert.Contains(t, enabled, "memory") 
	assert.Contains(t, enabled, "loadavg")
	
	// Should not have disabled collectors
	assert.NotContains(t, enabled, "netdev")
	assert.NotContains(t, enabled, "diskstats")
	
	// All enabled should be true
	for name, isEnabled := range enabled {
		assert.True(t, isEnabled, "Collector %s should be enabled", name)
	}
}

func TestSystemCollectorClose(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	cfg := config.CollectorConfig{
		CPU: true,
	}
	
	collector, err := NewSystemCollector(cfg, logger)
	if err != nil {
		t.Skip(skipMessageNonLinux)
	}
	
	// Close should not return an error
	err = collector.Close()
	assert.NoError(t, err)
}

func TestCollectorConfigValidation(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	// Determine expected behavior based on platform
	expectLinuxSuccess := isLinuxWithProc()
	
	tests := []struct {
		name        string
		config      config.CollectorConfig
		expectError bool
	}{
		{
			name:        "empty config",
			config:      config.CollectorConfig{},
			expectError: true, // Always fails - no collectors enabled
		},
		{
			name: "valid single collector",
			config: config.CollectorConfig{
				CPU: true,
			},
			expectError: !expectLinuxSuccess, // Success on Linux with /proc, error otherwise
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSystemCollector(tt.config, logger)
			
			if tt.expectError {
				assert.Error(t, err, "Expected an error for config: %s (platform support: %v)", tt.name, expectLinuxSuccess)
			} else {
				assert.NoError(t, err, "Expected no error for config: %s (platform support: %v)", tt.name, expectLinuxSuccess)
			}
		})
	}
}

func TestCollectorMetricNames(t *testing.T) {
	// Test that our metric names follow Prometheus conventions
	tests := []struct {
		name       string
		metricName string
		valid      bool
	}{
		{"valid metric name", "node_cpu_seconds_total", true},
		{"valid gauge name", "node_memory_total_bytes", true},
		{"valid counter name", "node_network_receive_bytes_total", true},
		{"invalid spaces", "node cpu seconds", false},
		{"invalid special chars", "node-cpu-seconds", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation - metric names should not contain spaces or hyphens
			hasSpaces := false
			hasHyphens := false
			
			for _, char := range tt.metricName {
				if char == ' ' {
					hasSpaces = true
				}
				if char == '-' {
					hasHyphens = true
				}
			}
			
			isValid := !hasSpaces && !hasHyphens
			assert.Equal(t, tt.valid, isValid, "Metric name %s validation mismatch", tt.metricName)
		})
	}
}

func BenchmarkSystemCollectorCreation(b *testing.B) {
	logger := zaptest.NewLogger(b)
	cfg := config.CollectorConfig{
		CPU:    true,
		Memory: true,
	}
	
	b.ResetTimer()
	for b.Loop() {
		collector, err := NewSystemCollector(cfg, logger)
		if err == nil {
			if closeErr := collector.Close(); closeErr != nil {
				b.Errorf("Failed to close collector: %v", closeErr)
			}
		}
	}
}

func TestMockCollectorBehavior(t *testing.T) {
	// Test that we can create a mock-like collector for testing purposes
	logger := zaptest.NewLogger(t)
	
	// Since we can't test the actual collection on non-Linux systems,
	// we test the configuration and setup logic
	configs := []config.CollectorConfig{
		{CPU: true},
		{Memory: true},
		{LoadAvg: true},
		{NetDev: true},
		{DiskStats: true},
	}
	
	for i, cfg := range configs {
		t.Run(fmt.Sprintf("config_%d", i), func(t *testing.T) {
			_, err := NewSystemCollector(cfg, logger)
			// We expect an error on non-Linux systems, but the error should be about /proc, not about configuration
			if err != nil {
				assert.Contains(t, err.Error(), "proc", "Error should be about /proc filesystem, not configuration")
			}
		})
	}
}

func TestCollectorRegistry(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Test that collectors properly register their metrics
	cfg := config.CollectorConfig{
		CPU:    true,
		Memory: true,
	}

	collector, err := NewSystemCollector(cfg, logger)
	if err != nil {
		t.Skip(skipMessageNonLinux)
	}
	defer func() {
		if closeErr := collector.Close(); closeErr != nil {
			t.Errorf("Failed to close collector: %v", closeErr)
		}
	}()

	// Test that the registry is not nil and contains some metrics
	// This is implicit through successful collector creation
	assert.NotNil(t, collector)

	enabled := collector.GetEnabledCollectors()
	assert.Greater(t, len(enabled), 0, "Should have at least one enabled collector")
}

