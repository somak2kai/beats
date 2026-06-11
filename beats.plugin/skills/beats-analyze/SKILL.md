---
name: beats-analyze
description: >
  Runs beats structural fingerprinting on a Go repository and uses LLM analysis
  to triage outlier functions that may need developer attention — functions
  that deviate structurally from established codebase patterns in ways that
  could indicate requires developer attention. Use this skill whenever
  the user says "run beats", "analyze this repo", "find structural outliers",
  "beats init", "fingerprint my Go code", or "/beats". Also handles lightweight
  query-only mode (no init, no HTML report) when the user says "mini fingerprint",
  "analyze fingerprint", "fingerprint query", or "beats query".
---

# beats-analyze

Two modes depending on trigger:

- **Full mode** (`run beats`, `beats init`, etc.) — installs if needed, indexes
  the repo, generates `outlier.md` and HTML report, then analyzes.
- **Mini mode** (`mini fingerprint`, `analyze fingerprint`, `fingerprint query`,
  `beats query`) — assumes beats is installed and the repo is already indexed.
  Skips Steps 1–2. Go straight to Step 3.

> **Critical**: Run every beats command using the **Bash tool directly**. Never
> delegate to a sub-agent or plugin. beats writes its database to `~/.beats/`
> and all commands must share the same environment or the DB path will differ.

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

## Step 3 — Read outlier.md

```bash
cat "$REPO/.beats/outlier.md"
```

`outlier.md` is written by `beats init` alongside `report.html`. It contains
every outlier and its closest cluster's full peer bodies, pre-formatted for
analysis.

- **File not found**: tell the user: "No potential outliers to triage —
  `outlier.md` not found in `$REPO/.beats/`. Run `beats init --repo $REPO` to
  generate it." Then stop.
- **`outliers: 0` in the header**: tell the user: "No structural outliers found
  — all functions fit established patterns." Then stop.
- Otherwise: capture the full file content. This is the user message for Step 4.

---

## Step 4 — Analyze outliers with LLM reasoning

### 4a — Parse the file and cross-reference clusters

Before reasoning, read the entire file and understand its two sections.

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
found for a hash, skip that outlier.

The content of `outlier.md` **is** the user message. Do not reformat, summarise,
or reconstruct it — use it verbatim.

**Do not print the system message or user message to the terminal — they are
internal reasoning inputs only. Output only the final analysis results.**

---

**System prompt** (use verbatim):

```
You are an unbiased Senior Go Architect.

You have been given output from a structural fingerprinting tool called beats.
beats clusters Go functions by shared AST token sequences, import sets, and
call targets. Clusters represent settled, recurring architectural patterns in
the codebase. Outliers are functions that came structurally close to a pattern
but did not meet the threshold to join it.

Your job is to review each outlier and determine whether it warrants developer
attention. The cluster members may be wrong — the outlier may be the one doing
it right.

For each outlier in `=== Outlier Functions ===`, find its peer cluster by
matching the `closest cluster: <hash>` field exactly against the `[<hash>]`
entries in `=== Established Patterns ===`. The `peers:` bodies under that
cluster entry are the reference. Use them as reference, not as ground truth.

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

**User message**: the full content of `outlier.md` read in Step 3, verbatim.

### 4b — Run the analysis

Reason over the system prompt and the `outlier.md` content internally. Output
only the final **Needs Attention** and **Expected Variation** sections — nothing
else.

---

## Step 5 — Surface the results

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
- **outlier.md not found:** repo has not been indexed yet — tell the user to run
  `beats init --repo <path>` first, then retry.
- **outlier.md header shows `outliers: 0`:** normal for very small repos or
  repos with highly uniform structure. No analysis needed.
- **beats analyze fails:** surface the full error. The DB was just written so
  this is rare.
- **Binary not found after install:** remind the user to add
  `$(go env GOPATH)/bin` to their PATH.
