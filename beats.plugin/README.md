# beats plugin

Structural fingerprinting and LLM-enriched cluster analysis for Go codebases.

## What it does

beats analyses a Go repository at the structural level — not what your code
says, but *how* it's shaped. It tokenises every function into a
vocabulary-independent sequence (control flow, call types, return arity) and
clusters functions that share the same structural pattern regardless of naming
or domain.

This plugin wires that pipeline into Claude so you can run it conversationally
and get each cluster automatically enriched with:

- **Idiom name** — a 3–6 word label for the structural convention
- **Verdict** — one sentence on what the cluster represents
- **Canonical member** — the best example to point a new engineer at
- **Suggested action** — "none" or a specific refactoring note
- **Confidence** — high / medium / low
- **Search questions** — 5–8 natural-language questions that map to this pattern
  (used for semantic structural search in coding agents)

The result is an interactive HTML report at `<repo>/.beats/report.html`.

## Requirements

- The `beats` binary must be installed and on your PATH.
  Install with: `go install github.com/somak2kai/beats/cmd@latest`
- Go 1.21+
- The repository you want to analyse must be a Go module (has a `go.mod`).

## Skills

### `beats-analyze`

Trigger: say `"run beats on <path>"`, `"fingerprint my Go repo"`, or `/beats-analyze`.

Runs the full pipeline:
1. Verifies `beats` is installed (offers to install if not)
2. `beats init --repo <path>` — indexes and clusters all functions
3. Pages through clusters via `beats query --format compact`
4. Reads representative source files and generates enrichment per cluster
5. `beats update cluster <idx>` — writes metadata back to the database
6. `beats analyze --repo <path>` — regenerates the HTML report with all enrichment

## Source

https://github.com/somak2kai/beats
