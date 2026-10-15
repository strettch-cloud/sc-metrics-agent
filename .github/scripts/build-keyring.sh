#!/bin/bash
# build-keyring.sh - Build the binary APT keyring shipped in the package.
# Usage: build-keyring.sh <output-file>
# Imports packaging/keys/*.asc and fails unless the resulting keys match
# packaging/keys/fingerprints.txt exactly, so a key cannot be added or dropped
# by changing only one of the two.
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

actual=$(gpg --batch --with-colons --list-keys | awk -F: '/^pub/{getline; print $10}' | sort)
expected=$(grep -vE '^\s*(#|$)' "${KEYS_DIR}/fingerprints.txt" | tr -d ' ' | sort)

if [ "$actual" != "$expected" ]; then
    echo "Error: keys in ${KEYS_DIR}/*.asc do not match ${KEYS_DIR}/fingerprints.txt" >&2
    echo "In .asc files:" >&2
    echo "$actual" >&2
    echo "In fingerprints.txt:" >&2
    echo "$expected" >&2
    exit 1
fi

mkdir -p "$(dirname "$OUTPUT")"
# Binary (not armored) export: the keyring path ends in .gpg, which APT reads as binary.
# shellcheck disable=SC2086
gpg --batch --export --export-options export-minimal $expected > "$OUTPUT"
chmod 644 "$OUTPUT"

echo "Keyring built with $(echo "$expected" | wc -l | tr -d ' ') key(s): ${OUTPUT}"
