#!/usr/bin/env python3
"""verify_resolutions.py — ground-truth verification for beats release-study
outlier "resolutions".

The release study says an outlier was "resolved" when it stops appearing in the
next release's outlier list. That has many causes, and only one of them
validates beats. This tool classifies every resolution event using git itself:

  FIXED_TOWARD_CONVENTION  code changed, fix-flavoured commits touched it, and
                           its shape moved toward the cluster's common shape
  LIKELY_FIX               code changed and fix-flavoured commits touched it
  REFACTOR                 code changed; commit language is refactor/cleanup
  REQUIREMENT_CHANGE       code changed; commit language is feature/behavioral
  DELETED / MOVED          function no longer exists at that path
  BEATS_ARTIFACT           function code did NOT change — the flip came from
                           the corpus side (cluster dissolved/shifted, threshold
                           interplay). These must not be counted as ground truth.
  AMBIGUOUS                changed, but evidence is mixed — needs a human

It also re-derives release order by git creator date and reports how many of the
study's resolution events were artifacts of wrong tag ordering (git's default
version sort places v1.2.3 BEFORE v1.2.3-rc1; prereleases and junk tags like
v2-catalog-test corrupt consecutive-pair comparisons).

Usage:
  python3 verify_resolutions.py --study /tmp/beats-release-study/etcd
  # options: --order chrono|study   (default chrono)
  #          --report FILE          (default <study>/verification-report.md)

Requires: the study dir produced by release-study.sh (outliers/*.json snapshots
and the repo clone at <study>/repo with its tags intact). Stdlib only.
"""

import argparse
import csv
import difflib
import json
import os
import re
import subprocess
import sys
from datetime import datetime

FIX_RE = re.compile(r"\b(fix(es|ed)?|bug|race|leak|deadlock|panic|crash|correct|regress\w*|CVE-\d)", re.I)
REFACTOR_RE = re.compile(r"\b(refactor\w*|clean\s?up|simplify|rename|restructure|reorganiz\w*|lint|gofmt|style|typo|comment)", re.I)
FEAT_RE = re.compile(r"\b(feat\w*|add(s|ed)?|support|implement\w*|introduc\w*|allow|enable|new)\b", re.I)


def sh(args, cwd=None, ok_fail=False):
    r = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if r.returncode != 0 and not ok_fail:
        raise RuntimeError(f"{' '.join(args)}: {r.stderr.strip()[:300]}")
    return r.stdout


# ── function extraction (same engine validated against beats in the ablation) ─
def strip_go(src):
    out = list(src)
    i, n, mode = 0, len(src), 0
    while i < n:
        c = src[i]
        if mode == 0:
            if c == "/" and i + 1 < n and src[i + 1] == "/":
                mode = 1; out[i] = out[i + 1] = " "; i += 2; continue
            if c == "/" and i + 1 < n and src[i + 1] == "*":
                mode = 2; out[i] = out[i + 1] = " "; i += 2; continue
            if c == '"': mode = 3
            elif c == "`": mode = 4
            elif c == "'": mode = 5
        elif mode == 1:
            if c == "\n": mode = 0
            else: out[i] = " "
        elif mode == 2:
            if c == "*" and i + 1 < n and src[i + 1] == "/":
                out[i] = out[i + 1] = " "; mode = 0; i += 2; continue
            if c != "\n": out[i] = " "
        elif mode == 3:
            if c == "\\" and i + 1 < n: out[i] = out[i + 1] = " "; i += 2; continue
            if c == '"': mode = 0
            elif c != "\n": out[i] = " "
        elif mode == 4:
            if c == "`": mode = 0
            elif c != "\n": out[i] = " "
        elif mode == 5:
            if c == "\\" and i + 1 < n: out[i] = out[i + 1] = " "; i += 2; continue
            if c == "'": mode = 0
            else: out[i] = " "
        i += 1
    return "".join(out)


def find_function(file_src, name):
    """Return (raw_body, stripped_body) of func <name> in file source, or None."""
    stripped = strip_go(file_src)
    for m in re.finditer(r"^func\s+(?:\([^)]*\)\s+)?(%s)\s*\(" % re.escape(name), stripped, re.M):
        brace = stripped.find("{", m.end() - 1)
        if brace == -1: continue
        depth, i, end = 0, brace, -1
        while i < len(stripped):
            if stripped[i] == "{": depth += 1
            elif stripped[i] == "}":
                depth -= 1
                if depth == 0: end = i; break
            i += 1
        if end == -1: continue
        return file_src[m.start():end + 1], stripped[m.start():end + 1]
    return None


def norm(body):
    """Whitespace/comment-insensitive normal form for changed/unchanged check."""
    return re.sub(r"\s+", " ", strip_go(body)).strip()


