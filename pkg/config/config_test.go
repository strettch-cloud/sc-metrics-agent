package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.NotNil(t, cfg)
	assert.Equal(t, 30*time.Second, cfg.CollectionInterval)
	assert.Equal(t, 30*time.Second, cfg.HTTPTimeout)
	// VM ID might be empty on test systems without dmidecode
	if cfg.VMID == "" {
		cfg.VMID = "test-vm-id"
	}
	assert.NotEmpty(t, cfg.VMID)
	assert.NotNil(t, cfg.Labels)
	assert.True(t, cfg.Collectors.Processes)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, 5*time.Second, cfg.RetryInterval)
}

func TestLoad_DefaultsOnly(t *testing.T) {
	// Create temporary agent.yaml
	tmpDir := t.TempDir()
	agentConfigPath := filepath.Join(tmpDir, "agent.yaml")
	agentYAMLContent := `metadata_service:
  base_url: http://test.example.com
`
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(agentYAMLContent), 0644))

	// Clear environment variables
	clearEnvVars()

	// Set SC_AGENT_CONFIG to use our test file
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))
	defer func() {
		if err := os.Unsetenv("SC_AGENT_CONFIG"); err != nil {
			t.Logf("Failed to unset SC_AGENT_CONFIG: %v", err)
		}
	}()

	// Set test VM ID to avoid validation failure on test systems
	require.NoError(t, os.Setenv("SC_VM_ID", "test-vm-default"))
	defer func() {
		if err := os.Unsetenv("SC_VM_ID"); err != nil {
			t.Logf("Failed to unset SC_VM_ID: %v", err)
		}
	}()

	cfg, err := Load()
	require.NoError(t, err)

	// Should match defaults (except VM ID which we set)
	assert.Equal(t, 30*time.Second, cfg.CollectionInterval)
	assert.Equal(t, 30*time.Second, cfg.HTTPTimeout)
	assert.Equal(t, "http://test.example.com/metadata/v1/auth-token", cfg.MetadataServiceEndpoint)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, 5*time.Second, cfg.RetryInterval)
	assert.Equal(t, true, cfg.Collectors.Processes)
	assert.Equal(t, "test-vm-default", cfg.VMID)
}

func TestLoad_FromEnvironment(t *testing.T) {
	// Create temporary agent.yaml
	tmpDir := t.TempDir()
	agentConfigPath := filepath.Join(tmpDir, "agent.yaml")
	agentYAMLContent := `metadata_service:
  base_url: http://test-env.example.com
`
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(agentYAMLContent), 0644))

	// Clear environment first
	clearEnvVars()

	// Set SC_AGENT_CONFIG to use our test file
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))

	// Set test environment variables
	testEnvVars := map[string]string{
		"SC_COLLECTION_INTERVAL": "60s",
		"SC_HTTP_TIMEOUT":        "45s",
		"SC_VM_ID":               "test-vm-123",
		"SC_LOG_LEVEL":           "debug",
		"SC_MAX_RETRIES":         "5",
		"SC_RETRY_INTERVAL":      "10s",
		"SC_LABELS":              "env=test,region=us-west-2",
		"SC_COLLECTOR_PROCESSES": "false",
	}

	for key, value := range testEnvVars {
		require.NoError(t, os.Setenv(key, value))
	}
	defer clearEnvVars()

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 60*time.Second, cfg.CollectionInterval)
	assert.Equal(t, 45*time.Second, cfg.HTTPTimeout)
	assert.Equal(t, "test-vm-123", cfg.VMID)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, 5, cfg.MaxRetries)
	assert.Equal(t, 10*time.Second, cfg.RetryInterval)
	assert.Equal(t, "test", cfg.Labels["env"])
	assert.Equal(t, "us-west-2", cfg.Labels["region"])
	assert.False(t, cfg.Collectors.Processes)
	assert.Equal(t, "http://test-env.example.com/metadata/v1/auth-token", cfg.MetadataServiceEndpoint)
}

func TestLoad_FromFile(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "collection_interval: 45s\nlabels:\n  team: web\ncollectors:\n  thermal: false\n")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, cfg.CollectionInterval)
	assert.Equal(t, map[string]string{"team": "web"}, cfg.Labels)
	assert.False(t, cfg.Collectors.Thermal)
	assert.True(t, cfg.Collectors.CPU, "a collector the file doesn't mention keeps its default")
}

