#!/bin/bash
# scripts/validate-version.sh - Validate version increment
set -euo pipefail

NEW_VERSION="${1:?New version required}"
RELEASE_TYPE="${2:?Release type required}"

# Get last stable version
LAST_STABLE=$(git tag -l --sort=-version:refname | \
              grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | \
              head -1 || echo "0.0.0")

# Parse versions
IFS='.' read -r last_major last_minor last_patch <<< "$LAST_STABLE"

# Calculate expected version
case "$RELEASE_TYPE" in
    major)
        expected_major=$((last_major + 1))
        expected_minor=0
        expected_patch=0
        ;;
    minor)
        expected_major=$last_major
        expected_minor=$((last_minor + 1))
        expected_patch=0
        ;;
    patch)
        expected_major=$last_major
        expected_minor=$last_minor
        expected_patch=$((last_patch + 1))
        ;;
    *)
        echo "Invalid release type: $RELEASE_TYPE" >&2
        exit 1
        ;;
esac

EXPECTED="${expected_major}.${expected_minor}.${expected_patch}"

if [ "$NEW_VERSION" != "$EXPECTED" ]; then
    echo "::error::Version mismatch for $RELEASE_TYPE release"
    echo "::error::Expected: $EXPECTED, Got: $NEW_VERSION"
    exit 1
fi

echo "✅ Version $NEW_VERSION is valid for $RELEASE_TYPE release"