def coarse_tokens(stripped_body):
    """Coarse structural token stream. Not beats' exact tokenizer — used only to
    measure DIRECTION of shape movement, with the same lens on old and new."""
    toks = []
    for m in re.finditer(
            r"\b(if|for|range|switch|case|select|return|go|defer|continue|break|goto)\b"
            r"|:=|(?<![=!<>:+\-*/%&|^])=(?!=)|\w+(?:\.\w+)*\s*\(", stripped_body):
        t = m.group(0)
        if t.endswith("("):
            toks.append("CALL_Q" if "." in t else "CALL")
        elif t == ":=" or t == "=":
            toks.append("ASSIGN")
        else:
            toks.append(t.upper())
    return toks


def seqsim(a, b):
    if not a and not b: return 1.0
    return difflib.SequenceMatcher(a=a, b=b).ratio()


# ── study loading ─────────────────────────────────────────────────────────────
def load_snapshots(study):
    snaps = {}
    odir = os.path.join(study, "outliers")
    for f in sorted(os.listdir(odir)):
        m = re.match(r"outliers-(.+)\.json$", f)
        if not m: continue
        try:
            data = json.load(open(os.path.join(odir, f)))
        except json.JSONDecodeError:
            data = []
        snaps[m.group(1)] = data if isinstance(data, list) else []
    return snaps


def study_order(study):
    path = os.path.join(study, "releases.csv")
    if not os.path.exists(path): return []
    rows = list(csv.DictReader(open(path)))
    rows.sort(key=lambda r: int(r["release_idx"]))
    return [r["release"] for r in rows]


def chrono_order(repo, tags):
    dated = []
    for t in tags:
        out = sh(["git", "-C", repo, "log", "-1", "--format=%ct", t], ok_fail=True).strip()
        dated.append((int(out) if out.isdigit() else 0, t))
    dated.sort()
    return [t for _, t in dated]


LINE_RE = re.compile(r"v?(\d+)\.(\d+)")


def release_line(tag):
    m = LINE_RE.match(tag)
    return (int(m.group(1)), int(m.group(2))) if m else (-1, -1)


def build_chains(repo, tags):
    """Group tags into release lines (major.minor), order each line by tag
    creation date, and connect the end of one line to the start of the next
    ONLY when both version and time move forward. Parallel maintenance lines
    (etcd's 3.6.x patches shipping alongside 3.7 pre-releases) therefore stay
    separate chains instead of braiding — braided chains re-detect the same
    one-time event at every branch crossing."""
    date = {}
    for t in tags:
        out = sh(["git", "-C", repo, "log", "-1", "--format=%ct", t], ok_fail=True).strip()
        date[t] = int(out) if out.isdigit() else 0

    lines = {}
    for t in tags:
        lines.setdefault(release_line(t), []).append(t)
    for k in lines:
        lines[k].sort(key=lambda t: date[t])
    ordered = sorted(lines, key=lambda k: date[lines[k][0]])

    chains, cur = [], list(lines[ordered[0]])
    for prev, nxt in zip(ordered, ordered[1:]):
        if nxt > prev and date[lines[nxt][0]] > date[lines[prev][-1]]:
            cur += lines[nxt]        # sequential successor line — extend chain
        else:
            chains.append(cur)       # parallel or backport line — new chain
            cur = list(lines[nxt])
    chains.append(cur)
    return chains


def rel_file(path):
    """Snapshot paths are absolute on the machine that ran the study; the repo
    was always cloned to .../repo/, so take the part after '/repo/'."""
    i = path.find("/repo/")
    return path[i + len("/repo/"):] if i != -1 else path


def key_of(o):
    return (o.get("package", ""), o.get("func", ""), rel_file(o.get("file", "")))


# ── per-event verification ────────────────────────────────────────────────────
def commits_touching(repo, tag_a, tag_b, relpath, func):
    """Commit subjects between tags touching the function (line-log) with a
    file-level fallback."""
    out = sh(["git", "-C", repo, "log", f"-L:{func}:{relpath}",
              "--format=%h %s", f"{tag_a}..{tag_b}"], ok_fail=True)
    subjects = re.findall(r"^[0-9a-f]{7,}\s+(.+)$", out, re.M)
    if not subjects:
        out = sh(["git", "-C", repo, "log", "--format=%h %s",
                  f"{tag_a}..{tag_b}", "--", relpath], ok_fail=True)
        subjects = re.findall(r"^[0-9a-f]{7,}\s+(.+)$", out, re.M)
    return subjects[:30]


def classify(subjects):
    fix = sum(1 for s in subjects if FIX_RE.search(s))
    ref = sum(1 for s in subjects if REFACTOR_RE.search(s))
    feat = sum(1 for s in subjects if FEAT_RE.search(s))
    return fix, ref, feat


