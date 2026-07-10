#!/usr/bin/env python3
"""
analyze-release-study.py — Read the output of release-study.sh and produce
a summary with conclusions across all 3 ground-truth axes.

Usage:
    python3 scripts/analyze-release-study.py /tmp/beats-release-study/<repo-name>

Reads:
    releases.csv            — per-release cluster stats
    threshold-drift.csv     — threshold values per release
    cluster-tracking.csv    — Axis 1: which clusters appear in which releases
    outlier-tracking.csv    — Axis 2: which outlier functions persist across releases
    outlier-resolution.csv  — Axis 3: how outliers get resolved between releases

Prints:
    A structured evaluation to stdout + writes analysis.md in the study dir.
"""

import csv
import sys
import os
import statistics
from collections import defaultdict


def load_csv(path):
    """Load a CSV file, return list of dicts. Tolerant of missing files."""
    if not os.path.exists(path):
        return []
    with open(path, "r") as f:
        return list(csv.DictReader(f))


def load_metadata(study_dir):
    """Load metadata.txt key=value pairs."""
    meta_path = os.path.join(study_dir, "metadata.txt")
    meta = {}
    if os.path.exists(meta_path):
        with open(meta_path) as f:
            for line in f:
                line = line.strip()
                if "=" in line:
                    k, v = line.split("=", 1)
                    meta[k] = v
    return meta


def rel_path(filepath):
    """Strip absolute repo checkout prefix to get the relative path.

    beats stores absolute paths like /tmp/beats-release-study/foo/repo/pkg/bar.go.
    We want just pkg/bar.go.
    """
    fp = filepath or ""
    if "/repo/" in fp:
        fp = fp.split("/repo/", 1)[1]
    return fp.lstrip("/")


def git_link(browse_url, tag, filepath):
    """Build a GitHub/GitLab blob link: browse_url/blob/tag/filepath."""
    if not browse_url or not filepath:
        return ""
    fp = rel_path(filepath)
    if not fp:
        return ""
    return f"{browse_url}/blob/{tag}/{fp}"


def safe_float(val, default=0.0):
    try:
        return float(val)
    except (ValueError, TypeError):
        return default


def safe_int(val, default=0):
    try:
        return int(val)
    except (ValueError, TypeError):
        return default


def analyze_threshold_drift(rows):
    """Analyze how threshold values change across releases."""
    if not rows:
        return None

    fields = ["identifyThreshold", "seqSimilarityThreshold", "maxTrigranBucket", "minTokenSeqLen"]
    results = {}

    for field in fields:
        values = [safe_float(r[field]) for r in rows if r.get(field, "?") != "?"]
        if len(values) < 2:
            results[field] = {"verdict": "insufficient data"}
            continue

        mean = statistics.mean(values)
        stdev = statistics.stdev(values) if len(values) > 1 else 0
        cv = stdev / mean if mean != 0 else 0  # coefficient of variation
        drift = values[-1] - values[0]  # first to last
        max_jump = max(abs(values[i] - values[i - 1]) for i in range(1, len(values)))

        if cv < 0.03:
            verdict = "STABLE — consistent across releases."
        elif cv < 0.08:
            verdict = "MODERATE — some variation."
        else:
            verdict = "VARIABLE — changes significantly across releases."

        results[field] = {
            "values": values,
            "mean": mean,
            "stdev": stdev,
            "cv": cv,
            "drift": drift,
            "max_jump": max_jump,
            "verdict": verdict,
        }

    return results


def analyze_cluster_persistence(rows, num_releases):
    """Analyze which clusters persist across releases."""
    if not rows:
        return None

    total = len(rows)
    if total == 0:
        return None

    counts = [safe_int(r.get("num_releases_present", 0)) for r in rows]

    persistent = sum(1 for c in counts if c >= num_releases / 2)
    transient = sum(1 for c in counts if c <= 2)
    universal = sum(1 for c in counts if c == num_releases)

    persistence_ratio = persistent / total if total else 0

    if persistence_ratio > 0.6:
        verdict = (
            "HIGH PERSISTENCE — most clusters represent real, lasting conventions. "
            "The thresholds are capturing stable structure."
        )
    elif persistence_ratio > 0.3:
        verdict = (
            "MODERATE PERSISTENCE — mix of stable and transient clusters. "
            "Some noise, but core conventions are being found."
        )
    else:
        verdict = (
            "LOW PERSISTENCE — most clusters don't survive across releases. "
            "Either the thresholds are too loose (capturing noise) or the codebase "
            "is undergoing heavy structural churn."
        )

    return {
        "total_unique": total,
        "persistent": persistent,
        "transient": transient,
        "universal": universal,
        "persistence_ratio": persistence_ratio,
        "verdict": verdict,
    }


