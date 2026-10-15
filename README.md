# SC Metrics Agent

[![Go Report Card](https://goreportcard.com/badge/github.com/strettch/sc-metrics-agent)](https://goreportcard.com/report/github.com/strettch/sc-metrics-agent)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

SC Metrics Agent is a comprehensive, production-ready agent for collecting VM-level system metrics and transmitting them to a central ingestor service. Built with Go, it's designed for reliability, performance, and complete observability.

## Overview

The SC Metrics Agent implements a **Collect → Decorate → Aggregate → Write** pipeline. It gathers a wide array of system metrics from VMs using Prometheus collectors and gopsutil, then transmits them to a configurable HTTP endpoint with Snappy compression for efficiency.

## Key Features

-   **Comprehensive Metrics Collection**: Gathers data on processes, CPU, memory, network, storage, load averages, and system information.
-   **Automatic VM Identification**: Uses `dmidecode -s system-uuid` with intelligent fallbacks to `/etc/machine-id`, `/proc/sys/kernel/random/boot_id`, and hostname.
-   **High-Performance Logging**: Structured JSON output using [Zap logger](https://github.com/uber-go/zap) for efficient and parseable logs.
-   **Advanced Compression**: Utilizes [klauspost/compress/snappy](https://github.com/klauspost/compress) for optimal data transmission efficiency.
-   **Payload Transparency**: Logs pre-compression payloads for easier debugging and monitoring.
-   **Configurable Collectors**: Granular control to enable or disable specific metric collectors.
-   **Resilient HTTP Client**: Includes retry logic and rate limiting for robust data delivery.
-   **Diagnostic Reporting**: Provides agent health monitoring capabilities.
-   **Graceful Shutdown**: Ensures final diagnostics are transmitted before exiting.
-   **Container-Ready**: Supports Docker and configuration via environment variables.

## Architecture

The agent follows a modular pipeline architecture:

```
┌─────────┐    ┌───────────┐    ┌───────────┐    ┌───────┐
│ Collect │ -> │ Decorate  │ -> │ Aggregate │ -> │ Write │
└─────────┘    └───────────┘    └───────────┘    └───────┘
```

### Components

1.  **Collector**: Gathers system metrics using gopsutil and procfs.
2.  **Decorator**: Enriches metrics with VM ID and custom labels.
3.  **Aggregator**: Converts Prometheus metrics into an internal format for transmission.
4.  **Writer**: Sends compressed metric batches via HTTP POST, complete with retry logic.

### Data Flow

1.  **Collection**: Metrics are gathered from various system sources (procfs, sysfs, gopsutil).
2.  **Decoration**: A unique VM identifier and any custom-defined labels are added to each metric.
3.  **Aggregation**: Metrics are transformed into `MetricWithValue` structs and sorted for consistency.
4.  **Transmission**: Data is serialized to JSON, compressed using Snappy, and then sent via HTTP POST to the configured ingestor.

## Installation

### Prerequisites

-   A Linux system (Ubuntu, Debian, or compatible)
-   Root privileges for installation
-   Network connectivity

### Stable Release (Recommended)

For production deployments, install the stable release:

```bash
curl -sSL https://repo.cloud.strettch.com/metrics/install.sh | sudo bash
```

### Beta Release

For testing latest features, install the beta release:

```bash
curl -sSL https://repo.cloud.strettch.com/metrics/beta/install.sh | sudo bash
```

The installation script will:
- Add the repository to your package manager
- Install the agent and configure it as a systemd service
- Automatically detect your VM ID
- Start the service immediately

### Verify Installation

Check that the agent is running:

```bash
sudo systemctl status sc-metrics-agent
```

### Build from Source

For developers who want to build from source:

```bash
git clone https://github.com/strettch-cloud/sc-metrics-agent.git
cd sc-metrics-agent
mise install   # toolchain pinned in mise.toml
mise run build
```

## Configuration

The agent works out-of-the-box with sensible defaults. Advanced users can customize behavior through the configuration file at `/etc/sc-metrics-agent/config.yaml`.

### Optional Configuration

Key options you can customize:

-   `collection_interval`: How often to collect metrics (default: `30s`)
-   `vm_id`: Manually set VM ID (auto-detected by default)
-   `labels`: Custom key-value pairs to add to all metrics
-   `collectors`: Enable/disable specific metric groups
-   `log_level`: Logging verbosity (`info`, `debug`, etc.)

### VM ID Detection

The agent automatically detects your VM's unique identifier using `dmidecode`. This requires no configuration in most standard environments.

## Usage

### Running Directly

```bash
# Build the agent
mise run build

# Run with default configuration (looks for config.yaml in current dir)
sudo ./build/sc-metrics-agent

# Specify a config file
sudo SC_AGENT_CONFIG=/path/to/your/config.yaml ./build/sc-metrics-agent
```

### Checking Version

```bash
./build/sc-metrics-agent -v
# or if installed via package manager
sc-metrics-agent -v
```

## Metrics Collected

The agent collects a wide range of metrics. Below is a summary. For a detailed list, please refer to the `collectors` section in `config.example.yaml` and the source code under `pkg/collector/`.

-   **Process Metrics**: PID count, process states, thread counts.
-   **CPU Metrics**: Usage per core (user, system, idle, iowait), CPU frequency, context switches, interrupts.
-   **Memory Metrics**: Total, free, available, used memory; buffer and cache sizes; swap space details.
-   **Virtual Memory Stats**: Page-ins/outs, swap-ins/outs (`vmstat`).
-   **Storage Metrics**: Disk reads/writes (completed operations, bytes, time spent).
-   **Filesystem Metrics**: Total, free, used space per filesystem; inode counts.
-   **Network Metrics**: Bytes/packets received/transmitted per interface, errors, drops.
-   **Network Connection Metrics**: Active connections by protocol/state (`netstat`), socket usage (`sockstat`).
-   **System Information**: Load averages (1, 5, 15 min), boot time, system time, uptime, entropy.
-   **Advanced Metrics**: Thermal zone temperatures, CPU/memory/IO pressure stall information.

## Development

Tools and tasks are defined in `mise.toml`. Install [mise](https://mise.jdx.dev), then run
`mise install` once; `mise tasks` lists everything available.

```bash
mise run build          # Build for this machine (GOOS/GOARCH to cross-compile)
mise run test           # Tests with the race detector
mise run lint           # golangci-lint
mise run security-scan  # Trivy + govulncheck
```

### Releasing

Every merge to `main` produces a beta release and stable releases are dispatched manually;
see [.github/README.md](.github/README.md).

## Contributing

Contributions are welcome! Please follow these steps:

1.  Fork the repository.
2.  Create a new branch (`git checkout -b feature/your-feature-name`).
3.  Make your changes.
4.  Ensure tests pass (`mise run test`).
5.  Ensure code is linted (`mise run lint`).
6.  Commit your changes (`git commit -am 'Add some feature'`).
7.  Push to the branch (`git push origin feature/your-feature-name`).
8.  Create a new Pull Request.

Please ensure your code follows Go best practices and includes tests for new functionality.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details (assuming you will add one, if not, state the license directly).

---

*This README was generated with assistance from an AI coding partner.*
