#!/usr/bin/env python3
"""
analyze_report.py — beats HTML cluster report analyser

Parses a beats HTML cluster report and produces a structured analysis:
  - Summary statistics
  - Quadrant distribution
  - Largest clusters
  - Highest-coherence clusters
  - Generated-code detection (Cyclo P95 outliers)
  - Cross-package structural patterns
  - Per-package contribution table
  - Coherence distribution

Usage:
    python analyze_report.py <report.html>
    python analyze_report.py <report.html> --json
    python analyze_report.py <report.html> --top 20
"""

import sys
import json
import re
import argparse
from collections import defaultdict
from pathlib import Path
from dataclasses import dataclass, field
from typing import Optional

try:
    from bs4 import BeautifulSoup
except ImportError:
    sys.exit("beautifulsoup4 not installed. Run: pip install beautifulsoup4 --break-system-packages")

try:
    from rich.console import Console
    from rich.table import Table
    from rich.panel import Panel
    from rich.text import Text
    from rich import box
    HAS_RICH = True
except ImportError:
    HAS_RICH = False

# ──────────────────────────────────────────────────────────────────────────────
# Data model
# ──────────────────────────────────────────────────────────────────────────────

@dataclass
class Member:
    fn_name: str
    pkg: str
    file: str
    line: str

@dataclass
class Cluster:
    idx: int
    hash_id: str
    quadrant: str          # hh / lh / hl / ll
    size: int
    import_coh: float
    call_coh: float
    cyclo_p95: float
    packages: list[str] = field(default_factory=list)
    imports: list[str]  = field(default_factory=list)
    members: list[Member] = field(default_factory=list)

    @property
    def combined_coh(self) -> float:
        return (self.import_coh + self.call_coh) / 2

    @property
    def is_cross_package(self) -> bool:
        return len(set(self.packages)) > 1

    @property
    def likely_generated(self) -> bool:
        return self.cyclo_p95 >= 50

@dataclass
class ReportMeta:
    repo: str
    generated_at: str
    total_clusters: int
    total_functions: int
    mean_import_coh: float
    mean_call_coh: float
    quad_hh: int = 0
    quad_lh: int = 0
    quad_hl: int = 0
    quad_ll: int = 0

# ──────────────────────────────────────────────────────────────────────────────
# Parser
# ──────────────────────────────────────────────────────────────────────────────

def _safe_float(text: str) -> float:
    try:
        return float(text.strip())
    except (ValueError, AttributeError):
        return 0.0

def _safe_int(text: str) -> int:
    try:
        return int(text.strip())
    except (ValueError, AttributeError):
        return 0

