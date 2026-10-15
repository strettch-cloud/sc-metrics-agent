# CI and Releases

This repository builds, tests and packages the agent, and publishes the `.deb` packages
as GitHub releases.

## Workflows

| Workflow | Trigger | What it does |
|---|---|---|
| `pr.yml` | Pull request to `main` | polin-guard scan, Trivy, govulncheck, tests, golangci-lint, build |
| `beta.yml` | Merge to `main` | Builds `X.Y.Z-beta.N`, creates a GitHub pre-release with the `.deb` packages |
| `release.yml` | Manual dispatch from `main` | Validates `X.Y.Z`, creates a GitHub release with the `.deb` packages |
| `_build.yml` | Called by the two above | Scans, builds and packages `amd64` and `arm64` |

Every `.deb` attached to a release has a signed build-provenance attestation. Verify one with:

```bash
gh attestation verify sc-metrics-agent_<version>_amd64.deb --repo strettch-cloud/sc-metrics-agent
```

## Security model

- Workflows default to `permissions: {}`; each job requests only what it needs.
- Third-party code (npm, gems, Go modules) runs only in jobs with a read-only token and no
  OIDC token. Only the `publish` job can create tags, releases and attestations.
- `release.yml` refuses to run from any branch except `main`.
- Actions are pinned to commit SHAs.
- Changes to `.github/`, `packaging/` and the build require review from `CODEOWNERS`.

## Versioning

- **Beta:** `X.Y.Z-beta.N`, generated from the last stable and beta tags on every merge to `main`.
- **Stable:** `X.Y.Z`, entered manually; `validate-version.sh` checks it is the next patch, minor
  or major after the last stable tag.

## Scripts

| Script | Purpose |
|---|---|
| `generate-version.sh` | Computes the next beta version |
| `validate-version.sh` | Checks a stable version is a valid increment |
| `build-all-packages.sh` | Builds binaries and `.deb` packages for `amd64` and `arm64`, writes `packages.sha256` |
| `build-package.sh` | Builds one `.deb` with fpm |
| `generate-changelog.sh` | Builds release notes from commits since the last tag |