def verify_event(repo, tag_a, tag_b, outlier, next_snapshot):
    pkg, func = outlier.get("package", ""), outlier.get("func", "")
    relpath = rel_file(outlier.get("file", ""))
    old_body = outlier.get("body", "")
    cands = outlier.get("candidates", [])
    cluster = cands[0].get("cluster_id", "") if cands else ""
    lcs = (cands[0].get("common_shape", "") if cands else "").split()

    ev = dict(package=pkg, func=func, file=relpath, outlier_in=tag_a,
              gone_by=tag_b, cluster=cluster, verdict="", evidence="")

    # 1. does the function still exist at tag_b?
    src_b = sh(["git", "-C", repo, "show", f"{tag_b}:{relpath}"], ok_fail=True)
    hit = find_function(src_b, func) if src_b else None
    moved = False
    if hit is None:
        # search repo-wide (renamed file / moved package)
        out = sh(["git", "-C", repo, "grep", "-l", "-E",
                  r"^func\s+(\([^)]*\)\s+)?%s\s*\(" % re.escape(func), tag_b], ok_fail=True)
        files = [l.split(":", 1)[1] for l in out.strip().splitlines() if ":" in l]
        for f in files:
            src_b = sh(["git", "-C", repo, "show", f"{tag_b}:{f}"], ok_fail=True)
            # a bare name match across the repo is not enough — Validate/Parse/New
            # exist everywhere. Require the file to declare the same package.
            if not re.search(r"^package\s+%s\b" % re.escape(pkg), src_b, re.M):
                continue
            hit = find_function(src_b, func)
            if hit:
                moved, relpath_new = True, f
                break
        if hit is None:
            ev["verdict"] = "DELETED"
            ev["evidence"] = "function not found anywhere at " + tag_b
            return ev

    raw_b, stripped_b = hit

    # 2. did the code actually change?
    if old_body and norm(old_body) == norm(raw_b):
        alive = any(c.get("cluster_id") == cluster
                    for o in next_snapshot for c in o.get("candidates", []))
        ev["verdict"] = "BEATS_ARTIFACT"
        ev["evidence"] = ("code identical at both tags; corpus-side flip "
                          f"(candidate cluster {'still' if alive else 'no longer'} "
                          f"attracts outliers at {tag_b})")
        return ev

    # 3. changed — gather evidence
    old_hit = find_function(strip_go(old_body) and old_body, func)
    old_stripped = old_hit[1] if old_hit else strip_go(old_body)
    t_old, t_new = coarse_tokens(old_stripped), coarse_tokens(stripped_b)
    movement = None
    if lcs:
        movement = seqsim(t_new, lcs) - seqsim(t_old, lcs)
    subjects = commits_touching(repo, tag_a, tag_b, relpath, func)
    fix, ref, feat = classify(subjects)
    diff_ratio = 1.0 - difflib.SequenceMatcher(
        a=norm(old_body), b=norm(raw_b)).ratio()

    bits = [f"changed (≈{diff_ratio*100:.0f}% of normalized text)",
            f"commits touching it: {len(subjects)} (fix={fix} refactor={ref} feat={feat})"]
    if movement is not None:
        bits.append(f"shape moved {'toward' if movement > 0 else 'away from'} "
                    f"cluster LCS by {movement:+.2f} (coarse)")
    if moved:
        bits.append(f"file moved to {relpath_new}")
    if subjects:
        bits.append("top commits: " + " | ".join(subjects[:3]))
    ev["evidence"] = "; ".join(bits)

    # 4. verdict
    if moved and diff_ratio < 0.1:
        ev["verdict"] = "MOVED"
    elif fix > 0 and movement is not None and movement > 0.05:
        ev["verdict"] = "FIXED_TOWARD_CONVENTION"
    elif fix > 0 and fix >= max(ref, feat):
        ev["verdict"] = "LIKELY_FIX"
    elif feat > max(fix, ref):
        ev["verdict"] = "REQUIREMENT_CHANGE"
    elif ref > 0:
        ev["verdict"] = "REFACTOR"
    else:
        ev["verdict"] = "AMBIGUOUS"
    return ev


