//go:build e2e

package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// The end-to-end tests build the agent with the e2e tag, which compiles this
// file in. It lets them run the agent against fake servers and a fake
// dmidecode, with a temporary buffer and short intervals. Release builds don't
// compile it, so none of this can be changed on a VM.

func init() {
	if path := os.Getenv("SC_TEST_AGENT_YAML"); path != "" {
		agentConfigPath = path
	}
	if path := os.Getenv("SC_TEST_CONFIG_YAML"); path != "" {
		settingsPath = path
	}
	if path := os.Getenv("SC_TEST_DMIDECODE"); path != "" {
		dmidecodePaths = []string{path}
	}
}

func applyTestOverrides(cfg *Config) {
	for name, setting := range map[string]*time.Duration{
		"SC_TEST_COLLECTION_INTERVAL": &cfg.CollectionInterval,
		"SC_TEST_HTTP_TIMEOUT":        &cfg.HTTPTimeout,
		"SC_TEST_RETRY_INTERVAL":      &cfg.RetryInterval,
	} {
		if value := os.Getenv(name); value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil {
				panic(fmt.Sprintf("%s=%q: %v", name, value, err))
			}
			*setting = duration
		}
	}
	if value := os.Getenv("SC_TEST_MAX_RETRIES"); value != "" {
		retries, err := strconv.Atoi(value)
		if err != nil {
			panic(fmt.Sprintf("SC_TEST_MAX_RETRIES=%q: %v", value, err))
		}
		cfg.MaxRetries = retries
	}
	if path := os.Getenv("SC_TEST_BUFFER_DIR"); path != "" {
		cfg.BufferDir = path
	}
}
