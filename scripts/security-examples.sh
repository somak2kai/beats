#!/usr/bin/env bash
#
# security-examples.sh — Clone a set of Go repos at their default branch (latest
# main) and run `beats init` on each, producing ready-to-triage example repos
# for the beats-security skill.
#
# Indexing happens HERE, in your shell (cheap, no LLM). You then fire the
# beats-security skill inside Claude to get the CWE findings + security matrix.
#
# NOTE: cloning at main means known CVEs are usually already patched. This is
# for seeing the skill work on real, cluster-rich code. To validate against a
# specific known CVE, pin the vulnerable tag instead — see
# beats.plugin/skills/beats-security/RUNBOOK.md.
#
# Usage:
#   ./scripts/security-examples.sh [--repos owner/repo,...] [--workdir dir] [--beats /path/to/beats] [--depth N]
#
# Examples:
#   ./scripts/security-examples.sh
#   ./scripts/security-examples.sh --repos go-git/go-git,gogs/gogs
#   ./scripts/security-examples.sh --repos minio/minio,go-gitea/gitea --workdir /tmp/sec
#
# Output (per repo):
#   $WORKDIR/<repo>/.beats/outlier.md   — clusters + outliers (feeds Pass A/B)
#   $WORKDIR/<repo>/.beats/report.html  — all clusters (feeds the Pass C collector)
#
# When it finishes, it prints the exact Claude command + trigger to run.
#
# Prerequisites:
#   go build -o beats ./cmd/     (or: go install github.com/somak2kai/beats/cmd@latest)

set -euo pipefail

# ── defaults ──────────────────────────────────────────────────────────────────

# Security-rich Go repos that produce dense clusters at HEAD. Lighter/faster
# ones first so you get feedback early; the last two are large (several minutes
# each to index). Override anytime with --repos owner/repo,...
#   go-git  — VCS: path/symlink/crypto      gin    — web framework: routing/path
#   gogs    — git server: ssh/exec          caddy  — HTTP/TLS server
#   rclone  — cloud sync: crypto/net/path    cli    — GitHub CLI: exec/oauth/http
#   minio   — object store: crypto/secrets   gitea  — full forge (large)
DEFAULT_REPOS="go-git/go-git,gogs/gogs,caddyserver/caddy,gin-gonic/gin,rclone/rclone,cli/cli,minio/minio,go-gitea/gitea"
WORKDIR="/tmp/beats-security-examples"
BEATS="./dist/beats_darwin_arm64_v8.0/beats"
BEATS=$(cd "$(dirname "$BEATS")" && pwd)/$(basename "$BEATS")
DEPTH=1
PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/beats.plugin"

# ── parse args ────────────────────────────────────────────────────────────────

while [[ $# -gt 0 ]]; do
  case $1 in
    --repos)   DEFAULT_REPOS="$2"; shift 2 ;;
    --workdir) WORKDIR="$2"; shift 2 ;;
    --beats)   BEATS="$2"; shift 2 ;;
    --depth)   DEPTH="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 [--repos owner/repo,...] [--workdir dir] [--beats /path/to/beats] [--depth N]"
      exit 0 ;;
    *) echo "Unknown arg: $1"; exit 1 ;;
  esac
done

# ── locate beats binary ───────────────────────────────────────────────────────

if [[ -z "$BEATS" ]]; then
  if [[ -x "./beats" ]]; then BEATS="./beats"
  elif command -v beats >/dev/null 2>&1; then BEATS="beats"
  else
    echo "beats binary not found. Build it (go build -o beats ./cmd/) or pass --beats /path"
    exit 1
  fi
fi
echo "Using beats: $BEATS"
"$BEATS" version 2>/dev/null || true

mkdir -p "$WORKDIR"
IFS=',' read -ra REPOS <<< "$DEFAULT_REPOS"

# ── clone + index each repo ───────────────────────────────────────────────────

INDEXED=()
for slug in "${REPOS[@]}"; do
  name="${slug##*/}"
  dest="$WORKDIR/$name"
  echo
  echo "──────────────────────────────────────────────────────────────"
  echo "▶ $slug"
  if [[ -d "$dest" ]]; then
    # delete any prior clone so each run starts clean — with a guard so a bad
    # expansion can never rm -rf outside the workdir
    if [[ -n "$name" && "$dest" == "$WORKDIR/"* ]]; then
      echo "  removing existing → $dest"
      rm -rf "$dest"
    else
      echo "  refusing to delete suspicious path: '$dest'"; exit 1
    fi
  fi
  echo "  cloning (depth=$DEPTH) → $dest"
  git clone --depth "$DEPTH" "https://github.com/$slug.git" "$dest" 2>&1 | sed 's/^/    /'
  # resolve to the physical path so `beats init` and the later `beats query`
  # (fired by the skill) key on the same string — macOS /tmp → /private/tmp
  dest="$(cd "$dest" && pwd -P)"
  echo "  indexing with beats init …"
  if "$BEATS" init --repo "$dest" 2>&1 | sed 's/^/    /'; then
    if [[ -f "$dest/.beats/outlier.md" ]]; then
      out=$(grep -m1 '^outliers:' "$dest/.beats/outlier.md" 2>/dev/null || echo "outliers: (see report)")
      echo "  ✓ indexed — $out"
    else
      echo "  ✓ indexed — no outlier.md (few/no outliers); report.html still written for the systemic pass"
    fi
    INDEXED+=("$dest")
  else
    echo "  ✗ beats init failed for $slug (continuing)"
  fi
done

# ── how to triage in Claude ───────────────────────────────────────────────────

echo
echo "════════════════════════════════════════════════════════════════"
echo "Indexed ${#INDEXED[@]} repo(s). Triage each in Claude Code:"
echo
echo "  claude --plugin-dir $PLUGIN_DIR"
echo
echo "then type (mini mode — skips re-indexing, reads what we just wrote):"
echo
for d in "${INDEXED[@]}"; do
  echo "  mini security $d"
done
echo
echo "Each run writes <repo>/.beats/security.md and security-matrix.html"
echo "════════════════════════════════════════════════════════════════"
