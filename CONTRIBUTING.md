# Contributing to SC Metrics Agent

Thanks for helping. Bug reports, fixes and improvements are all welcome.

## Before you start

- **Security issues** go through [private reporting](SECURITY.md), never a public issue.
- For anything larger than a small fix, open an issue first so we can agree on the approach.
- By contributing you agree that your work is licensed under the [Apache License 2.0](LICENSE),
  and you follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Development setup

The toolchain (Go, golangci-lint, Trivy, govulncheck) is pinned in `mise.toml`.

```bash
# Install mise: https://mise.jdx.dev
mise install           # once
mise tasks             # list every task
mise run build         # build for this machine; GOOS/GOARCH to cross-compile
mise run test          # tests with the race detector
mise run lint          # golangci-lint
mise run security-scan # Trivy + govulncheck
```

The agent only runs on Linux; on macOS use a Linux VM or container to run it.

## Pull requests

1. Fork the repository and create a branch from `main`.
2. Keep the change focused, add or update tests, and make sure `mise run test` and
   `mise run lint` pass.
3. Use a [Conventional Commits](https://www.conventionalcommits.org) title, e.g.
   `fix: retry metadata requests on timeout`. Pull requests are squash-merged, so the title
   becomes the commit message.
4. Open the pull request against `main`. CI runs the same checks; a maintainer will review it.

Changes to `.github/`, `packaging/`, `go.mod`/`go.sum` and `mise.toml` affect how releases are
built and what runs on customer machines, so they always need a maintainer's review.

## Releases

Every merge to `main` produces a beta release; maintainers publish stable releases. See
[.github/README.md](.github/README.md).
