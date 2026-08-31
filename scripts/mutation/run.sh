#!/usr/bin/env bash
# scripts/mutation/run.sh — runs mutago + gopls against sample/. See
# README.md for what this is and why these two tools.
#
# Usage:
#   ./install.sh
#   ./run.sh
#
# Output lands in scripts/mutation/out/ (gitignored).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/mutago.sh
source "$SCRIPT_DIR/lib/mutago.sh"
# shellcheck source=lib/gopls.sh
source "$SCRIPT_DIR/lib/gopls.sh"

command -v go >/dev/null 2>&1 || { echo "error: 'go' not on PATH." >&2; exit 1; }

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

sample_sanity_check
run_mutago
run_gopls

log "done"
echo "All output under: $OUT_DIR"
