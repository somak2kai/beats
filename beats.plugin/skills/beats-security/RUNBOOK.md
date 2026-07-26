# beats-security — run it yourself

Index in your terminal (cheap, no LLM), then fire the skill in Claude to triage.
This mirrors beats' own "index outside Claude, analyse inside" workflow.

---

## 0. One-time setup

```bash
# a) beats binary on PATH
go install github.com/somak2kai/beats/cmd@latest   # or: brew install somak2kai/tap/beats
beats version

# b) make the skill available to Claude Code (local install, no push needed)
#    point the marketplace at the repo dir that holds .claude-plugin/marketplace.json
```
Then inside Claude Code:
```
/plugin marketplace add /Users/admin/ws/golang/beats
/plugin install beats@beats
```
You now have two skills: `beats-analyze` (structural) and `beats-security` (this one).

---

## 1. The pattern (per repo)

```bash
# 1. clone + pin to a KNOWN-VULNERABLE ref
git clone <url> /tmp/tc-<name> && cd /tmp/tc-<name>
git checkout <affected-ref>

# 2. index in the terminal — writes .beats/outlier.md and .beats/report.html
beats init --repo /tmp/tc-<name>
```
Then in Claude Code, fire the skill (it runs the cluster collector, the three
analysis passes, and the matrix renderer):
```
beats security /tmp/tc-<name>
```
Outputs land in `/tmp/tc-<name>/.beats/`: **`security.md`** (ranked CWE findings +
matrix) and **`security-matrix.html`** (the heat-map).

Headless / unattended variant:
```bash
claude -p "beats security /tmp/tc-<name>" --dangerously-skip-permissions
```
> `--dangerously-skip-permissions` auto-approves the skill's bash/python steps.
> Only use it on repos you trust. Interactive mode prompts you instead.

---

## 2. All nine repos — clone, pin, index

Copy-paste. Each pins a ref that OSV confirms is vulnerable. Large repos
(grafana, minio, argo-cd) take a few minutes to index.

```bash
mkdir -p /tmp/beatsec && cd /tmp/beatsec

# ── original corpus (from CWE_OSV_BEATS_ANALYSIS.md) ────────────────────────
# 1. otel schema — CWE-772 missing defer Close (SYSTEMIC: v1.0 + v1.1)
git clone https://github.com/open-telemetry/opentelemetry-go otel && cd otel
git checkout f12d198f161b61735d65705248715aa97021ba8d~1   # parent of the fix = vulnerable
beats init --repo /tmp/beatsec/otel/schema                # point at schema/ for the two ParseFile peers
cd ..

# 2. gorest — CWE-362 unsynchronized 2FA map
git clone https://github.com/pilinux/gorest && cd gorest
git checkout v1.12.1                                       # <= 1.12.1 affected
beats init --repo /tmp/beatsec/gorest
cd ..

# 3. beego — CWE-327 MD5 cache filenames (beats-blind → Pass B)
git clone https://github.com/beego/beego && cd beego
git checkout v2.3.3                                        # before v2.3.4 fix
beats init --repo /tmp/beatsec/beego
cd ..

# 4. go-pg/pg — CWE-89 SQL injection (beats-blind → Pass B)
git clone https://github.com/go-pg/pg && cd pg
git checkout v10.13.0                                      # before v10.15.0 fix
beats init --repo /tmp/beatsec/pg
cd ..

# 5. go-ipld-prime — CWE-674 unbounded recursion depth
git clone https://github.com/ipld/go-ipld-prime && cd go-ipld-prime
git checkout v0.21.0                                       # any tag < v0.23.0
beats init --repo /tmp/beatsec/go-ipld-prime
cd ..

# ── extended corpus (outside the CWE analysis) ─────────────────────────────
# 6. grafana — CWE-22 plugin path traversal (big repo)
git clone https://github.com/grafana/grafana && cd grafana
git checkout v8.3.0                                        # before v8.3.1 fix
beats init --repo /tmp/beatsec/grafana
cd ..

# 7. argo-cd — CWE-59 symlink file leak (big repo)
git clone https://github.com/argoproj/argo-cd && cd argo-cd
git checkout v2.3.3                                        # before v2.3.4 fix
beats init --repo /tmp/beatsec/argo-cd
cd ..

# 8. gogs — CWE-88/78 SSH argument-injection RCE (beats-blind → Pass B)
git clone https://github.com/gogs/gogs && cd gogs
git checkout v0.13.0                                       # through 0.13.0 affected
beats init --repo /tmp/beatsec/gogs
cd ..

# 9. minio — CWE-200 env info disclosure (stretch / false-positive calibration)
git clone https://github.com/minio/minio && cd minio
git checkout RELEASE.2023-03-13T19-46-17Z                  # before the 2023-03-20 fix
beats init --repo /tmp/beatsec/minio
cd ..
```

> If a pinned tag ever fails to resolve (projects retag), pick any release below
> the "fixed" version in `TEST_CASES.md` — the vulnerability is present across
> the whole affected range.

---

## 3. Fire the skill (per repo)

Interactive Claude Code — just name the path:
```
beats security /tmp/beatsec/otel/schema
beats security /tmp/beatsec/gorest
beats security /tmp/beatsec/beego
beats security /tmp/beatsec/pg
beats security /tmp/beatsec/go-ipld-prime
beats security /tmp/beatsec/grafana
beats security /tmp/beatsec/argo-cd
beats security /tmp/beatsec/gogs
beats security /tmp/beatsec/minio
```

Then open the matrix, e.g.:
```bash
open /tmp/beatsec/grafana/.beats/security-matrix.html
cat  /tmp/beatsec/grafana/.beats/security.md
```

---

## 4. Manual fallback (skip Claude, run the scripts directly)

If you want the systemic cluster dump or the matrix without firing the skill —
useful for debugging — call the bundled scripts with their explicit path:

```bash
SEC=/Users/admin/ws/golang/beats/beats.plugin/skills/beats-security/scripts
REPO=/tmp/beatsec/otel/schema

# enumerate every cluster (with bodies) from report.html for the systemic pass
bash "$SEC/collect_clusters.sh" "$REPO" > "$REPO/.beats/security.clusters.json"

# after Claude has written security.findings.json, render the deliverables
python3 "$SEC/render_matrix.py" "$REPO/.beats/security.findings.json" \
  --md "$REPO/.beats/security.md" --html "$REPO/.beats/security-matrix.html"
```

The three analysis passes themselves need Claude (they are the semantic verdict
beats deliberately doesn't compute) — the scripts only handle the deterministic
collection and rendering around them.