def analyze_outlier_persistence(rows, num_releases):
    """Axis 2: Analyze which outlier functions persist across releases."""
    if not rows:
        return None

    total = len(rows)
    if total == 0:
        return None

    counts = [safe_int(r.get("num_releases", 0)) for r in rows]
    scores = [safe_float(r.get("mean_score", 0)) for r in rows]

    persistent = sum(1 for c in counts if c >= num_releases / 2)
    one_off = sum(1 for c in counts if c <= 1)
    universal = sum(1 for c in counts if c == num_releases)

    persistence_ratio = persistent / total if total else 0
    mean_persistence = statistics.mean(counts) if counts else 0

    # Persistent outliers with high scores are the most meaningful
    persistent_high_score = sum(
        1 for c, s in zip(counts, scores)
        if c >= num_releases / 2 and s > 0.5
    )

    if persistence_ratio > 0.5:
        verdict = (
            "HIGH OUTLIER PERSISTENCE — most outlier functions are consistently detected "
            "across releases. These represent genuine structural deviations, not noise."
        )
    elif persistence_ratio > 0.2:
        verdict = (
            "MODERATE OUTLIER PERSISTENCE — some outliers persist while others are transient. "
            "The persistent ones are likely real; the transient ones may be noise or quick fixes."
        )
    else:
        verdict = (
            "LOW OUTLIER PERSISTENCE — most outliers appear briefly and disappear. "
            "Either the codebase is actively being cleaned up, or the outlier detection "
            "is too sensitive and picking up noise."
        )

    return {
        "total_unique": total,
        "persistent": persistent,
        "persistent_high_score": persistent_high_score,
        "one_off": one_off,
        "universal": universal,
        "persistence_ratio": persistence_ratio,
        "mean_persistence": mean_persistence,
        "verdict": verdict,
    }


def analyze_outlier_resolution(rows):
    """Axis 3: Analyze how outliers get resolved between releases."""
    if not rows:
        return None

    total = len(rows)
    if total == 0:
        return None

    resolution_counts = defaultdict(int)
    for r in rows:
        resolution_counts[r.get("resolution", "unknown")] += 1

    rejoined = resolution_counts.get("rejoined_cluster", 0)
    fixed = resolution_counts.get("fixed_or_refactored", 0)
    deleted = resolution_counts.get("deleted_or_moved", 0)

    rejoined_pct = rejoined / total if total else 0

    if rejoined_pct > 0.3:
        verdict = (
            "STRONG GROUND TRUTH SIGNAL — a significant fraction of outliers rejoin their "
            "clusters in later releases. This means developers are fixing the structural "
            "deviations that beats detected. This is the strongest evidence that beats "
            "outlier detection is finding real, actionable problems."
        )
    elif rejoined_pct > 0.1:
        verdict = (
            "MODERATE GROUND TRUTH SIGNAL — some outliers do get fixed to match their cluster's "
            "convention, providing partial validation. Most resolutions are deletions or refactors."
        )
    else:
        verdict = (
            "WEAK GROUND TRUTH SIGNAL — few outliers rejoin their clusters. Most are deleted "
            "or refactored away. This could mean the outliers weren't meaningful, or that the "
            "codebase evolves by replacing rather than fixing."
        )

    # Per-transition stats
    transitions = defaultdict(lambda: defaultdict(int))
    for r in rows:
        transitions[f"{r.get('outlier_in', '?')} → {r.get('resolved_by', '?')}"][r.get("resolution", "unknown")] += 1

    return {
        "total_resolutions": total,
        "rejoined": rejoined,
        "fixed": fixed,
        "deleted": deleted,
        "rejoined_pct": rejoined_pct,
        "verdict": verdict,
        "transitions": dict(transitions),
    }


