# beats-init

Structural fingerprinting and pattern labelling for Go repositories.

Runs the beats binary against a repo, reads the cluster output, labels every cluster
in-session using native reasoning, and writes the result to `.beats/` inside the repo.

---

## When to activate

User says any of: "run beats", "beats init", "beats this repo", "fingerprint this repo",
"index this repo", "analyse with beats", "what patterns does this repo use",
"beats <path>", "label the clusters"

---

## What you will do — in order, no skipping steps

1. Determine the target repo path
2. Run the beats binary
3. Read the generated cluster file from `.beats/`
4. Label every cluster (rules in this file — native reasoning, no extra tool calls)
5. Write the labelled file to `.beats/<name>_labelled.md`
6. Report a summary to the user

---

## Step 1 — Determine repo path

If the user provided a path, use it. Otherwise use the workspace folder (cwd).
Resolve to an absolute path. Extract the base name.

```
REPO = <absolute path>
BASE = basename(REPO)
```

---

## Step 2 — Run the binary

```bash
go run /Users/admin/ws/golang/beats/cmd/main.go --repo=<REPO>
```

When compiled and installed as a plugin binary, this becomes:
```bash
./bin/beats --repo=<REPO>
```

The binary will:
- Parse all Go files in the repo
- Build 16-token control-flow fingerprints for every function
- Cluster by structural shape + import coherence (Jaccard)
- Collapse shape-variant families (CollapseToFamilies runs in Go — do NOT re-detect families)
- Filter noise clusters via Labelable() thresholds
- Write `<REPO>/.beats/beats_label_<BASE>.md`
- Print `cluster file written to <path>` on success

Wait for the command to complete. If it errors, report the error and stop.

---

## Step 3 — Read the cluster file

Read `<REPO>/.beats/beats_label_<BASE>.md` in full.

Detect format version:
- **New format**: has `id:` field, `top_calls:`, percentile stats on `cyclo:` / `nesting:` / `calls:`,
  and `label: {}` placeholder on every cluster.
- **Old format**: no `id:`, no percentile lines, call targets under `calls:`, no `label: {}`.

---

## Step 4 — Label every cluster

This is a pattern recognition task. No extended thinking. No step-by-step reasoning.
Read the cluster, write the label, move on. Target: all clusters labelled in one pass.

**Family collapse is already done in Go.** Clusters with `shape_variant:` lines are already
merged families — do not re-detect families. Label them as the family they are.

### Field reference

```
cluster: N           — ordinal (may shift between runs — use id: for stable refs)
id: <hex>            — stable SHA-256 shape identity
size: N              — total functions including absorbed family members
coherence: X.XX      — mean pairwise Jaccard of DirectImports
shape: ...           — primary token sequence
shape_variant: ...   — absorbed family member shape (0 or more; family already merged)
rates: ctx=% err=% defer=% go=%   — feature rates (omitted if 0)
cyclo: min-max (mean M p75 A p95 B)
nesting: p50 A p75 B p95 C
calls: p50 A p75 B p95 C
early_returns: p50 A p75 B p95 C  (omitted if all zero)
defer_count: p50 A p75 B p95 C    (omitted if all zero)
imports: ...         — most frequent DirectImports
top_calls: ...       — most frequent call targets
representatives:
  pkg.Func  path/file.go:line
label: {}            — REPLACE THIS
```

### Labelling rules by coherence tier

**Coherence >= 0.80 with imports or top_calls present:**
Read imports + top_calls only. One representative is enough. Label immediately.

**Coherence 0.50–0.79:**
Read imports + top_calls + all three representatives. Label from structural evidence.

**Coherence < 0.50:**
Write `AMBIGUOUS — <one phrase: why the domain is mixed>`. Do not try harder.

**Shape variants present (family cluster):**
Label the family, not just the primary shape. Describe what all variants share.
Example: five btree operation variants → `interval btree node operation (template family)`

**Shape > 15 tokens:**
Ignore shape detail. Focus entirely on imports, top_calls, and representative names.
Functions with cyclo p95 >= 10 label as `multi-step <domain> operation` unless top_calls
are distinctive.

**No imports, no top_calls, coherence 1.0:**
Intra-package cluster. Use representative names + package to infer the pattern.

### Percentile hints — use when they sharpen the label

- `cyclo p95 >= 8` → consider "complex" or "multi-path"
- `calls p75 >= 8` → consider "high-fanout"
- `defer_count p50 >= 1` → pattern always uses defer; mention if distinctive
- `early_returns p50 >= 2` → guard-clause pattern
- `nesting p95 >= 4` → flag for complex clusters