def parse_report(html_path: Path) -> tuple[ReportMeta, list[Cluster]]:
    soup = BeautifulSoup(html_path.read_text(encoding="utf-8"), "html.parser")

    # ── Meta ──────────────────────────────────────────────────────────────────
    repo = ""
    repo_el = soup.select_one(".hdr-left .repo")
    if repo_el:
        repo = repo_el.get_text(strip=True)

    generated_at = ""
    hdr_right = soup.select_one(".hdr-right")
    if hdr_right:
        lines = [l.strip() for l in hdr_right.get_text("\n").split("\n") if l.strip()]
        # last line is usually the timestamp
        generated_at = lines[-1] if lines else ""

    # ── Summary cards ─────────────────────────────────────────────────────────
    cards = soup.select(".card")
    card_vals: dict[str, str] = {}
    for c in cards:
        lbl_el = c.select_one(".lbl")
        val_el = c.select_one(".val")
        if lbl_el and val_el:
            card_vals[lbl_el.get_text(strip=True).lower()] = val_el.get_text(strip=True)

    total_clusters   = _safe_int(card_vals.get("total clusters", "0"))
    total_functions  = _safe_int(card_vals.get("functions in clusters", "0"))
    mean_import_coh  = _safe_float(card_vals.get("mean import coh.", "0"))
    mean_call_coh    = _safe_float(card_vals.get("mean call coh.", "0"))

    quad_counts = {"hh": 0, "lh": 0, "hl": 0, "ll": 0}
    for key, val in card_vals.items():
        if "high import · high call" in key:
            quad_counts["hh"] = _safe_int(val)
        elif "low import · high call" in key:
            quad_counts["lh"] = _safe_int(val)
        elif "high import · low call" in key:
            quad_counts["hl"] = _safe_int(val)
        elif "low import · low call" in key:
            quad_counts["ll"] = _safe_int(val)

    meta = ReportMeta(
        repo=repo,
        generated_at=generated_at,
        total_clusters=total_clusters,
        total_functions=total_functions,
        mean_import_coh=mean_import_coh,
        mean_call_coh=mean_call_coh,
        quad_hh=quad_counts["hh"],
        quad_lh=quad_counts["lh"],
        quad_hl=quad_counts["hl"],
        quad_ll=quad_counts["ll"],
    )

    # ── Clusters ──────────────────────────────────────────────────────────────
    clusters: list[Cluster] = []
    cl_rows = soup.select("tr.cl-row")

    for row in cl_rows:
        idx       = _safe_int(row.get("data-idx", "-1"))
        quadrant  = row.get("data-quadrant", "ll").lower()

        hash_el   = row.select_one(".cl-hash")
        hash_id   = hash_el.get_text(strip=True) if hash_el else f"unknown-{idx}"

        tds = row.select("td")
        # td[0] = quadrant pill, td[1] = hash/label, td[2] = size,
        # td[3] = import coh, td[4] = call coh, td[5] = cyclo,
        # td[6] = packages, td[7] = imports
        size       = _safe_int(tds[2].get_text(strip=True)) if len(tds) > 2 else 0
        import_coh = _safe_float(tds[3].get_text(strip=True)) if len(tds) > 3 else 0.0
        call_coh   = _safe_float(tds[4].get_text(strip=True)) if len(tds) > 4 else 0.0
        cyclo_p95  = _safe_float(tds[5].get_text(strip=True)) if len(tds) > 5 else 0.0

        packages   = [p.get_text(strip=True) for p in row.select(".pkg-pill")]
        imports_raw = tds[7].get_text(strip=True) if len(tds) > 7 else ""
        imports    = [i.strip() for i in imports_raw.split(",") if i.strip()]

        # Detail row members
        members: list[Member] = []
        detail_row = soup.select_one(f"#detail-{idx}")
        if detail_row:
            for mrow in detail_row.select("table.members tbody tr"):
            	mtds = mrow.select("td")
            	if len(mtds) >= 4:
            		fn_name = mtds[0].get_text(strip=True)
            		pkg     = mtds[1].get_text(strip=True)
            		file_el = mtds[2].select_one("span.fn-file")
            		fn_file = file_el.get("title", file_el.get_text(strip=True)) if file_el else mtds[2].get_text(strip=True)
            		line    = mtds[3].get_text(strip=True)
            		members.append(Member(fn_name=fn_name, pkg=pkg, file=fn_file, line=line))

        clusters.append(Cluster(
            idx=idx, hash_id=hash_id, quadrant=quadrant,
            size=size, import_coh=import_coh, call_coh=call_coh,
            cyclo_p95=cyclo_p95, packages=packages,
            imports=imports, members=members,
        ))

    return meta, clusters

# ──────────────────────────────────────────────────────────────────────────────
# Analysis
# ──────────────────────────────────────────────────────────────────────────────

