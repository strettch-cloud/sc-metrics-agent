package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	agentYAMLWithAddress = "metadata_service:\n  base_url: http://metadata.test.invalid\n"
	// installersConfig is what post-install.sh writes to config.yaml.
	installersConfig = "# SC-Metrics-Agent Configuration\n"
	testVMID         = "6F1C2B3A-9D4E-4F5A-8B6C-7D8E9F0A1B2C"
)

// useConfigFiles stands in for a VM's two files: agent.yaml, which the
// installer downloads with the metadata address, and the package's
// config.yaml. Empty content means the file doesn't exist. dmidecode reports
// testVMID.
func useConfigFiles(t *testing.T, agentYAML, settings string) {
	t.Helper()
	directory := t.TempDir()
	pointAt(t, &agentConfigPath, writeUnlessEmpty(t, filepath.Join(directory, "agent.yaml"), agentYAML))
	pointAt(t, &settingsPath, writeUnlessEmpty(t, filepath.Join(directory, "config.yaml"), settings))
	useFakeDMIDecode(t, testVMID)
	t.Setenv("SC_LOG_LEVEL", "")
	t.Setenv("SC_VM_ID", "")
}

// useFakeDMIDecode makes dmidecode print output, as a VM's firmware would.
func useFakeDMIDecode(t *testing.T, output string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "dmidecode")
	writeUnlessEmpty(t, script, "#!/bin/sh\nprintf '%s\\n' '"+output+"'\n")
	require.NoError(t, os.Chmod(script, 0o755))
	pointAtList(t, &dmidecodePaths, []string{script})
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

func pointAtList(t *testing.T, variable *[]string, value []string) {
	t.Helper()
	original := *variable
	*variable = value
	t.Cleanup(func() { *variable = original })
}

// Every setting but the log level is fixed: on many VMs at once, a mistaken
// interval or retry count could overload the platform.
func TestLoadIgnoresTheVariablesThatUsedToChangeSettings(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")
	for name, value := range map[string]string{
		"SC_COLLECTION_INTERVAL":       "1ms",
		"SC_HTTP_TIMEOUT":              "1ms",
		"SC_MAX_RETRIES":               "1000",
		"SC_RETRY_INTERVAL":            "1ms",
		"SC_LABELS":                    "team=web",
		"SC_COLLECTOR_CPU":             "false",
		"SC_METADATA_SERVICE_ENDPOINT": "http://elsewhere.test.invalid",
		"SC_AGENT_CONFIG":              filepath.Join(t.TempDir(), "settings.yaml"),
	} {
		t.Setenv(name, value)
	}

	cfg, err := Load()

	require.NoError(t, err)
	defaults := DefaultConfig()
	assert.Equal(t, defaults.CollectionInterval, cfg.CollectionInterval)
	assert.Equal(t, defaults.HTTPTimeout, cfg.HTTPTimeout)
	assert.Equal(t, defaults.MaxRetries, cfg.MaxRetries)
	assert.Equal(t, defaults.RetryInterval, cfg.RetryInterval)
	assert.Empty(t, cfg.Labels)
	assert.True(t, cfg.Collectors.CPU)
	assert.Equal(t, "http://metadata.test.invalid/metadata/v1/auth-token", cfg.MetadataServiceEndpoint)
}

func TestLoadTakesTheLogLevelFromConfigYAMLInAnyCase(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "log_level: DEBUG\n")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestLoadTakesTheLogLevelFromSCLogLevelOverConfigYAML(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "log_level: warn\n")
	t.Setenv("SC_LOG_LEVEL", "Error")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, "error", cfg.LogLevel)
}

// config.yaml is all or nothing. A file that sets anything besides a known log
// level is ignored, so the agent keeps running on its defaults, but
// --validate-config reports it.
func TestAConfigYAMLThatSetsMoreThanTheLogLevelIsIgnored(t *testing.T) {
	for name, settings := range map[string]string{
		"another setting":                    "collection_interval: 1ms\n",
		"another setting with the log level": "log_level: debug\nlabels:\n  team: web\n",
		"an unknown log level":               "log_level: verbose\n",
		"not YAML":                           "log_level: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			useConfigFiles(t, agentYAMLWithAddress, settings)

			cfg, err := Load()

			require.NoError(t, err, "the agent must still start")
			assert.Equal(t, "info", cfg.LogLevel)
			assert.Equal(t, 30*time.Second, cfg.CollectionInterval)
			assert.Empty(t, cfg.Labels)
			assert.ErrorContains(t, Validate(settingsPath), settingsPath)
		})
	}
}

func TestAnUnknownSCLogLevelIsIgnored(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")
	t.Setenv("SC_LOG_LEVEL", "verbose")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.ErrorContains(t, Validate(settingsPath), "SC_LOG_LEVEL")
}

// The updater runs --validate-config on config.yaml after every update.
func TestValidateAcceptsTheConfigTheInstallerWrites(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, installersConfig)

	assert.NoError(t, Validate(settingsPath))
}

func TestValidateAcceptsAMissingConfigYAML(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")

	assert.NoError(t, Validate(settingsPath))
}

// config.example.yaml is what people copy into config.yaml.
func TestValidateAcceptsTheExampleConfig(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")

	assert.NoError(t, Validate(filepath.Join("..", "..", "config.example.yaml")))
}

func TestLoadFailsWithoutAMetadataAddress(t *testing.T) {
	useConfigFiles(t, "", installersConfig)

	_, err := Load()

	assert.ErrorContains(t, err, agentConfigPath)
}

// SC_VM_ID is only a fallback: where dmidecode reads an ID, nothing can make
// the agent report as another VM.
func TestTheVMIDComesFromDMIDecodeEvenWhenSCVMIDIsSet(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")
	t.Setenv("SC_VM_ID", "another-vm")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, testVMID, cfg.VMID)
}

// A VM whose firmware has no ID, and which set SC_VM_ID for that reason, keeps
// working.
func TestSCVMIDIsUsedWhenDMIDecodeFindsNoID(t *testing.T) {
	for name, dmidecodeOutput := range map[string]string{
		"no output":     "",
		"not settable":  "Not Settable",
		"not specified": "Not Specified",
		"all zeros":     "00000000-0000-0000-0000-000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			useConfigFiles(t, agentYAMLWithAddress, "")
			useFakeDMIDecode(t, dmidecodeOutput)
			t.Setenv("SC_VM_ID", "vm-from-environment")

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, "vm-from-environment", cfg.VMID)
		})
	}
}

func TestLoadFailsWithoutAVMID(t *testing.T) {
	useConfigFiles(t, agentYAMLWithAddress, "")
	useFakeDMIDecode(t, "Not Settable")

	_, err := Load()

	assert.ErrorContains(t, err, "vm_id cannot be determined")
}
