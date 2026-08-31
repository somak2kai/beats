# scripts/mutation

A sandbox for seeing how `mutago` and `gopls` actually behave on Go code,
before deciding whether either belongs in the real ground-truth pipeline
discussed alongside this.

## Contents

- `sample/` — 20 self-contained Go functions with tests, own `go.mod`
  (kept independent of the main `beats` module on purpose):
  - `functions.go` — the original 10 small utility functions (`Max`,
    `Min`, `Clamp`, `IsEven`, `IsPositive`, `Sum`, `Contains`, `Reverse`,
    `CountVowels`, `Filter`, `SafeDivide`).
  - `enterprise.go` — 10 more realistic, enterprise-style functions
    (`ValidateEmail`, `MaskSensitive`, `Deduplicate`, `ChunkSlice`,
    `Paginate`, `MergeConfigs`, `TruncateWithEllipsis`,
    `ValidatePasswordStrength`, `RetryWithBackoff`,
    `CircuitBreakerAllow`), plus three small helpers
    (`hasUpper`/`hasDigit`/`hasSpecial`) factored out specifically so
    there's a real call site for gopls's inline-call demo to work on.
- `tools/offsetof/` — a tiny standalone Go program that prints the exact
  byte offset of a substring in a file, for gopls's `file:#offset` CLI
  syntax. A real Go program rather than shell/awk byte-counting on
  purpose — offsets need to be byte-exact, and shell portability across
  macOS/Linux has already bitten this script twice.
- `install.sh` — installs `mutago` and `gopls`. Nothing else.
- `run.sh` — thin orchestrator: sources `lib/`, runs a sanity check, then
  both tools, output to `out/` (gitignored).
- `lib/common.sh` — shared paths (`SAMPLE_DIR`, `OUT_DIR`) and helpers
  (`log`, `warn`, `offsetof`, `sample_sanity_check`).
- `lib/mutago.sh` — `run_mutago`: runs mutago, reconstructs escaped
  mutants into real `.go` files, `go vet`s each.
- `lib/gopls.sh` — `run_gopls`: runs the rename + inline-call demos below.

## Quick start

```bash
cd scripts/mutation
./install.sh
./run.sh
```

## What each tool is for, and why the others got dropped

**mutago** — mutation testing. Small, single-point structural edits
(comparison/arithmetic operator swaps, branch/statement removal). Real
unified diffs for escaped mutants get reconstructed into actual `.go`
files and `go vet`-ed. See install.sh's header for the full story of why
gremlins and go-mutesting were tried first and dropped (gremlins never
persists a mutant's source; go-mutesting is dead — panics on modern Go).

**gopls** — the official Go language server, used strictly through its
headless CLI, not as an editor backend:
- `gopls rename` — safe, scope/type-aware identifier renaming. Tier-1
  material: renames don't touch behavior or (much) AST shape.
- `gopls codeaction -exec -kind=refactor.inline.call` — inlines a
  function call. The Go team built this inliner specifically to preserve
  evaluation order, side effects, `defer` semantics, and scoping —
  genuine, deterministic, behavior-preserving heavy restructuring.
  Tier-2 material, and better-grounded than anything else tried (garble
  can't emit source at all; an LLM rewrite carries its own style
  fingerprint).

garble and OpenRewrite were also considered for Tier 2 and ruled out:
garble's control-flow flattening only ever produces a compiled binary,
never rewritten source, so there's nothing to diff or vet; OpenRewrite has
no Go support at all (confirmed against its official supported-languages
docs — Java/Kotlin/Groovy plus config formats only, and Go isn't even on
Moderne's paid extended-language list).

Both demos have been confirmed against a real run: `gopls rename` and
`gopls codeaction -exec -kind=refactor.inline.call` both work exactly as
documented, across 8 rename targets and all 3 real inline call sites in
`sample/`. `lib/gopls.sh` still captures each target's output/diff/vet
log under its own `out/gopls/<kind>_<label>/` regardless of success or
failure, so a bad target is visible and fixable rather than silently
swallowed.

Renaming a target needs no extra knowledge beyond the function's own
name — that's always known. Inlining needs one mechanical precondition:
an existing call to a local function. That's why `enterprise.go` has
`hasUpper`/`hasDigit`/`hasSpecial` factored out — they're the only real
call sites in `sample/` today. Finding such call sites in an arbitrary
file is itself mechanical (a `go/ast` walk for call expressions to
package-local functions), just not built here yet.

## Why this exists

Follows from the mutation-generator discussion for the ground-truth work:
mechanical/tool-based mutation is a deterministic, zero-LLM alternative
for building the graded mutation tiers. This script is "install, run on
sample functions, see what actually comes out" — not yet wired into the
real corpus.
