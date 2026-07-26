#!/usr/bin/env python3
"""Render beats-security findings into a cluster x CWE matrix.

Input : security.findings.json  (the JSON object emitted by the skill's Step 4)
Output: security.md              (ranked findings + inline matrix table)
        security-matrix.html     (self-contained heat-map, no external deps)

Columns are FULLY DATA-DRIVEN: one column per distinct CWE the run produced,
labelled from each finding's own `cwe_name`, ordered by CWE number. There is no
hardcoded CWE taxonomy — nothing to maintain and nothing to drop. In the HTML,
a column header is tinted by the pass that surfaced it (A=structural,
B=semantic, C=systemic). Deterministic and dependency-free (Python stdlib only).
"""
import argparse
import html
import json
import sys
from collections import defaultdict

SEV_ORDER = {"High": 3, "Medium": 2, "Low": 1, "Info": 0}
SEV_COLOR = {"High": "#e5484d", "Medium": "#e79c31", "Low": "#e6c229", "Info": "#7a8493"}
SEV_MARK = {"High": "H", "Medium": "M", "Low": "L", "Info": "i"}

# Band is derived from the finding's pass, not a hardcoded CWE list.
PASS_BAND = {"A": "structural", "B": "semantic", "C": "systemic"}
BAND_COLOR = {"structural": "#79c0ff", "semantic": "#ff9bce",
              "systemic": "#c9a3ff", "other": "#8b949e"}


def num(cwe):
    return "".join(ch for ch in str(cwe if cwe is not None else "") if ch.isdigit())


def col_key(f):
    n = num(f.get("cwe"))
    return f"CWE-{n}" if n else ((f.get("cwe") or "").strip() or "CWE-?")


def col_short(col):
    return num(col) or col


def sev_rank(f):
    return SEV_ORDER.get(f.get("severity", "Info"), 0)


def row_key(f):
    cid = (f.get("cluster_id") or "").strip()
    return cid if cid else (f.get("package") or "(unclustered)")


def build(findings):
    """cells[row][col] -> [findings]; ordered cols; col -> label; col -> band.
    Columns and labels come entirely from the findings themselves."""
    cells = defaultdict(lambda: defaultdict(list))
    labels, passes = {}, defaultdict(lambda: defaultdict(int))
    for f in findings:
        c = col_key(f)
        cells[row_key(f)][c].append(f)
        if not labels.get(c):
            labels[c] = f.get("cwe_name") or c
        passes[c][f.get("pass") or "?"] += 1
    cols = sorted({c for r in cells.values() for c in r},
                  key=lambda c: (int(num(c) or 10 ** 9), c))
    bands = {}
    for c in cols:
        dom = max(passes[c].items(), key=lambda kv: kv[1])[0] if passes[c] else "?"
        bands[c] = PASS_BAND.get(dom, "other")
    return cells, cols, labels, bands


def load(path):
    with open(path) as fh:
        data = json.load(fh)
    if isinstance(data, list):
        data = {"findings": data}
    data.setdefault("findings", [])
    return data


