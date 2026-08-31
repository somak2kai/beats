# shellcheck shell=bash
# mutago: mutation testing. See README.md for what it's for and why other
# tools (gremlins, go-mutesting) were tried and dropped.

run_mutago() {
  log "mutago"
  if ! command -v mutago >/dev/null 2>&1; then
    warn "mutago not found — run ./install.sh first. Skipping."
    return
  fi

  local dir="$OUT_DIR/mutago"
  mkdir -p "$dir"

  # mutago writes report.json / mutago-summary.json / mutago-agentic.json
  # (fixed names, confirmed by an actual run) into its CWD — harvested
  # into $dir below, then cleaned out of sample/ either way.
  ( cd "$SAMPLE_DIR" && mutago --logger-summary-json --logger-agentic-json . ) \
    | tee "$OUT_DIR/mutago-run.txt" \
    || warn "mutago exited non-zero (often just means a mutant escaped) — see $OUT_DIR/mutago-run.txt"

  mv "$SAMPLE_DIR"/report.json "$dir/report.json" 2>/dev/null || true
  mv "$SAMPLE_DIR"/mutago-summary.json "$dir/summary.json" 2>/dev/null || true
  mv "$SAMPLE_DIR"/mutago-agentic.json "$dir/agentic.json" 2>/dev/null || true
  rm -f "$SAMPLE_DIR"/mutago-*.json

  reconstruct_mutants "$dir"
}

# Escaped mutants get a real unified diff printed (killed ones don't).
# Split those out with awk (not csplit — BSD vs GNU csplit disagree on
# flags) and patch each into a real, go-vet-able mutant .go file.
reconstruct_mutants() {
  local dir="$1"
  awk -v outdir="$dir" '
    /^--- Original/ { n++; fname = sprintf("%s/diff_%02d.patch", outdir, n) }
    n>0 { print > fname }
  ' "$OUT_DIR/mutago-run.txt" 2>/dev/null || true

  local reconstructed=0 patchfile n candidate
  for patchfile in "$dir"/diff_*.patch; do
    [[ -e "$patchfile" ]] || continue
    n="$(basename "$patchfile" .patch)"
    candidate="$dir/functions.$n.go"
    cp "$SAMPLE_DIR/functions.go" "$candidate"
    if patch -s -p0 "$candidate" < "$patchfile" 2>"$dir/$n.patch.err"; then
      reconstructed=$((reconstructed + 1))
      if go vet "$candidate" >"$dir/$n.vet.log" 2>&1; then
        echo "$n: reconstructed + go vet clean"
      else
        warn "$n: reconstructed, go vet flagged — see $dir/$n.vet.log"
      fi
    else
      rm -f "$candidate"
    fi
  done

  if [[ "$reconstructed" -gt 0 ]]; then
    echo "reconstructed $reconstructed mutant file(s) in $dir"
  else
    warn "couldn't reconstruct any mutant files — check $OUT_DIR/mutago-run.txt by hand"
  fi
}
