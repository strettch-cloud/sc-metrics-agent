#!/usr/bin/env bash
# Prints the next release version, derived from the Conventional Commit types
# since the last stable release. Nobody types a version by hand.
#
#   next-version.sh beta              → 0.4.0-beta.1
#   next-version.sh stable [--major]  → 0.4.0
#
#   last stable  = highest X.Y.Z tag; with none the script fails (exit 1):
#                  the tags were not fetched, or the first one was never pushed
#   any feat     → minor   0.3.1 → 0.4.0
#   otherwise    → patch   0.3.1 → 0.3.2  (fix, chore, docs, non-conventional, …)
#   --major      → major   0.3.1 → 1.0.0  (stable only; a person ticks it)
#   breaking     → never a major by itself: listed on stderr and in
#                  $GITHUB_STEP_SUMMARY for a person to decide
#   beta         → <next>-beta.N, N continuing from that version's beta tags
#
# Exit 3 when there are no commits since the last stable release.
set -euo pipefail

usage() { echo "usage: $0 beta | stable [--major]" >&2; exit 2; }

KIND="${1:-}"
MAJOR_BUMP=false
case "$KIND" in
  beta) [ $# -eq 1 ] || usage ;;
  stable)
    if [ $# -eq 2 ] && [ "$2" = "--major" ]; then MAJOR_BUMP=true
    elif [ $# -ne 1 ]; then usage; fi ;;
  *) usage ;;
esac

LAST_STABLE=$(git tag -l --sort=-version:refname | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1 || true)
if [ -z "$LAST_STABLE" ]; then
  echo "no stable release tag found: expected at least one X.Y.Z tag; are the tags fetched?" >&2
  exit 1
fi
RANGE="${LAST_STABLE}..HEAD"
IFS=. read -r MAJOR MINOR PATCH <<< "$LAST_STABLE"

commits=$(git log --no-merges --format=%H "$RANGE")
if [ -z "$commits" ]; then
  echo "nothing to release: no commits since ${LAST_STABLE}" >&2
  exit 3
fi

bump="patch"
breaking=""
while IFS= read -r sha; do
  subject=$(git log -1 --format=%s "$sha")
  if printf '%s' "$subject" | grep -qE '^feat(\([^)]*\))?!?:'; then bump=minor; fi
  if printf '%s' "$subject" | grep -qE '^[a-z]+(\([^)]*\))?!:' \
    || git log -1 --format=%b "$sha" | grep -qE '^[[:space:]]*BREAKING[ -]CHANGE:'; then
    breaking+="${subject} ($(git rev-parse --short "$sha"))"$'\n'
  fi
done <<< "$commits"

if [ -n "$breaking" ]; then
  {
    echo "Breaking changes since ${LAST_STABLE} (the major number is never raised automatically):"
    printf '%s' "$breaking" | sed 's/^/  /'
  } >&2
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
      echo "### Breaking changes since ${LAST_STABLE}"
      echo
      echo "The major number is never raised automatically. To release a major version, run the stable release with \"major\" ticked."
      echo
      printf '%s' "$breaking" | sed 's/^/- /'
      echo
    } >> "$GITHUB_STEP_SUMMARY"
  fi
fi

if [ "$MAJOR_BUMP" = true ]; then
  NEXT="$((MAJOR + 1)).0.0"
elif [ "$bump" = minor ]; then
  NEXT="${MAJOR}.$((MINOR + 1)).0"
else
  NEXT="${MAJOR}.${MINOR}.$((PATCH + 1))"
fi

if [ "$KIND" = stable ]; then
  echo "$NEXT"
  exit 0
fi

last_beta=$(git tag -l "${NEXT}-beta.*" | sed -n "s/^${NEXT//./\\.}-beta\.\([0-9][0-9]*\)$/\1/p" | sort -n | tail -n 1)
echo "${NEXT}-beta.$(( ${last_beta:-0} + 1 ))"
