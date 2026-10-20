#!/bin/bash

set -e

RELEASE_TYPE="$1"

if [[ "$RELEASE_TYPE" != "beta" && "$RELEASE_TYPE" != "stable" ]]; then
    echo "Usage: $0 {beta|stable}"
    exit 1
fi

# Get all tags and sort them
git fetch --tags >/dev/null 2>&1 || true

if [[ "$RELEASE_TYPE" == "beta" ]]; then
    # For beta releases, auto-generate version
    
    # Get the last beta version for the current base version
    LAST_BETA=$(git tag -l --sort=-version:refname | grep -E '^[0-9]+\.[0-9]+\.[0-9]+-beta\.[0-9]+$' | head -1 || echo "")
    
    # Get the last stable version
    LAST_STABLE=$(git tag -l --sort=-version:refname | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || echo "0.0.0")
    
    
    if [[ -n "$LAST_BETA" ]]; then
        # Extract base version and beta number from last beta
        BETA_BASE=${LAST_BETA%-beta.*}
        BETA_NUM=${LAST_BETA##*-beta.}
        
        # Compare beta base with last stable to determine next version
        # Parse versions for comparison (no 'v' prefix to remove)
        IFS='.' read -r -a BETA_PARTS <<< "${BETA_BASE}"
        IFS='.' read -r -a STABLE_PARTS <<< "${LAST_STABLE}"
        
        # Compare versions semantically
        BETA_MAJOR=${BETA_PARTS[0]:-0}
        BETA_MINOR=${BETA_PARTS[1]:-0}
        BETA_PATCH=${BETA_PARTS[2]:-0}
        
        STABLE_MAJOR=${STABLE_PARTS[0]:-0}
        STABLE_MINOR=${STABLE_PARTS[1]:-0}
        STABLE_PATCH=${STABLE_PARTS[2]:-0}
        
        if [[ $BETA_MAJOR -gt $STABLE_MAJOR ]] || \
           [[ $BETA_MAJOR -eq $STABLE_MAJOR && $BETA_MINOR -gt $STABLE_MINOR ]] || \
           [[ $BETA_MAJOR -eq $STABLE_MAJOR && $BETA_MINOR -eq $STABLE_MINOR && $BETA_PATCH -gt $STABLE_PATCH ]]; then
            # Beta is ahead of stable, increment beta number
            NEW_BETA_NUM=$((BETA_NUM + 1))
            NEW_VERSION="${BETA_BASE}-beta.${NEW_BETA_NUM}"
        elif [[ $BETA_MAJOR -eq $STABLE_MAJOR && $BETA_MINOR -eq $STABLE_MINOR && $BETA_PATCH -eq $STABLE_PATCH ]]; then
            # Beta base equals stable, increment beta number
            NEW_BETA_NUM=$((BETA_NUM + 1))
            NEW_VERSION="${BETA_BASE}-beta.${NEW_BETA_NUM}"
        else
            # Stable is newer, start new beta series
            NEW_PATCH=$((STABLE_PATCH + 1))
            NEW_VERSION="${STABLE_MAJOR}.${STABLE_MINOR}.${NEW_PATCH}-beta.1"
        fi
    else
        # No beta versions exist, create first one based on stable
        IFS='.' read -r -a STABLE_PARTS <<< "${LAST_STABLE}"
        MAJOR=${STABLE_PARTS[0]:-0}
        MINOR=${STABLE_PARTS[1]:-0}
        PATCH=${STABLE_PARTS[2]:-0}
        NEW_PATCH=$((PATCH + 1))
        NEW_VERSION="${MAJOR}.${MINOR}.${NEW_PATCH}-beta.1"
    fi
    
elif [[ "$RELEASE_TYPE" == "stable" ]]; then
    # For stable releases, the version is provided via workflow input
    # This script is used for validation only
    echo "Stable version should be provided via workflow input"
    exit 1
fi

# Validate that the version doesn't already exist
EXISTING_TAG=$(git tag -l | grep "^${NEW_VERSION}$" || echo "")
if [[ -n "$EXISTING_TAG" ]]; then
    echo "Error: Version ${NEW_VERSION} already exists as tag: $EXISTING_TAG" >&2
    git tag -l --sort=-version:refname | head -10 >&2
    exit 1
fi

echo "${NEW_VERSION}"