# ── markdown ────────────────────────────────────────────────────────────────
def render_md(data):
    findings = data.get("findings", [])
    cells, cols, labels, _bands = build(findings)
    counts = defaultdict(int)
    for f in findings:
        counts[f.get("severity", "Info")] += 1

    out = [f"# beats security matrix — {data.get('repo','')}", ""]
    out.append(f"**{len(findings)} findings** · High {counts['High']} · "
               f"Medium {counts['Medium']} · Low {counts['Low']} · Info {counts['Info']}")
    lev, ver, kn = defaultdict(int), defaultdict(int), defaultdict(int)
    for f in findings:
        lev[f.get("beats_leverage", "?")] += 1
        ver[(f.get("verification") or {}).get("status", "?")] += 1
        kn[f.get("known_status", "?")] += 1
    if findings:
        out.append("")
        out.append(f"Verification: {ver['confirmed']} confirmed · {ver['partial']} partial"
                   + (f" · {ver['refuted']} refuted" if ver.get("refuted") else "")
                   + f"  |  beats leverage: {lev['unique']} unique · "
                   f"{lev['assisted']} assisted · {lev['incidental']} incidental")
        if any(f.get("known_status") for f in findings):
            out.append(f"Known-CVE cross-check: {kn['novel']} novel · "
                       f"{kn['variant-of-known']} variant-of-known · "
                       f"{kn['already-fixed']} already-fixed"
                       + (f" · {kn['cross-check-unavailable']} unchecked"
                          if kn.get("cross-check-unavailable") else ""))
    systemic = [f for f in findings if f.get("pass") == "C"]
    if systemic:
        out += ["", f"> {len(systemic)} systemic (Pass C) finding(s): a shared convention "
                    f"is insecure, so the blast radius spans every cluster member."]
    out += ["", "_Triage only — structural targeting + semantic reading, not proof of "
                "exploitability. Confirm with govulncheck and CodeQL/gosec._", ""]

    if not findings:
        out.append("_No findings._")
        return "\n".join(out)

    out += ["## Cluster × CWE", ""]
    out.append("| cluster | " + " | ".join(col_short(c) for c in cols) + " |")
    out.append("|" + "---|" * (len(cols) + 1))
    for rk in sorted(cells):
        row = [f"`{rk}`"]
        for c in cols:
            fs = cells[rk].get(c, [])
            if fs:
                worst = max(fs, key=sev_rank)
                row.append(f"{SEV_MARK[worst.get('severity', 'Info')]}{len(fs)}")
            else:
                row.append("·")
        out.append("| " + " | ".join(row) + " |")
    out.append("")
    out.append("Columns present: " + " · ".join(
        f"{col_short(c)} = {labels[c]}" for c in cols))
    out += ["", "Legend: H/M/L/i = worst severity in cell · number = finding count. "
                "Columns are the CWE classes this run produced, ordered by number.", ""]

    out += ["## Findings (ranked)", ""]
    ordered = sorted(findings, key=lambda f: (sev_rank(f),
                     SEV_ORDER.get(f.get("confidence", "Low"), 0)), reverse=True)
    for f in ordered:
        out.append(f"### {f.get('id','')} · {f.get('cwe','')} {f.get('cwe_name','')} — "
                   f"{f.get('severity','')}/{f.get('confidence','')} conf · pass {f.get('pass','')}")
        out.append("")
        fn = f"`{f.get('package','')}/{f.get('function','')}`"
        loc = f.get("location", "")
        out.append(f"- **Where**: {fn} ({loc})" +
                   (f" · cluster `{f.get('cluster_id')}`" if f.get("cluster_id") else ""))
        out.append(f"- **Signal**: {f.get('signal','')}")
        out.append(f"- **Why**: {f.get('reason','')}")
        if f.get("assumption"):
            out.append(f"- **Assumption**: {f['assumption']}")
        br = f.get("blast_radius") or []
        if len(br) > 1:
            out.append(f"- **Blast radius** ({len(br)}): " + ", ".join(f"`{x}`" for x in br))
        out.append(f"- **Fix**: {f.get('suggested_fix','')}")
        out.append(f"- **Confirm with**: {f.get('confirm_with','govulncheck / human review')}")
        v = f.get("verification") or {}
        out.append(f"- **Verified**: {v.get('status','?')}"
                   + (f" — {v['corrected_location']}" if v.get("corrected_location") else "")
                   + f" · **beats leverage**: {f.get('beats_leverage','?')} "
                   f"({f.get('sast_overlap','?')} SAST overlap)")
        if f.get("beats_leverage_reason"):
            out.append(f"- **Why beats (or not)**: {f['beats_leverage_reason']}")
        ks = f.get("known_status")
        if ks:
            ra = f.get("related_advisories") or []
            ra_s = "; ".join((a.get("id") if isinstance(a, dict) else str(a)) for a in ra)
            out.append(f"- **Known status**: {ks}" + (f" — {ra_s}" if ra_s else ""))
        out.append("")
    return "\n".join(out)


