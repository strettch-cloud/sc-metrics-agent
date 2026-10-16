# APT signing keys

Every public key in `*.asc` here goes into the keyring the package installs for
the APT repository (`.github/scripts/build-keyring.sh`). APT accepts a repository
signed by any key in that keyring, so this directory decides which signing keys
every VM trusts. Keep it identical in sc-agent and sc-metrics-agent; `pr.yml`
checks that.

| File | Key | Notes |
|---|---|---|
| `strettch-apt-2025.asc` | `E582D5D153A4B23240EBEDD670926ED53E991488` | Strettch Engineering, created 2025-09-04 |
| `strettch-cloud-apt-2026.asc` | `0DF9DD50243C7F6D5BB59EDA050812D66FB0D006` | Strettch Cloud APT Signing, created 2026-10-15. Certify-only primary kept offline; signs through subkey `E65DB17BCFC1EC50460DEA6EF96CF7B580189DA4`. Neither the primary nor the subkey expires. |

Rotation: add the new key alongside the old one, release, wait for the fleet to
update, switch the repository's signing key, then remove the old key here.