def analyze(meta: ReportMeta, clusters: list[Cluster]) -> dict:
    total = len(clusters)
    if total == 0:
        return {}

    quad_map = {"hh": [], "lh": [], "hl": [], "ll": []}
    for c in clusters:
        quad_map.get(c.quadrant, quad_map["ll"]).append(c)

    # ── Coherence distribution ────────────────────────────────────────────────
    coh_buckets = {"≥0.90": 0, "0.70–0.89": 0, "0.50–0.69": 0, "<0.50": 0}
    for c in clusters:
        cc = c.combined_coh
        if cc >= 0.90:   coh_buckets["≥0.90"] += 1
        elif cc >= 0.70: coh_buckets["0.70–0.89"] += 1
        elif cc >= 0.50: coh_buckets["0.50–0.69"] += 1
        else:            coh_buckets["<0.50"] += 1

    # ── Generated code candidates ─────────────────────────────────────────────
    generated = sorted([c for c in clusters if c.likely_generated],
                       key=lambda c: c.cyclo_p95, reverse=True)

    # ── Cross-package clusters ────────────────────────────────────────────────
    cross_pkg = sorted([c for c in clusters if c.is_cross_package],
                       key=lambda c: c.combined_coh, reverse=True)

    # ── Package contribution ──────────────────────────────────────────────────
    pkg_members: dict[str, int] = defaultdict(int)
    pkg_clusters: dict[str, int] = defaultdict(int)
    for c in clusters:
        for m in c.members:
            pkg_members[m.pkg] += 1
        for p in set(c.packages):
            pkg_clusters[p] += 1

    top_pkgs = sorted(pkg_members.items(), key=lambda x: x[1], reverse=True)[:15]

    # ── Top clusters by size ──────────────────────────────────────────────────
    top_by_size = sorted(clusters, key=lambda c: c.size, reverse=True)[:10]

    # ── Top clusters by coherence ─────────────────────────────────────────────
    top_by_coh = sorted(clusters, key=lambda c: (c.combined_coh, c.size), reverse=True)[:10]

    return {
        "meta": meta,
        "total_clusters": total,
        "quad_distribution": {
            "HH": len(quad_map["hh"]),
            "LH": len(quad_map["lh"]),
            "HL": len(quad_map["hl"]),
            "LL": len(quad_map["ll"]),
        },
        "coh_distribution": coh_buckets,
        "top_by_size": top_by_size,
        "top_by_coh": top_by_coh,
        "generated_candidates": generated,
        "cross_package": cross_pkg,
        "top_packages": top_pkgs,
    }

# ──────────────────────────────────────────────────────────────────────────────
# Rich output
# ──────────────────────────────────────────────────────────────────────────────

QUAD_STYLE = {
    "hh": "bold green",
    "lh": "bold cyan",
    "hl": "bold yellow",
    "ll": "bold red",
}

QUAD_LABEL = {
    "hh": "HH tight domain-local",
    "lh": "LH cross-cutting",
    "hl": "HL domain-cohesive",
    "ll": "LL noise",
}

def _quad_pill(q: str) -> str:
    return f"[{QUAD_STYLE.get(q,'white')}]{q.upper()}[/{QUAD_STYLE.get(q,'white')}]"

def _coh_color(v: float) -> str:
    if v >= 0.60: return "green"
    if v >= 0.40: return "yellow"
    return "red"

def _fmt_coh(v: float) -> str:
    c = _coh_color(v)
    return f"[{c}]{v:.2f}[/{c}]"

def _fmt_imports(imports: list[str], max_n: int = 3) -> str:
    short = [i.split("/")[-1] for i in imports[:max_n]]
    suffix = f" +{len(imports)-max_n}" if len(imports) > max_n else ""
    return ", ".join(short) + suffix

