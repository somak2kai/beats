# shellcheck shell=bash
# gopls: rename (Tier 1) + inline function call (Tier 2), via its headless
# CLI. See README.md for what each demo means and why it's grounded.

run_gopls() {
  log "gopls"
  if ! command -v gopls >/dev/null 2>&1; then
    warn "gopls not found — run ./install.sh first. Skipping."
    return
  fi

  echo "--- rename (Tier 1) across a representative spread of functions ---"
  gopls_rename functions.go  Max                  MaxRenamed
  gopls_rename functions.go  Clamp                ClampRenamed
  gopls_rename functions.go  Reverse              ReverseRenamed
  gopls_rename functions.go  Filter               FilterRenamed
  gopls_rename enterprise.go ValidateEmail        ValidateEmailRenamed
  gopls_rename enterprise.go Paginate             PaginateRenamed
  gopls_rename enterprise.go RetryWithBackoff     RetryWithBackoffRenamed
  gopls_rename enterprise.go CircuitBreakerAllow  CircuitBreakerAllowRenamed

  echo
  echo "--- inline function call (Tier 2) across all 3 real call sites in the sample ---"
  # The only spots in sample/ where one sample function calls another —
  # everything else is a leaf function with nothing to inline (yet).
  gopls_inline enterprise.go "hasUpper(pw)"   hasupper
  gopls_inline enterprise.go "hasDigit(pw)"   hasdigit
  gopls_inline enterprise.go "hasSpecial(pw)" hasspecial

  echo
  echo "Per-target diffs and logs under $OUT_DIR/gopls/{rename,inline}_<label>/"
}

# Each target gets its own fresh scratch copy of sample/, so the demos stay
# independent and reviewable one at a time instead of compounding edits.
gopls_scratch() {
  local dir="$1" rel_file="$2"
  mkdir -p "$dir"
  rm -rf "$dir/sample"
  cp -r "$SAMPLE_DIR" "$dir/sample"
  echo "$dir/sample/$rel_file"
}

gopls_rename() {
  local rel_file="$1" funcname="$2" newname="$3"
  local label; label="$(printf '%s' "$funcname" | tr 'A-Z' 'a-z')"
  local dir="$OUT_DIR/gopls/rename_$label"
  local target; target="$(gopls_scratch "$dir" "$rel_file")"
  local scratch="$dir/sample"

  # offsetof -func parses the file and points straight at the identifier —
  # no need to know or hardcode how long "func " is.
  local offset
  if ! offset="$(offsetof -func "$target" "$funcname" 2>"$dir/offset.err")"; then
    warn "rename $funcname: not found — see $dir/offset.err"
    return
  fi

  if ! ( cd "$scratch" && gopls rename "$rel_file:#$offset" "$newname" ) \
      > "$dir/out.txt" 2>&1; then
    warn "rename $funcname: gopls rename failed — see $dir/out.txt"
    return
  fi

  diff -u "$SAMPLE_DIR/$rel_file" "$target" > "$dir/diff" || true
  if ( cd "$scratch" && go vet ./... ) > "$dir/vet.log" 2>&1; then
    echo "rename $funcname -> $newname: OK, go vet clean"
  else
    warn "rename $funcname: applied but go vet flagged something — see $dir/vet.log"
  fi
}

gopls_inline() {
  local rel_file="$1" search="$2" label="$3"
  local dir="$OUT_DIR/gopls/inline_$label"
  local target; target="$(gopls_scratch "$dir" "$rel_file")"
  local scratch="$dir/sample"

  local offset
  if ! offset="$(offsetof "$target" "$search" 2>"$dir/offset.err")"; then
    warn "inline $label: couldn't find '$search' — see $dir/offset.err"
    return
  fi

  if ! ( cd "$scratch" && gopls codeaction -exec -kind=refactor.inline.call -write "$rel_file:#$offset" ) \
      > "$dir/out.txt" 2>&1; then
    warn "inline $label: gopls codeaction inline failed — see $dir/out.txt"
    return
  fi

  diff -u "$SAMPLE_DIR/$rel_file" "$target" > "$dir/diff" || true
  if ( cd "$scratch" && go vet ./... ) > "$dir/vet.log" 2>&1; then
    echo "inline $label: OK, go vet clean"
  else
    warn "inline $label: applied but go vet flagged something — see $dir/vet.log"
  fi
}