func TestLoad_EnvironmentOverridesFile(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "collection_interval: 45s\n")
	t.Setenv("SC_COLLECTION_INTERVAL", "60s")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, 60*time.Second, cfg.CollectionInterval)
}

func TestLoad_InvalidConfigFile(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "collection_interval: [\n")

	_, err := Load()

	assert.ErrorContains(t, err, settingsPath)
}

// The package's own settings file is optional, but a file named in
// SC_AGENT_CONFIG must exist: a typo there shouldn't silently fall back.
func TestLoad_NonexistentConfigFile(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("SC_AGENT_CONFIG", missing)

	_, err := Load()

	assert.ErrorContains(t, err, missing)
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{
			name:  "simple labels",
			input: "key1=value1,key2=value2",
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:  "labels with spaces",
			input: " key1 = value1 , key2 = value2 ",
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:  "single label",
			input: "env=production",
			expected: map[string]string{
				"env": "production",
			},
		},
		{
			name:     "empty string",
			input:    "",
			expected: map[string]string{},
		},
		{
			name:  "label with equals in value",
			input: "query=SELECT * FROM table WHERE id=123",
			expected: map[string]string{
				"query": "SELECT * FROM table WHERE id=123",
			},
		},
		{
			name:     "malformed labels",
			input:    "key1,key2=value2,key3",
			expected: map[string]string{"key2": "value2"},
		},
		{
			name:     "empty keys",
			input:    "=value1,key2=value2",
			expected: map[string]string{"key2": "value2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseLabels(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidate(t *testing.T) {
	// Create temporary agent.yaml for DefaultConfig
	tmpDir := t.TempDir()
	agentConfigPath := filepath.Join(tmpDir, "agent.yaml")
	agentYAMLContent := `metadata_service:
  base_url: http://test.example.com
`
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(agentYAMLContent), 0644))
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))
	defer func() {
		if err := os.Unsetenv("SC_AGENT_CONFIG"); err != nil {
			t.Logf("Failed to unset SC_AGENT_CONFIG: %v", err)
		}
	}()

	// Valid config should pass
	validConfig := DefaultConfig()
	validConfig.VMID = "test-vm-validation" // Set test VM ID
	err := validConfig.validate()
	assert.NoError(t, err)

	// Test invalid collection interval
	invalidConfig := *validConfig
	invalidConfig.CollectionInterval = 0
	err = invalidConfig.validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "collection_interval must be positive")

	// Test invalid HTTP timeout
	invalidConfig = *validConfig
	invalidConfig.HTTPTimeout = 0
	err = invalidConfig.validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "http_timeout must be positive")

	// Test empty VM ID
	invalidConfig = *validConfig
	invalidConfig.VMID = ""
	err = invalidConfig.validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vm_id cannot be determined")

	// Test negative max retries
	invalidConfig = *validConfig
	invalidConfig.MaxRetries = -1
	err = invalidConfig.validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max_retries cannot be negative")

	// Test invalid retry interval
	invalidConfig = *validConfig
	invalidConfig.RetryInterval = 0
	err = invalidConfig.validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "retry_interval must be positive")

	// Test invalid log level
	invalidConfig = *validConfig
	invalidConfig.LogLevel = "invalid"
	err = invalidConfig.validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid log_level")

	// Test valid log levels
	validLogLevels := []string{"debug", "info", "warn", "error", "fatal", "panic"}
	for _, level := range validLogLevels {
		validConfig.LogLevel = level
		err = validConfig.validate()
		assert.NoError(t, err, "Log level %s should be valid", level)

		validConfig.LogLevel = strings.ToUpper(level)
		err = validConfig.validate()
		assert.NoError(t, err, "Uppercase log level should be valid due to case-insensitive validation")
	}
}