def render_rich(result: dict, top_n: int = 10) -> None:
    console = Console()
    meta: ReportMeta = result["meta"]

    # ── Header ────────────────────────────────────────────────────────────────
    header_text = (
        f"[bold white]beats analyze[/bold white]  "
        f"[bold cyan]{meta.repo or 'unknown repo'}[/bold cyan]\n"
        f"[dim]{meta.generated_at}[/dim]\n\n"
        f"[white]Clusters[/white]  [bold]{result['total_clusters']}[/bold]   "
        f"[white]Functions[/white]  [bold]{meta.total_functions}[/bold]   "
        f"[white]Mean Import Coh.[/white]  [bold green]{meta.mean_import_coh:.2f}[/bold green]   "
        f"[white]Mean Call Coh.[/white]  [bold green]{meta.mean_call_coh:.2f}[/bold green]"
    )
    console.print(Panel(header_text, title="beats report analysis", border_style="bright_black"))

    # ── Quadrant distribution ─────────────────────────────────────────────────
    qdist = result["quad_distribution"]
    total = result["total_clusters"]
    qt = Table(title="Quadrant Distribution", box=box.SIMPLE_HEAVY, show_header=True)
    qt.add_column("Quadrant", style="bold")
    qt.add_column("Count", justify="right")
    qt.add_column("Share", justify="right")
    qt.add_column("Description")
    for q, label in QUAD_LABEL.items():
        n = qdist.get(q.upper(), 0)
        pct = n / total * 100 if total else 0
        qt.add_row(
            f"[{QUAD_STYLE[q]}]{q.upper()}[/{QUAD_STYLE[q]}]",
            str(n),
            f"{pct:.0f}%",
            label,
        )
    console.print(qt)

    # ── Coherence distribution ────────────────────────────────────────────────
    coh = result["coh_distribution"]
    cot = Table(title="Combined Coherence Distribution", box=box.SIMPLE_HEAVY)
    cot.add_column("Band")
    cot.add_column("Clusters", justify="right")
    cot.add_column("Share", justify="right")
    for band, n in coh.items():
        pct = n / total * 100 if total else 0
        color = "green" if band == "≥0.90" else "yellow" if "0.70" in band else "orange3" if "0.50" in band else "red"
        cot.add_row(f"[{color}]{band}[/{color}]", str(n), f"{pct:.0f}%")
    console.print(cot)

    # ── Top clusters by size ──────────────────────────────────────────────────
    st = Table(title=f"Top {top_n} Clusters by Size", box=box.SIMPLE_HEAVY)
    st.add_column("Type")
    st.add_column("Hash", style="bright_black")
    st.add_column("Size", justify="right")
    st.add_column("ImportCoh", justify="right")
    st.add_column("CallCoh", justify="right")
    st.add_column("CycloP95", justify="right")
    st.add_column("Packages")
    st.add_column("Top Imports")
    for c in result["top_by_size"][:top_n]:
        st.add_row(
            _quad_pill(c.quadrant),
            c.hash_id,
            str(c.size),
            _fmt_coh(c.import_coh),
            _fmt_coh(c.call_coh),
            f"[{'red' if c.cyclo_p95 >= 100 else 'yellow' if c.cyclo_p95 >= 20 else 'white'}]{c.cyclo_p95:.1f}[/{'red' if c.cyclo_p95 >= 100 else 'yellow' if c.cyclo_p95 >= 20 else 'white'}]",
            ", ".join(c.packages[:2]),
            _fmt_imports(c.imports),
        )
    console.print(st)

    # ── Top clusters by coherence ─────────────────────────────────────────────
    ct = Table(title=f"Top {top_n} Clusters by Combined Coherence", box=box.SIMPLE_HEAVY)
    ct.add_column("Type")
    ct.add_column("Hash", style="bright_black")
    ct.add_column("Size", justify="right")
    ct.add_column("ImportCoh", justify="right")
    ct.add_column("CallCoh", justify="right")
    ct.add_column("Combined", justify="right")
    ct.add_column("Packages")
    for c in result["top_by_coh"][:top_n]:
        ct.add_row(
            _quad_pill(c.quadrant),
            c.hash_id,
            str(c.size),
            _fmt_coh(c.import_coh),
            _fmt_coh(c.call_coh),
            _fmt_coh(c.combined_coh),
            ", ".join(c.packages[:3]),
        )
    console.print(ct)

    # ── Generated code candidates ─────────────────────────────────────────────
    gen = result["generated_candidates"]
    if gen:
        gent = Table(title=f"Generated Code Candidates  (Cyclo P95 ≥ 50, {len(gen)} clusters)", box=box.SIMPLE_HEAVY)
        gent.add_column("Hash", style="bright_black")
        gent.add_column("Size", justify="right")
        gent.add_column("CycloP95", justify="right", style="red")
        gent.add_column("Packages")
        gent.add_column("Sample members")
        for c in gen[:top_n]:
            sample = ", ".join(set(m.fn_name for m in c.members[:3]))
            gent.add_row(
                c.hash_id,
                str(c.size),
                f"{c.cyclo_p95:.1f}",
                ", ".join(c.packages[:2]),
                sample,
            )
        console.print(gent)
    else:
        console.print("[dim]No generated-code candidates detected (Cyclo P95 < 50 for all clusters)[/dim]")

    # ── Cross-package clusters ────────────────────────────────────────────────
    xpkg = result["cross_package"]
    if xpkg:
        xpt = Table(title=f"Cross-Package Structural Patterns  ({len(xpkg)} clusters)", box=box.SIMPLE_HEAVY)
        xpt.add_column("Type")
        xpt.add_column("Hash", style="bright_black")
        xpt.add_column("Size", justify="right")
        xpt.add_column("Combined", justify="right")
        xpt.add_column("Packages")
        for c in xpkg[:top_n]:
            xpt.add_row(
                _quad_pill(c.quadrant),
                c.hash_id,
                str(c.size),
                _fmt_coh(c.combined_coh),
                ", ".join(c.packages),
            )
        console.print(xpt)
    else:
        console.print("[dim]No cross-package clusters detected[/dim]")

    # ── Package contribution ──────────────────────────────────────────────────
    pkt = Table(title="Top Packages by Clustered Members", box=box.SIMPLE_HEAVY)
    pkt.add_column("Package")
    pkt.add_column("Members", justify="right")
    pkt.add_column("Bar")
    top_pkgs = result["top_packages"]
    max_m = top_pkgs[0][1] if top_pkgs else 1
    for pkg, n in top_pkgs:
        bar_len = int(n / max_m * 30)
        bar = "█" * bar_len
        pkt.add_row(pkg, str(n), f"[cyan]{bar}[/cyan]")
    console.print(pkt)

