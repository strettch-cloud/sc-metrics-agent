# CI and Releases

This repository builds, tests and packages the agent, and publishes the `.deb` packages
as GitHub releases.

## Workflows

| Workflow | Trigger | What it does |
|---|---|---|
| `pr.yml` | Any pull request, stacked ones included | polin-guard scan, release version script tests, Trivy, govulncheck, tests (coverage report in the run summary), golangci-lint, shellcheck and systemd unit checks, build; then the `.deb` installed, upgraded and removed under systemd on Ubuntu 22.04 and 24.04 and Debian 12 and 13 |
| `beta.yml` | Merge to `main` | Builds `X.Y.Z-beta.N`, creates a GitHub pre-release with the `.deb` packages |
| `release.yml` | Manual dispatch from `main` | Derives `X.Y.Z`, creates a GitHub release with the `.deb` packages |
| `pr-title.yml` | Any pull request, and title edits | Checks the title follows Conventional Commits |
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

Nobody types a version. `next-version.sh` derives it from the commit types since the last
stable tag: any `feat` raises the minor number, anything else the patch number. A breaking change
(`!` or a `BREAKING CHANGE` footer) never raises the major number by itself; it is listed in the
run summary, and a maintainer releases a major version by ticking "major" on the stable release.
The script fails when the repository has no `X.Y.Z` tag at all: a new repository needs its
first stable tag pushed by a maintainer.

- **Beta:** `X.Y.Z-beta.N` on every merge to `main`, where `X.Y.Z` is the next stable version and
  `N` continues from that version's betas. A merge whose commit already has the stable tag makes
  no beta.
- **Stable:** `X.Y.Z`, from the manually started `release.yml`.

Commit types come from pull request titles (pull requests are squash-merged), so `pr-title.yml`
checks every title with commitlint (`commitlint.config.mjs`).

## Scripts

| Script | Purpose |
|---|---|
| `next-version.sh` | Computes the next beta or stable version from the commits since the last stable tag |
| `next-version.test.sh` | Tests `next-version.sh` against throwaway repositories |
| `build-all-packages.sh` | Builds binaries and `.deb` packages for `amd64` and `arm64`, writes `packages.sha256` |
| `build-package.sh` | Builds one `.deb` with fpm |
| `generate-changelog.sh` | Builds release notes from commits since the last tag |
