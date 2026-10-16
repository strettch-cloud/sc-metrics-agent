# Security Policy

SC Metrics Agent runs as root on customer VMs and reports their resource usage, and it updates itself automatically. We take reports about it seriously.

## Reporting a vulnerability

**Do not open a public issue.** Report it privately through GitHub:
[Security → Report a vulnerability](https://github.com/strettch-cloud/sc-metrics-agent/security/advisories/new).

Please include:

- the affected version (`sc-metrics-agent -v`) and Ubuntu release,
- what an attacker can do and what they need first (network position, local access, etc.),
- steps or a proof of concept to reproduce it.

We will acknowledge the report, keep you updated while we work on a fix, and credit you in the
advisory unless you prefer otherwise.

## Supported versions

Only the latest stable release receives security fixes. Installed agents update themselves, so
a fix reaches every machine on the stable channel shortly after it is released.

## Verifying what you install

- **Packages from the APT repository** are signed. The trusted signing keys are in
  [`packaging/keys/`](packaging/keys/); the package keeps the host's keyring in sync with them.
- **`.deb` files attached to GitHub releases** carry a build-provenance attestation. Verify one
  with:

  ```bash
  gh attestation verify sc-metrics-agent_<version>_amd64.deb --repo strettch-cloud/sc-metrics-agent
  ```
