#!/usr/bin/env bash
#
# release-study.sh — Run beats init across the last N releases of a git repo
# and evaluate how clusters evolve over time.
#
# All git checkout and cross-release evaluation happens HERE, outside beats.
# beats is called as a black box per release.
#
# Usage:
#   ./scripts/release-study.sh <git-clone-url> [--releases 10] [--workdir /tmp/release-study] [--beats ./beats]
#
# Example:
#   ./scripts/release-study.sh https://github.com/gin-gonic/gin.git
#   ./scripts/release-study.sh https://github.com/hashicorp/hcl.git --releases 5
#
# Output:
#   $WORKDIR/report.md              — Full study report
#   $WORKDIR/releases.csv           — Per-release cluster stats
#   $WORKDIR/cluster-tracking.csv   — Which clusters appear in which releases
#   $WORKDIR/threshold-drift.csv    — Threshold values logged per release
#   $WORKDIR/outlier-tracking.csv   — Which outlier functions persist across releases
#   $WORKDIR/outlier-resolution.csv — Outliers that were resolved between releases
#   $WORKDIR/outliers/              — Per-release outlier JSON snapshots
#
# Prerequisites:
#   go build -o beats ./cmd/

set -euo pipefail

# ── defaults ──────────────────────────────────────────────────────────────────

NUM_RELEASES=10
WORKDIR="/tmp/beats-release-study"
BEATS=""
CLONE_URL=""

# ── parse args ────────────────────────────────────────────────────────────────

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 <git-clone-url> [--releases N] [--workdir dir] [--beats /path/to/beats]"
  exit 1
fi

CLONE_URL="$1"; shift

while [[ $# -gt 0 ]]; do
  case $1 in
    --releases)  NUM_RELEASES="$2"; shift 2 ;;
    --workdir)   WORKDIR="$2"; shift 2 ;;
    --beats)     BEATS="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 <git-clone-url> [--releases N] [--workdir dir] [--beats /path/to/beats]"
      exit 0
      ;;
    *) echo "Unknown arg: $1"; exit 1 ;;
  esac
done

# Find beats binary
# if [[ -z "$BEATS" ]]; then
#   if [[ -x "./beats" ]]; then
#     BEATS="./beats"
#   elif command -v beats &>/dev/null; then
#     BEATS="beats"
#   else
#     echo "ERROR: beats binary not found. Run 'go build -o beats ./cmd/' first."
#     exit 1
#   fi
# fi

BEATS="./dist/beats_darwin_arm64_v8.0/beats"
BEATS=$(cd "$(dirname "$BEATS")" && pwd)/$(basename "$BEATS")

REPO_NAME=$(basename "$CLONE_URL" .git)
WORKDIR="$WORKDIR/$REPO_NAME"
REPO_DIR="$WORKDIR/repo"

mkdir -p "$WORKDIR"

# ── output files ──────────────────────────────────────────────────────────────

REPORT="$WORKDIR/report.md"
RELEASES_CSV="$WORKDIR/releases.csv"
TRACKING_CSV="$WORKDIR/cluster-tracking.csv"
THRESHOLD_CSV="$WORKDIR/threshold-drift.csv"
OUTLIER_TRACKING_CSV="$WORKDIR/outlier-tracking.csv"
OUTLIER_RESOLUTION_CSV="$WORKDIR/outlier-resolution.csv"
OUTLIER_DIR="$WORKDIR/outliers"
rm -rf "$OUTLIER_DIR"
mkdir -p "$OUTLIER_DIR"

# Save repo metadata for analysis script (git links, etc.)
METADATA_FILE="$WORKDIR/metadata.txt"
# Convert clone URL to browsable URL (strip .git suffix, convert ssh→https)
BROWSE_URL=$(echo "$CLONE_URL" | sed 's/\.git$//' | sed 's|git@github.com:|https://github.com/|')
echo "clone_url=$CLONE_URL" > "$METADATA_FILE"
echo "browse_url=$BROWSE_URL" >> "$METADATA_FILE"
echo "repo_name=$REPO_NAME" >> "$METADATA_FILE"

