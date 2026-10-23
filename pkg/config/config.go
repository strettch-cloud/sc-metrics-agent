package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig represents the agent.yaml configuration structure
type AgentConfig struct {
	MetadataService struct {
		BaseURL string `yaml:"base_url"`
	} `yaml:"metadata_service"`
}

// These are variables only so tests can use temporary files and a fake
// dmidecode.
var (
	// agentConfigPath is where the installer saves agent.yaml, which holds the
	// metadata service address.
	agentConfigPath = "/etc/strettchcloud/config/agent.yaml"
	// settingsPath is the package's config.yaml, which may set the log level
	// and nothing else.
	settingsPath = "/etc/sc-metrics-agent/config.yaml"
	// dmidecodePaths are tried in order to read the VM ID.
	dmidecodePaths = []string{"/usr/sbin/dmidecode", "/sbin/dmidecode", "dmidecode"}
)

// validLogLevels are the names the logger knows.
var validLogLevels = []string{"debug", "info", "warn", "error", "fatal", "panic"}

// Config represents the agent configuration. Only LogLevel can be changed on a
// VM; every other setting is fixed in DefaultConfig, so a mistaken edit on many
// VMs can't make their agents overload the platform.
type Config struct {
	// Collection settings
	CollectionInterval time.Duration `json:"collection_interval"`
	HTTPTimeout        time.Duration `json:"http_timeout"`

	// Metadata service settings
	MetadataServiceEndpoint string `json:"metadata_service_endpoint"`

	// Agent identification
	VMID   string            `json:"vm_id"`
	Labels map[string]string `json:"labels"`

	// Collector configuration
	Collectors CollectorConfig `json:"collectors"`

	// Logging
	LogLevel string `json:"log_level"`

	// Rate limiting
	MaxRetries    int           `json:"max_retries"`
	RetryInterval time.Duration `json:"retry_interval"`

	// Agent version
	AgentVersion string `json:"agent_version"`

	// On-disk directory for the buffered metric queue.
	BufferDir string `json:"buffer_dir"`
}

// DefaultBufferDir is the default on-disk buffer location.
const DefaultBufferDir = "/var/lib/sc-metrics-agent/buffer"

// CollectorConfig defines which collectors are enabled
type CollectorConfig struct {
	// Process metrics
	Processes bool `json:"processes"`

	// CPU metrics
	CPU     bool `json:"cpu"`
	CPUFreq bool `json:"cpu_freq"`
	LoadAvg bool `json:"loadavg"`

	// Memory metrics
	Memory bool `json:"memory"`
	VMStat bool `json:"vmstat"`

	// Storage metrics
	Disk       bool `json:"disk"`
	DiskStats  bool `json:"diskstats"`
	Filesystem bool `json:"filesystem"`

	// Network metrics
	Network  bool `json:"network"`
	NetDev   bool `json:"netdev"`
	NetStat  bool `json:"netstat"`
	Sockstat bool `json:"sockstat"`

	// System metrics
	Uname      bool `json:"uname"`
	Time       bool `json:"time"`
	Uptime     bool `json:"uptime"`
	Entropy    bool `json:"entropy"`
	Interrupts bool `json:"interrupts"`

	// Additional metrics
	Thermal   bool `json:"thermal"`
	Pressure  bool `json:"pressure"`
	Schedstat bool `json:"schedstat"`
}

// readMetadataBaseURL reads the metadata service base URL from agent.yaml
func readMetadataBaseURL() (string, error) {
	data, err := os.ReadFile(agentConfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to read agent config file %s: %w", agentConfigPath, err)
	}

	var config AgentConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("failed to parse agent config %s: %w", agentConfigPath, err)
	}

	if config.MetadataService.BaseURL == "" {
		return "", fmt.Errorf("metadata_service.base_url not found in %s", agentConfigPath)
	}

	return config.MetadataService.BaseURL, nil
}

// DefaultConfig returns the agent's fixed settings.
func DefaultConfig() *Config {
	return &Config{
		CollectionInterval: 30 * time.Second,
		HTTPTimeout:        30 * time.Second,
		VMID:               detectVMID(),
		Labels:             make(map[string]string),
		Collectors: CollectorConfig{
			Processes:  true,
			CPU:        true,
			CPUFreq:    true,
			LoadAvg:    true,
			Memory:     true,
			VMStat:     true,
			Disk:       true,
			DiskStats:  true,
			Filesystem: true,
			Network:    true,
			NetDev:     true,
			NetStat:    true,
			Sockstat:   true,
			Uname:      true,
			Time:       true,
			Uptime:     true,
			Entropy:    true,
			Interrupts: true,
			Thermal:    true,
			Pressure:   true,
			Schedstat:  true,
		},
		LogLevel:      "info",
		MaxRetries:    3,
		RetryInterval: 5 * time.Second,
		AgentVersion:  detectAgentVersion(),
		BufferDir:     DefaultBufferDir,
	}
}

