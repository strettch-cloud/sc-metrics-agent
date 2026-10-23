package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"strconv"
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

// agentConfigPath is where the installer saves agent.yaml, which holds the
// metadata service address. A variable so tests can use a temporary file.
var agentConfigPath = "/etc/strettchcloud/config/agent.yaml"

// settingsPath is the settings file the package installs; SC_AGENT_CONFIG
// replaces it. A variable so tests can use a temporary file.
var settingsPath = "/etc/sc-metrics-agent/config.yaml"

// Config represents the agent configuration
type Config struct {
	// Collection settings
	CollectionInterval time.Duration `yaml:"collection_interval" json:"collection_interval"`
	HTTPTimeout        time.Duration `yaml:"http_timeout" json:"http_timeout"`

	// Metadata service settings
	MetadataServiceEndpoint string `yaml:"metadata_service_endpoint" json:"metadata_service_endpoint"`

	// Agent identification
	VMID   string            `yaml:"vm_id" json:"vm_id"`
	Labels map[string]string `yaml:"labels" json:"labels"`

	// Collector configuration
	Collectors CollectorConfig `yaml:"collectors" json:"collectors"`

	// Logging
	LogLevel string `yaml:"log_level" json:"log_level"`

	// Rate limiting
	MaxRetries    int           `yaml:"max_retries" json:"max_retries"`
	RetryInterval time.Duration `yaml:"retry_interval" json:"retry_interval"`

	// Agent version
	AgentVersion string `yaml:"agent_version" json:"agent_version"`

	// On-disk directory for the buffered metric queue.
	BufferDir string `yaml:"buffer_dir" json:"buffer_dir"`
}

// DefaultBufferDir is the default on-disk buffer location.
const DefaultBufferDir = "/var/lib/sc-metrics-agent/buffer"

// CollectorConfig defines which collectors are enabled
type CollectorConfig struct {
	// Process metrics
	Processes bool `yaml:"processes" json:"processes"`

	// CPU metrics
	CPU     bool `yaml:"cpu" json:"cpu"`
	CPUFreq bool `yaml:"cpu_freq" json:"cpu_freq"`
	LoadAvg bool `yaml:"loadavg" json:"loadavg"`

	// Memory metrics
	Memory bool `yaml:"memory" json:"memory"`
	VMStat bool `yaml:"vmstat" json:"vmstat"`

	// Storage metrics
	Disk       bool `yaml:"disk" json:"disk"`
	DiskStats  bool `yaml:"diskstats" json:"diskstats"`
	Filesystem bool `yaml:"filesystem" json:"filesystem"`

	// Network metrics
	Network  bool `yaml:"network" json:"network"`
	NetDev   bool `yaml:"netdev" json:"netdev"`
	NetStat  bool `yaml:"netstat" json:"netstat"`
	Sockstat bool `yaml:"sockstat" json:"sockstat"`

	// System metrics
	Uname      bool `yaml:"uname" json:"uname"`
	Time       bool `yaml:"time" json:"time"`
	Uptime     bool `yaml:"uptime" json:"uptime"`
	Entropy    bool `yaml:"entropy" json:"entropy"`
	Interrupts bool `yaml:"interrupts" json:"interrupts"`

	// Additional metrics
	Thermal   bool `yaml:"thermal" json:"thermal"`
	Pressure  bool `yaml:"pressure" json:"pressure"`
	Schedstat bool `yaml:"schedstat" json:"schedstat"`
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

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	vmID := getVMIDFromDMIDecode()
	// If empty, it will be caught in validation

	return &Config{
		CollectionInterval: 30 * time.Second,
		HTTPTimeout:        30 * time.Second,
		VMID:               vmID,
		Labels:             make(map[string]string),
		Collectors: CollectorConfig{
			// Process metrics (required for this story)
			Processes: true,

			// CPU metrics
			CPU:     true,
			CPUFreq: true,
			LoadAvg: true,

			// Memory metrics
			Memory: true,
			VMStat: true,

			// Storage metrics
			Disk:       true,
			DiskStats:  true,
			Filesystem: true,

			// Network metrics
			Network:  true,
			NetDev:   true,
			NetStat:  true,
			Sockstat: true,

			// System metrics
			Uname:      true,
			Time:       true,
			Uptime:     true,
			Entropy:    true,
			Interrupts: true,

			// Additional metrics
			Thermal:   true,
			Pressure:  true,
			Schedstat: true,
		},
		LogLevel:      "info",
		MaxRetries:    3,
		RetryInterval: 5 * time.Second,
		AgentVersion:  detectAgentVersion(),
		BufferDir:     DefaultBufferDir,
	}
}