# ── main ──────────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--study", required=True, help="study dir from release-study.sh")
    ap.add_argument("--order", choices=["lines", "chrono", "study"], default="lines",
                    help="lines (default): per release-line chains, no branch braiding; "
                         "chrono: single time-ordered chain; study: order as studied")
    ap.add_argument("--report", help="default <study>/verification-report.md")
    args = ap.parse_args()

    study = os.path.abspath(args.study)
    repo = os.path.join(study, "repo")
    if not os.path.isdir(os.path.join(repo, ".git")):
        sys.exit(f"repo clone with tags required at {repo}")

    snaps_raw = load_snapshots(study)
    tags_study = [t for t in study_order(study) if t in snaps_raw] or sorted(snaps_raw)
    tags_chrono = chrono_order(repo, tags_study)
    tags = tags_chrono if args.order == "chrono" else tags_study

    order_mismatch = tags_study != tags_chrono
    snaps = {t: {key_of(o): o for o in snaps_raw[t]} for t in tags_study}
    chains = build_chains(repo, tags_study)

    def events_for(order):
        evs = []
        for i in range(len(order) - 1):
            a, b = order[i], order[i + 1]
            for k, o in snaps[a].items():
                if k not in snaps[b]:
                    evs.append((a, b, o))
        return evs

    if args.order == "lines":
        events = [e for c in chains for e in events_for(c)]
    else:
        events = events_for(tags)
    events_study = events_for(tags_study)

    print(f"[verify] releases (study order):  {' '.join(tags_study)}")
    print(f"[verify] releases (chrono order): {' '.join(tags_chrono)}")
    print(f"[verify] release-line chains:     {'  |  '.join(' → '.join(c) for c in chains)}")
    if order_mismatch:
        print(f"[verify] WARNING: study order != chronological order — "
              f"{len(events_study)} events under study order vs {len(events)} "
              f"under {args.order} order; the difference is ordering/braiding artifacts")
    if args.order == "lines" and len(chains) > 1:
        print(f"[verify] NOTE: {len(chains)} parallel release lines detected — "
              f"cross-line comparisons are excluded (they re-detect one-time "
              f"events at every branch crossing)")
    print(f"[verify] verifying {len(events)} resolution events ({args.order} order)\n")

    results = []
    for a, b, o in events:
        ev = verify_event(repo, a, b, o, snaps_raw[b])
        results.append(ev)
        print(f"  {ev['verdict']:24} {ev['package']}/{ev['func']}  ({a} → {b})")
        print(f"      {ev['evidence'][:200]}")

    # write CSV + report
    out_csv = os.path.join(study, "verified-resolutions.csv")
    with open(out_csv, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["package", "func", "file", "outlier_in",
                                          "gone_by", "cluster", "verdict", "evidence"])
        w.writeheader()
        w.writerows(results)

    counts = {}
    for r in results:
        counts[r["verdict"]] = counts.get(r["verdict"], 0) + 1
    ground_truth = sum(counts.get(k, 0) for k in
                       ("FIXED_TOWARD_CONVENTION", "LIKELY_FIX"))
    artifacts = counts.get("BEATS_ARTIFACT", 0)

    report = args.report or os.path.join(study, "verification-report.md")
    with open(report, "w") as f:
        f.write(f"""# resolution verification — {os.path.basename(study)}
generated: {datetime.now().isoformat()}
order used: {args.order}{' (study order was NOT chronological — see warning)' if order_mismatch else ''}

## Verdict distribution over {len(results)} resolution events
{chr(10).join(f'- {k}: {v}' for k, v in sorted(counts.items(), key=lambda x: -x[1]))}

**Defensible ground-truth resolutions (fix-class): {ground_truth} / {len(results)}**
**Corpus-side artifacts (must NOT be claimed as fixes): {artifacts}**

## Ordering check
study order:  {' '.join(tags_study)}
chrono order: {' '.join(tags_chrono)}
release-line chains (used by --order lines): {'  |  '.join(' → '.join(c) for c in chains)}
{f"MISMATCH — {abs(len(events_study)-len(events))} event-count difference vs study order; "
 f"prefer 'lines': parallel maintenance lines braided into one chain re-detect each "
 f"one-time event at every branch crossing." if order_mismatch else "orders agree."}

## Per-event evidence
""")
        for r in results:
            f.write(f"### {r['package']}/{r['func']}  ({r['outlier_in']} → {r['gone_by']})\n"
                    f"- file: {r['file']}\n- cluster: {r['cluster']}\n"
                    f"- **{r['verdict']}** — {r['evidence']}\n\n")
        f.write("""## How to read this
Only FIXED_TOWARD_CONVENTION and LIKELY_FIX support the claim "developers fixed
what beats flagged". REFACTOR and REQUIREMENT_CHANGE are code churn that happened
to remove the outlier. BEATS_ARTIFACT means the function did not change at all —
the outlier list flipped for corpus-side reasons; counting these as resolutions
overstates the tool. AMBIGUOUS events need a human (or a blind LLM adjudication
pass — see beats_ablation.py's judge pattern) before they count either way.
""")
    print(f"\n[verify] {out_csv}")
    print(f"[verify] {report}")
    print(f"[verify] fix-class {ground_truth}/{len(results)}, artifacts {artifacts}/{len(results)}")


if __name__ == "__main__":
    main()
