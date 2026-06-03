#!/usr/bin/env bash
# beats_update_page.sh — write enrichment for all clusters on one page in parallel.
#
# Usage:
#   beats_update_page.sh <repo> <newline-delimited enrichment records>
#
# Each enrichment record is a single line of tab-separated fields:
#   IDX\tIDIOM\tVERDICT\tCANONICAL\tACTION\tCONFIDENCE\tQUESTIONS
#
# QUESTIONS is pipe-separated internally, e.g. "How is X done?|Where is Y wired?"
#
# The script fires one `beats update cluster` per record as a background job,
# then waits for all of them before exiting.
#
# Exit code: 0 if all updates succeed, non-zero if any fail.

set -euo pipefail

REPO="$1"
shift
INPUT="$1"   # path to a temp file containing the enrichment records

pids=()
failures=0

while IFS=$'\t' read -r idx idiom verdict canonical action confidence questions; do
  [[ -z "$idx" ]] && continue
  beats update cluster "$idx" \
    --repo "$REPO" \
    --idiom "$idiom" \
    --verdict "$verdict" \
    --canonical "$canonical" \
    --action "$action" \
    --confidence "$confidence" \
    --questions "$questions" &
  pids+=($!)
done < "$INPUT"

for pid in "${pids[@]}"; do
  wait "$pid" || (( failures++ )) || true
done

if (( failures > 0 )); then
  echo "WARNING: $failures update(s) failed" >&2
  exit 1
fi

echo "All updates complete."