// getVMIDFromDMIDecode attempts to get VM ID from dmidecode
func getVMIDFromDMIDecode() string {
	// Only use dmidecode - no other VM ID sources
	// Set a timeout for the command to prevent indefinite hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try common dmidecode locations in order of preference
	dmidecodePaths := []string{
		"/usr/sbin/dmidecode", // Most common location
		"/sbin/dmidecode",     // Alternative location
		"dmidecode",           // Fallback to PATH
	}

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

// Load reads configuration from environment variables and config file
func Load() (*Config, error) {
	cfg := DefaultConfig()

	configPath := os.Getenv("SC_AGENT_CONFIG")
	configPathRequired := configPath != ""
	if !configPathRequired {
		configPath = settingsPath
	}
	metadataBaseURL, err := cfg.loadFromFile(configPath)
	// The package installs the settings file, but the agent also runs without it.
	if err != nil && (configPathRequired || !errors.Is(err, fs.ErrNotExist)) {
		return nil, fmt.Errorf("failed to load config file %s: %w", configPath, err)
	}

	// Override with environment variables
	cfg.loadFromEnv()

	if cfg.MetadataServiceEndpoint == "" {
		if metadataBaseURL == "" {
			if metadataBaseURL, err = readMetadataBaseURL(); err != nil {
				return nil, err
			}
		}
		cfg.MetadataServiceEndpoint = metadataBaseURL + "/metadata/v1/auth-token"
	}

	// The logger only knows lowercase level names.
	cfg.LogLevel = strings.ToLower(cfg.LogLevel)

	// Validate configuration
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

// loadFromFile loads configuration from a YAML file, and returns the metadata
// service base URL when the file sets one.
func (c *Config) loadFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Preserve the detected VM ID before unmarshaling
	detectedVMID := c.VMID

	if err := yaml.Unmarshal(data, c); err != nil {
		return "", err
	}

	// If config file has empty vm_id, restore the detected one
	if c.VMID == "" && detectedVMID != "" {
		c.VMID = detectedVMID
	}

	var agentConfig AgentConfig
	if err := yaml.Unmarshal(data, &agentConfig); err != nil {
		return "", err
	}
	return agentConfig.MetadataService.BaseURL, nil
}

// loadFromEnv loads configuration from environment variables
func (c *Config) loadFromEnv() {
	if val := os.Getenv("SC_COLLECTION_INTERVAL"); val != "" {
		if duration, err := time.ParseDuration(val); err == nil {
			c.CollectionInterval = duration
		}
	}

	if val := os.Getenv("SC_METADATA_SERVICE_ENDPOINT"); val != "" {
		c.MetadataServiceEndpoint = val
	}

	if val := os.Getenv("SC_HTTP_TIMEOUT"); val != "" {
		if duration, err := time.ParseDuration(val); err == nil {
			c.HTTPTimeout = duration
		}
	}

	if val := os.Getenv("SC_VM_ID"); val != "" {
		c.VMID = val
	}

	if val := os.Getenv("SC_LOG_LEVEL"); val != "" {
		c.LogLevel = val
	}

	if val := os.Getenv("SC_MAX_RETRIES"); val != "" {
		if retries, err := strconv.Atoi(val); err == nil {
			c.MaxRetries = retries
		}
	}

	if val := os.Getenv("SC_RETRY_INTERVAL"); val != "" {
		if duration, err := time.ParseDuration(val); err == nil {
			c.RetryInterval = duration
		}
	}

	// Load labels from SC_LABELS (format: key1=value1,key2=value2)
	if val := os.Getenv("SC_LABELS"); val != "" {
		// An empty `labels:` in the file leaves the map nil.
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		labels := parseLabels(val)
		for k, v := range labels {
			c.Labels[k] = v
		}
	}

	// Load collector settings
	loadCollectorEnvVars(&c.Collectors)
}

// loadCollectorEnvVars loads collector configuration from environment variables
func loadCollectorEnvVars(collectors *CollectorConfig) {
	if val := os.Getenv("SC_COLLECTOR_PROCESSES"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Processes = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_CPU"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.CPU = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_CPU_FREQ"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.CPUFreq = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_LOADAVG"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.LoadAvg = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_MEMORY"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Memory = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_VMSTAT"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.VMStat = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_DISK"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Disk = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_DISKSTATS"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.DiskStats = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_FILESYSTEM"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Filesystem = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_NETWORK"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Network = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_NETDEV"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.NetDev = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_NETSTAT"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.NetStat = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_SOCKSTAT"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Sockstat = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_UNAME"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Uname = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_TIME"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Time = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_UPTIME"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Uptime = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_ENTROPY"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Entropy = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_INTERRUPTS"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Interrupts = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_THERMAL"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Thermal = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_PRESSURE"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Pressure = enabled
		}
	}
	if val := os.Getenv("SC_COLLECTOR_SCHEDSTAT"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			collectors.Schedstat = enabled
		}
	}
}

