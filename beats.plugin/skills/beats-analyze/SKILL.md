---
name: beats-analyze
description: >
  Runs beats structural fingerprinting on a Go repository and uses LLM analysis
  to surface outlier functions that may need developer attention — functions
  that deviate structurally from established codebase patterns in ways that
  could indicate bugs or architectural inconsistencies. Use this skill whenever
  the user says "run beats", "analyze this repo", "find structural outliers",
  "beats init", "fingerprint my Go code", or "/beats". Also handles lightweight
  query-only mode (no init, no HTML report) when the user says "mini fingerprint",
  "analyze fingerprint", "fingerprint query", or "beats query".
---

# beats-analyze

Two modes depending on trigger:

- **Full mode** (`run beats`, `beats init`, etc.) — installs if needed, indexes
  the repo, queries outliers, analyzes, generates HTML report.
- **Mini mode** (`mini fingerprint`, `analyze fingerprint`, `fingerprint query`,
  `beats query`) — assumes beats is installed and the repo is already indexed.
  Skips Steps 1–2 and Step 5. Go straight to Step 3.

> **Critical**: Run every beats command using the **Bash tool directly**. Never
> delegate to a sub-agent or plugin. beats writes its database to `~/.beats/`
> and all commands must share the same environment or the DB path will differ.

> **HARD RULE — no scripts, no truncation, ever**: Do NOT pipe beats output to
> python3, node, jq, head, tail, or any other tool. Do NOT write a script to a
> file and run it. Run beats commands bare and read the full output directly in
> your context window. Truncating with `head` breaks the JSON array and silently
> drops outliers. There is no exception.

---

## Step 0 — Ask for the repo path (if not already known)

If the user has not provided a repository path, ask:

> Which Go repository should I analyze? Please give me the absolute path.

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

This parses every Go file, clusters functions by structural shape, and persists
to BadgerDB. It may take a minute for large repos — tell the user.

If the command fails, surface the full error and stop.

---

## Step 3 — Query structural outliers

```bash
beats query outlier --repo "$REPO" --format json
```

Capture the full JSON output. This returns an array of outlier functions — each
one did not fit any established structural cluster but came close to one or more.

If the array is empty, tell the user: "No structural outliers found — all
functions fit established patterns." Then skip to Step 5.

---

## Step 4 — Analyze outliers with LLM reasoning

This is the core step. You will build a structured prompt from the JSON and
reason about which outliers warrant developer attention.

**STOP**: Do not write a script to parse the JSON. Read it in your context window
and build the prompt text manually. No python3, no node, no jq, no shell pipes.

### 4a — Fetch peer bodies for every cluster

From the outlier JSON, collect all **unique** `cluster_id` values from
`candidates[0]` across all outliers.

> **CRITICAL**: `cluster_id` is a **full SHA-256 hex string** (32+ characters),
> e.g. `9cbe48a3f1d2...`. Copy it verbatim from the JSON field. **Never**
> truncate it to 6 characters or use the short display form. The command will
> return "no cluster found" if the hash is shortened.

For each unique cluster_id, run:

```bash
beats query cluster shape <full_cluster_id> --repo "$REPO" --format json
```

Capture the JSON. This returns the cluster's members with their actual function
bodies. Store the result keyed by cluster_id.

### 4b — Build the analysis prompt

Construct the following prompt exactly. Use the outlier JSON and the cluster
JSON from 4a to fill in the values.

---

**System prompt** (use verbatim):

```
You are a structural anomaly analyst for Go codebases.

You have been given output from a structural fingerprinting tool called beats.
beats clusters Go functions by shared AST token sequences, import sets, and
call targets. Clusters represent settled, recurring architectural patterns in
the codebase. Outliers are functions that came structurally close to a pattern
but did not meet the threshold to join it.

Your job is to review each outlier and determine whether it warrants developer
attention.

For each outlier you have been given the actual bodies of its closest cluster's
members. Use them as the ground truth for what peers look like.

Ask one question per signal (token, import, call, cyclo), and always ask it
against the peer bodies:

  Does the function body explain this deviation from peers?

If yes → Expected Variation.
If no → Needs Attention.

When all deltas are "none", read the peer bodies directly and ask: is there
anything peers consistently do that this function does not — or vice versa?
That comparison is the signal. Apply the same question above.

Use the outlier body and peer bodies only to answer that question. They are not
an independent source of general code review findings.

Do NOT:
- Flag anything that cannot be directly traced to a token, import, call, or
  cyclo delta — if the structural signals look unremarkable, the function passes
  regardless of what else you notice in the body
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

Output format — use exactly this structure:

## Needs Attention
**pkg/FuncName** (file:line) — pattern: <cluster_id>
Signal: cite the specific delta or peer comparison and what it shows (e.g. "token delta: −RETURN vs peers", "call delta: −errors.Is", "peers all call X before returning, this function does not")
Concern: one sentence describing what the structural gap is — not what might happen downstream as a result.

## Expected Variation
- pkg/FuncName — one clause why the deviation looks intentional
```

---

**User message** (build this from the JSON):

```
=== Established Patterns ===

[For each unique cluster_id from candidates[0], one entry:]
[<cluster_id>] tier: <tier>  size: <size>  idiom: "<idiom or 'unenriched'>"
  common shape: <common_shape>
  peers:
  [For each member in the cluster query result:]
    <package>/<func> (<file>:<line>)
    <body>

=== Outlier Functions ===

[For each filtered outlier, numbered:]
--- [N] <package>/<func> (<file>:<line>) ---
closest cluster: <cluster_id>  score: <score:.3f>  cyclo delta: <cyclo_delta:+.1f>
token delta:  <token_delta.added as +X>  <token_delta.removed as -X>  (or "none" if empty)
import delta: <import_delta.added as +X>  <import_delta.removed as -X>  (or "none")
call delta:   <call_delta.added as +X>  <call_delta.removed as -X>  (or "none")

<body>

```

For the deltas: prefix added items with `+`, removed with `−`. If both added
and removed are empty arrays, write `none`.

### 4c — Run the analysis

Send the system prompt and user message. Read the response carefully.

---

## Step 5 — Generate the HTML report

*(Skip in mini mode)*

```bash
beats analyze --repo "$REPO"
```

This writes `$REPO/.beats/report.html` with the full cluster and outlier data
visible in the browser.

---

## Step 6 — Surface the results

Present the LLM analysis output directly to the user — the "Needs Attention"
and "Expected Variation" sections.

Then tell the user:

> Full structural report written to `$REPO/.beats/report.html` — open it to
> explore all clusters and potential deviations interactively.

If you have access to the `present_files` tool, present the report file.

---

## Error handling

- **beats init fails:** surface the slog error line. Common causes: non-Go
  files with malformed package clauses (usually harmless), permission errors on
  the temp DB path.
- **beats query outlier returns no candidates:** normal for very small repos or
  repos with highly uniform structure.
- **beats query outlier errors in mini mode:** repo was likely not indexed —
  tell the user to run `beats init --repo <path>` first, then retry.
- **beats analyze fails:** surface the full error. The DB was just written so
  this is rare.
- **Binary not found after install:** remind the user to add
  `$(go env GOPATH)/bin` to their PATH.
