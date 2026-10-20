#!/bin/bash
# coverage-report.sh - Print the coverage total and a per-package table as
# Markdown, for the pull request workflow's run summary.
# Usage: coverage-report.sh [profile]    (default: coverage.out)
#
# Run it from the module root, on the profile `mise run test` writes.
# shellcheck disable=SC2016 # the backticks are Markdown code spans, not commands
set -euo pipefail

PROFILE="${1:-coverage.out}"
MODULE=$(awk '$1 == "module" { print $2 }' go.mod)

# -coverpkg=./... repeats a block once per test binary, so each block is
# counted once, as covered if any binary ran it. This matches go tool cover.
rows=$(awk -v module="$MODULE" '
    NR > 1 {
        block = $1
        statements[block] = $2
        if ($3 > 0) covered[block] = 1
    }
    END {
        for (block in statements) {
            split(block, location, ":")
            package = location[1]
            sub(/\/[^\/]*$/, "", package)
            if (package == module) package = "."
            else sub("^" module "/", "", package)
            package_statements[package] += statements[block]
            if (block in covered) package_covered[package] += statements[block]
        }
        for (package in package_statements)
            printf "%s %d %d\n", package, package_covered[package], package_statements[package]
    }
' "$PROFILE" | sort)

percent() { # percent <covered> <statements>
    awk -v covered="$1" -v statements="$2" 'BEGIN { printf "%.1f%%", statements ? covered * 100 / statements : 0 }'
}

total_covered=0
total_statements=0
table=""
while read -r package covered statements; do
    total_covered=$((total_covered + covered))
    total_statements=$((total_statements + statements))
    table+=$(printf '| `%s` | %d | %d | %s |' "$package" "$covered" "$statements" "$(percent "$covered" "$statements")")
    table+=$'\n'
done <<< "$rows"

total_percent=$(percent "$total_covered" "$total_statements")
printf '### Test coverage: %s\n\n' "$total_percent"
printf 'Unit and integration tests, measured by `mise run test` with `.coverageignore` applied. The target is 98%%.\n\n'
printf '| Package | Covered | Statements | Coverage |\n| :-- | --: | --: | --: |\n'
printf '%s' "$table"
printf '| **Total** | **%d** | **%d** | **%s** |\n' "$total_covered" "$total_statements" "$total_percent"
