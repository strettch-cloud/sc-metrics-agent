#!/bin/bash
# build-keyring.sh - Build the binary APT keyring shipped in the package.
# Usage: build-keyring.sh <output-file>
# Contains every public key in packaging/keys/*.asc; see packaging/keys/README.md.
set -euo pipefail

OUTPUT="${1:?Output keyring path required}"
KEYS_DIR="packaging/keys"

GNUPGHOME="$(mktemp -d)"
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT

shopt -s nullglob
key_files=("${KEYS_DIR}"/*.asc)
if [ ${#key_files[@]} -eq 0 ]; then
    echo "Error: no keys found in ${KEYS_DIR}" >&2
    exit 1
fi

gpg --batch --quiet --import "${key_files[@]}"

mkdir -p "$(dirname "$OUTPUT")"
# Binary (not armored) export: the keyring path ends in .gpg, which APT reads as binary.
gpg --batch --export --export-options export-minimal > "$OUTPUT"
chmod 644 "$OUTPUT"

echo "Keyring built: ${OUTPUT}"
gpg --batch --with-colons --list-keys | awk -F: '/^pub/{getline; print "  " $10}'