# ── clone ─────────────────────────────────────────────────────────────────────

echo "=== beats Release Study: $REPO_NAME ==="
echo ""

if [[ -d "$REPO_DIR" ]]; then
  echo "Removing previous clone at $REPO_DIR..."
  rm -rf "$REPO_DIR"
fi
echo "Cloning $CLONE_URL..."
git clone "$CLONE_URL" "$REPO_DIR"

cd "$REPO_DIR"

# ── find releases ─────────────────────────────────────────────────────────────

echo "Finding last $NUM_RELEASES releases..."

# Get tags sorted by version (most recent first), take N
# Prefer semver-looking tags (v1.2.3) but fall back to all tags
SEMVER_TAGS=$(git tag -l --sort=-version:refname 'v[0-9]*' 2>/dev/null | head -n "$NUM_RELEASES")
if [[ -z "$SEMVER_TAGS" ]]; then
  SEMVER_TAGS=$(git tag -l --sort=-creatordate | head -n "$NUM_RELEASES")
fi

if [[ -z "$SEMVER_TAGS" ]]; then
  echo "ERROR: No tags found in this repo."
  exit 1
fi

# Reverse so we go oldest → newest (tail -r is macOS tac equivalent)
REVERSED=$(echo "$SEMVER_TAGS" | tail -r 2>/dev/null || echo "$SEMVER_TAGS" | tac 2>/dev/null || echo "$SEMVER_TAGS")
TAGS=()
while IFS= read -r line; do
  [[ -n "$line" ]] && TAGS+=("$line")
done <<< "$REVERSED"

echo "Found ${#TAGS[@]} releases: ${TAGS[*]}"
echo ""

# ── CSV headers ───────────────────────────────────────────────────────────────

echo "release,release_idx,functions,eligible,clusters,clustered,orphans,coverage_pct,mean_coherence,mean_call_coherence,mean_score,primitive_clusters,outlier_candidates" > "$RELEASES_CSV"
echo "release,release_idx,identifyThreshold,seqSimilarityThreshold,maxTrigranBucket,minTokenSeqLen" > "$THRESHOLD_CSV"

# Cluster tracking via temp files (avoids bash 4+ associative arrays)
CLUSTER_DATA_DIR="$WORKDIR/.cluster-tracking"
rm -rf "$CLUSTER_DATA_DIR"
mkdir -p "$CLUSTER_DATA_DIR"

# ── per-release loop ─────────────────────────────────────────────────────────

# Remember original branch to restore later
ORIG_REF=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || git rev-parse HEAD)

cleanup() {
  cd "$REPO_DIR" 2>/dev/null
  git checkout "$ORIG_REF" --force 2>/dev/null || true
  git clean -fd 2>/dev/null || true
}
trap cleanup EXIT

