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
meet the threshold to join are *structural outliers*: they look like they should
follow a convention but deviate in a specific, measurable way.

**Potential outliers** are functions with a structural delta against their
closest cluster — a missing token type, a dropped error check, an absent
`defer`, a call target peers share that this function doesn't have. Most turn
out to be intentional (different domain, different contract). A small number are
genuine structural gaps: a peer pattern the function should follow but doesn't.
beats surfaces both and lets Claude decide which is which.

After indexing, beats writes `<repo>/.beats/outlier.md` — a pre-computed,
self-contained document with every outlier, its structural deltas, and the full
bodies of its closest cluster's peer functions. The Claude skill reads this file
directly so analysis runs without any additional queries.

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

## Using with Claude

### Install the plugin

```
/plugin install beats@beats
```

Or load directly from a local clone:

```bash
claude --plugin-dir /path/to/beats/beats.plugin
```

### Run a full analysis

Once the plugin is installed, tell Claude:

> `run beats on /path/to/your/go/repo`

Claude will:
1. Check that `beats` is installed (and offer to install it if not)
2. Run `beats init --repo <path>` to index every function and build structural clusters
3. Read `<repo>/.beats/outlier.md` — the pre-computed outlier document written by init
4. Triage every outlier against its closest cluster's peer bodies
5. Output **Needs Attention** and **Expected Variation** sections
6. Generate the interactive HTML report at `<repo>/.beats/report.html`

The `outlier.md` file is written during `beats init`, so the LLM analysis step
reads a single file rather than issuing repeated database queries. This keeps the
analysis fast and the token cost predictable regardless of outlier count.

### Re-analyse without re-indexing

If the repo is already indexed and you just want to re-run the LLM triage:

> `mini fingerprint`

Claude skips install verification and `beats init`, reads `outlier.md` directly,
and outputs the triage. Useful after reviewing findings or when iterating on the
skill itself.

---

## Skills

### `beats-analyze` — full mode

Trigger: `"run beats on <path>"`, `"beats init"`, `"fingerprint my Go repo"`, or `/beats`.

Runs the full pipeline:
1. Verifies `beats` is installed (offers to install if not)
2. `beats init --repo <path>` — indexes all functions, identifies structural clusters, writes `outlier.md`
3. Reads `<repo>/.beats/outlier.md` — pre-computed outlier document with all deltas and peer bodies
4. LLM triage grounded in peer bodies — classifies each outlier as Needs Attention or Expected Variation
5. `beats analyze --repo <path>` — generates the interactive HTML report

### `beats-analyze` — mini mode

Trigger: `"mini fingerprint"`, `"analyze fingerprint"`, `"fingerprint query"`, or `"beats query"`.

Assumes `beats` is already installed and the repo is already indexed. Skips
install verification and `beats init`. Reads `outlier.md` directly and runs LLM
triage. Faster and lower token cost — useful for re-analysing after a targeted
`beats init` run you did yourself.

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
