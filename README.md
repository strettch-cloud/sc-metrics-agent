# SC Metrics Agent

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

The SC Metrics Agent (`sc-metrics-agent`) runs on [Strettch Cloud](https://cloud.strettch.com)
virtual machines and reports their resource usage (CPU, memory, disk, network and processes)
to the platform, where it powers the metrics shown for each VM.

## How it works

```
 ┌─────────┐    ┌──────────┐    ┌───────────┐    ┌─────────────────────────┐
 │ Collect │ -> │ Decorate │ -> │ Aggregate │ -> │ Write                    │
 └─────────┘    └──────────┘    └───────────┘    │ snappy-compressed batches │
  procfs, sysfs   VM ID +         sorted,         │ to the Strettch Cloud API │
  gopsutil        labels          batched         │ on-disk buffer if offline │
                                                  └─────────────────────────┘
```

1. **Collect** system metrics every `collection_interval` (30 s by default) with Prometheus
   collectors, procfs and gopsutil.
2. **Decorate** each metric with the VM's ID (from `dmidecode -s system-uuid`) and any custom
   labels.
3. **Aggregate** them into batches.
4. **Write** each batch, snappy-compressed, to the Strettch Cloud API. If the API cannot be
   reached, batches are kept in an on-disk buffer (`/var/lib/sc-metrics-agent/buffer`, capped at
   10 MB, roughly 16–40 hours of data) and sent when it is back. Once the buffer is full, newer
   batches are dropped, so a longer outage leaves a gap in the metrics.

The agent holds no secrets on disk: it gets a short-lived token and the API address from the
link-local metadata service.

## Metrics collected

| Group | Examples |
|---|---|
| Processes | count, states, threads |
| CPU | usage per core (user, system, idle, iowait), frequency, context switches, interrupts |
| Memory | total, available, used, buffers, cache, swap; `vmstat` paging counters |
| Storage | disk reads/writes and time spent; filesystem space and inodes |
| Network | bytes, packets, errors and drops per interface; connections (`netstat`, `sockstat`) |
| System | load averages, uptime, boot time, entropy, thermal zones, pressure stall information |

Each group can be turned off; see `collectors` in [`config.example.yaml`](config.example.yaml)
and the code under [`pkg/collector/`](pkg/collector/).

## Installation

On an Ubuntu VM, as root:

```bash
curl -sSL https://repo.cloud.strettch.com/metrics/install.sh | sudo bash
```

This adds the signed APT repository, installs the package and starts two systemd units:

| Unit | Purpose |
|---|---|
| `sc-metrics-agent.service` | The agent |
| `sc-metrics-agent-update-scheduler.timer` | Checks for a new version every hour and installs it |

To try pre-release builds, use `https://repo.cloud.strettch.com/metrics/beta/install.sh` instead.

Supported: Ubuntu 18.04, 20.04, 22.04 and 24.04 on `amd64` and `arm64`.

## Operating the agent

```bash
sudo systemctl status sc-metrics-agent                       # status
sudo journalctl -u sc-metrics-agent -f                       # logs
sudo systemctl start sc-metrics-agent-update-scheduler.service  # check for an update now
sc-metrics-agent -v                                          # installed version
```

Remove it with `sudo apt-get purge sc-metrics-agent`.

## Configuration

The agent works without configuration. The installer writes the metadata service address to
`/etc/strettchcloud/config/agent.yaml`; everything else has a default.

To override a setting, add it to `/etc/sc-metrics-agent/environment` and restart the service:

| Variable | Default | Meaning |
|---|---|---|
| `SC_COLLECTION_INTERVAL` | `30s` | How often metrics are collected |
| `SC_LABELS` | — | Extra labels for every metric, e.g. `env=prod,team=web` |
| `SC_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `SC_VM_ID` | from `dmidecode` | Set this if `dmidecode -s system-uuid` returns nothing on your VM |
| `SC_COLLECTOR_<NAME>` | `true` | Turn one collector group off, e.g. `SC_COLLECTOR_THERMAL=false` |
| `SC_AGENT_CONFIG` | — | Path to a YAML file used *instead of* `/etc/strettchcloud/config/agent.yaml`. It must contain `metadata_service.base_url` (copy it from that file) and may add any setting from [`config.example.yaml`](config.example.yaml). |

## Verifying packages

- **APT packages** are signed. The trusted signing keys and their fingerprints are in
  [`packaging/keys/`](packaging/keys/).
- **Release `.deb` files** carry a build-provenance attestation:

  ```bash
  gh attestation verify sc-metrics-agent_<version>_amd64.deb --repo strettch-cloud/sc-metrics-agent
  ```

## Development

The toolchain is pinned in `mise.toml`. Install [mise](https://mise.jdx.dev), then:

```bash
mise install           # once
mise run build         # GOOS=linux GOARCH=arm64 mise run build to cross-compile
mise run test          # tests with the race detector
mise run lint
mise run security-scan # Trivy + govulncheck
```

Run a local build with `sudo ./build/sc-metrics-agent`; `-validate-config <file>` checks a
configuration file and exits. The file must contain `metadata_service.base_url`, as described for
`SC_AGENT_CONFIG` above. How CI builds and publishes releases is described in
[.github/README.md](.github/README.md).

## Contributing and security

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). Please report
vulnerabilities privately as described in [SECURITY.md](SECURITY.md), not in a public issue.

## License

[Apache License 2.0](LICENSE).