def analyze_growth_trend(rows):
    """Analyze how cluster count and function count grow across releases."""
    if len(rows) < 2:
        return None

    cluster_counts = [safe_int(r.get("clusters", 0)) for r in rows]
    function_counts = [safe_int(r.get("functions", 0)) for r in rows]
    outlier_counts = [safe_int(r.get("outlier_candidates", 0)) for r in rows]

    # Trend: simple linear regression slope
    def slope(ys):
        n = len(ys)
        if n < 2:
            return 0
        xs = list(range(n))
        x_mean = statistics.mean(xs)
        y_mean = statistics.mean(ys)
        num = sum((x - x_mean) * (y - y_mean) for x, y in zip(xs, ys))
        den = sum((x - x_mean) ** 2 for x in xs)
        return num / den if den != 0 else 0

    cluster_slope = slope(cluster_counts)
    function_slope = slope(function_counts)

    # Cluster-to-function ratio over time
    ratios = [
        c / f if f > 0 else 0
        for c, f in zip(cluster_counts, function_counts)
    ]
    ratio_slope = slope(ratios)

    if abs(ratio_slope) < 0.001:
        ratio_verdict = "STABLE ratio — clusters scale proportionally with codebase growth."
    elif ratio_slope > 0:
        ratio_verdict = (
            "RISING ratio — finding proportionally more clusters in later releases. "
            "Could indicate growing conventions."
        )
    else:
        ratio_verdict = (
            "FALLING ratio — proportionally fewer clusters in later releases. "
            "Codebase may be diversifying faster than conventions form."
        )

    return {
        "cluster_counts": cluster_counts,
        "function_counts": function_counts,
        "outlier_counts": outlier_counts,
        "cluster_slope": cluster_slope,
        "function_slope": function_slope,
        "ratio_slope": ratio_slope,
        "ratio_verdict": ratio_verdict,
    }


