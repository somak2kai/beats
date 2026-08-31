#!/usr/bin/env bash
# scripts/mutation/install.sh — installs the two tools run.sh uses
# (mutago, gopls). See README.md for what they're for and why other
# tools were tried and dropped.

set -euo pipefail

command -v go >/dev/null 2>&1 || {
  echo "error: 'go' not found on PATH. Install Go first: https://go.dev/dl/" >&2
  exit 1
}

echo "==> go version: $(go version)"

echo "==> installing mutago"
go install github.com/quality-gates/mutago/v2/cmd/mutago@latest

echo "==> installing gopls"
go install golang.org/x/tools/gopls@latest

GOBIN="$(go env GOBIN)"
[[ -z "$GOBIN" ]] && GOBIN="$(go env GOPATH)/bin"

echo
echo "==> done. Installed binaries should be in: $GOBIN"
echo "    Make sure that directory is on your PATH, e.g.:"
echo "      export PATH=\"\$PATH:$GOBIN\""
echo
command -v mutago >/dev/null 2>&1 && echo "    mutago: $(command -v mutago)" || echo "    mutago: NOT on PATH (install may have failed, see above)"
command -v gopls  >/dev/null 2>&1 && echo "    gopls:  $(command -v gopls)"  || echo "    gopls:  NOT on PATH (install may have failed, see above)"
