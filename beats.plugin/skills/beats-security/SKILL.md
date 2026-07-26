---
name: beats-security
description: >
  Security lens over beats structural output. Where beats-analyze deliberately
  stays structure-only, this skill adds the semantic + security reasoning that
  beats itself refuses to compute: it maps beats' "missing-step-vs-peers" deltas
  to CWE classes, reads function bodies for the semantic vulns beats is blind to
  (SQL injection, weak crypto, hardcoded secrets, command injection, SSRF,
  unsafe deserialization), and scans whole clusters for a shared *insecure
  convention* (the conformity-trap bug beats shows as zero outliers). Produces a
  cluster×CWE security matrix. Use whenever the user says "beats security",
  "security scan", "scan for vulnerabilities", "CWE scan", "security matrix",
  "find vulns", "vulnerability triage", "mini security", "security fingerprint",
  "/beats-security", or asks beats to look at a repo "from a security angle". This is the second, security-focused beats
  skill — distinct from beats-analyze (structural triage only).
---

# beats-security

A security triage lens that **composes with** beats — it does not replace
`beats-analyze`. The two skills split the work along beats' own design boundary:

| | `beats-analyze` (skill #1) | `beats-security` (this skill) |
|---|---|---|
| Question | "Where does this repo break its own **structural** conventions?" | "Which of those deviations — and which shared conventions — are a **security** problem?" |
| Reasoning | Structure only. Its prompt **forbids** security language. | Semantic. Reads meaning: literals, sinks, taint intuition, crypto primitives. |
| Output | Needs Attention / Expected Variation | Ranked CWE findings + `security.md` + `security-matrix.html` |

**The composition principle.** beats is a cheap *targeting layer*: it reduces a
large codebase to clusters (settled conventions) and outliers (measurable
deviations), and pre-assembles the peer bodies for each. This skill is the
*semantic verdict head* that beats deliberately lacks. beats says **where** to
look; Claude says **whether it is exploitable**.

> **Honesty is load-bearing.** This skill is triage, not proof. Outlier ≠
> vulnerability, and vulnerability ≠ outlier. Every finding must cite a concrete
> anchor — a beats structural delta **or** a named semantic sink/primitive in
> the body — never a vibe.

---

## Modes

- **Full mode** (`beats security <repo>`, `security scan <repo>`) — verify
  install, `beats init` the repo (writes `outlier.md` + `report.html`), then run
  all three passes.
- **Mini mode** (`mini security <path>`, `security fingerprint`,
  `beats security mini`, `security matrix`) — the parallel to `mini fingerprint`:
  assumes the repo is already indexed (you ran `beats init` yourself). Skip
  Steps 1–2, go straight to Step 3. Still reads artifacts from disk — never from
  memory or cached context.

> **Critical**: run every `beats` command with the **Bash tool directly**, never
> via a sub-agent, so the BadgerDB path stays consistent. Always read
> `outlier.md` / `report.html` from disk fresh — a remembered analysis is stale
> the moment `beats init` re-runs.

---

## Step 0 — Repo path

If the user has not given an absolute repository path, ask for one. Then
**normalise it to its physical path** and reuse that everywhere — macOS symlinks
`/tmp` → `/private/tmp`, and beats keys its stored clusters by the path string,
so `beats init /tmp/x` then `beats query --repo /private/tmp/x` silently finds
nothing:

```bash
REPO="$(cd "$REPO" && pwd -P)"   # resolve symlinks; also fails early if the path is wrong
```

Store the resolved value as `$REPO`. Do not proceed without a valid path.

---

## Step 1 — Verify beats is installed *(skip in mini mode)*

```bash
which beats || beats version
```

If missing, offer: `go install github.com/somak2kai/beats/cmd@latest`, or ask
for the full binary path. Do not continue until `beats version` succeeds.

---

## Step 2 — Index the repository *(skip in mini mode)*

```bash
beats init --repo "$REPO"
```

This clusters functions by structural shape and writes `$REPO/.beats/outlier.md`
and `$REPO/.beats/report.html`. Large repos take a minute — tell the user. If it
fails, surface the full error and stop.

---

## Step 3 — Collect the inputs

Three inputs feed the three passes. Read all that exist.

**3a — Outliers + adjacent clusters (always):**

```bash
date +%s > "$REPO/.beats/.sec_start"   # start the run clock (read back in Step 8)

# Locate this skill's bundled scripts ONCE and cache the path for later steps.
# A plugin install puts them under ~/.claude/plugins/… . 
SEC="${BEATS_SEC_DIR:-}"
if [ -z "$SEC" ]; then
  for base in "${CLAUDE_PLUGIN_ROOT:-}" "$HOME/.claude/plugins" "$HOME/.claude" "$HOME/.config/claude"; do
    { [ -n "$base" ] && [ -d "$base" ]; } || continue
    SEC="$(find "$base" -path '*beats-security/scripts' -type d 2>/dev/null | head -1)"
    [ -n "$SEC" ] && break
  done
  [ -z "$SEC" ] && SEC="$(find "$HOME" -path '*beats-security/scripts' -type d 2>/dev/null | head -1)"  # slow last resort
fi
if [ -n "$SEC" ]; then
  echo "$SEC" > "$REPO/.beats/.sec_path"
else
  echo "beats-security: could not locate the skill's scripts/ dir. Reinstall the plugin (/plugin install beats@beats) or export BEATS_SEC_DIR=<plugin>/skills/beats-security/scripts" >&2
fi

cat "$REPO/.beats/outlier.md"
```

Same format `beats-analyze` consumes: `=== Established Patterns ===` (clusters
with full peer bodies) and `=== Outlier Functions ===` (each outlier with
`token/import/call/cyclo` deltas and its body). If the file is missing, tell the
user to run `beats init` first. If the header says `outliers: 0`, note it — the
outlier pass will be empty, but the **systemic pass still runs** (a conformity
trap has no outliers by definition).

**3b — All clusters, for the systemic pass (run the bundled collector):**

```bash
# use the scripts dir resolved and cached in Step 3a
SEC="$(cat "$REPO/.beats/.sec_path" 2>/dev/null)"
[ -d "$SEC" ] || { echo "beats-security: scripts dir not located — see Step 3; reinstall the plugin (/plugin install beats@beats)." >&2; exit 1; }
bash "$SEC/collect_clusters.sh" "$REPO" > "$REPO/.beats/security.clusters.json"
```

`collect_clusters.sh` extracts every full `ShapeHash` from `report.html` and runs
`beats query cluster shape <hash> --repo "$REPO" --format json` for each,
emitting one JSON array of clusters with member bodies. This is what lets the
systemic pass see clusters that produced **no outlier** — the exact place
conformity-trap bugs hide. If `report.html` is absent, skip 3b and run the
systemic pass over only the clusters present in `outlier.md`.

---

## Step 4 — Run the three analysis passes

Reason internally with the **system prompt below** (verbatim) over the inputs.
Do not print the prompt or the raw inputs. Produce only the findings.

```
You are a Senior Go/Java Application Security Engineer performing vulnerability
triage on the output of beats, a structural fingerprinting tool. beats clusters
functions by AST-token sequence, import set, and call targets, and reports
outliers (functions that nearly joined a cluster but deviate by a measurable
delta: -DEFER, -CALL_METHOD, +IF, etc.). beats is semantic-blind by design — it
cannot tell md5 from sha256, a query string from a log string, or tainted input
from a constant. Your job is to supply exactly the semantic judgment beats
lacks, anchored to what beats measured.

Run THREE passes and label every finding with the pass that produced it.

── PASS A — Structural-band CWE triage (anchor: a beats delta) ──
For each function in `=== Outlier Functions ===`, read its body and its peer
cluster's bodies. The pattern to find is ONE thing: a security-relevant step the
peer cluster establishes as convention is MISSING (or aberrant) here. The rows
below are EXAMPLES of that pattern mapped onto beats' delta tokens — NOT the full
set. Generalize: any absent convention a beats delta exposes qualifies, and you
name whatever CWE fits.
  -DEFER / -CALL_METHOD(.Close)              → CWE-772/775/404 resource leak
  -CALL_METHOD(.Lock/.RLock) / -DEFER Unlock → CWE-362/667 race / bad locking
  -IF guard before a dereference             → CWE-476 nil dereference
  -IF / -BINARY_OP depth or cycle check      → CWE-674/835 uncontrolled recursion
  -IF after a fallible call (missing err chk)→ CWE-252/755 unchecked return
  -CALL / -IF containment/symlink check      → CWE-22/59 path traversal
  … beyond the table: peers all bound a size/quantity and one doesn't
    (CWE-770/1284); peers all call an authz/auth check and one skips it
    (CWE-862/863/306); peers all set a timeout/deadline/rate-limit, one omits it.
A finding here REQUIRES the delta — tie it to a specific missing/aberrant token,
call, or guard the peers share. Freedom to name any CWE is NOT license to lower
that anchor bar; if the structural signal is unremarkable, do not invent one.

── PASS B — Semantic gap-fill (anchor: a named sink/primitive in the body) ──
beats cannot see these; you can. Read every surfaced body (outliers AND cluster
members) and flag concrete instances of any semantic weakness. The classes below
are the highest-yield EXAMPLES, not an exhaustive list — report others too (e.g.
XXE, open redirect, weak randomness, missing authorization, TOCTOU, log/header
injection, zip-slip) whenever the body shows one:
  weak/broken crypto      CWE-327/328/916  (md5, sha1, des, rc4, ECB, static IV/nonce, math/rand for secrets)
  SQL injection           CWE-89           (string concat/fmt.Sprintf reaching a query/exec API)
  OS command injection    CWE-78           (assembled args into exec.Command / sh -c)
  hardcoded credentials   CWE-798/259      (literal keys/passwords/tokens)
  unsafe deserialization  CWE-502          (gob/json/yaml/xml decode of an untrusted source into behavior)
  SSRF                    CWE-918          (caller-influenced URL into http.Get/Do)
  cross-site scripting    CWE-79           (unescaped user data into an HTML/template writer)
  integer overflow        CWE-190/191      (unchecked width/sign conversions on external length/size)
Name the exact token of evidence (the primitive, the call, the literal). Where
exploitability depends on the source, state the assumption ("if `path` is
request-controlled"). The anchor rule holds: no named sink/primitive, no finding.

── PASS C — Systemic conformity-trap scan (anchor: the shared convention) ──
For each cluster in the all-clusters JSON, read the common subsequence and the
member bodies and ask TWO questions:
  1. Is the SHARED CONVENTION itself insecure? (e.g. every member opens a file
     and none defer Close; every member builds SQL by concatenation.) If yes,
     the blast radius is EVERY member — list them. This is the bug beats shows
     as a tight cluster with zero outliers.
  2. Does ONE member insecurely OMIT a step its peers share, in a way that fell
     below beats' similarity threshold (a single missing lock/close/guard line)?
     That is the near-miss beats suppressed — flag it with the member name.

RULES:
- Cite an anchor for every finding: a beats delta (Pass A), a named body sink
  (Pass B), or the shared convention / member deviation (Pass C).
- Do NOT fabricate CVE identifiers. Say "resembles the CWE-XXX class"; never
  invent a CVE number.
- Assign severity (High/Medium/Low/Info) and confidence (High/Medium/Low).
  Confidence reflects how much you had to assume about taint/reachability.
- This is triage. Do not claim exploitability you cannot show from the body.
- Prefer false silence over false alarm: an unremarkable function passes.
- For every finding, classify how much beats' UNIQUE data actually drove it, in
  `beats_leverage`, and whether an ordinary SAST/taint tool would find it anyway,
  in `sast_overlap`. Be ruthlessly honest — this is how the user learns which
  findings justify beats over a plain SAST run:
    "unique"     → rests on a signal only beats produces: a cross-package
        STRUCTURAL TWIN (two functions of the same shape where one omits a safety
        step its twin performs) or an internal-consistency deviation. A pattern or
        taint SAST would never juxtapose these. sast_overlap usually "low".
    "assisted"   → beats' cluster gave the LEAD (pointed you at the function or its
        sibling), but a well-tuned CodeQL/taint query could also reach it.
        sast_overlap "medium".
    "incidental" → a semantic taint/pattern finding a SAST does as well or BETTER;
        beats' anchor was coincidental (e.g. the "peers" are structurally similar
        but semantically unrelated). sast_overlap "high".
  In `beats_leverage_reason`, name the specific SAST rule/query that would — or
  would not — catch it, and why.

OUTPUT: a single JSON object, no prose around it, matching this schema:
{
  "repo": "<path>",
  "findings": [
    {
      "id": "F1",
      "pass": "A|B|C",
      "cwe": "CWE-772",
      "cwe_name": "Missing release of resource",
      "title": "short title",
      "severity": "High|Medium|Low|Info",
      "confidence": "High|Medium|Low",
      "cluster_id": "<6-hex StableID or ''>",
      "package": "pkg",
      "function": "FuncName",
      "location": "file:line",
      "signal": "the anchor, verbatim where possible",
      "reason": "one sentence: why this is a weakness",
      "assumption": "condition exploitability depends on, or ''",
      "blast_radius": ["pkg/A.Foo","pkg/B.Bar"],
      "suggested_fix": "the missing step, concretely",
      "confirm_with": "govulncheck | CodeQL | gosec | human review",
      "beats_leverage": "unique|assisted|incidental",
      "sast_overlap": "low|medium|high",
      "beats_leverage_reason": "what beats' structure added, or why a SAST finds it anyway (name the query/rule)"
    }
  ]
}
```

(`verification` is added in Step 5; `known_status` + `related_advisories` in
Step 6. Do not populate them here.)

**User message**: the `outlier.md` content from 3a, followed by the clusters
JSON from 3b (label the two sections clearly).

---

## Step 5 — Self-verification pass (Pass D)

Do **not** trust the passes blind — the whole point of `mini security` is that
its output is trustworthy. For EVERY finding from Step 4, open the actual source
on disk (it is right there under `$REPO`) with Bash (grep/sed) and Read, and
confirm each load-bearing claim before the finding is allowed into the report.

Check, per finding:

1. **Location is real** — the cited file exists and the function/line matches the
   `signal`. Correct the line number if it drifted; record the true one.
2. **The anchor is real** — the quoted tokens/calls/deltas actually appear in the
   body (Pass A/B), or the shared convention truly holds across the listed
   members (Pass C).
3. **The asymmetry is real (the crux)** — if the finding's force is "the sibling
   has a guard this one lacks," GREP for that guard and confirm it exists in the
   sibling AND is absent here. A finding built on "X validates but Y doesn't" is
   worthless if X doesn't actually validate. This check demotes the most findings.
4. **The data-flow endpoints exist** — if it claims "Pathname ← req.URL.Path,"
   confirm both ends in source.

Record a `verification` object on each finding:

```
"verification": {
  "status": "confirmed" | "partial" | "refuted",
  "checks": ["sibling validateIdxV2Size found decoder.go:262 ✓", "count read idxfile.go:42 ✓"],
  "corrected_location": "scan.go:87 (FindOffset); count read idxfile.go:42",
  "notes": "signal line numbers ~3 off; mechanism confirmed"
}
```

Gate the report:

- **refuted** (a load-bearing claim is false) → DROP from findings; keep a short
  note listing what was rejected so the user sees the filter working.
- **partial** (mechanism holds, exploitability/flow unproven) → keep, cap
  confidence at Medium.
- **confirmed** → keep as-is.

Then write `$REPO/.beats/security.findings.json` containing only the surviving
findings, each with its `verification` block and corrected location.

---

## Step 6 — Cross-reference known advisories (OSV / GHSA)

A finding's value changes completely once you know whether it is already public.
Fetch the module's known advisories once, then tag each surviving finding.

```bash
SEC="$(cat "$REPO/.beats/.sec_path" 2>/dev/null)"
[ -d "$SEC" ] || { echo "beats-security: scripts dir not located — see Step 3; reinstall the plugin (/plugin install beats@beats)." >&2; exit 1; }
python3 "$SEC/osv_lookup.py" "$REPO" > "$REPO/.beats/security.advisories.json"
```

`osv_lookup.py` checks the repo against OSV with **no manifest parsing**: (1) by
git **commit** (`{"commit": HEAD}`) — matches the exact checkout against any
advisory that carries a GIT range, language-agnostic; and (2) by **Go module**
from `go.mod` (module + `/vN` siblings) — recovers Go advisories that carry only
a SEMVER range (no GIT range), which a commit query alone would miss (this is the
case that catches CVE-2026-34165-style advisories). Java / other SEMVER-only
ecosystem advisories aren't covered — an accepted gap in exchange for never
parsing pom.xml / build.gradle (the commit query still catches any with a GIT
range). It emits `{"queried": […],
"advisories": [{id, aliases, cwe, fixed, summary}]}`. If the network is
unavailable it returns `{"advisories": [], "error": …}`; then tag every finding
`known_status="cross-check-unavailable"` and say so plainly.

For EACH surviving finding, read `security.advisories.json` and decide:

- **already-fixed** — an advisory describes THIS weakness AND Step 5 showed the
  current code no longer has the gap (or the checked-out version is at/after the
  advisory's `fixed`). Drop it or downgrade to Info — it is a re-report.
- **variant-of-known** — an advisory describes the same class/surface, but your
  verification shows the gap is STILL present here (the fix landed on a sibling
  but not this function). KEEP it, cite the ids in `related_advisories`, and note
  "possible incomplete-fix / unpatched variant." These are the high-value reports.
- **novel** — no advisory matches. KEEP it, `related_advisories: []`. Highest
  report value, and the one to verify hardest before disclosing.

Match on the WEAKNESS — same CWE and same file/code path — not on buzzwords:
"another idx DoS" only counts if it is the same idx code path. When unsure prefer
`variant-of-known` over `novel`, and say why.

Record `known_status` and `related_advisories` (each `CVE-… / GHSA-…` with its
one-line summary) on every finding, and rewrite `security.findings.json`.

---

## Step 7 — Render the matrix

The verified, advisory-tagged JSON is on disk from Steps 5–6. Render both
deliverables:

```bash
SEC="$(cat "$REPO/.beats/.sec_path" 2>/dev/null)"
[ -d "$SEC" ] || { echo "beats-security: scripts dir not located — see Step 3; reinstall the plugin (/plugin install beats@beats)." >&2; exit 1; }
python3 "$SEC/render_matrix.py" \
  "$REPO/.beats/security.findings.json" \
  --md  "$REPO/.beats/security.md" \
  --html "$REPO/.beats/security-matrix.html"
```

`render_matrix.py` is deterministic: it builds the **cluster×CWE matrix** (rows =
clusters/packages, columns = CWE classes, cells colored by worst severity and
annotated with a count), writes a ranked findings table into `security.md`, and
emits a self-contained `security-matrix.html` (no external dependencies).

---

## Step 8 — Surface results

Show the user, in chat:

1. **Trust line first**: how many findings survived verification and how they
   cross-referenced — "N reported · X confirmed, Y partial · Z rejected · of the
   survivors A novel / B variant-of-known / C already-fixed" — so the user knows
   the results were checked against source AND against the public CVE record.
2. A severity headline + how many are Pass C (systemic, multi-function blast
   radius) — those matter most.
3. The top 3–5 findings: `CWE — pkg/Func (file:line)` · severity/confidence ·
   **beats leverage** (unique/assisted/incidental) · **known status** (novel /
   variant-of CVE-…) · the anchor · the one-line reason. Explicitly call out (a)
   any `beats_leverage="unique"` finding — the one a plain SAST would miss — and
   (b) any `variant-of-known` finding, naming the CVE/GHSA it extends.
4. The honest caveat, every time: *"This is structural-targeting + semantic
   triage, not proof of exploitability. Confirm with govulncheck and CodeQL/gosec
   before acting."*

Then, if `present_files` is available, present `security-matrix.html` and
`security.md`.

---

## Step 9 — Run report (time + cost)

Close the clock and report the run's footprint so the user can weigh cost vs.
value:

```bash
start=$(cat "$REPO/.beats/.sec_start" 2>/dev/null || date +%s)
end=$(date +%s); elapsed=$((end - start))
in_bytes=$(cat "$REPO/.beats/outlier.md" "$REPO/.beats/security.clusters.json" 2>/dev/null | wc -c | tr -d ' ')
n_clusters=$(grep -o '"shape_hash"' "$REPO/.beats/security.clusters.json" 2>/dev/null | wc -l | tr -d ' ')
printf 'wall: %ss | model input: ~%s KB (~%s tokens) | clusters scanned: %s\n' \
  "$elapsed" "$((in_bytes/1024))" "$((in_bytes/4))" "$n_clusters"
```

Report to the user, in one short block:

- **Wall time** — the analysis elapsed above (the `beats init` you ran yourself
  is not counted).
- **Workload** — clusters scanned, functions/bytes analysed across the four
  passes, and roughly how many `beats query` + verification tool calls you made.
- **Token/$ estimate** — you cannot read your own exact token count, so give the
  input-size estimate above (≈ bytes ÷ 4 for the dominant input) and tell the
  user the authoritative figure is Claude Code's `/cost`. Do not invent a dollar
  amount.

---

## Error handling

- **outlier.md not found** → repo not indexed; run `beats init --repo $REPO`.
- **outliers: 0** → normal; Pass A is empty but Passes B and C still run.
- **report.html not found** → skip 3b; systemic pass covers only outlier.md's
  clusters. Tell the user the systemic scan was partial.
- **collect_clusters.sh returns []** → no non-primitive clusters; note it.
- **model did not return valid JSON** → re-run Step 4 asking for JSON only; do
  not hand-write the matrix.
- **binary not found after install** → add `$(go env GOPATH)/bin` to PATH.
