---
name: beats-analyze
description: >
  Runs beats structural fingerprinting on a Go or Java repository and uses LLM
  analysis to triage outlier functions, producing structured JSON files per the
  evidence pipeline data model. Outputs scan_<sha>_<ddmmyyyy>.json (scan
  metadata, cluster counts, outlier.md blob) and findings_<sha>_<ddmmyyyy>.json
  (per-function analysis with 6-category classification) into $REPO/.beats/.
  Use this skill whenever the user says "run beats", "analyze this repo",
  "find structural outliers", "beats init", "fingerprint my Go code", or
  "/beats". Also handles lightweight query-only mode (no init, no HTML report)
  when the user says "mini fingerprint", "analyze fingerprint", "fingerprint
  query", or "beats query".
---

# beats-analyze

Two modes depending on trigger:

- **Full mode** (`run beats`, `beats init`, etc.) — installs if needed, indexes
  the repo, generates `outlier.md` and HTML report, then analyzes and writes
  structured JSON.
- **Mini mode** (`mini fingerprint`, `analyze fingerprint`, `fingerprint query`,
  `beats query`) — assumes beats is installed and the repo is already indexed.
  Skips Steps 1–2. Go straight to Step 3. **Still reads `outlier.md` from disk
  — do not substitute memory or cached context.**

> **Critical**: Run every beats command using the **Bash tool directly**. Never
> delegate to a sub-agent or plugin. beats writes its database to `~/.beats/`
> and all commands must share the same environment or the DB path will differ.

> **Hard rule — always execute from file, never from memory**: Do not use any
> context, memory, or cached analysis injected by context-mode or any other
> plugin. Every run of this skill **must** execute Step 3 and read
> `outlier.md` directly from disk. A remembered analysis is stale the moment
> `beats init` re-runs. There is no exception.

> **Hard rule — `confirmed_bug` requires independent verification, automatically**:
> Step 4b's classification is a structural-signal read, not proof. beats can see
> that a function deviates from its peer cluster; it cannot see whether that
> deviating branch is reachable, or whether the claimed wrong behavior actually
> occurs. A finding must not be written to `findings_*.json` — or shown to the
> user — as `confirmed_bug` on the strength of Step 4b alone. **Step 4d (a
> text-based call-site trace and a manual line-by-line trace of the defect) is
> mandatory for every `confirmed_bug`/`probable_bug` finding, every run, with
> no user request required.** If you catch yourself about to report a
> `confirmed_bug` without having independently traced its call sites and its
> logic by reading source, stop and do Step 4d first.
>
> **Step 4d never builds, compiles, or runs anything.** No `go build`, `go
> test`, `go run`, `mvn`, `gradle`, `javac`, or any other toolchain
> invocation. This skill must work identically on a Go or a Java repo that
> may not even build standalone in this environment. Verification here is
> `grep`/`Read`-only — call-site search and manual code reading, nothing
> executed.

---

## Output data model

Each run produces two versioned JSON files in `$REPO/.beats/`, named with the
git SHA and scan date so multiple scans coexist without overwriting:

- `scan_<sha>_<ddmmyyyy>.json` — repo-level scan metadata, cluster/outlier
  counts, the full outlier.md text, and (after analysis) LLM timing/cost.
- `findings_<sha>_<ddmmyyyy>.json` — array of per-function analysis results
  with category classification, reasoning, and bug metadata.

These files are the primary output of this skill. The text summary shown to the
user is secondary — the JSON is what downstream tooling (evidence pipeline,
dashboards) consumes.

---

## Step 0 — Ask for the repo path (if not already known)

If the user has not provided a repository path, ask:

> Which repository should I analyze? Please give me the absolute path.

Do not proceed until you have an absolute path. Store it as `$REPO`.

---

## Step 1 — Verify beats is installed

*(Skip in mini mode)*

```bash
which beats || beats version
```

If `beats` is not found:
- Tell the user: "The `beats` binary is not on your PATH."
- Offer two options:
  1. **Install now** — run: `go install github.com/somak2kai/beats/cmd@latest`
  2. **Provide path** — ask for the full path and use it for every subsequent
     command.