for idx in "${!TAGS[@]}"; do
  tag="${TAGS[$idx]}"
  echo "────────────────────────────────────────"
  echo "[$((idx+1))/${#TAGS[@]}] Release: $tag"
  echo "────────────────────────────────────────"

  # Checkout
  git checkout "$tag" --force 2>/dev/null
  git clean -fd 2>/dev/null || true

  # Clear previous beats DB for this repo (so each release starts clean)
  BEATS_DB_DIR="$HOME/.beats/badger"
  if [[ -d "$BEATS_DB_DIR" ]]; then
    # Find and remove the DB for this repo path
    # beats uses the repo absolute path as the DB key
    rm -rf "$BEATS_DB_DIR"/* 2>/dev/null || true
  fi

  # Also clean .beats in repo
  rm -rf "$REPO_DIR/.beats" 2>/dev/null || true

  # Run beats init, capture output
  echo "  Running beats init..."
  init_output=$("$BEATS" init --repo "$REPO_DIR" 2>&1) || true
  echo "  beats init finished (${#init_output} chars of output)"

  # ── Extract stats from beats output ──

  # Parse log lines (macOS-compatible — no grep -P, || true for set -e safety)
  ident_thresh=$(echo "$init_output" | grep 'identifyThreshold' | sed 's/.*identifyThreshold=\([0-9.]*\).*/\1/' | head -1 || true)
  seq_thresh=$(echo "$init_output" | grep 'seqSimilarityThreshold' | sed 's/.*seqSimilarityThreshold=\([0-9.]*\).*/\1/' | head -1 || true)
  max_bucket=$(echo "$init_output" | grep 'maxTrigranBucket' | sed 's/.*maxTrigranBucket=\([0-9]*\).*/\1/' | head -1 || true)
  min_seq_len=$(echo "$init_output" | grep 'minTokenSeqLen' | sed 's/.*minTokenSeqLen=\([0-9]*\).*/\1/' | head -1 || true)

  echo "$tag,$idx,${ident_thresh:-?},${seq_thresh:-?},${max_bucket:-?},${min_seq_len:-?}" >> "$THRESHOLD_CSV"

  # Parse cluster/function counts from log output (|| true to avoid set -e exit)
  num_functions=$(echo "$init_output" | grep 'parsed functions' | grep -oE 'total=[0-9]+' | head -1 | cut -d= -f2 || true)
  num_clusters=$(echo "$init_output" | grep 'identified clusters' | grep -oE 'count=[0-9]+' | head -1 | cut -d= -f2 || true)
  num_orphans=$(echo "$init_output" | grep 'identified clusters' | grep -oE 'orphans=[0-9]+' | head -1 | cut -d= -f2 || true)
  num_outliers=$(echo "$init_output" | grep 'orphaned functions persisted' | grep -oE 'count=[0-9]+' | head -1 | cut -d= -f2 || true)

  # ── Extract per-cluster data via beats query ──

  # Get outlier data as JSON
  outlier_json=$("$BEATS" query outlier --repo "$REPO_DIR" --format json 2>/dev/null) || outlier_json="[]"

  # Save outlier JSON snapshot for cross-release analysis
  echo "$outlier_json" > "$OUTLIER_DIR/outliers-${tag}.json"

  # Count outlier candidates and extract outlier function identifiers
  outlier_count=$(echo "$outlier_json" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    if not isinstance(data, list):
        print(0)
    else:
        print(len(data))
        # Write per-outlier summary: pkg.func -> top candidate cluster
        for o in data:
            pkg = o.get('package', '')
            func = o.get('func', '')
            fpath = o.get('file', '')
            cands = o.get('candidates', [])
            top_cluster = cands[0]['cluster_id'] if cands else ''
            top_score = cands[0]['score'] if cands else 0
            # Print to stderr so we can capture it separately
            print(f'{pkg}\t{func}\t{fpath}\t{top_cluster}\t{top_score}', file=sys.stderr)
except:
    print(0)
" 2>"$OUTLIER_DIR/outlier-summary-${tag}.tsv" || echo 0)

  # Extract cluster shape hashes and sizes from beats analyze output
  # We need to scan the DB directly — let's use beats query for each cluster
  # Instead, parse the analyze HTML or use the outlier JSON to get cluster hashes

  # Get cluster info by running analyze and parsing the generated HTML
  "$BEATS" analyze --repo "$REPO_DIR" 2>/dev/null || true

  analyze_html="$REPO_DIR/.beats/report.html"
  if [[ -f "$analyze_html" ]]; then
    # Extract cluster data from HTML — shape hashes and sizes
    # The HTML has cluster cards with data we can parse
    # Simpler: count clusters and extract stats from the HTML title/summary

    # Extract cluster hashes and member counts from HTML
    # Look for patterns like: <td>abc123</td> ... <td>5 functions</td>
    python3 -c "
import re, sys

html = open('$analyze_html', 'r', errors='replace').read()

# Find cluster shape hashes and sizes
# Pattern depends on report format — try common patterns
clusters = []

# Try: shape hash in code/td tags followed by member counts
# This is fragile but best we can do without a JSON dump command
hash_pattern = re.findall(r'(?:shape|hash)[^>]*>([0-9a-f]{6,16})<', html, re.I)
size_pattern = re.findall(r'(\d+)\s*(?:functions|members)', html, re.I)

# Also try to get coherence values
coh_pattern = re.findall(r'coherence[^>]*>([0-9.]+)', html, re.I)

for i, h in enumerate(hash_pattern):
    size = int(size_pattern[i]) if i < len(size_pattern) else 0
    print(f'{h},{size}')
" 2>/dev/null | while IFS=',' read -r hash size; do
      if [[ -n "$hash" && ${#hash} -ge 6 ]]; then
        hash_file="$CLUSTER_DATA_DIR/$hash"
        if [[ ! -f "$hash_file" ]]; then
          echo "$idx" > "$hash_file.first"
        fi
        echo "$idx" >> "$hash_file.releases"
        echo "$size" >> "$hash_file.sizes"
      fi
    done
  fi

  # Compute coverage
  eligible=${num_functions:-0}
  clustered=0
  coverage_pct=0
  mean_coh="?"
  mean_call_coh="?"
  mean_score="?"
  primitives=0

  # Write release row (some fields may be missing — that's fine)
  echo "$tag,$idx,${num_functions:-0},${eligible},${num_clusters:-0},${clustered},${num_orphans:-0},${coverage_pct},${mean_coh},${mean_call_coh},${mean_score},${primitives},${outlier_count}" >> "$RELEASES_CSV"

  echo "  Clusters: ${num_clusters:-?}  Orphans: ${num_orphans:-?}  Outliers: ${outlier_count}"
  echo ""
done

# ── Build cluster tracking CSV ────────────────────────────────────────────────

echo "shapeHash,first_seen_idx,first_seen_release,num_releases_present,release_indices,sizes" > "$TRACKING_CSV"
if [[ -d "$CLUSTER_DATA_DIR" ]]; then
  for first_file in "$CLUSTER_DATA_DIR"/*.first; do
    [[ -f "$first_file" ]] || continue
    hash=$(basename "$first_file" .first)
    first_idx=$(cat "$first_file")
    first_rel="${TAGS[$first_idx]}"
    releases=$(paste -sd',' "$CLUSTER_DATA_DIR/$hash.releases")
    sizes=$(paste -sd',' "$CLUSTER_DATA_DIR/$hash.sizes")
    num_present=$(wc -l < "$CLUSTER_DATA_DIR/$hash.releases" | tr -d ' ')
    echo "$hash,$first_idx,$first_rel,$num_present,$releases,$sizes" >> "$TRACKING_CSV"
  done
fi

# ── Build outlier tracking and resolution CSVs ──────────────────────────────

echo "  Building outlier tracking data..."

python3 -c "
import json, os, sys, csv

outlier_dir = '$OUTLIER_DIR'
tags_str = '${TAGS[*]}'
tags = tags_str.split()

# Load all outlier snapshots: tag -> {(pkg, func): {cluster, score, file}}
snapshots = {}
for tag in tags:
    path = os.path.join(outlier_dir, f'outliers-{tag}.json')
    if not os.path.exists(path):
        snapshots[tag] = {}
        continue
    try:
        with open(path) as f:
            data = json.load(f)
        if not isinstance(data, list):
            snapshots[tag] = {}
            continue
        outliers = {}
        for o in data:
            key = (o.get('package', ''), o.get('func', ''))
            cands = o.get('candidates', [])
            outliers[key] = {
                'cluster': cands[0]['cluster_id'] if cands else '',
                'score': cands[0]['score'] if cands else 0,
                'file': o.get('file', ''),
            }
        snapshots[tag] = outliers
    except:
        snapshots[tag] = {}

# ── Axis 2: Outlier persistence ──
# Track which (pkg, func) appears as an outlier in which releases
all_outlier_keys = set()
for tag in tags:
    all_outlier_keys.update(snapshots[tag].keys())

tracking_path = '$OUTLIER_TRACKING_CSV'
with open(tracking_path, 'w', newline='') as f:
    w = csv.writer(f)
    w.writerow(['package', 'func', 'file', 'first_seen', 'last_seen', 'num_releases',
                'release_list', 'top_cluster', 'mean_score'])
    for key in sorted(all_outlier_keys):
        pkg, func = key
        present_tags = []
        scores = []
        clusters = []
        files = []
        for tag in tags:
            if key in snapshots[tag]:
                present_tags.append(tag)
                scores.append(snapshots[tag][key]['score'])
                clusters.append(snapshots[tag][key]['cluster'])
                files.append(snapshots[tag][key].get('file', ''))
        if not present_tags:
            continue
        # Most common cluster and most recent file path
        from collections import Counter
        top_cluster = Counter(clusters).most_common(1)[0][0] if clusters else ''
        latest_file = files[-1] if files else ''
        mean_score = sum(scores) / len(scores) if scores else 0
        w.writerow([pkg, func, latest_file, present_tags[0], present_tags[-1],
                    len(present_tags), ';'.join(present_tags),
                    top_cluster, f'{mean_score:.3f}'])

# ── Axis 3: Outlier resolution ──
# For each consecutive pair of releases, find outliers that disappeared
resolution_path = '$OUTLIER_RESOLUTION_CSV'
with open(resolution_path, 'w', newline='') as f:
    w = csv.writer(f)
    w.writerow(['package', 'func', 'file', 'outlier_in', 'resolved_by', 'resolution',
                'top_cluster', 'score_at_disappearance'])
    for i in range(len(tags) - 1):
        prev_tag = tags[i]
        next_tag = tags[i + 1]
        prev_outliers = snapshots[prev_tag]
        next_outliers = snapshots[next_tag]

        for key in prev_outliers:
            if key not in next_outliers:
                # This outlier disappeared — was it resolved or deleted?
                pkg, func = key
                info = prev_outliers[key]

                # Heuristic: if the function's file still exists in the next
                # release's outlier set (any function from same file), it wasn't
                # deleted. But we can't be 100% sure without checking cluster
                # membership. Best we can do from outlier JSON alone:
                # - If any other outlier in next release shares the same top
                #   cluster, the cluster still exists → likely rejoined
                # - Otherwise → likely deleted or refactored away

                cluster = info['cluster']
                next_has_cluster = any(
                    v['cluster'] == cluster
                    for v in next_outliers.values()
                ) if cluster else False

                # Check if same file has outliers in next release (file still exists)
                fpath = info.get('file', '')
                next_has_file = any(
                    v.get('file', '') == fpath
                    for v in next_outliers.values()
                ) if fpath else False

                if next_has_cluster:
                    resolution = 'rejoined_cluster'
                elif next_has_file:
                    resolution = 'fixed_or_refactored'
                else:
                    resolution = 'deleted_or_moved'

                w.writerow([pkg, func, fpath, prev_tag, next_tag, resolution,
                            cluster, f'{info[\"score\"]:.3f}'])

print(f'  Outlier tracking: {len(all_outlier_keys)} unique outlier functions across {len(tags)} releases')

# Print summary stats
persistent = sum(1 for k in all_outlier_keys
                 if sum(1 for t in tags if k in snapshots[t]) >= len(tags) / 2)
print(f'  Persistent outliers (>= half of releases): {persistent}')

# Resolution counts
res_counts = {}
for i in range(len(tags) - 1):
    prev = snapshots[tags[i]]
    nxt = snapshots[tags[i + 1]]
    for key in prev:
        if key not in nxt:
            res_counts['resolved'] = res_counts.get('resolved', 0) + 1
total_resolved = res_counts.get('resolved', 0)
print(f'  Total outlier resolutions across releases: {total_resolved}')
" 2>&1 || echo "  (outlier tracking analysis failed — will still generate report)"

# ── Generate report ──────────────────────────────────────────────────────────

NUM_TOTAL_TAGS=${#TAGS[@]}

cat > "$REPORT" <<EOF
# beats Release Study: $REPO_NAME

**Date**: $(date -u '+%Y-%m-%d %H:%M UTC')
**Repo**: $CLONE_URL
**Releases analyzed**: ${TAGS[*]}

---

## 1. Per-Release Summary

EOF

# Pretty-print the releases CSV as a table
echo '```' >> "$REPORT"
column -t -s',' "$RELEASES_CSV" >> "$REPORT" 2>/dev/null || cat "$RELEASES_CSV" >> "$REPORT"
echo '```' >> "$REPORT"

cat >> "$REPORT" <<EOF

## 2. Threshold Values

Thresholds used per release (default values unless overridden).

EOF

echo '```' >> "$REPORT"
column -t -s',' "$THRESHOLD_CSV" >> "$REPORT" 2>/dev/null || cat "$THRESHOLD_CSV" >> "$REPORT"
echo '```' >> "$REPORT"

cat >> "$REPORT" <<EOF

## 3. Cluster Persistence

Which clusters appear across multiple releases? Persistent clusters represent
real structural conventions in the codebase. Transient clusters are either
noise or patterns that were refactored away.

EOF

if [[ -s "$TRACKING_CSV" ]] && [[ $(wc -l < "$TRACKING_CSV") -gt 1 ]]; then
  # Sort by persistence (most releases first)
  echo '```' >> "$REPORT"
  head -1 "$TRACKING_CSV" >> "$REPORT"
  tail -n +2 "$TRACKING_CSV" | sort -t',' -k4 -nr | head -20 >> "$REPORT"
  echo '```' >> "$REPORT"

  # Stats
  total_clusters=$(tail -n +2 "$TRACKING_CSV" | wc -l | tr -d ' ')
  persistent=$(tail -n +2 "$TRACKING_CSV" | awk -F',' "\$4 >= $((NUM_TOTAL_TAGS / 2))" | wc -l | tr -d ' ')
  transient=$(tail -n +2 "$TRACKING_CSV" | awk -F',' '$4 <= 2' | wc -l | tr -d ' ')

  cat >> "$REPORT" <<EOF

- **Total unique clusters seen**: $total_clusters
- **Persistent** (appear in ≥ half the releases): $persistent
- **Transient** (appear in ≤ 2 releases): $transient
- **Persistence ratio**: $(echo "scale=1; $persistent * 100 / $total_clusters" | bc 2>/dev/null || echo "?")%

A high persistence ratio (>60%) means the clustering is capturing real, lasting
conventions. A low ratio means either the thresholds are too loose (noise clusters)
or the codebase is actively changing structure.

EOF
else
  echo "(Cluster tracking data not available — HTML parsing may need adjustment for this repo's report format)" >> "$REPORT"
fi

cat >> "$REPORT" <<EOF

## 4. Outlier Persistence (Axis 2)

Which functions remain outliers across multiple releases? A function that is
consistently an outlier is a genuine structural deviation — not noise.
A function that appears as an outlier in only one release was likely transient.

EOF

if [[ -s "$OUTLIER_TRACKING_CSV" ]] && [[ $(wc -l < "$OUTLIER_TRACKING_CSV") -gt 1 ]]; then
  # Show most persistent outliers (sorted by num_releases desc)
  echo '```' >> "$REPORT"
  head -1 "$OUTLIER_TRACKING_CSV" >> "$REPORT"
  tail -n +2 "$OUTLIER_TRACKING_CSV" | sort -t',' -k5 -nr | head -20 >> "$REPORT"
  echo '```' >> "$REPORT"

  total_outlier_funcs=$(tail -n +2 "$OUTLIER_TRACKING_CSV" | wc -l | tr -d ' ')
  persistent_outliers=$(tail -n +2 "$OUTLIER_TRACKING_CSV" | awk -F',' "\$5 >= $((NUM_TOTAL_TAGS / 2))" | wc -l | tr -d ' ')
  transient_outliers=$(tail -n +2 "$OUTLIER_TRACKING_CSV" | awk -F',' '$5 <= 1' | wc -l | tr -d ' ')

  cat >> "$REPORT" <<EOF

- **Total unique outlier functions**: $total_outlier_funcs
- **Persistent outliers** (≥ half of releases): $persistent_outliers
- **One-off outliers** (single release only): $transient_outliers
- **Outlier persistence ratio**: $(echo "scale=1; $persistent_outliers * 100 / $total_outlier_funcs" | bc 2>/dev/null || echo "?")%

High outlier persistence means beats is consistently finding the same structural
deviations. Low persistence suggests either the codebase is actively being cleaned
up, or the outlier detection is too sensitive.

EOF
else
  echo "(Outlier tracking data not available)" >> "$REPORT"
  echo "" >> "$REPORT"
fi

cat >> "$REPORT" <<EOF

## 5. Outlier Resolution (Axis 3)

When an outlier disappears between releases, what happened?
- **rejoined_cluster**: The outlier's candidate cluster still exists and the function
  is no longer an outlier → it was likely fixed to match the convention.
- **fixed_or_refactored**: The function's file still has outliers but this function
  isn't one anymore → localized fix.
- **deleted_or_moved**: No trace of the function or its file in the outlier set →
  the function was removed or moved to a different package.

EOF

if [[ -s "$OUTLIER_RESOLUTION_CSV" ]] && [[ $(wc -l < "$OUTLIER_RESOLUTION_CSV") -gt 1 ]]; then
  echo '```' >> "$REPORT"
  head -1 "$OUTLIER_RESOLUTION_CSV" >> "$REPORT"
  tail -n +2 "$OUTLIER_RESOLUTION_CSV" | head -30 >> "$REPORT"
  echo '```' >> "$REPORT"

  total_resolutions=$(tail -n +2 "$OUTLIER_RESOLUTION_CSV" | wc -l | tr -d ' ')
  rejoined=$(grep -c 'rejoined_cluster' "$OUTLIER_RESOLUTION_CSV" || echo 0)
  fixed=$(grep -c 'fixed_or_refactored' "$OUTLIER_RESOLUTION_CSV" || echo 0)
  deleted=$(grep -c 'deleted_or_moved' "$OUTLIER_RESOLUTION_CSV" || echo 0)

  cat >> "$REPORT" <<EOF

- **Total outlier resolutions**: $total_resolutions
- **Rejoined cluster**: $rejoined (outlier was fixed to match the convention)
- **Fixed/refactored**: $fixed (function changed but file still active)
- **Deleted/moved**: $deleted (function removed from codebase)

A high "rejoined" count is the strongest signal that beats outlier detection
is finding real deviations that developers actually fix. This is ground truth.

EOF
else
  echo "(Outlier resolution data not available)" >> "$REPORT"
  echo "" >> "$REPORT"
fi

cat >> "$REPORT" <<EOF

## 6. Interpretation Guide

### This study answers:

1. **"Do the same clusters keep forming?"**
   Look at cluster persistence. If the same shape hashes appear release after
   release, the clustering is capturing real conventions — regardless of what
   threshold produced them.

2. **"Is the codebase changing structurally?"**
   Look at cluster count trend. Rising cluster count with stable thresholds
   indicates organic growth in coding conventions.

3. **"Are the outliers real?"**
   Look at outlier persistence. If the same functions keep showing up as outliers
   release after release, they represent genuine structural deviations, not noise.

4. **"Do developers actually fix outliers?"**
   Look at outlier resolution. A high "rejoined_cluster" count means developers
   are fixing deviations that beats detected — the closest thing to ground truth.
   This is the strongest validation signal for beats.

EOF

# ── Done ──────────────────────────────────────────────────────────────────────

echo "=== Study complete ==="
echo ""
echo "Results:"
echo "  Report:              $REPORT"
echo "  Per-release stats:   $RELEASES_CSV"
echo "  Cluster tracking:    $TRACKING_CSV"
echo "  Threshold drift:     $THRESHOLD_CSV"
echo "  Outlier tracking:    $OUTLIER_TRACKING_CSV"
echo "  Outlier resolution:  $OUTLIER_RESOLUTION_CSV"
echo "  Outlier snapshots:   $OUTLIER_DIR/"