// detectVMID reads the VM ID with dmidecode. SC_VM_ID is used only when
// dmidecode finds none, so it can't make a VM report as another one, and a VM
// that already needed it keeps working.
func detectVMID() string {
	if vmID := getVMIDFromDMIDecode(); vmID != "" {
		return vmID
	}
	return os.Getenv("SC_VM_ID")
}

// getVMIDFromDMIDecode attempts to get VM ID from dmidecode
func getVMIDFromDMIDecode() string {
	// Set a timeout for the command to prevent indefinite hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, dmidecodeCmd := range dmidecodePaths {
		cmd := exec.CommandContext(ctx, dmidecodeCmd, "-s", "system-uuid")
		output, err := cmd.Output()

		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("dmidecode command timed out")
			return ""
		}

		if err != nil {
			log.Printf("dmidecode failed with %s: %v", dmidecodeCmd, err)
			continue // Try next path
		}

		vmID := strings.TrimSpace(string(output))

		// Check for common invalid or unset dmidecode outputs
		if vmID != "" && vmID != "Not Settable" && vmID != "Not Specified" && !strings.HasPrefix(vmID, "00000000-0000-0000") {
			return vmID
		}

		log.Printf("dmidecode at %s returned invalid VM ID: '%s'", dmidecodeCmd, vmID)
	}

	log.Printf("dmidecode not found or failed at all attempted paths")
	return ""
}

// detectAgentVersion retrieves the agent version from the installed package
func detectAgentVersion() string {
	cmd := exec.Command("dpkg-query", "-W", "-f=${Version}", "sc-metrics-agent")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}

	version := strings.TrimSpace(string(output))
	if version == "" {
		return "unknown"
	}

	return version
}

// Load returns the configuration the agent runs with. A config.yaml or
// SC_LOG_LEVEL the agent can't use is ignored with a warning, so a bad edit
// can't stop the agent.
func Load() (*Config, error) {
	return load(settingsPath, false)
}

// Validate checks the configuration as Load would, but fails on a settings file
// or SC_LOG_LEVEL the agent can't use instead of ignoring it, so
// --validate-config reports them.
func Validate(path string) error {
	_, err := load(path, true)
	return err
}

func load(settingsFile string, strict bool) (*Config, error) {
	cfg := DefaultConfig()
	applyTestOverrides(cfg)

	settingsErrors := []error{cfg.applySettingsFile(settingsFile)}
	if level := os.Getenv("SC_LOG_LEVEL"); level != "" {
		settingsErrors = append(settingsErrors, cfg.setLogLevel(level, "SC_LOG_LEVEL"))
	}
	if err := errors.Join(settingsErrors...); err != nil {
		if strict {
			return nil, err
		}
		log.Printf("Ignoring %v", err)
	}

	metadataBaseURL, err := readMetadataBaseURL()
	if err != nil {
		return nil, err
	}
	cfg.MetadataServiceEndpoint = metadataBaseURL + "/metadata/v1/auth-token"

	if cfg.VMID == "" {
		return nil, fmt.Errorf("configuration validation failed: vm_id cannot be determined: dmidecode failed to return a valid UUID. Set SC_VM_ID in /etc/sc-metrics-agent/environment")
	}

	return cfg, nil
}

// applySettingsFile applies config.yaml, which may set log_level and nothing
// else. A missing file changes nothing. Any other file that breaks that rule
// is rejected whole, so an edit is never half applied.
func (c *Config) applySettingsFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	var settings struct {
		LogLevel string `yaml:"log_level"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	// A file with only comments has no YAML document, which is fine.
	if err := decoder.Decode(&settings); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s may only set log_level: %w", path, err)
	}
	if settings.LogLevel == "" {
		return nil
	}
	return c.setLogLevel(settings.LogLevel, path)
}

// setLogLevel sets a level the logger knows, in any case. source names where
// the level came from, for the error.
func (c *Config) setLogLevel(level, source string) error {
	normalized := strings.ToLower(strings.TrimSpace(level))
	if !slices.Contains(validLogLevels, normalized) {
		return fmt.Errorf("%s: unknown log level %q, expected one of %s", source, level, strings.Join(validLogLevels, ", "))
	}
	c.LogLevel = normalized
	return nil
}

// String returns a string representation of the config (excluding sensitive data)
func (c *Config) String() string {
	return fmt.Sprintf("Config{CollectionInterval:%v, VMID:%s, LogLevel:%s, Collectors:%+v}",
		c.CollectionInterval, c.VMID, c.LogLevel, c.Collectors)
}
