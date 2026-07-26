#!/usr/bin/env bash
# collect_clusters.sh <repo>
#
# Emits a JSON array of every non-primitive cluster (with member bodies) so the
# systemic (Pass C) conformity-trap scan can see clusters that produced NO
# outlier — the exact place a shared insecure convention hides.
#
# Method: the full ShapeHash of every cluster is rendered in report.html inside
# <span class="cl-hash">HASH ...</span>. We extract each, then ask beats for the
# cluster's bodies via:  beats query cluster shape <hash> --repo <path> --format json
#
# Override the binary with BEATS_BIN=/path/to/beats if it is not on PATH.
set -euo pipefail

REPO="${1:?usage: collect_clusters.sh <repo>}"
REPORT="$REPO/.beats/report.html"
BEATS="${BEATS_BIN:-beats}"

if [[ ! -f "$REPORT" ]]; then
  echo "[]"
  exit 0
fi

# Full ShapeHash = the hex run immediately after the cl-hash span opener.
HASHES=()
while IFS= read -r line; do
  HASHES+=("$line")
done < <(grep -oE 'class="cl-hash">[0-9a-f]+' "$REPORT" \
           | sed 's/.*>//' | sort -u)

if [[ ${#HASHES[@]} -eq 0 ]]; then
  echo "[]"
  exit 0
fi

printf '['
first=1
for h in "${HASHES[@]}"; do
  obj="$("$BEATS" query cluster shape "$h" --repo "$REPO" --format json 2>/dev/null || true)"
  [[ -z "$obj" ]] && continue
  # keep only things that look like a JSON object/array
  case "$obj" in
    \{*|\[*) : ;;
    *) continue ;;
  esac
  [[ $first -eq 0 ]] && printf ','
  printf '%s' "$obj"
  first=0
done
printf ']\n'
