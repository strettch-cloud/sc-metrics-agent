#!/bin/bash
# verify-units.sh - Fail if systemd-analyze reports anything about our units.
# Usage: verify-units.sh [unit-dir]    (default: packaging/systemd)
#
# `systemd-analyze verify` exits 0 on problems such as an unknown key and only
# prints a warning, so any output fails this script. It also requires every
# Exec*= program to exist. The units run programs from /usr/bin, which the
# package installs but CI does not, so each one is pointed at an empty stub.
set -euo pipefail

UNIT_DIR="${1:-packaging/systemd}"
if [ ! -d "$UNIT_DIR" ]; then
    echo "No unit directory: $UNIT_DIR" >&2
    exit 1
fi

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT
mkdir -p "$WORK_DIR/bin"

# Exec lines may put prefixes such as "-" or "!" before the program path.
EXEC_PROGRAM='^(Exec[A-Za-z]*=[-@:+!]*)/usr/bin/'

shopt -s nullglob
for unit in "$UNIT_DIR"/*.service "$UNIT_DIR"/*.timer; do
    # A timer has no Exec line, and grep's "no match" exit would end the script.
    { grep -E "$EXEC_PROGRAM" "$unit" || true; } | sed -E "s#${EXEC_PROGRAM}([^[:space:]]+).*#\2#" |
        while read -r program; do
            printf '#!/bin/sh\n' > "$WORK_DIR/bin/$program"
            chmod +x "$WORK_DIR/bin/$program"
        done
    sed -E "s#${EXEC_PROGRAM}#\1${WORK_DIR}/bin/#" "$unit" > "$WORK_DIR/$(basename "$unit")"
done

cd "$WORK_DIR"
units=(./*.service ./*.timer)
if [ ${#units[@]} -eq 0 ]; then
    echo "No .service or .timer files in $UNIT_DIR" >&2
    exit 1
fi

if ! output=$(systemd-analyze verify "${units[@]}" 2>&1) || [ -n "$output" ]; then
    echo "$output"
    exit 1
fi
echo "systemd-analyze verify: ${#units[@]} units in $UNIT_DIR are clean"