Do not continue until `beats version` succeeds.

---

## Step 2 — Index the repository

*(Skip in mini mode)*

```bash
beats init --repo "$REPO"
```

This parses every Go/Java file, clusters functions by structural shape, and
persists to BadgerDB. It may take a minute for large repos — tell the user.

If the command fails, surface the full error and stop.

---

## Step 3 — Gather git metadata and write scan JSON

### 3a — Collect git state

```bash
cd "$REPO"
SHA=$(git rev-parse --short=12 HEAD)
BRANCH=$(git rev-parse --abbrev-ref HEAD)
SCAN_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
DATE_SUFFIX=$(date -u +"%d%m%Y")
```

### 3b — Detect language

```bash
if [ -f "$REPO/go.mod" ]; then
  LANG="go"
elif [ -f "$REPO/pom.xml" ] || [ -f "$REPO/build.gradle" ] || [ -f "$REPO/build.gradle.kts" ]; then
  LANG="java"
else
  LANG="unknown"
fi
```

If both `go.mod` and a Java build file exist, set `LANG="go"` (Go takes
precedence — beats' Go parser is the primary path).

### 3c — Check for idempotency

```bash
ls "$REPO/.beats/scan_${SHA}_"*.json 2>/dev/null
```

If a `scan_<sha>_*.json` already exists at the same SHA, log
`"skipping — already scanned at $SHA"` and stop. Do not re-scan or
re-analyze. This makes the skill safe to re-run.

### 3d — Read outlier.md

```bash
cat "$REPO/.beats/outlier.md"
```

- **File not found**: tell the user: "No potential outliers to triage —
  `outlier.md` not found in `$REPO/.beats/`. Run `beats init --repo $REPO` to
  generate it." Then stop.
- **`outliers: 0` in the header**: tell the user: "No structural outliers found
  — all functions fit established patterns." Then stop.
- Otherwise: capture the full file content as `$OUTLIER_MD_BLOB`. This is the
  user message for Step 4.

### 3e — Extract cluster and outlier counts from outlier.md

Parse the header of `outlier.md` which contains lines like:
```
outliers: N  clusters: M
```

Also count cluster tiers from the `=== Established Patterns ===` section:
```bash
# Count clusters by tier from outlier.md
HIGH=$(grep -c 'tier: high' "$REPO/.beats/outlier.md" || echo 0)
MEDIUM=$(grep -c 'tier: medium' "$REPO/.beats/outlier.md" || echo 0)
LOW=$(grep -c 'tier: low' "$REPO/.beats/outlier.md" || echo 0)
TOTAL_CLUSTERS=$((HIGH + MEDIUM + LOW))

# Count outlier entries
TOTAL_OUTLIERS=$(grep -c '^\-\-\- \[' "$REPO/.beats/outlier.md" || echo 0)
```

### 3f — Derive the repo identifier

Extract `org/repo` from the git remote:
```bash
REPO_ID=$(cd "$REPO" && git remote get-url origin 2>/dev/null \
  | sed -E 's#.+github\.com[:/]##; s/\.git$//' || basename "$REPO")
```

### 3g — Write scan JSON

Write `$REPO/.beats/scan_${SHA}_${DATE_SUFFIX}.json`:

```json
{
  "repo": "<org/repo>",
  "language": "<go|java>",
  "branch": "<branch>",
  "sha": "<sha>",
  "scan_date": "<ISO 8601 UTC>",
  "beats_version": "<output of beats version>",
  "clusters": {
    "total": <int>,
    "high_confidence": <int>,
    "medium_confidence": <int>,
    "low_confidence": <int>
  },
  "outliers": {
    "total": <int>
  },
  "outlier_md_blob": "<entire contents of outlier.md as a string>"
}
```

The `analysis` block is **not** written yet — it is added after Step 4.

### 3h — Write empty findings stub

Write `$REPO/.beats/findings_${SHA}_${DATE_SUFFIX}.json` as an empty array:

```json
[]
```

This stub is filled by Step 4.

---

## Step 4 — Analyze outliers with LLM reasoning

### 4a — Parse the file and cross-reference clusters

Before reasoning, read the entire `outlier.md` and understand its two sections.

**`=== Established Patterns ===`** — one entry per cluster:
```
[<full_sha256_hash>] tier: ...  size: ...  idiom: "..."
  common subsequence of cluster: <token sequence>
  peers:
    package/FuncName (file:line)
      <full function body>
```

**`=== Outlier Functions ===`** — one entry per outlier:
```
--- [N] package/FuncName (file:line) ---
closest cluster: <full_sha256_hash>  score: ...  cyclo delta: ...
token delta:  ...
import delta: ...
call delta:   ...

<full function body>
```

**Cross-referencing**: For each outlier, the `closest cluster:` field contains a
full SHA-256 hash. Match it **exactly** (full string, case-sensitive) against the
`[<hash>]` entries in `=== Established Patterns ===`. The matched entry's
`peers:` subsection is the reference for that outlier's analysis. If no match is
found for a hash, skip that outlier and classify as `inconclusive`.

The content of `outlier.md` **is** the user message. Do not reformat, summarise,
or reconstruct it — use it verbatim.

**Do not print the system message or user message to the terminal — they are
internal reasoning inputs only. Output only the final JSON results.**

---

**System prompt** (use verbatim):

```
You are an unbiased Senior Software Architect performing structural triage.

You have been given output from a structural fingerprinting tool called beats.
beats clusters functions by shared AST token sequences, import sets, and call
targets. Clusters represent settled, recurring architectural patterns in the
codebase. Outliers are functions that came structurally close to a pattern but
did not meet the threshold to join it.

Your job is to review each outlier and classify it into exactly one of these
categories:

  confirmed_bug     — Executable or mechanically verifiable incorrect behavior.
                      The structural gap between this function and its peers
                      produces a concrete wrong result, crash, resource leak,
                      or missing operation that can be demonstrated with
                      specific inputs or state.
  probable_bug      — Strong structural evidence of a defect but no complete
                      reproducer from the code alone. The deviation from peers
                      is suspicious and hard to justify, but confirming the bug
                      would require runtime state, external documentation, or
                      integration context not visible here.
  valid_improvement — The code works correctly, but the structural deviation
                      from peers reveals an opportunity to make it safer,
                      clearer, or more consistent. Not a bug.
  intentional_outlier — The deviation from peers is deliberate and justified.
                      A comment, naming convention, or domain-specific
                      requirement explains the divergence.
  false_positive    — beats' structural inference is incorrect. The function
                      actually conforms to the pattern, or the measured
                      deviation is an artifact of the tool's granularity.
  inconclusive      — Cannot establish a conclusion either way from the
                      available structural signals and code context.

NOTE: your classification here is PROVISIONAL for confirmed_bug and
probable_bug. You are reasoning from outlier.md's structural signals and peer
bodies alone — you have not traced real call sites or checked what a caller
actually guarantees about input shape/type. A later, independent verification
pass (Step 4d) will trace reachability and the defect's logic by reading
source (no execution) before either category is allowed to stand. Do not let
that fact loosen your bar here — still only assign confirmed_bug/probable_bug
when the structural evidence genuinely supports it — but do not editorialize
about severity or certainty beyond what the structural signal shows; that's
Step 4d's job, not yours.

For each outlier in === Outlier Functions ===, find its peer cluster by matching
the `closest cluster: <hash>` field exactly against the `[<hash>]` entries in
=== Established Patterns ===. The peers: bodies under that cluster entry are
the reference. Use them as reference, not as ground truth.

Ask one question per signal (token, import, call, cyclo), and always ask it
against the peer bodies:

  Does the function body explain this deviation from peers?

If yes → likely intentional_outlier or false_positive.
If no  → likely confirmed_bug, probable_bug, or valid_improvement.

When all deltas are "none", read the peer bodies directly and ask: is there
anything peers consistently do that this function does not — or vice versa?
That comparison is the signal. Apply the same question above.

Use the outlier body and peer bodies only to answer that question. They are not
an independent source of general code review findings.

Do NOT:
- Flag anything that cannot be directly traced to a token, import, call, or
  cyclo delta — if the structural signals look unremarkable, classify as
  false_positive regardless of what else you notice in the body
- Perform general code review — string literals, variable names, comments, and
  logic unrelated to the structural deviation are out of scope
- Suggest any refactoring, abstraction, or code changes
- Comment on naming, style, or formatting
- Speculate about downstream consequences, call chains, or what a caller might
  do with a bad return value
- Use severity language like "panic", "nil dereference", "data exposure", or
  "security" unless the structural signal itself directly shows the defect

Token reference: CALL=local/builtin call, CALL_PKG=package-qualified call
(e.g. fmt.Sprintf), CALL_METHOD=method on variable, IF=branch/guard,
FOR=index loop, RANGE=range iteration, ASSIGN=local variable assignment,
RETURN=one token per return value, DEFER=cleanup/unlock, GO=goroutine spawn,
SWITCH/CASE=dispatch, FUNCLIT=inline closure, SELECT=channel multiplexing.

ADDITIONAL FIELDS (fill only when category is confirmed_bug or probable_bug):

pr_worthy: true only when the finding is concrete enough to justify a pull
  request. A confirmed_bug is almost always PR-worthy. A probable_bug is
  PR-worthy only when the evidence is strong enough that a maintainer would
  likely accept the fix.

impact: one sentence describing the consequence of NOT fixing the bug. Focus
  on what goes wrong at runtime — not code quality opinions.

bug_present_since: the approximate date (YYYY-MM-DD) the bug was introduced.
  Derive this by reading the git blame for the specific lines that cause the
  structural deviation. If you cannot determine a date, set to null.

bug_age_method: how you determined bug_present_since. One of:
  "git_blame" — used git blame on the relevant lines
  "git_bisect" — used git bisect
  "manual_history" — read git log / commit history manually
  "unknown" — could not determine

found_by_others: comma-separated list of other tool names that would likely
  catch this same bug (e.g. "staticcheck", "go vet", "errorlint",
  "greptile", "coderabbit"). Set to "none" if no commonly-used tool would
  catch this via its default rules.

OUTPUT FORMAT — respond with a JSON array. One object per outlier function.
Do not wrap in markdown code fences. Output raw JSON only.

[
  {
    "function_name": "processNextWorkItem",
    "file": "pkg/controller/cronjob/cronjob_controller.go",
    "line_start": 245,
    "line_end": 289,
    "function_text": "<full text of the function>",
    "cluster_id": "b311c1338528ba34...",
    "cluster_size": 5,
    "category": "confirmed_bug",
    "category_reasoning": "The five peer processNextWorkItem implementations all call queue.Forget(key) on the success path before returning. This function returns without calling Forget, leaving stale rate-limiter backoff state. (max 200 words)",
    "pr_worthy": true,
    "impact": "Stale exponential-backoff state survives successful reconciliation, causing inflated retry delays for later failures.",
    "found_by_others": "none",
    "bug_present_since": "2021-04-03",
    "bug_age_method": "git_blame"
  },
  {
    "function_name": "Run",
    "file": "pkg/controller/foo/foo_controller.go",
    "line_start": 50,
    "line_end": 78,
    "function_text": "<full text>",
    "cluster_id": "f7850175ea5ef4dc...",
    "cluster_size": 12,
    "category": "false_positive",
    "category_reasoning": "The structural divergence is an additional context.WithTimeout wrapper not present in peers. This is intentional — the foo controller has a documented SLA requirement. Comment on line 55 explains the timeout. (max 200 words)",
    "pr_worthy": false,
    "impact": null,
    "found_by_others": null,
    "bug_present_since": null,
    "bug_age_method": null
  }
]

Rules for category_reasoning:
- Max 200 words.
- Must cite the specific structural signal (token/import/call/cyclo delta or
  peer comparison) that drove the classification.
- For confirmed_bug: describe the mechanically verifiable incorrect behavior.
- For probable_bug: explain what evidence is missing for full confirmation.
- For valid_improvement: explain what peers do better and why it matters.
- For intentional_outlier: cite the comment, naming, or domain reason.
- For false_positive: explain why beats' inference is wrong.
- For inconclusive: explain what information would resolve the ambiguity.
```

---

**User message**: the full content of `outlier.md` read in Step 3d, verbatim.

### 4b — Run the analysis

Reason over the system prompt and the `outlier.md` content internally. Record
the wall-clock start time before analysis and the end time after.

**Output**: a JSON array — one object per outlier function. Each object must
contain all fields from the system prompt's output format.

### 4c — Bug age lookup (for confirmed_bug and probable_bug only)

For each finding classified as `confirmed_bug` or `probable_bug`, if
`bug_present_since` was not determined during the main analysis, attempt a
git blame lookup:

```bash
cd "$REPO"
git blame -L <line_start>,<line_end> -- "<file>" | head -1
```

Parse the commit date from the blame output and set `bug_present_since` to
that date (YYYY-MM-DD format). Set `bug_age_method` to `"git_blame"`.

If blame fails or is ambiguous, set `bug_present_since` to `null` and
`bug_age_method` to `"unknown"`.

---

## Step 4d — Verify confirmed_bug and probable_bug findings (mandatory)

**Run this automatically, every time, for every finding currently classified
`confirmed_bug` or `probable_bug`. Do not wait for the user to question a
finding — that is what this step exists to make unnecessary.** It is cheap:
these two categories are typically a handful of findings out of hundreds of
outliers.

beats' structural signal (token/import/call/cyclo delta vs. a peer cluster)
tells you a function looks different from its peers. It tells you nothing
about two things that determine whether "bug" is actually true:

1. **Reachability** — can the flagged code path execute with the
   input/state the finding claims, given how the function is really wired
   into the program?
2. **Defect logic** — read closely enough (including any library/dependency
   source available in the repo) to determine whether the wrong behavior the
   finding describes actually follows from the code as written.

**This step is text-search-and-reading only. Never build, compile, or run
anything** — no `go build`/`go test`/`go run`, no `mvn`/`gradle`/`javac`, no
invoking any compiler, interpreter, or test runner, on any language. That
rule is absolute and applies equally to Go and Java repos; do not make an
exception because a repro "would be quick" or "is just a unit test." This
step must also be an *independent* pass: re-derive evidence from primary
sources (`grep`, `Read`, git) rather than re-reading and agreeing with Step
4b's own `category_reasoning`. A verifier that just restates the classifier's
argument is not verification. Running it in a fresh subagent/context per
finding is a good way to enforce this — it's fine to delegate, since none of
this touches the beats DB (the "run beats commands directly" rule is scoped
to beats CLI commands only).

### 4d-i — Call-site trace (text search only)

For the flagged function, trace how it is *actually* invoked in `$REPO` by
reading, not running:
- `grep`/search for every call site and every registration (event handler
  wiring, interface implementation, HTTP route, CLI command tree, etc.).
- If it's wired through a framework contract (a typed informer's
  `AddFunc`/`UpdateFunc`/`DeleteFunc`, an admission chain, a flag parser),
  read *that framework's* dispatch code (still just reading it — no
  build/run needed) to check whether it can actually deliver the input
  shape the finding blames. Example from experience, established purely by
  reading source: a `cache.DeletedFinalStateUnknown` tombstone is
  constructed by client-go only for `Deleted` deltas, dispatched only to
  `DeleteFunc`/`OnDelete` — it can never reach an `AddFunc`/`OnAdd` handler,
  no matter how structurally similar that handler looks to a `DeleteFunc`
  peer that legitimately needs the tombstone fallback. A deviation that
  only matters on an unreachable branch is not a bug.