# ── html ──────────────────────────────────────────────────────────────────────
def render_html(data):
    findings = data.get("findings", [])
    cells, cols, labels, bands = build(findings)
    counts = defaultdict(int)
    lev, ver, kn = defaultdict(int), defaultdict(int), defaultdict(int)
    for f in findings:
        counts[f.get("severity", "Info")] += 1
        lev[f.get("beats_leverage", "?")] += 1
        ver[(f.get("verification") or {}).get("status", "?")] += 1
        kn[f.get("known_status", "?")] += 1
    _ref = f" · {ver['refuted']} refuted" if ver.get("refuted") else ""
    ver_sub_html = (f'<p class="sub">verified: {ver["confirmed"]} confirmed · '
                    f'{ver["partial"]} partial{_ref} · beats leverage: {lev["unique"]} unique / '
                    f'{lev["assisted"]} assisted / {lev["incidental"]} incidental</p>') if findings else ""
    known_sub_html = (f'<p class="sub">known-CVE cross-check: {kn["novel"]} novel / '
                      f'{kn["variant-of-known"]} variant-of-known / {kn["already-fixed"]} already-fixed</p>'
                      ) if any(f.get("known_status") for f in findings) else ""

    def esc(x):
        return html.escape(str(x if x is not None else ""))

    th = ["<th class='rk'>cluster</th>"]
    for c in cols:
        th.append(f"<th style='color:{BAND_COLOR[bands[c]]}' title='{esc(labels[c])}'>"
                  f"{esc(col_short(c))}</th>")
    thead = "<tr>" + "".join(th) + "</tr>"

    body_rows = []
    for rk in sorted(cells):
        tds = [f"<td class='rk'><code>{esc(rk)}</code></td>"]
        for c in cols:
            fs = cells[rk].get(c, [])
            if not fs:
                tds.append("<td class='empty'></td>")
                continue
            worst = max(fs, key=sev_rank)
            sev = worst.get("severity", "Info")
            tip = esc(f"{worst.get('cwe','')} {worst.get('function','')} — {worst.get('reason','')}")
            tds.append(f"<td class='cell' style='background:{SEV_COLOR[sev]}' title='{tip}'>"
                       f"{len(fs)}</td>")
        body_rows.append("<tr>" + "".join(tds) + "</tr>")

    keyrow = " · ".join(
        f"<b style='color:{BAND_COLOR[bands[c]]}'>{esc(col_short(c))}</b> {esc(labels[c])}"
        for c in cols)

    ordered = sorted(findings, key=lambda f: (sev_rank(f),
                     SEV_ORDER.get(f.get("confidence", "Low"), 0)), reverse=True)
    frows = []
    VER_COLOR = {"confirmed": "#2ea043", "partial": "#e79c31", "refuted": "#e5484d"}
    for f in ordered:
        sev = f.get("severity", "Info")
        vst = (f.get("verification") or {}).get("status", "?")
        lvg = f.get("beats_leverage", "?")
        br = f.get("blast_radius") or []
        br_html = (f"<div class='br'>blast radius ({len(br)}): {esc(', '.join(br))}</div>"
                   if len(br) > 1 else "")
        assum = (f"<div class='as'>assumes: {esc(f['assumption'])}</div>"
                 if f.get("assumption") else "")
        lev_html = (f"<div class='lev'>beats leverage: <b>{esc(lvg)}</b> "
                    f"({esc(f.get('sast_overlap','?'))} SAST overlap) — {esc(f.get('beats_leverage_reason',''))}</div>"
                    if f.get("beats_leverage_reason") else "")
        ks = f.get("known_status", "")
        ra = f.get("related_advisories") or []
        ra_s = ", ".join((a.get("id") if isinstance(a, dict) else str(a)) for a in ra)
        known_pill = f'<span class="pill known">{esc(ks)}</span>' if ks else ""
        known_html = (f"<div class='known'>known: <b>{esc(ks)}</b>"
                      + (f" — {esc(ra_s)}" if ra_s else "") + "</div>") if ks else ""
        frows.append(f"""
        <div class="finding">
          <div class="fh">
            <span class="pill" style="background:{SEV_COLOR[sev]}">{esc(sev)}</span>
            <span class="pill conf">{esc(f.get('confidence',''))} conf</span>
            <span class="pill pass">pass {esc(f.get('pass',''))}</span>
            <span class="pill" style="background:{VER_COLOR.get(vst,'#30363d')}">{esc(vst)}</span>
            <span class="pill lev">beats: {esc(lvg)}</span>
            {known_pill}
            <span class="cwe">{esc(f.get('cwe',''))} · {esc(f.get('cwe_name',''))}</span>
          </div>
          <div class="loc"><code>{esc(f.get('package',''))}/{esc(f.get('function',''))}</code>
            <span class="file">{esc(f.get('location',''))}</span>
            {f"<span class='cid'>cluster {esc(f.get('cluster_id'))}</span>" if f.get('cluster_id') else ""}</div>
          <div class="sig"><b>signal</b> {esc(f.get('signal',''))}</div>
          <div class="why">{esc(f.get('reason',''))}</div>
          {assum}{br_html}{lev_html}{known_html}
          <div class="fix"><b>fix</b> {esc(f.get('suggested_fix',''))}
            <span class="cw">confirm: {esc(f.get('confirm_with','govulncheck / human'))}</span></div>
        </div>""")

    legend = ("Columns present: " + keyrow +
              " &nbsp;·&nbsp; header colour = pass that surfaced it "
              "(<b style='color:#79c0ff'>A structural</b>, "
              "<b style='color:#ff9bce'>B semantic</b>, "
              "<b style='color:#c9a3ff'>C systemic</b>)")
    matrix = (f"<table><thead>{thead}</thead><tbody>{''.join(body_rows)}</tbody></table>"
              f"<p class='legend'>{legend}</p>"
              if findings else "<p class='sub'>No findings.</p>")

    return f"""<!doctype html><html><head><meta charset="utf-8">
<title>beats security matrix — {esc(data.get('repo',''))}</title>
<style>
:root{{color-scheme:dark}}
body{{background:#0e1116;color:#c9d1d9;font:14px/1.5 -apple-system,Segoe UI,Roboto,sans-serif;margin:0;padding:28px}}
h1{{font-size:18px;margin:0 0 4px}} .sub{{color:#8b949e;margin:0 0 18px}}
.note{{background:#161b22;border-left:3px solid #e79c31;padding:8px 12px;border-radius:4px;margin:0 0 18px}}
table{{border-collapse:collapse;margin:8px 0 10px}}
th,td{{border:1px solid #21262d;padding:6px 9px;text-align:center;font-size:12px}}
th.rk,td.rk{{text-align:left}}
td.cell{{color:#0e1116;font-weight:700;cursor:default}} td.empty{{color:#30363d}}
code{{background:#161b22;padding:1px 5px;border-radius:4px;color:#adbac7}}
.legend{{color:#8b949e;font-size:12px;margin:0 0 24px;max-width:900px}}
.finding{{background:#161b22;border:1px solid #21262d;border-radius:8px;padding:12px 14px;margin:0 0 12px}}
.fh{{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:6px}}
.pill{{color:#0e1116;font-weight:700;border-radius:20px;padding:1px 10px;font-size:11px}}
.pill.conf,.pill.pass,.pill.lev{{background:#30363d;color:#c9d1d9}}
.lev{{color:#c9a3ff;font-size:12px;margin:3px 0}}
.pill.known{{background:#3b2d5e;color:#d7c6ff}} .known{{color:#b48ff0;font-size:12px;margin:3px 0}}
.cwe{{color:#e6edf3;font-weight:600}} .loc{{margin-bottom:6px}}
.file{{color:#8b949e;margin-left:8px}} .cid{{color:#8b949e;margin-left:8px;font-size:12px}}
.sig{{color:#adbac7;margin:4px 0}} .why{{margin:4px 0}}
.as{{color:#e79c31;font-size:12px}} .br{{color:#ff9bce;font-size:12px;margin:3px 0}}
.fix{{margin-top:6px;color:#7ee787}} .cw{{color:#8b949e;margin-left:10px;font-size:12px}}
</style></head><body>
<h1>beats security matrix</h1>
<p class="sub">{esc(data.get('repo',''))} · {len(findings)} findings ·
High {counts['High']} · Medium {counts['Medium']} · Low {counts['Low']} · Info {counts['Info']}</p>
{ver_sub_html}
{known_sub_html}
<div class="note">Triage only — beats supplies structural targeting, Claude supplies the semantic
verdict. Not proof of exploitability. Confirm with <code>govulncheck</code> and CodeQL/gosec.</div>
{matrix}
<h1 style="font-size:15px;margin-top:22px">Findings (ranked)</h1>
{''.join(frows) or "<p class='sub'>No findings.</p>"}
</body></html>"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("findings")
    ap.add_argument("--md")
    ap.add_argument("--html")
    args = ap.parse_args()
    data = load(args.findings)
    if args.md:
        with open(args.md, "w") as fh:
            fh.write(render_md(data))
    if args.html:
        with open(args.html, "w") as fh:
            fh.write(render_html(data))
    print(f"rendered {len(data.get('findings', []))} finding(s) -> "
          f"{args.md or '(no md)'}, {args.html or '(no html)'}", file=sys.stderr)


if __name__ == "__main__":
    main()