func TestString(t *testing.T) {
	// Create temporary agent.yaml for DefaultConfig
	tmpDir := t.TempDir()
	agentConfigPath := filepath.Join(tmpDir, "agent.yaml")
	agentYAMLContent := `metadata_service:
  base_url: http://test.example.com
`
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(agentYAMLContent), 0644))
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))
	defer func() {
		if err := os.Unsetenv("SC_AGENT_CONFIG"); err != nil {
			t.Logf("Failed to unset SC_AGENT_CONFIG: %v", err)
		}
	}()

	cfg := DefaultConfig()
	cfg.VMID = "test-vm"
	cfg.LogLevel = "debug"

	str := cfg.String()

	assert.Contains(t, str, "Config{")
	assert.Contains(t, str, "CollectionInterval:")
	assert.Contains(t, str, "VMID:test-vm")
	assert.Contains(t, str, "LogLevel:debug")
	assert.Contains(t, str, "Collectors:")
}

func TestLoadFromEnv_InvalidValues(t *testing.T) {
	// Create temporary agent.yaml for Load
	tmpDir := t.TempDir()
	agentConfigPath := filepath.Join(tmpDir, "agent.yaml")
	agentYAMLContent := `metadata_service:
  base_url: http://test.example.com
`
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(agentYAMLContent), 0644))

	clearEnvVars()

	// Set SC_AGENT_CONFIG to use our test file
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))

	// Set invalid environment variables
	require.NoError(t, os.Setenv("SC_COLLECTION_INTERVAL", "invalid"))
	require.NoError(t, os.Setenv("SC_HTTP_TIMEOUT", "invalid"))
	require.NoError(t, os.Setenv("SC_MAX_RETRIES", "invalid"))
	require.NoError(t, os.Setenv("SC_RETRY_INTERVAL", "invalid"))
	require.NoError(t, os.Setenv("SC_COLLECTOR_PROCESSES", "invalid"))
	require.NoError(t, os.Setenv("SC_VM_ID", "test-vm-invalid")) // Set test VM ID
	defer clearEnvVars()

	cfg, err := Load()
	require.NoError(t, err)

	// Should use default values for invalid env vars
	assert.Equal(t, 30*time.Second, cfg.CollectionInterval)
	assert.Equal(t, 30*time.Second, cfg.HTTPTimeout)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, 5*time.Second, cfg.RetryInterval)
	assert.Equal(t, true, cfg.Collectors.Processes)
	assert.Equal(t, "test-vm-invalid", cfg.VMID)
	assert.Equal(t, "http://test.example.com/metadata/v1/auth-token", cfg.MetadataServiceEndpoint)
}

func TestCollectorConfig(t *testing.T) {
	// Create temporary agent.yaml for DefaultConfig
	tmpDir := t.TempDir()
	agentConfigPath := filepath.Join(tmpDir, "agent.yaml")
	agentYAMLContent := `metadata_service:
  base_url: http://test.example.com
`
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(agentYAMLContent), 0644))
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))
	defer func() {
		if err := os.Unsetenv("SC_AGENT_CONFIG"); err != nil {
			t.Logf("Failed to unset SC_AGENT_CONFIG: %v", err)
		}
	}()

	cfg := DefaultConfig()
	cfg.VMID = "test-vm-collector" // Set test VM ID

	// Test default collector config
	assert.True(t, cfg.Collectors.Processes)

	// Test collector config from environment
	clearEnvVars()
	require.NoError(t, os.Setenv("SC_AGENT_CONFIG", agentConfigPath))
	require.NoError(t, os.Setenv("SC_COLLECTOR_PROCESSES", "false"))
	require.NoError(t, os.Setenv("SC_VM_ID", "test-vm-collector-env")) // Set test VM ID
	defer clearEnvVars()

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.Collectors.Processes)
	assert.Equal(t, "test-vm-collector-env", cfg.VMID)
}

// Helper functions

func clearEnvVars() {
	envVars := []string{
		"SC_AGENT_CONFIG",
		"SC_COLLECTION_INTERVAL",
		"SC_HTTP_TIMEOUT",
		"SC_VM_ID",
		"SC_LOG_LEVEL",
		"SC_MAX_RETRIES",
		"SC_RETRY_INTERVAL",
		"SC_LABELS",
		"SC_COLLECTOR_PROCESSES",
	}

	for _, envVar := range envVars {
		_ = os.Unsetenv(envVar) // Ignore errors in cleanup
	}
}