- **Know the limits of grep-based tracing and say so honestly.** Plain
  function calls and directly-named methods are usually safe to trace this
  way. Indirect dispatch is not: an interface method call (Go or Java), a
  Java class hierarchy with overriding, a dependency-injected/wired
  component, a reflection-based call, or generated/dynamic dispatch can all
  mean the real caller is invisible to a textual search, or that a
  same-named grep hit is a different implementation entirely. Java is
  usually harder here than Go — heavier use of interfaces, DI frameworks,
  and overriding means call graphs are frequently not recoverable by text
  search alone. **When the trace can't rule out an indirect path with
  confidence, say so in `verification_notes` and cap the verdict at
  `PLAUSIBLE`** — do not round an ambiguous trace up to "reachable" or down
  to "unreachable."
- If the trace *does* confidently show neither a production nor a test path
  can reach the deviating branch, that's a **reachability failure** for this
  finding.

### 4d-ii — Defect logic trace (manual reading only)

Trace the exact claim in `category_reasoning` by hand, reading the actual
code involved:
- If the claim depends on a library/dependency function's behavior (e.g.
  "this passes the wrong variable to `X`, which normally propagates it"),
  find and read that function's real source if it's available in the repo
  (vendored source, a submodule, anything checked in) — do not assume what
  a library does from its name. If the dependency's source isn't available
  to read, say so and cap the verdict at `PLAUSIBLE`.
