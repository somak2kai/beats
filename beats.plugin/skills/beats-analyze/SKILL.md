---
name: beats-analyze
description: >
  Runs the full beats structural fingerprinting pipeline on a Go repository:
  indexes functions, clusters them by structural shape, enriches every cluster
  with LLM-generated idiom names, verdicts, canonical examples, and developer
  search questions, then regenerates the interactive HTML report. Use this
  skill whenever the user says "run beats", "analyze this repo with beats",
  "beats init", "fingerprint my Go code", "find structural patterns in this
  codebase", or "/beats". Also triggers when the user wants to label or enrich
  beats clusters with metadata.
---

# beats-analyze

End-to-end pipeline: index a Go repository with beats, enrich every cluster
with LLM-generated metadata (idiom name, verdict, canonical example, suggested
action, confidence, and developer search questions), then regenerate the HTML
report so the enrichment shows up.

> **Critical**: Run every single beats command — init, query, update, analyze —
> using the **Bash tool directly**. Never delegate to a sub-plugin, context-mode
> tool, or any other execution layer. beats writes its database to `os.TempDir()`
> and all commands must share the same process environment or the DB path will
> differ between steps, causing double-indexing.

---

## Step 0 — Ask for the repo path (if not already known)

If the user has not provided a repository path, ask:

> Which Go repository should I analyze? Please give me the absolute path.

Do not proceed until you have an absolute path. Store it as `$REPO`.

---

## Step 1 — Verify beats is installed

```bash
which beats || beats version
```

If `beats` is not found:
- Tell the user: "The `beats` binary is not on your PATH."
- Offer two options:
  1. **Install now** — run: `go install github.com/somak2kai/beats/cmd@latest`
  2. **Provide path** — ask for the full path to the binary and use it for
     every subsequent command (replace `beats` with that path).

Do not continue until `beats version` succeeds.

---

## Step 2 — Index the repository

```bash
beats init --repo "$REPO"
```

This is the slow step — it parses every Go file, clusters functions
structurally, persists to BadgerDB, and writes an initial (unenriched)
`$REPO/.beats/report.html`. Let the user know it may take a minute for large
repos.

If the command fails, surface the full error and stop. Common causes:
- Non-Go files that look like packages (e.g. generated seed files) — beats
  skips files where the `package` clause is missing or malformed, so this is
  usually harmless noise in logs.
- Permission errors on the temp DB path.

---

## Step 3 — Discover total cluster count

```bash
beats query cluster 0 --repo "$REPO" --format compact
```

The last line of compact output is always:

```
TOTAL:<n> PAGES:<n> PAGE:<n>
```

Parse `TOTAL` and `PAGES` from this line. Store as `$TOTAL_CLUSTERS` and
`$TOTAL_PAGES`. If TOTAL is 0, tell the user no clusters were found and stop.

---

## Step 4 — Enrich all clusters (page by page)

For each page `P` from `0` to `$TOTAL_PAGES - 1`:

### 4a — Fetch the page

```bash
beats query cluster $P --repo "$REPO" --format compact
```

Run this command exactly as shown. **No grep. No awk. No pipes. No for loops
across multiple pages.** One page, one command, full raw output. Every `M:`
line is a cluster member — do not filter them before reading.

Parse every cluster block between successive `---` delimiters. Each block has
the form:

```
IDX:<n> SIZE:<n> SCORE:<f> QUAD:<HH|HL|LH|LL>
SHAPE:<token sequence>
M:<FuncName>|<package>|<absolute_file_path>|<start_line>
M:...
---
```

Collect all clusters on this page as a list.

### 4b — Read source files for the whole page at once

Before generating any enrichment, do all file reads for the page in one pass.

For every cluster on the page, read one representative — pick the `M:` line
whose function name is most descriptive. Read lines `start_line` to
`start_line + 40` only. Do not read the whole file.

After all reads are done, proceed to 4c.

### 4c — Generate all enrichments for the page in one pass