# ──────────────────────────────────────────────────────────────────────────────
# Plain text fallback (no rich)
# ──────────────────────────────────────────────────────────────────────────────

def render_plain(result: dict, top_n: int = 10) -> None:
    meta: ReportMeta = result["meta"]
    print(f"\n=== beats analyze — {meta.repo} ===")
    print(f"Generated: {meta.generated_at}")
    print(f"Clusters: {result['total_clusters']}  Functions: {meta.total_functions}  "
          f"Mean Import Coh: {meta.mean_import_coh:.2f}  Mean Call Coh: {meta.mean_call_coh:.2f}\n")

    print("── Quadrant Distribution ──────────────────────────────────────")
    qdist = result["quad_distribution"]
    total = result["total_clusters"]
    for q, label in QUAD_LABEL.items():
        n = qdist.get(q.upper(), 0)
        pct = n / total * 100 if total else 0
        print(f"  {q.upper():4s}  {n:4d}  ({pct:4.0f}%)  {label}")

    print("\n── Top Clusters by Size ───────────────────────────────────────")
    print(f"  {'Hash':18s} {'Sz':>4} {'ImpCoh':>8} {'CallCoh':>8} {'CycloP95':>10}  Packages")
    for c in result["top_by_size"][:top_n]:
        print(f"  {c.hash_id:18s} {c.size:4d} {c.import_coh:8.2f} {c.call_coh:8.2f} {c.cyclo_p95:10.1f}  {', '.join(c.packages[:2])}")

    print("\n── Generated Code Candidates (Cyclo P95 ≥ 50) ────────────────")
    for c in result["generated_candidates"][:top_n]:
        print(f"  {c.hash_id:18s}  size={c.size:3d}  cyclo={c.cyclo_p95:.1f}  {', '.join(c.packages[:2])}")

    print("\n── Cross-Package Patterns ─────────────────────────────────────")
    for c in result["cross_package"][:top_n]:
        print(f"  {c.hash_id:18s}  size={c.size:3d}  coh={c.combined_coh:.2f}  {', '.join(c.packages)}")

    print("\n── Top Packages by Members ────────────────────────────────────")
    for pkg, n in result["top_packages"]:
        print(f"  {pkg:40s}  {n:4d}")

