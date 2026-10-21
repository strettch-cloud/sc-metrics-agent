package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useAgentConfig points SC_AGENT_CONFIG at a file that holds agent.yaml's
// metadata address followed by settings, because Load reads both from that one
// file.
func useAgentConfig(t *testing.T, settings string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	content := "metadata_service:\n  base_url: http://metadata.test.invalid\n" + settings
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	t.Setenv("SC_AGENT_CONFIG", path)
	t.Setenv("SC_VM_ID", "test-vm")
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
