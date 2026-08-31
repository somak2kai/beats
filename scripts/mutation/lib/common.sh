# shellcheck shell=bash
# Shared paths and helpers, sourced by run.sh.

SAMPLE_DIR="$SCRIPT_DIR/sample"
OUT_DIR="$SCRIPT_DIR/out"

log()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }

# Byte offset for gopls's "file:#offset" syntax — see tools/offsetof/main.go
# for the two modes (plain substring, or -func for a function identifier).
offsetof() { ( cd "$SCRIPT_DIR/tools/offsetof" && go run . "$@" ); }

sample_sanity_check() {
  log "sanity: sample module builds and passes its own tests"
  ( cd "$SAMPLE_DIR" && go vet ./... && go test ./... )
}