// parseLabels parses label string in format "key1=value1,key2=value2"
func parseLabels(labelStr string) map[string]string {
	labels := make(map[string]string)
	pairs := strings.Split(labelStr, ",")

	for _, pair := range pairs {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key != "" {
				labels[key] = value
			}
		}
	}

	return labels
}

// validate checks if the configuration is valid
func (c *Config) validate() error {
	if c.CollectionInterval <= 0 {
		return fmt.Errorf("collection_interval must be positive")
	}

	if c.HTTPTimeout <= 0 {
		return fmt.Errorf("http_timeout must be positive")
	}

	if c.VMID == "" {
		return fmt.Errorf("vm_id cannot be determined: dmidecode failed to return a valid UUID. Please set vm_id manually in config.yaml or use SC_VM_ID environment variable")
	}

	if c.MaxRetries < 0 {
		return fmt.Errorf("max_retries cannot be negative")
	}

	if c.RetryInterval <= 0 {
		return fmt.Errorf("retry_interval must be positive")
	}

	if c.BufferDir == "" {
		return fmt.Errorf("buffer_dir must be set")
	}

	// Validate log level
	validLogLevels := []string{"debug", "info", "warn", "error", "fatal", "panic"}
	validLevel := false
	for _, level := range validLogLevels {
		if strings.ToLower(c.LogLevel) == level {
			validLevel = true
			break
		}
	}
	if !validLevel {
		return fmt.Errorf("invalid log_level: %s", c.LogLevel)
	}

	// Validate at least one collector is enabled
	if !c.hasEnabledCollectors() {
		return fmt.Errorf("at least one collector must be enabled")
	}

	return nil
}

// hasEnabledCollectors checks if at least one collector is enabled
func (c *Config) hasEnabledCollectors() bool {
	collectors := c.Collectors
	return collectors.Processes || collectors.CPU || collectors.CPUFreq || collectors.LoadAvg ||
		collectors.Memory || collectors.VMStat || collectors.Disk || collectors.DiskStats ||
		collectors.Filesystem || collectors.Network || collectors.NetDev || collectors.NetStat ||
		collectors.Sockstat || collectors.Uname || collectors.Time || collectors.Uptime ||
		collectors.Entropy || collectors.Interrupts || collectors.Thermal || collectors.Pressure ||
		collectors.Schedstat
}

// String returns a string representation of the config (excluding sensitive data)
func (c *Config) String() string {
	return fmt.Sprintf("Config{CollectionInterval:%v, VMID:%s, LogLevel:%s, Collectors:%+v}",
		c.CollectionInterval, c.VMID, c.LogLevel, c.Collectors)
}