Do not over-qualify. If coherence is high and imports are clear, short is better.

### Filters — apply silently before labelling

Output `label: SKIPPED` and count in summary:
- Clusters where coherence < 0.30 AND size < 10
- Clusters where shape length > 25 tokens AND cyclo p95 > 12 AND coherence < 0.70

### Label format rules

A good label:
- 4–8 words
- Describes what the construction *does*, not which repo it's from
- Specific enough to distinguish from other clusters in this file
- Derived from imports + top_calls, not function names (names are hints only)

Bad:  `error handling with early return`
Bad:  `utility function`
Good: `flatbuffers accessor with bounds check`
Good: `mutex-guarded metric read with defer unlock`
Good: `SDK field converter with nil-check chain`
Good: `interval btree node operation (template family)`
Good: `schema changer dep rule registration (versioned family)`
Good: `sqlstore single-record get with not-found mapping`

### Fast-path pattern recognition

Apply these before reading representatives — if the imports match, label immediately.

**Flatbuffers generated accessor:**
`github.com/google/flatbuffers/go` import
→ `flatbuffers table field accessor with bounds check`

**PDQSort / sort template instantiation:**
`sort` import only, representatives in `pdqsort_tmpl.go` / `pdqsort.eg.go` / `sort.eg.go`
→ `pdqsort template — <function name> variant`

**Interval btree template instantiation:**
No or minimal imports, representatives in `interval_btree_tmpl.go` / `example_interval_btree.go`
→ `interval btree <operation> (template family)`

**Schema changer dep rule registration:**
`scgraph` + `scpb` + `rel` imports, `shape_variant:` lines, representatives in `scplan/internal/rules/`
→ `schema changer dep rule registration (versioned family)`

**Schema changer opgen init:**
`scpb` + `scop` imports, `CALL CALL CALL FUNCLIT RETURN` shape, representatives in `opgen/opgen_*.go`
→ `schema changer opgen element state init`

**Staticcheck analyzer init:**
`staticcheck` / `honnef.co/go/tools` imports, `RANGE IF BREAK CALL` shape
→ `staticcheck analyzer initialiser`

**Kafka SASL / franz-go:**
`twmb/franz-go` imports, `CALL FUNCLIT RETURN RETURN CALL` shape
→ `Kafka SASL auth option builder`

**Mutex-guarded reads/writes:**
`defer=100%`, shape `CALL DEFER CALL RETURN` or `CALL DEFER CALL CALL`
→ `mutex-guarded read with defer unlock` / `mutex-guarded write with defer unlock`

**SDK converters (go-genai style):**
Intra-package, coherence 1.0, `ToMldev` / `ToVertex` representative names
→ `SDK object field converter with nil-check chain`

**RoachTest test registration:**
`roachtest/registry` + `roachtest/spec` imports, `CALL CALL CALL FUNCLIT CALL` shape
→ `roachtest suite registration with spec`

**gomock stub:**
`github.com/golang/mock/gomock` + `reflect` imports, `CALL RETURN CALL CALL CALL` shape
→ `gomock mock method stub with reflect dispatch`

---

## Step 5 — Write output

**New format:** replace every `label: {}` with the real label in-place.

**Old format:** append `label: <value>` after the `representatives:` block of each cluster.

Write the complete labelled file to:
```
<REPO>/.beats/beats_label_<BASE>_labelled.md
```

Add this line at the end of the file:
```
skipped: <N> clusters (low coherence or overfit shape)
```

---

## Step 6 — Report to user

Print a brief summary (prose, no bullet overload):

- Total clusters, labelled count, AMBIGUOUS count, SKIPPED count
- The 3–5 most structurally interesting labels found (largest families, highest coherence, or novel patterns)
- Full path to the labelled file as a `computer://` link
- One-sentence read on what the label distribution says about this codebase

Example:

> Ran beats on linkerd2: 9 clusters, 9 labelled, 0 ambiguous, 0 skipped.
> Dominant pattern: Kubernetes typed REST client factory (clusters 1–3, all coherence > 0.63).
> Also found: RBAC resource lister, healthcheck validator, YAML fake factory.
> The 100% label rate and narrow import vocabulary confirm this is a single-domain infrastructure repo.
>
> [View labelled clusters](computer:///path/to/.beats/beats_label_linkerd2_labelled.md)
