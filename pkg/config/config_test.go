package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDefaultConfig(t *testing.T) {
	useFakeDMIDecode(t, "")

	cfg := DefaultConfig()

	assert.Equal(t, 30*time.Second, cfg.CollectionInterval)
	assert.Equal(t, 30*time.Second, cfg.HTTPTimeout)
	assert.Empty(t, cfg.MetadataServiceEndpoint, "Load fills it in from agent.yaml")
	assert.Empty(t, cfg.Labels)
	assert.True(t, cfg.Collectors.Processes)
	assert.True(t, cfg.Collectors.Schedstat)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, 5*time.Second, cfg.RetryInterval)
	assert.Equal(t, DefaultBufferDir, cfg.BufferDir)
}