Now produce enrichment for every cluster on the page in a single step — do not
interleave reads and writes. For each cluster generate:

| Field | Guidance |
|---|---|
| `--idiom` | 3–6 words naming the structural convention. Focus on *what the shape does*, not the domain. |
| `--verdict` | One sentence: what this cluster represents. |
| `--canonical` | `pkg/FuncName` of the best example member. |
| `--action` | `"none"` or a short specific note. Reserve non-none for genuine structural debt. |
| `--confidence` | `"high"` if SCORE > 0.80 and SIZE ≥ 3; `"medium"` if SCORE 0.65–0.80 or SIZE 2; `"low"` otherwise. |
| `--questions` | 5–8 pipe-separated natural-language questions covering developer intent — what someone would type to find this pattern. Include at least one junior phrasing and one senior phrasing. |

### 4d — Write all updates for the page in parallel

After all enrichments are generated, write them to a temp file — one cluster
per line, tab-separated:

```
IDX\tIDIOM\tVERDICT\tCANONICAL\tACTION\tCONFIDENCE\tQUESTIONS
```

`QUESTIONS` is pipe-separated within the field, e.g. `How is X done?|Where is Y wired?`

Do not include any literal tab or newline characters inside field values —
replace them with a space if needed.

Then call the bundled script in a single Bash tool call:

```bash
SCRIPT_DIR="$(dirname "$(claude plugin path beats)")/scripts"
bash "$SCRIPT_DIR/beats_update_page.sh" "$REPO" /tmp/beats_page_$P.tsv
```

The script fires one `beats update cluster` per line as a background job and
`wait`s for all of them — guaranteed parallel, not dependent on Claude's
discretion.

### 4e — Progress reporting

After completing a page: `Page P/$TOTAL_PAGES done
(clusters IDX_START–IDX_END of $TOTAL_CLUSTERS)`.

---

## Step 5 — Regenerate the HTML report

```bash
beats analyze --repo "$REPO"
```

This reads the now-enriched cluster data from the database and rewrites
`$REPO/.beats/report.html` with all the LLM metadata visible in the detail
panels.

---

## Step 6 — Surface the result

Tell the user:

> Analysis complete. `$TOTAL_CLUSTERS` clusters indexed and enriched.
> Report written to `$REPO/.beats/report.html` — open it in your browser to
> explore the structural patterns. Each cluster now shows an idiom name,
> verdict, canonical example, and suggested developer questions you can use
> for structural search.

If you have access to the `present_files` tool, present the report file so the
user can open it with one click.

---

## Error handling

- **beats init fails mid-run:** surface the slog error line and suggest the
  user check for non-Go files or permission issues on the temp directory.
- **beats update cluster fails:** log a warning and continue — a missed
  enrichment for one cluster does not block the rest.
- **beats analyze fails:** this is rare (the DB was just written). Surface the
  full error.
- **Binary not found after install:** remind the user to ensure `$(go env GOPATH)/bin`
  is on their PATH.

---

## What the tokens mean (quick reference for enrichment)

When reading the SHAPE token sequence, these are the most common tokens and
what they signal:

- `CALL` — local/builtin call; `CALL_PKG` — package-qualified call (e.g. `fmt.Sprintf`); `CALL_METHOD` — method on a variable
- `IF` — branch / guard; `FOR` — index loop; `RANGE` — range iteration
- `ASSIGN` — local state being built up; `RETURN` — one token per return value
- `DEFER` — cleanup / unlock; `GO` — goroutine spawn; `SEND` — channel write
- `FUNCLIT` — inline closure / callback; `SELECT` / `COMM` — channel multiplexing
- `SWITCH` / `CASE` — dispatch

A shape of `CALL_PKG IF RETURN RETURN` in a HH cluster is typically an
error-checking wrapper. `RANGE IF CALL_METHOD` is typically an iterator with
filtering. `DEFER CALL_METHOD GO FUNCLIT` is a goroutine launcher with cleanup.
Let the actual code confirm the pattern.
