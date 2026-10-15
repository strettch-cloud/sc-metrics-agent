#!/bin/bash
# check-keys-sync.sh - Fail if this repo's APT signing keys differ from the other agent's.
# Usage: check-keys-sync.sh <owner/other-agent-repo> [branch]
# Both agents install the same keyring, so packaging/keys/ and build-keyring.sh
# must stay identical. Compares against <branch> in the other repo when it exists
# there (a key change opened as same-named PRs in both repos), otherwise its main.
# Needs GH_TOKEN; both repos are public, so a read-only token is enough.
set -euo pipefail

OTHER="${1:?Other agent repository required (owner/name)}"
BRANCH="${2:-}"

ref="main"
if [ -n "$BRANCH" ] && gh api "repos/${OTHER}/branches/${BRANCH}" --silent 2>/dev/null; then
    ref="$BRANCH"
fi

# "path blob-sha" lines; GitHub's contents API reports the same git blob SHA.
local_files() {
    for f in packaging/keys/* .github/scripts/build-keyring.sh; do
        echo "$f $(git hash-object "$f")"
    done | sort
}
other_files() {
    {
        gh api -X GET "repos/${OTHER}/contents/packaging/keys" -f ref="$ref" \
            --jq '.[] | select(.type == "file") | "\(.path) \(.sha)"'
        gh api -X GET "repos/${OTHER}/contents/.github/scripts/build-keyring.sh" -f ref="$ref" \
            --jq '"\(.path) \(.sha)"'
    } | sort
}

if ! diff_out="$(diff <(local_files) <(other_files))"; then
    echo "Error: APT signing keys differ from ${OTHER}@${ref} (< here, > there):" >&2
    echo "$diff_out" >&2
    echo "Make the same change in ${OTHER}, on a branch with this branch's name." >&2
    exit 1
fi
echo "APT signing keys match ${OTHER}@${ref}"
