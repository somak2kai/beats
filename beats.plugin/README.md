# beats plugin

Structural fingerprinting and outlier analysis for Go codebases.

## What it does

beats analyses a Go repository at the structural level — not what your code
says, but *how* it's shaped. It tokenises every function into a
vocabulary-independent sequence (control flow, call types, return arity) and
clusters functions that share the same structural pattern regardless of naming
or domain.

Clusters represent *settled conventions* — recurring patterns the codebase has
organically converged on. Functions that came close to a cluster but did not
meet the threshold to join are *structural outliers*. These are the functions
worth reviewing: they look like they should follow a convention but deviate in
a specific, measurable way.

This plugin wires that pipeline into Claude so you can run it conversationally
and get a peer-grounded LLM analysis of every outlier:

- **Signal** — the exact structural delta (token, import, call, or cyclo) that
  separates the function from its closest cluster
- **Peer comparison** — Claude reads the actual bodies of cluster members to
  ground every verdict in real code, not abstract metrics
- **Verdict** — Needs Attention or Expected Variation, with a one-sentence
  explanation tied to the structural gap
- **HTML report** — an interactive browser report at `<repo>/.beats/report.html`
  with cluster exploration, outlier diffs, and package coverage charts

## Requirements

- The `beats` binary must be installed and on your PATH.
  Install with: `go install github.com/somak2kai/beats/cmd@latest`
- Go 1.21+
- The repository you want to analyse must be a Go module (has a `go.mod`).

## Skills

### `beats-analyze` — full mode

Trigger: `"run beats on <path>"`, `"beats init"`, `"fingerprint my Go repo"`, or `/beats`.

Runs the full pipeline:
1. Verifies `beats` is installed (offers to install if not)
2. `beats init --repo <path>` — indexes all functions and identifies structural clusters
3. `beats query outlier --format json` — retrieves structural outliers
4. `beats query cluster shape <hash>` — fetches peer function bodies for each outlier's closest cluster
5. LLM analysis grounded in peer bodies — classifies each outlier as Needs Attention or Expected Variation
6. `beats analyze --repo <path>` — generates the interactive HTML report

### `beats-analyze` — mini mode

Trigger: `"mini fingerprint"`, `"analyze fingerprint"`, `"fingerprint query"`, or `"beats query"`.

Assumes `beats` is already installed and the repo is already indexed. Skips
install verification, `beats init`, and HTML report generation. Runs only the
query + peer fetch + LLM analysis. Faster and lower token cost — useful for
re-analysing after a targeted `beats init` run you did yourself.

## What the analysis looks for

Each outlier is compared structurally to its closest cluster. The LLM asks one
question per signal dimension:

> Does the function body explain this deviation from its peers?

If yes → Expected Variation (intentional, domain-justified).
If no → Needs Attention (structural gap that peers don't share).

When all deltas are `none`, the LLM reads peer bodies directly and asks whether
the function is missing something peers consistently do — or does something they
consistently avoid.

The analysis does **not** do general code review. It only flags what can be
directly traced to a structural delta.

## Source

https://github.com/somak2kai/beats