# ──────────────────────────────────────────────────────────────────────────────
# JSON output
# ──────────────────────────────────────────────────────────────────────────────

def render_json(result: dict) -> None:
    def serialise(obj):
        if isinstance(obj, (ReportMeta, Cluster, Member)):
            return obj.__dict__
        if isinstance(obj, list):
            return [serialise(i) for i in obj]
        if isinstance(obj, dict):
            return {k: serialise(v) for k, v in obj.items()}
        return obj

    # Build a serialisable summary (omit full member lists for brevity)
    summary = {
        "meta": serialise(result["meta"]),
        "total_clusters": result["total_clusters"],
        "quad_distribution": result["quad_distribution"],
        "coh_distribution": result["coh_distribution"],
        "top_by_size": [
            {
                "hash_id": c.hash_id, "quadrant": c.quadrant, "size": c.size,
                "import_coh": c.import_coh, "call_coh": c.call_coh,
                "cyclo_p95": c.cyclo_p95, "packages": c.packages,
                "imports": c.imports[:5],
                "members": [m.__dict__ for m in c.members[:5]],
            }
            for c in result["top_by_size"]
        ],
        "top_by_coh": [
            {
                "hash_id": c.hash_id, "quadrant": c.quadrant, "size": c.size,
                "import_coh": c.import_coh, "call_coh": c.call_coh,
                "combined_coh": c.combined_coh, "packages": c.packages,
            }
            for c in result["top_by_coh"]
        ],
        "generated_candidates": [
            {
                "hash_id": c.hash_id, "size": c.size, "cyclo_p95": c.cyclo_p95,
                "packages": c.packages,
                "sample_members": list(set(m.fn_name for m in c.members[:5])),
            }
            for c in result["generated_candidates"]
        ],
        "cross_package_clusters": [
            {
                "hash_id": c.hash_id, "quadrant": c.quadrant, "size": c.size,
                "combined_coh": c.combined_coh, "packages": c.packages,
            }
            for c in result["cross_package"]
        ],
        "top_packages": [{"package": p, "members": n} for p, n in result["top_packages"]],
    }
    print(json.dumps(summary, indent=2))

# ──────────────────────────────────────────────────────────────────────────────
# Entry point
# ──────────────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Analyse a beats HTML cluster report",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("report", type=Path, help="Path to beats HTML report")
    parser.add_argument("--json", action="store_true", help="Output as JSON")
    parser.add_argument("--top", type=int, default=10, metavar="N",
                        help="Number of rows to show in each table (default: 10)")
    args = parser.parse_args()

    if not args.report.exists():
        sys.exit(f"File not found: {args.report}")

    print(f"Parsing {args.report} …", file=sys.stderr)
    meta, clusters = parse_report(args.report)
    print(f"Found {len(clusters)} clusters with {sum(c.size for c in clusters)} member functions", file=sys.stderr)

    result = analyze(meta, clusters)

    if args.json:
        render_json(result)
    elif HAS_RICH:
        render_rich(result, top_n=args.top)
    else:
        render_plain(result, top_n=args.top)

if __name__ == "__main__":
    main()