def format_report(study_dir, releases, thresholds, tracking,
                   outlier_tracking, outlier_resolution, num_releases,
                   metadata=None):
    """Generate the analysis report."""
    lines = []
    w = lines.append

    repo_name = os.path.basename(study_dir)
    w(f"# Release Study Analysis: {repo_name}")
    w("")

    # ── 1. Threshold drift ──
    drift = analyze_threshold_drift(thresholds)
    w("## 1. Threshold Stability")
    w("")
    if drift:
        w("| Threshold | Mean | StdDev | CV | Drift | Max Jump | Verdict |")
        w("|-----------|------|--------|----|-------|----------|---------|")
        for field in ["identifyThreshold", "seqSimilarityThreshold", "maxTrigranBucket", "minTokenSeqLen"]:
            d = drift[field]
            if "mean" in d:
                w(f"| {field} | {d['mean']:.3f} | {d['stdev']:.3f} | {d['cv']:.1%} | {d['drift']:+.3f} | {d['max_jump']:.3f} | {d['verdict'].split('—')[0].strip()} |")
            else:
                w(f"| {field} | — | — | — | — | — | {d['verdict']} |")
        w("")

        # Detailed verdicts
        for field in ["identifyThreshold", "seqSimilarityThreshold", "maxTrigranBucket", "minTokenSeqLen"]:
            d = drift[field]
            if "verdict" in d and "mean" in d:
                w(f"**{field}**: {d['verdict']}")
                if "values" in d:
                    vals = " → ".join(f"{v:.3f}" for v in d["values"])
                    w(f"  Values: {vals}")
                w("")
    else:
        w("No threshold drift data available.")
        w("")

    # ── 2. Growth trend ──
    trend = analyze_growth_trend(releases)
    w("## 2. Growth Trend")
    w("")
    if trend:
        release_names = [r.get("release", f"r{i}") for i, r in enumerate(releases)]

        w("| Release | Functions | Clusters | Outliers |")
        w("|---------|-----------|----------|----------|")
        for i, r in enumerate(releases):
            w(f"| {release_names[i]} | {trend['function_counts'][i]} | {trend['cluster_counts'][i]} | {trend['outlier_counts'][i]} |")
        w("")

        w(f"Function growth rate: {trend['function_slope']:+.1f} per release")
        w(f"Cluster growth rate: {trend['cluster_slope']:+.1f} per release")
        w(f"")
        w(f"**{trend['ratio_verdict']}**")
        w("")
    else:
        w("Insufficient data for trend analysis.")
        w("")

    # ── 3. Cluster persistence ──
    persistence = analyze_cluster_persistence(tracking, num_releases)
    w("## 3. Cluster Persistence")
    w("")
    if persistence:
        w(f"- Total unique clusters seen across all releases: **{persistence['total_unique']}**")
        w(f"- Persistent (≥ half of releases): **{persistence['persistent']}**")
        w(f"- Universal (every release): **{persistence['universal']}**")
        w(f"- Transient (≤ 2 releases): **{persistence['transient']}**")
        w(f"- Persistence ratio: **{persistence['persistence_ratio']:.0%}**")
        w("")
        w(f"**{persistence['verdict']}**")
        w("")
    else:
        w("No cluster tracking data available.")
        w("")

    # ── 4. Outlier persistence (Axis 2) ──
    browse_url = (metadata or {}).get("browse_url", "")
    outlier_persist = analyze_outlier_persistence(outlier_tracking, num_releases)
    w("## 4. Outlier Persistence (Axis 2)")
    w("")
    if outlier_persist:
        w(f"- Total unique outlier functions: **{outlier_persist['total_unique']}**")
        w(f"- Persistent (≥ half of releases): **{outlier_persist['persistent']}**")
        w(f"  - Of which high-score (>0.5): **{outlier_persist['persistent_high_score']}**")
        w(f"- Universal (every release): **{outlier_persist['universal']}**")
        w(f"- One-off (single release): **{outlier_persist['one_off']}**")
        w(f"- Persistence ratio: **{outlier_persist['persistence_ratio']:.0%}**")
        w(f"- Mean releases per outlier: **{outlier_persist['mean_persistence']:.1f}**")
        w("")
        w(f"**{outlier_persist['verdict']}**")
        w("")

        # Detailed proof: show top persistent outliers with git links
        persistent_rows = sorted(
            [r for r in outlier_tracking if safe_int(r.get("num_releases", 0)) >= num_releases / 2],
            key=lambda r: safe_float(r.get("mean_score", 0)),
            reverse=True,
        )
        if persistent_rows:
            w("### Top Persistent Outliers (by score)")
            w("")
            shown = min(20, len(persistent_rows))
            for r in persistent_rows[:shown]:
                pkg = r.get("package", "?")
                func = r.get("func", "?")
                fpath = r.get("file", "")
                score = r.get("mean_score", "?")
                n = r.get("num_releases", "?")
                cluster = r.get("top_cluster", "?")
                last_tag = r.get("last_seen", "")
                link = git_link(browse_url, last_tag, fpath)
                rp = rel_path(fpath)
                w(f"- **{pkg}.{func}** — score={score}, {n} releases, cluster={cluster[:8] if len(cluster) > 8 else cluster}")
                if link:
                    w(f"  [{rp}]({link})")
                elif rp:
                    w(f"  `{rp}`")
            w("")
    else:
        w("No outlier tracking data available.")
        w("")

    # ── 5. Outlier resolution (Axis 3) ──
    resolution = analyze_outlier_resolution(outlier_resolution)
    w("## 5. Outlier Resolution (Axis 3)")
    w("")
    if resolution:
        w(f"- Total resolutions (outlier disappeared between releases): **{resolution['total_resolutions']}**")
        w(f"- Rejoined cluster: **{resolution['rejoined']}** ({resolution['rejoined_pct']:.0%})")
        w(f"- Fixed/refactored: **{resolution['fixed']}**")
        w(f"- Deleted/moved: **{resolution['deleted']}**")
        w("")
        w(f"**{resolution['verdict']}**")
        w("")

        # Show per-transition breakdown if there are multiple transitions
        if len(resolution['transitions']) > 1:
            w("### Per-Release Transition Breakdown")
            w("")
            w("| Transition | Rejoined | Fixed | Deleted | Total |")
            w("|------------|----------|-------|---------|-------|")
            for transition, counts in sorted(resolution['transitions'].items()):
                r = counts.get('rejoined_cluster', 0)
                f = counts.get('fixed_or_refactored', 0)
                d = counts.get('deleted_or_moved', 0)
                t = r + f + d
                w(f"| {transition} | {r} | {f} | {d} | {t} |")
            w("")

        # Detailed proof: list every rejoined_cluster resolution with git links
        rejoined_rows = [r for r in outlier_resolution if r.get("resolution") == "rejoined_cluster"]
        if rejoined_rows:
            w("### Detailed: Functions That Rejoined Their Cluster")
            w("")
            w("These functions were outliers in one release and stopped being outliers in the next,")
            w("while their candidate cluster still existed. This means the function was likely fixed")
            w("to match the cluster's convention.")
            w("")
            # Group by transition
            by_transition = defaultdict(list)
            for r in rejoined_rows:
                key = f"{r.get('outlier_in', '?')} → {r.get('resolved_by', '?')}"
                by_transition[key].append(r)

            for transition, rows in sorted(by_transition.items()):
                w(f"**{transition}** ({len(rows)} functions)")
                w("")
                for r in rows:
                    pkg = r.get("package", "?")
                    func = r.get("func", "?")
                    fpath = r.get("file", "")
                    cluster = r.get("top_cluster", "?")
                    score = r.get("score_at_disappearance", "?")
                    outlier_tag = r.get("outlier_in", "")
                    resolved_tag = r.get("resolved_by", "")

                    # Link to file in the outlier release (before fix)
                    before_link = git_link(browse_url, outlier_tag, fpath)
                    # Link to file in the resolved release (after fix)
                    after_link = git_link(browse_url, resolved_tag, fpath)

                    rp = rel_path(fpath)
                    short_cluster = cluster[:8] if len(cluster) > 8 else cluster
                    w(f"- **{pkg}.{func}** — score={score}, cluster={short_cluster}")
                    if before_link:
                        w(f"  before: [{outlier_tag} {rp}]({before_link})")
                        w(f"  after:  [{resolved_tag} {rp}]({after_link})")
                    elif rp:
                        w(f"  file: `{rp}`")
                w("")

        # Also list deleted/moved for completeness
        deleted_rows = [r for r in outlier_resolution if r.get("resolution") == "deleted_or_moved"]
        if deleted_rows:
            w("### Detailed: Functions Deleted or Moved")
            w("")
            by_transition = defaultdict(list)
            for r in deleted_rows:
                key = f"{r.get('outlier_in', '?')} → {r.get('resolved_by', '?')}"
                by_transition[key].append(r)

            for transition, rows in sorted(by_transition.items()):
                w(f"**{transition}** ({len(rows)} functions)")
                w("")
                for r in rows:
                    pkg = r.get("package", "?")
                    func = r.get("func", "?")
                    fpath = r.get("file", "")
                    outlier_tag = r.get("outlier_in", "")
                    link = git_link(browse_url, outlier_tag, fpath)
                    rp = rel_path(fpath)
                    short_cluster = r.get("top_cluster", "?")
                    short_cluster = short_cluster[:8] if len(short_cluster) > 8 else short_cluster
                    w(f"- **{pkg}.{func}** — cluster={short_cluster}")
                    if link:
                        w(f"  last seen: [{outlier_tag} {rp}]({link})")
                    elif rp:
                        w(f"  file: `{rp}`")
                w("")
    else:
        w("No outlier resolution data available.")
        w("")

    # ── 6. Bottom line ──
    w("## 6. Bottom Line")
    w("")

    conclusions = []

    if drift:
        id_drift = drift.get("identifyThreshold", {})
        if "cv" in id_drift:
            if id_drift["cv"] < 0.03:
                conclusions.append(
                    "The identifyThreshold is stable across releases — "
                    "the default of 0.75 is validated for this repo."
                )
            else:
                conclusions.append(
                    f"The identifyThreshold varies (CV={id_drift['cv']:.1%}) across releases."
                )

    if persistence:
        if persistence["persistence_ratio"] > 0.5:
            conclusions.append(
                f"{persistence['persistence_ratio']:.0%} of clusters persist across releases — "
                "the clustering is finding real conventions, not noise."
            )
        else:
            conclusions.append(
                f"Only {persistence['persistence_ratio']:.0%} of clusters persist — "
                "many clusters are transient, suggesting thresholds may be too loose."
            )

    if trend:
        if abs(trend["ratio_slope"]) < 0.001:
            conclusions.append(
                "The cluster-to-function ratio is stable — beats scales well with this codebase."
            )

    if outlier_persist:
        if outlier_persist["persistence_ratio"] > 0.3:
            conclusions.append(
                f"{outlier_persist['persistence_ratio']:.0%} of outlier functions persist across releases — "
                "the outlier detection is finding consistent structural deviations."
            )
        else:
            conclusions.append(
                f"Only {outlier_persist['persistence_ratio']:.0%} of outliers persist — "
                "most outliers are transient, suggesting high codebase churn or noisy detection."
            )

    if resolution:
        if resolution["rejoined_pct"] > 0.2:
            conclusions.append(
                f"{resolution['rejoined_pct']:.0%} of resolved outliers rejoined their cluster — "
                "developers are fixing deviations that beats detects. This is ground truth."
            )
        elif resolution["total_resolutions"] > 0:
            conclusions.append(
                f"Of {resolution['total_resolutions']} outlier resolutions, "
                f"{resolution['rejoined']} rejoined their cluster. "
                "Limited but present evidence that beats finds actionable deviations."
            )

    if not conclusions:
        conclusions.append("Insufficient data to draw conclusions. Need more releases or better data extraction.")

    for c in conclusions:
        w(f"- {c}")
    w("")

    # ── 7. Recommendation ──
    w("## 7. Recommendation")
    w("")

    # Overall beats validation
    signals = []
    if persistence and persistence["persistence_ratio"] > 0.5:
        signals.append("clusters persist")
    if outlier_persist and outlier_persist["persistence_ratio"] > 0.3:
        signals.append("outliers persist")
    if resolution and resolution["rejoined_pct"] > 0.1:
        signals.append("outliers get fixed")

    if len(signals) >= 2:
        w(f"**Overall: beats is working well on this repo.** Evidence: {', '.join(signals)}. "
          "The clustering captures real conventions and the outlier detection finds "
          "deviations that developers act on.")
    elif len(signals) == 1:
        w(f"**Partial validation.** One signal is positive ({signals[0]}), "
          "but more data points would strengthen confidence.")
    elif any([persistence, outlier_persist, resolution]):
        w("**Inconclusive.** The three-axis analysis didn't produce strong positive "
          "signals. Consider running on more releases or a different repo archetype.")

    w("")

    return "\n".join(lines)


