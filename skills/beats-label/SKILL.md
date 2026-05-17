# beats-label skill

Fast structural pattern labelling for beats cluster files.
Input: a `beats_label_*.md` file. Output: the same file with every `label: {}` replaced by a real label.

This is a pattern recognition task. No extended thinking. No step-by-step reasoning.
Read the cluster, write the label, move on. Target: all clusters labelled in one pass.

**Family collapse is already done in Go.** Clusters with `shape_variant:` lines are already
merged families — do not re-detect families. Label them as the family they are.

---

## New file format — field reference

```
cluster: N           — ordinal (position may shift between runs, do not rely on it)
id: <hex>            — stable SHA-256 shape identity (use this for cross-run references)
size: N              — total functions in cluster (including absorbed family members)
coherence: X.XX      — mean pairwise Jaccard of DirectImports
shape: ...           — primary token sequence
shape_variant: ...   — absorbed family member shape (0 or more lines; family already merged)
rates: ctx=% err=% defer=% go=%   — fraction of members with these features (omitted if 0)
cyclo: min-max (mean M p75 A p95 B)   — cyclomatic complexity distribution
nesting: p50 A p75 B p95 C            — nesting depth percentiles
calls: p50 A p75 B p95 C              — outbound call count percentiles
early_returns: p50 A p75 B p95 C      — early return count percentiles (omitted if all zero)
defer_count: p50 A p75 B p95 C        — defer count percentiles (omitted if all zero)
imports: ...         — most frequent DirectImports across members
top_calls: ...       — most frequent call targets across members
representatives:
  pkg.Func  path/file.go:line
label: {}            — REPLACE THIS
```

---

## Labelling rules by coherence tier

**Coherence >= 0.80 with imports or top_calls present:**
Read imports + top_calls only. One representative is enough. Label immediately.

**Coherence 0.50–0.79:**
Read imports + top_calls + all three representatives. Label from structural evidence.

**Coherence < 0.50:**
Write `AMBIGUOUS — <one phrase: why the domain is mixed>`. Do not try harder.

**Shape variants present (family cluster):**
The cluster already represents N merged patterns. Label the family, not just the primary shape.
A family label should describe what all variants share, not just the primary shape.
Example: five btree operation variants → `interval btree node operation (template family)`

**Shape > 15 tokens:**
Ignore shape detail. Focus entirely on imports, top_calls, and representative names.
Functions with cyclo p95 >= 10 label as `multi-step <domain> operation` unless top_calls are distinctive.

**No imports, no top_calls, coherence 1.0:**
Intra-package cluster. Use representative names + package to infer the pattern.
`package.FunctionName` naming convention is usually enough.

---

## Using percentile fields to sharpen labels

These are hints, not required — use when they make the label more precise:

- `cyclo p95 >= 8` → consider adding "complex" or "multi-path" to the label
- `calls p75 >= 8` → consider "high-fanout" qualifier
- `defer_count p50 >= 1` → the pattern always uses defer; mention it if distinctive
- `early_returns p50 >= 2` → guard-clause pattern; mention if the shape doesn't already show it
- `nesting p95 >= 4` → deep nesting is unusual; flag it for complex clusters

Do not over-qualify simple clusters. If coherence is high and the imports are clear,
a short label is better than an annotated one.

---

## Label format rules

A good label:
- 4–8 words
- Describes what the construction *does*, not which repo it comes from
- Is specific enough to distinguish from other clusters in the same file
- Comes from imports + top_calls, not function names (function names are hints only)

Bad:  `error handling with early return`
Bad:  `utility function`
Good: `flatbuffers accessor with bounds check`
Good: `mutex-guarded metric read with defer unlock`
Good: `SDK field converter with nil-check chain`
Good: `interval btree node operation (template family)`
Good: `schema changer dep rule registration (versioned family)`
Good: `pdqsort template instantiation — insertion sort variant`

---

## Output format

Respond with the complete annotated cluster list.
For each cluster, replace `label: {}` with:

```
label: <your label>
```

For ambiguous clusters:

```
label: AMBIGUOUS — <reason in one phrase>
```

Do not change any other part of the file. Do not add explanation or preamble.
Start with cluster 1. Write labels only. No thinking out loud.

At the end of your output, add one summary line:
`skipped: <N> clusters (low coherence or overfit shape)`

---

## Filters — apply silently before labelling

Skip entirely (output `label: SKIPPED` and count in summary):

- Clusters where coherence < 0.30 AND size < 10
- Clusters where shape length > 25 tokens AND cyclo p95 > 12 AND coherence < 0.70

---

## Common patterns — fast-path recognition

**PDQSort / sort template instantiation:**
`sort` import only, representatives in `pdqsort_tmpl.go` / `pdqsort.eg.go` / `sort.eg.go`
→ `pdqsort template — <function name> variant` (e.g., `pdqsort template — insertion sort`)

**Interval btree template instantiation:**
No imports or minimal, representatives in `interval_btree_tmpl.go` / `btreefrontierentry_interval_btree.go` / `example_interval_btree.go`
→ `interval btree <operation> (template family)` (e.g., `interval btree node delete`)

**Schema changer dep rule registration:**
`scgraph` + `scpb` + `rel` imports, `shape_variant:` lines, `CALL FUNCLIT RETURN` chains,
representatives in `scplan/internal/rules/release_*/dep_*.go`
→ `schema changer dep rule registration (versioned family)`

**Schema changer opgen init:**
`scpb` + `scop` imports, `CALL CALL CALL FUNCLIT RETURN` shape, representatives in `opgen/opgen_*.go`
→ `schema changer opgen element state init`

**Staticcheck analyzer init:**
`staticcheck` / `honnef.co/go/tools` imports, `RANGE IF BREAK CALL` shape
→ `staticcheck analyzer initialiser`

**Flatbuffers generated accessor:**
`github.com/google/flatbuffers/go` import
→ `flatbuffers table field accessor with bounds check`

**Kafka SASL / franz-go:**
`twmb/franz-go` imports, `CALL FUNCLIT RETURN RETURN CALL` shape
→ `Kafka SASL auth option builder`

**SDK converters (go-genai style):**
Intra-package, coherence 1.0, `ToMldev`/`ToVertex` names
→ `SDK object field converter with nil-check chain`

**Mutex-guarded reads/writes:**
`defer=100%`, shape `CALL DEFER CALL RETURN` or `CALL DEFER CALL CALL`
→ `mutex-guarded read with defer unlock` / `mutex-guarded write with defer unlock`

**RoachTest test registration:**
`roachtest/registry` + `roachtest/spec` imports, `CALL CALL CALL FUNCLIT CALL` shape
→ `roachtest suite registration with spec`

**RoachProd centralized admin controller:**
`gin-gonic/gin` + `roachprod-centralized/controllers` imports
→ `roachprod-centralized admin <resource> <action> handler`