- Manually substitute the values/branches the claim describes and check
  whether the described wrong output genuinely follows — the same kind of
  trace you'd do on paper before ever running a debugger. Example: for a
  claim that a failed type assertion's zero value gets passed to an
  error-wrapping helper instead of the original argument, read the helper's
  source, confirm it only special-cases specific dynamic types, and confirm
  the zero value can never be one of those types — that chain is provable
  by reading alone, with no execution required.
- If the logic can only be resolved with unknown runtime state (config
  values, external data, timing) that isn't visible in source, that's not
  provable by reading — cap the verdict at `PLAUSIBLE`.

### 4d-iii — Verdict and category resolution

Assign each finding a `verdict`:
- **`CONFIRMED`** — the call-site trace confidently shows the branch is
  reachable (no unresolved indirect dispatch standing in the way), AND the
  defect logic trace shows the wrong behavior follows deterministically
  from the code as written, with every referenced function's real source
  read and accounted for. Category becomes/stays `confirmed_bug`.
- **`PLAUSIBLE`** — the structural evidence is still suspicious, but either
  reachability couldn't be nailed down through text search alone (indirect
  dispatch, DI, reflection) or the defect logic depends on runtime state or
  unavailable dependency source. Category becomes/stays `probable_bug`.
- **`DOWNGRADE`** — the call-site trace confidently shows the branch is
  unreachable, or the manual logic trace contradicts the claim outright.
  Category drops to `valid_improvement` (still a legitimate
  defensive-hardening or consistency gap) or `false_positive` (the claim
  itself doesn't hold up).

Rewrite `category_reasoning` to append what 4d-i/4d-ii actually found — the
verification evidence must be visible in the JSON output, not only in
conversation. Example append: *"Verified by reading source only: AddFunc is
dispatched only for Sync/Replaced/Added/Updated deltas (controller.go:639);
DeletedFinalStateUnknown is only ever constructed for Deleted deltas. This
branch is unreachable on the Add path — no production or test caller can
trigger it. Downgraded to valid_improvement."*

Set three new fields on the finding object (added to the data model — see
Field reference):
- `verdict`: `"CONFIRMED"` | `"PLAUSIBLE"` | `"DOWNGRADE"`
- `verification_method`: `"static_trace"` (both call-site reachability and
  defect logic fully resolved by reading) | `"partial_static_trace"`
  (reachability or logic capped by indirect dispatch, missing dependency
  source, or unknown runtime state) | `"unverifiable"` (no textual path to
  establish anything either way)
- `verification_notes`: what was actually read and checked (files, call
  sites, the library source consulted, or exactly what couldn't be
  resolved and why) — this is the audit trail; do not leave it generic.

If a finding is downgraded, update `pr_worthy`, `impact`, `bug_present_since`,
and `bug_age_method` to match its new category per the existing rules in
Step 5a's validation checklist (e.g. `pr_worthy` becomes `false` if the new
category isn't `confirmed_bug`/`probable_bug`).

---

## Step 5 — Write findings JSON and update scan JSON

### 5a — Write findings

Overwrite the stub `$REPO/.beats/findings_${SHA}_${DATE_SUFFIX}.json` with the
full JSON array from Step 4b.

Validate before writing:
- Every object has all required fields (`function_name`, `file`, `line_start`,
  `line_end`, `function_text`, `cluster_id`, `cluster_size`, `category`,
  `category_reasoning`, `pr_worthy`, `impact`, `found_by_others`,
  `bug_present_since`, `bug_age_method`, `verdict`, `verification_method`,
  `verification_notes`)
- `category` is one of the 6 valid values
- `pr_worthy` is `true` only when category is `confirmed_bug` or `probable_bug`
- `impact` is non-null only when `pr_worthy` is `true`
- `bug_present_since` and `bug_age_method` are non-null only when category is
  `confirmed_bug` or `probable_bug`
- **`verdict`, `verification_method`, and `verification_notes` are non-null
  if and only if category is `confirmed_bug` or `probable_bug`** — a finding
  in either category with `verdict: null` means Step 4d was skipped for it;
  that is a validation failure, not a thing to silently write anyway. Go back
  and run Step 4d for it before writing.
- **`category == "confirmed_bug"` requires `verdict == "CONFIRMED"`.** If
  Step 4d produced `PLAUSIBLE` or `DOWNGRADE` for a finding still labeled
  `confirmed_bug`, that's a bug in the skill's own execution — the category
  must already have been rewritten in 4d-iii before reaching this step.

### 5b — Update scan JSON with analysis metadata

Read back the `scan_${SHA}_${DATE_SUFFIX}.json` written in Step 3g and add the
`analysis` block:

```json
{
  "analysis": {
    "time_seconds": <wall-clock seconds for LLM analysis>,
    "cost_usd": null,
    "llm_model": "<model used for analysis>",
    "analyzed_at": "<ISO 8601 UTC timestamp>"
  }
}
```

Set `cost_usd` to `null` — exact API cost is not available from within the
skill. It can be filled in by external tooling if needed.

Write the updated scan JSON back to disk.

---

## Step 6 — Surface the results

Present a summary to the user showing:
- Number of findings by category (e.g., "2 confirmed_bug, 1 probable_bug,
  3 intentional_outlier, 5 false_positive")
- **Verification outcome for every finding that went through Step 4d**, even
  if that means reporting that a `confirmed_bug` from Step 4b got downgraded
  — e.g. "3 flagged as confirmed_bug by structural triage; 2 survived
  verification (call sites and defect logic confirmed by reading source),
  1 downgraded to valid_improvement (unreachable branch — see
  verification_notes)." Never report Step 4b's raw category count as if it
  were the final word.
- List of PR-worthy findings (post-verification) with function name, file,
  and one-line impact
- Paths to the generated JSON files

Then tell the user:

> Structured output written to:
> - `$REPO/.beats/scan_<sha>_<date>.json` — scan metadata and cluster counts
> - `$REPO/.beats/findings_<sha>_<date>.json` — per-function analysis
>
> Full structural report: `$REPO/.beats/report.html`

If you have access to the `present_files` tool, present the report file.

---

## Error handling

- **beats init fails:** surface the slog error line. Common causes: non-Go
  files with malformed package clauses (usually harmless), permission errors on
  the temp DB path.
- **outlier.md not found:** repo has not been indexed yet — tell the user to run
  `beats init --repo <path>` first, then retry.
- **outlier.md header shows `outliers: 0`:** normal for very small repos or
  repos with highly uniform structure. No analysis needed. Still write a
  `scan_<sha>_<date>.json` with zero counts and no analysis block — this
  records that the repo was scanned even though no outliers were found.
- **beats analyze fails:** surface the full error. The DB was just written so
  this is rare.
- **Binary not found after install:** remind the user to add
  `$(go env GOPATH)/bin` to their PATH.
- **JSON validation fails:** log which fields failed validation, write the
  findings as-is with a warning comment, and tell the user which entries need
  manual review.
- **Already scanned at same SHA:** log "skipping — already scanned at $SHA"
  and stop. This is normal and expected for idempotent re-runs.
- **Step 4d's call-site trace can't rule out indirect dispatch** (interface
  methods, Java overriding/DI, reflection, generated code): do not round up
  to "reachable" or down to "unreachable." Set
  `verification_method: "partial_static_trace"`, explain the ambiguity in
  `verification_notes`, and cap the verdict at `PLAUSIBLE` even if the
  finding started as `confirmed_bug`. Never skip Step 4d because tracing is
  hard — an honest `PLAUSIBLE` is always the right fallback, not silence.

---

## Field reference

| Field | Type | When filled | Notes |
|---|---|---|---|
| `repo` | string | always | org/repo format |
| `language` | string | always | `go` or `java` |
| `branch` | string | always | from git or default branch |
| `sha` | string | always | pinned HEAD at scan time |
| `clusters.total` | int | always | from outlier.md header |
| `clusters.high_confidence` | int | always | count of tier: high |
| `clusters.medium_confidence` | int | always | count of tier: medium |
| `clusters.low_confidence` | int | always | count of tier: low |
| `outliers.total` | int | always | from outlier.md header |
| `outlier_md_blob` | string | always | entire outlier.md contents |
| `function_name` | string | always | per outlier function |
| `file` | string | always | relative path in repo |
| `line_start` / `line_end` | int | always | from outlier.md |
| `function_text` | string | always | full source text of function |
| `cluster_id` | string | always | beats cluster hash |
| `cluster_size` | int | always | number of functions in cluster |
| `category` | enum | after analysis | one of the 6 categories |
| `category_reasoning` | string | after analysis | max 200 words |
| `pr_worthy` | bool | after analysis | `true` only for confirmed/probable_bug |
| `impact` | string | when pr_worthy=true | impact of NOT fixing |
| `found_by_others` | string | after analysis | comma-separated tool names, or `none` |
| `bug_present_since` | date | when confirmed/probable | YYYY-MM-DD |
| `bug_age_method` | string | when confirmed/probable | `git_blame`, `git_bisect`, `manual_history`, `unknown` |
| `verdict` | enum | when confirmed/probable | `CONFIRMED`, `PLAUSIBLE`, `DOWNGRADE` — set by Step 4d, independent of Step 4b's category |
| `verification_method` | enum | when confirmed/probable | `static_trace`, `partial_static_trace`, `unverifiable` — Step 4d is text-search/reading only, never a build or test run |
| `verification_notes` | string | when confirmed/probable | what Step 4d actually read and checked — the audit trail behind the verdict |
| `analysis.time_seconds` | int | after analysis | wall-clock LLM analysis time |
| `analysis.cost_usd` | float | after analysis | LLM API cost (null if unavailable) |
| `analysis.llm_model` | string | after analysis | model used for analysis |
| `analysis.analyzed_at` | string | after analysis | ISO 8601 UTC timestamp |

### Category values

| Value | Meaning |
|---|---|
| `confirmed_bug` | Executable or mechanically verifiable incorrect behavior — final only after Step 4d returns `verdict: CONFIRMED` |
| `probable_bug` | Strong analysis, but no complete reproducer — final only after Step 4d returns `verdict: PLAUSIBLE` (or better) |
| `valid_improvement` | Intentional behavior, but code could be safer or clearer |
| `intentional_outlier` | Difference is deliberate and justified |
| `false_positive` | beats' inference is incorrect |
| `inconclusive` | Cannot establish either conclusion |

`confirmed_bug` and `probable_bug` as written by Step 4b are provisional
labels only. They must not be surfaced to the user, written to
`findings_*.json`, or used to justify a PR until Step 4d has independently
verified them (see above) — that verification is part of this skill's normal,
automatic execution, not an optional follow-up.
