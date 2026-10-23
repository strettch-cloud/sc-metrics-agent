package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	agentYAMLWithAddress = "metadata_service:\n  base_url: http://metadata.test.invalid\n"
	// installersConfig is what post-install.sh writes to config.yaml.
	installersConfig = "# SC-Metrics-Agent Configuration\n"
)

// useConfigFiles stands in for a VM's two files: agent.yaml, which the
// installer downloads with the metadata address, and the package's
// config.yaml. Empty content means the file doesn't exist.
func useConfigFiles(t *testing.T, agentYAML, settings string) {
	t.Helper()
	directory := t.TempDir()
	pointAt(t, &agentConfigPath, writeUnlessEmpty(t, filepath.Join(directory, "agent.yaml"), agentYAML))
	pointAt(t, &settingsPath, writeUnlessEmpty(t, filepath.Join(directory, "config.yaml"), settings))
	t.Setenv("SC_AGENT_CONFIG", "")
	t.Setenv("SC_VM_ID", "test-vm")
}

// useAgentConfig points SC_AGENT_CONFIG at a settings file that also carries
// the metadata address, so no agent.yaml is needed.
func useAgentConfig(t *testing.T, settings string) {
	t.Helper()
	useConfigFiles(t, "", "")
	path := writeUnlessEmpty(t, filepath.Join(t.TempDir(), "config.yaml"), agentYAMLWithAddress+settings)
	t.Setenv("SC_AGENT_CONFIG", path)
}

func writeUnlessEmpty(t *testing.T, path, content string) string {
	t.Helper()
	if content != "" {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return path
}

func pointAt(t *testing.T, variable *string, value string) {
	t.Helper()
	original := *variable
	*variable = value
	t.Cleanup(func() { *variable = original })
}

// The updater runs --validate-config on the installer's config.yaml, which has
// no metadata address. Load looked for the address in that file and panicked.
func TestLoadAcceptsTheConfigTheInstallerWrites(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, installersConfig)
	t.Setenv("SC_AGENT_CONFIG", settingsPath)

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, "http://metadata.test.invalid/metadata/v1/auth-token", cfg.MetadataServiceEndpoint)
}

// The installer creates config.yaml for settings, but nothing used to read it.
func TestLoadReadsThePackagesConfigFile(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "log_level: debug\n")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestLoadRunsWithoutThePackagesConfigFile(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, DefaultConfig().CollectionInterval, cfg.CollectionInterval)
}

// A settings file may carry its own metadata address, as SC_AGENT_CONFIG files
// did when they replaced agent.yaml.
func TestLoadTakesTheMetadataAddressFromTheSettingsFileFirst(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "metadata_service:\n  base_url: http://other.test.invalid\n")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, "http://other.test.invalid/metadata/v1/auth-token", cfg.MetadataServiceEndpoint)
}

func TestLoadFailsWithoutAMetadataAddress(t *testing.T) {
	useConfigFiles(t, "", installersConfig)

	_, err := Load()

	assert.ErrorContains(t, err, agentConfigPath)
}

// An empty `labels:` leaves the map nil, and adding SC_LABELS to a nil map
// panicked, so the agent restarted in a loop.
func TestLoadAddsEnvironmentLabelsWhenTheFileHasAnEmptyLabelsKey(t *testing.T) {
	useAgentConfig(t, "labels:\n")
	t.Setenv("SC_LABELS", "environment=test")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"environment": "test"}, cfg.Labels)
}

// The logger only knows lowercase level names, so "DEBUG" used to pass
// validation and then log at info.
func TestLoadAcceptsTheLogLevelInAnyCase(t *testing.T) {
	tests := map[string]struct {
		settings    string
		environment string
		want        string
	}{
		"from the environment": {environment: "DEBUG", want: "debug"},
		"from the file":        {settings: "log_level: Warn\n", want: "warn"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			useAgentConfig(t, test.settings)
			t.Setenv("SC_LOG_LEVEL", test.environment)

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, test.want, cfg.LogLevel)
		})
	}
}