def main():
    if len(sys.argv) < 2:
        print("Usage: python3 analyze-release-study.py <study-dir>")
        print("  e.g.: python3 analyze-release-study.py /tmp/beats-release-study/gin")
        sys.exit(1)

    study_dir = sys.argv[1]

    if not os.path.isdir(study_dir):
        print(f"ERROR: {study_dir} is not a directory")
        sys.exit(1)

    releases = load_csv(os.path.join(study_dir, "releases.csv"))
    thresholds = load_csv(os.path.join(study_dir, "threshold-drift.csv"))
    tracking = load_csv(os.path.join(study_dir, "cluster-tracking.csv"))
    outlier_tracking = load_csv(os.path.join(study_dir, "outlier-tracking.csv"))
    outlier_resolution = load_csv(os.path.join(study_dir, "outlier-resolution.csv"))
    metadata = load_metadata(study_dir)

    if not releases and not thresholds:
        print(f"ERROR: No data found in {study_dir}. Run release-study.sh first.")
        sys.exit(1)

    num_releases = len(releases)
    report = format_report(study_dir, releases, thresholds, tracking,
                           outlier_tracking, outlier_resolution, num_releases,
                           metadata=metadata)

    # Print to stdout
    print(report)

    # Write to file
    out_path = os.path.join(study_dir, "analysis.md")
    with open(out_path, "w") as f:
        f.write(report)
    print(f"\nWritten to: {out_path}")


if __name__ == "__main__":
    main()
