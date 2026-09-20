#!/usr/bin/env python3
"""
beats_cluster_bench.py — Benchmark clustering algorithms against Beats.

Each algorithm receives RAW feature data (AST token sequences, call imports,
call targets) and uses its OWN clustering mechanism.  No part of Beats'
similarity formula (Levenshtein edit-distance, Jaccard, geometric mean)
enters any competing algorithm.

Feature vectorization:
  - Token sequences → bag-of-tokens counts (30 dims) + token bigram counts
  - Imports → binary indicator vector with IDF weighting
  - Calls → binary indicator vector with IDF weighting
  - All groups L2-normalised per group, then concatenated with equal weight
  - Final per-row L2 normalisation so Euclidean ≈ cosine distance

Algorithms cluster the combined feature matrix using Euclidean distance
(on L2-normalised vectors this is monotonically related to cosine distance:
    ‖a − b‖² = 2·(1 − cos(a,b))
).

Evaluation is independent of clustering input:
  - Silhouette score (internal cluster separation quality)
  - Domain coherence (pairwise import/call Jaccard within clusters — post-hoc)
  - Bootstrap stability (cluster survival under 80% resampling)
  - Outlier F1 (agreement with Beats' outlier labels)
  - ARI / NMI (agreement with Beats' cluster assignments)

Usage:
    beats export --repo /path/to/repo > export.json
    python beats_cluster_bench.py export.json [--stability] [--sample N]

Dependencies:
    pip install numpy scikit-learn hdbscan --break-system-packages
"""

from __future__ import annotations

import argparse
import json
import math
import sys
import time
import warnings
from collections import defaultdict
from dataclasses import dataclass
from typing import Callable

import numpy as np
from sklearn.cluster import (
    DBSCAN,
    OPTICS,
    KMeans,
    AgglomerativeClustering,
    SpectralClustering,
    AffinityPropagation,
)
from sklearn.metrics import (
    adjusted_rand_score,
    normalized_mutual_info_score,
    silhouette_score,
)
from sklearn.neighbors import NearestNeighbors

try:
    import hdbscan

    HAS_HDBSCAN = True
except ImportError:
    HAS_HDBSCAN = False
    warnings.warn("hdbscan not installed — HDBSCAN will be skipped.  pip install hdbscan")


# ═══════════════════════════════════════════════════════════════════════════════
#  Token vocabulary — mirrors pkg/ast/cluster.go tokenNames
# ═══════════════════════════════════════════════════════════════════════════════

TOKEN_NAMES = [
    "IF", "FOR", "RANGE", "SWITCH", "CASE", "SELECT", "COMM",
    "RETURN", "GO", "SEND", "DEFER", "CONTINUE", "BREAK", "GOTO",
    "CALL", "FUNCLIT", "ASSIGN", "CALL_PKG", "CALL_METHOD",
    "COMPOSITE_LIT", "BINARY_OP", "TYPE_ASSERT",
    # Java-specific (emitted by jbeats)
    "TRY", "CATCH", "THROW", "FINALLY", "SYNCHRONIZED",
    "WHILE", "DO_WHILE", "ASSERT_STMT",
]
VOCAB_SIZE = len(TOKEN_NAMES)
TOKEN_TO_INT = {name: i for i, name in enumerate(TOKEN_NAMES)}


def parse_tokens(token_str: str) -> list[int]:
    """Convert space-separated token names back to the integer array Beats uses."""
    if not token_str or not token_str.strip():
        return []
    return [TOKEN_TO_INT.get(t, VOCAB_SIZE) for t in token_str.split()]


# ═══════════════════════════════════════════════════════════════════════════════
#  Data model
# ═══════════════════════════════════════════════════════════════════════════════

@dataclass
class FuncRecord:
    name: str
    package: str
    file: str
    line: int
    tokens_str: str
    tokens_int: list[int]
    imports: set[str]
    calls: set[str]
    beats_cluster: int  # -1 = orphan / noise

    @property
    def qualified_name(self) -> str:
        return f"{self.package}/{self.name}"


@dataclass
class AlgoResult:
    name: str
    labels: np.ndarray
    n_clusters: int = 0
    noise_count: int = 0
    noise_pct: float = 0.0
    silhouette: float = float("nan")
    coherence: float = float("nan")
    call_coherence: float = float("nan")
    stability: float = float("nan")
    ari_vs_beats: float = float("nan")
    nmi_vs_beats: float = float("nan")
    outlier_f1: float = float("nan")
    params: str = ""


# ═══════════════════════════════════════════════════════════════════════════════
#  Loading
# ═══════════════════════════════════════════════════════════════════════════════

def load_functions(path: str) -> tuple[list[FuncRecord], dict]:
    """Parse beats-export JSON into FuncRecords.

    Cluster members get beats_cluster = <cluster index>.
    Orphans get beats_cluster = -1.
    """
    with open(path) as f:
        data = json.load(f)

    functions: list[FuncRecord] = []
    cluster_idx = 0

    for cl in data.get("clusters", []):
        for mem in cl.get("members", []):
            tok = mem.get("tokens", "")
            functions.append(FuncRecord(
                name=mem["func"],
                package=mem.get("package", ""),
                file=mem.get("file", ""),
                line=mem.get("line", 0),
                tokens_str=tok,
                tokens_int=parse_tokens(tok),
                imports=set(mem.get("imports") or []),
                calls=set(mem.get("calls") or []),
                beats_cluster=cluster_idx,
            ))
        cluster_idx += 1

    for orph in data.get("orphans", []):
        tok = orph.get("tokens", "")
        functions.append(FuncRecord(
            name=orph.get("func", ""),
            package=orph.get("package", ""),
            file=orph.get("file", ""),
            line=orph.get("line", 0),
            tokens_str=tok,
            tokens_int=parse_tokens(tok),
            imports=set(orph.get("imports") or []),
            calls=set(orph.get("calls") or []),
            beats_cluster=-1,
        ))

    meta = {
        "repo": data.get("repo", ""),
        "corpus_size": data.get("corpus_size", len(functions)),
        "n_beats_clusters": cluster_idx,
        "n_orphans": len(data.get("orphans", [])),
        "n_functions": len(functions),
    }
    return functions, meta


# ═══════════════════════════════════════════════════════════════════════════════
#  Feature extraction — each algorithm clusters on THESE features
#
#  IMPORTANT: no Beats similarity formula (Levenshtein, Jaccard, geometric
#  mean) is used here.  Each feature group is a standard vectorisation of
#  the RAW data: integer token counts, token-pair counts, and IDF-weighted
#  binary indicators for imports and call targets.
# ═══════════════════════════════════════════════════════════════════════════════

def build_feature_matrix(
    fns: list[FuncRecord], verbose: bool = True
) -> tuple[np.ndarray, dict]:
    """Vectorise raw function data into a combined feature matrix.

    Feature groups (each normalised independently, then concatenated):

    1. Token bag-of-words  (VOCAB_SIZE dims)
       Count of each token type in the function's AST sequence.

    2. Token bigrams  (variable dims)
       Count of each consecutive token pair — captures structural ordering
       without using edit-distance.

    3. Import indicators  (variable dims, IDF-weighted)
       Binary presence of each import, weighted by inverse document
       frequency so rare (= more discriminative) imports get more weight.

    4. Call-target indicators  (variable dims, IDF-weighted)
       Same as imports, for call targets.

    All groups are scaled to equal Frobenius norm, then each row is
    L2-normalised.  This means Euclidean distance in the combined space
    is monotonically related to cosine distance across the raw features.
    """
    n = len(fns)
    t0 = time.time()

    # ── Group 1: Token bag-of-words ──────────────────────────────────────
    tok_bag = np.zeros((n, VOCAB_SIZE), dtype=np.float64)
    for i, fn in enumerate(fns):
        for t in fn.tokens_int:
            if 0 <= t < VOCAB_SIZE:
                tok_bag[i, t] += 1

    # ── Group 2: Token bigrams ───────────────────────────────────────────
    bigram_vocab: dict[tuple[int, int], int] = {}
    fn_bigrams: list[dict[tuple[int, int], int]] = []
    for fn in fns:
        bgs: dict[tuple[int, int], int] = {}
        toks = fn.tokens_int
        for j in range(len(toks) - 1):
            a, b = toks[j], toks[j + 1]
            if a < VOCAB_SIZE and b < VOCAB_SIZE:
                bg = (a, b)
                bgs[bg] = bgs.get(bg, 0) + 1
                if bg not in bigram_vocab:
                    bigram_vocab[bg] = len(bigram_vocab)
        fn_bigrams.append(bgs)

    n_bigrams = max(len(bigram_vocab), 1)
    tok_bi = np.zeros((n, n_bigrams), dtype=np.float64)
    for i, bgs in enumerate(fn_bigrams):
        for bg, cnt in bgs.items():
            tok_bi[i, bigram_vocab[bg]] = cnt

    # ── Group 3: Import indicators (IDF-weighted) ────────────────────────
    import_vocab: dict[str, int] = {}
    for fn in fns:
        for imp in fn.imports:
            if imp not in import_vocab:
                import_vocab[imp] = len(import_vocab)

    n_imports = max(len(import_vocab), 1)
    imp_mat = np.zeros((n, n_imports), dtype=np.float64)
    for i, fn in enumerate(fns):
        for imp in fn.imports:
            imp_mat[i, import_vocab[imp]] = 1.0

    # IDF: log((N+1) / (df+1)) + 1   (sklearn-style smooth IDF)
    if import_vocab:
        df_imp = np.sum(imp_mat > 0, axis=0)
        idf_imp = np.log((n + 1) / (df_imp + 1)) + 1
        imp_mat *= idf_imp

    # ── Group 4: Call-target indicators (IDF-weighted) ───────────────────
    call_vocab: dict[str, int] = {}
    for fn in fns:
        for c in fn.calls:
            if c not in call_vocab:
                call_vocab[c] = len(call_vocab)

    n_calls = max(len(call_vocab), 1)
    call_mat = np.zeros((n, n_calls), dtype=np.float64)
    for i, fn in enumerate(fns):
        for c in fn.calls:
            call_mat[i, call_vocab[c]] = 1.0

    if call_vocab:
        df_call = np.sum(call_mat > 0, axis=0)
        idf_call = np.log((n + 1) / (df_call + 1)) + 1
        call_mat *= idf_call

    # ── Normalise each group to equal contribution ───────────────────────
    # Scale each group so its Frobenius norm = sqrt(N).  This means the
    # average row in each group has roughly unit L2 norm, preventing any
    # single group from dominating just because it has more columns.
    groups = [tok_bag, tok_bi, imp_mat, call_mat]
    for idx in range(len(groups)):
        frob = np.linalg.norm(groups[idx], "fro")
        if frob > 0:
            groups[idx] = groups[idx] * (np.sqrt(n) / frob)

    X = np.hstack(groups)

    # Final per-row L2 normalisation.
    row_norms = np.linalg.norm(X, axis=1, keepdims=True)
    row_norms[row_norms == 0] = 1.0
    X /= row_norms

    feat_info = {
        "n_token_features": VOCAB_SIZE,
        "n_bigram_features": len(bigram_vocab),
        "n_import_features": len(import_vocab),
        "n_call_features": len(call_vocab),
        "total_features": X.shape[1],
        "build_time": round(time.time() - t0, 3),
    }

    if verbose:
        print(
            f"  features: {feat_info['total_features']}  "
            f"(tokens={VOCAB_SIZE}  bigrams={feat_info['n_bigram_features']}  "
            f"imports={feat_info['n_import_features']}  "
            f"calls={feat_info['n_call_features']})"
        )
        print(f"  vectorised in {feat_info['build_time']:.2f}s")

    return X, feat_info


# ═══════════════════════════════════════════════════════════════════════════════
#  Algorithm runners — each uses its own clustering mechanism on the features
# ═══════════════════════════════════════════════════════════════════════════════

def _run_hdbscan(
    X: np.ndarray, min_cluster_size: int = 3
) -> np.ndarray | None:
    if not HAS_HDBSCAN:
        return None
    return hdbscan.HDBSCAN(
        min_cluster_size=min_cluster_size,
        min_samples=2,
        metric="euclidean",
    ).fit_predict(X)


def _run_dbscan(
    X: np.ndarray, eps: float = 0.5, min_samples: int = 3
) -> np.ndarray:
    return DBSCAN(
        eps=eps, min_samples=min_samples, metric="euclidean"
    ).fit_predict(X)


def _run_optics(X: np.ndarray, min_samples: int = 3) -> np.ndarray:
    return OPTICS(
        min_samples=min_samples, metric="euclidean", cluster_method="xi"
    ).fit_predict(X)


def _run_agglom(
    X: np.ndarray,
    linkage: str = "ward",
    n_clusters: int | None = None,
    threshold: float | None = None,
) -> np.ndarray:
    if n_clusters is not None:
        return AgglomerativeClustering(
            n_clusters=n_clusters, linkage=linkage
        ).fit_predict(X)
    return AgglomerativeClustering(
        n_clusters=None, linkage=linkage, distance_threshold=threshold
    ).fit_predict(X)


def _run_spectral(X: np.ndarray, n_clusters: int = 10) -> np.ndarray:
    return SpectralClustering(
        n_clusters=n_clusters, affinity="rbf", random_state=42
    ).fit_predict(X)


def _run_affprop(X: np.ndarray) -> np.ndarray:
    return AffinityPropagation(
        damping=0.7, max_iter=300, random_state=42
    ).fit_predict(X)


def _run_kmeans(X: np.ndarray, n_clusters: int = 10) -> np.ndarray:
    return KMeans(
        n_clusters=n_clusters, random_state=42, n_init=10
    ).fit_predict(X)


def estimate_dbscan_eps(X: np.ndarray, min_samples: int = 3) -> float:
    """Estimate a reasonable DBSCAN eps from the k-NN distance knee.

    Computes the sorted k-nearest-neighbour distances and finds the
    knee point (maximum second derivative) — standard practice for
    auto-tuning DBSCAN without domain-specific knowledge.
    """
    nn = NearestNeighbors(n_neighbors=min_samples, metric="euclidean")
    nn.fit(X)
    distances, _ = nn.kneighbors(X)
    k_dist = np.sort(distances[:, -1])

    if len(k_dist) < 5:
        return float(np.median(k_dist))

    # Smooth with a small window to reduce noise in the knee detection
    window = max(3, len(k_dist) // 50)
    smoothed = np.convolve(k_dist, np.ones(window) / window, mode="valid")

    diffs = np.diff(smoothed)
    diffs2 = np.diff(diffs)
    if len(diffs2) == 0:
        return float(np.median(k_dist))

    knee_idx = int(np.argmax(diffs2))
    original_idx = min(knee_idx + window // 2 + 1, len(k_dist) - 1)
    return float(k_dist[original_idx])


# ═══════════════════════════════════════════════════════════════════════════════
#  Evaluation metrics
#
#  NOTE ON DOMAIN COHERENCE:  Jaccard on imports/calls is used here as a
#  post-hoc evaluation metric — "do the algorithm's clusters capture real
#  structural idioms?"  It is NOT used as input to any clustering algorithm.
#  Every algorithm clustered on the feature matrix, not on Jaccard similarity.
# ═══════════════════════════════════════════════════════════════════════════════

def _jaccard_eval(a: set, b: set) -> float:
    """Jaccard index — used ONLY for evaluation, never for clustering."""
    if not a and not b:
        return 0.0
    inter = len(a & b)
    union = len(a | b)
    return inter / union if union else 0.0


def _label_stats(labels: np.ndarray) -> tuple[int, int, float]:
    """Return (n_clusters, noise_count, noise_pct)."""
    unique = set(labels)
    unique.discard(-1)
    n_cl = len(unique)
    noise = int(np.sum(labels == -1))
    pct = 100 * noise / len(labels) if len(labels) else 0.0
    return n_cl, noise, pct


def _silhouette(X: np.ndarray, labels: np.ndarray) -> float:
    """Silhouette score on the feature matrix (Euclidean ≈ cosine on L2-normed X)."""
    non_noise = labels != -1
    unique = set(labels[non_noise])
    if len(unique) < 2 or non_noise.sum() < 3:
        return float("nan")
    try:
        return float(
            silhouette_score(X[non_noise], labels[non_noise], metric="euclidean")
        )
    except Exception:
        return float("nan")


def _domain_coherence(
    fns: list[FuncRecord], labels: np.ndarray, attr: str
) -> float:
    """Mean within-cluster pairwise Jaccard on imports or calls.

    This is an EVALUATION metric — it measures whether the algorithm's
    clusters capture real domain structure (shared imports / call targets).
    It is NOT used as input to any clustering algorithm.
    """
    clusters: dict[int, list[int]] = defaultdict(list)
    for i, lab in enumerate(labels):
        if lab != -1:
            clusters[lab].append(i)

    if not clusters:
        return 0.0

    coh_values: list[float] = []
    for members in clusters.values():
        if len(members) < 2:
            continue
        sets = [getattr(fns[m], attr) for m in members]
        total, pairs = 0.0, 0
        for a in range(len(sets)):
            for b in range(a + 1, len(sets)):
                total += _jaccard_eval(sets[a], sets[b])
                pairs += 1
        if pairs:
            coh_values.append(total / pairs)

    return float(np.mean(coh_values)) if coh_values else 0.0


def outlier_agreement(
    beats_labels: np.ndarray, other_labels: np.ndarray
) -> dict:
    """Precision / recall / F1 of outlier detection vs Beats."""
    beats_outlier = beats_labels == -1
    other_outlier = other_labels == -1

    tp = int(np.sum(beats_outlier & other_outlier))
    fp = int(np.sum(~beats_outlier & other_outlier))
    fn = int(np.sum(beats_outlier & ~other_outlier))

    precision = tp / (tp + fp) if (tp + fp) else 0.0
    recall = tp / (tp + fn) if (tp + fn) else 0.0
    f1 = (
        2 * precision * recall / (precision + recall)
        if (precision + recall)
        else 0.0
    )
    return {"precision": precision, "recall": recall, "f1": f1}


def evaluate(
    fns: list[FuncRecord],
    X: np.ndarray,
    labels: np.ndarray,
    beats_labels: np.ndarray,
) -> dict:
    n_cl, noise, noise_pct = _label_stats(labels)
    return {
        "n_clusters": n_cl,
        "noise_count": noise,
        "noise_pct": noise_pct,
        "silhouette": _silhouette(X, labels),
        "coherence": _domain_coherence(fns, labels, "imports"),
        "call_coherence": _domain_coherence(fns, labels, "calls"),
        "ari_vs_beats": float(adjusted_rand_score(beats_labels, labels)),
        "nmi_vs_beats": float(
            normalized_mutual_info_score(beats_labels, labels)
        ),
    }


# ═══════════════════════════════════════════════════════════════════════════════
#  Bootstrap stability
# ═══════════════════════════════════════════════════════════════════════════════

def bootstrap_stability(
    fns: list[FuncRecord],
    X: np.ndarray,
    algo_fn: Callable[[np.ndarray], np.ndarray | None],
    n_resamples: int = 20,
    survival_threshold: float = 0.7,
) -> float:
    """Fraction of full-run clusters surviving in ≥ survival_threshold resamples.

    A cluster "survives" a resample when a resample cluster has Jaccard > 0.5
    with the full-run cluster (by member identity, not features).
    """
    n = len(fns)
    full_labels = algo_fn(X)
    if full_labels is None:
        return float("nan")

    full_clusters: dict[int, set[str]] = defaultdict(set)
    for i, lab in enumerate(full_labels):
        if lab != -1:
            full_clusters[lab].add(fns[i].qualified_name)

    if not full_clusters:
        return 0.0

    appearances = {k: 0 for k in full_clusters}
    rng = np.random.RandomState(42)

    for _ in range(n_resamples):
        indices = np.sort(rng.choice(n, int(n * 0.8), replace=False))
        sub_X = X[indices]
        sub_labels = algo_fn(sub_X)
        if sub_labels is None:
            continue

        boot_clusters: dict[int, set[str]] = defaultdict(set)
        for idx, lab in zip(indices, sub_labels):
            if lab != -1:
                boot_clusters[lab].add(fns[idx].qualified_name)

        for full_k, full_set in full_clusters.items():
            for boot_set in boot_clusters.values():
                inter = len(full_set & boot_set)
                union = len(full_set | boot_set)
                if union > 0 and inter / union > 0.5:
                    appearances[full_k] += 1
                    break

    stable = sum(
        1 for c in appearances.values()
        if c >= n_resamples * survival_threshold
    )
    return stable / len(full_clusters)


# ═══════════════════════════════════════════════════════════════════════════════
#  Run all algorithms
# ═══════════════════════════════════════════════════════════════════════════════

def run_all(
    fns: list[FuncRecord],
    X: np.ndarray,
    beats_labels: np.ndarray,
    n_beats_clusters: int,
    do_stability: bool = False,
    verbose: bool = True,
) -> list[AlgoResult]:
    results: list[AlgoResult] = []
    step = 0
    total_steps = 11

    def _step(name: str):
        nonlocal step
        step += 1
        if verbose:
            print(f"  [{step}/{total_steps}] {name}")

    def _make_result(
        name: str, labels: np.ndarray, params: str = ""
    ) -> AlgoResult:
        m = evaluate(fns, X, labels, beats_labels)
        oa = (
            outlier_agreement(beats_labels, labels)
            if "Beats" not in name
            else {"f1": 1.0}
        )
        return AlgoResult(
            name=name, labels=labels, params=params,
            outlier_f1=oa["f1"], **m
        )

    # ── 1. Beats reference ───────────────────────────────────────────────
    _step("Beats (reference)")
    results.append(_make_result("Beats (reference)", beats_labels))

    # ── 2–3. HDBSCAN ────────────────────────────────────────────────────
    if HAS_HDBSCAN:
        for mcs in [3, 5]:
            tag = f"HDBSCAN (mcs={mcs})"
            _step(tag)
            labels = _run_hdbscan(X, min_cluster_size=mcs)
            if labels is not None:
                r = _make_result(tag, labels, f"min_cluster_size={mcs}")
                if do_stability:
                    r.stability = bootstrap_stability(
                        fns, X,
                        lambda x, _mcs=mcs: _run_hdbscan(x, _mcs),
                    )
                results.append(r)

    # ── 4. DBSCAN (auto-tuned eps + sweep) ───────────────────────────────
    _step("DBSCAN (auto-eps)")
    auto_eps = estimate_dbscan_eps(X, min_samples=3)
    if verbose:
        print(f"    auto-eps = {auto_eps:.3f}")

    best_dbscan: AlgoResult | None = None
    eps_candidates = sorted(
        set([auto_eps * 0.7, auto_eps, auto_eps * 1.3, 0.5, 0.7, 1.0])
    )
    for eps in eps_candidates:
        labels = _run_dbscan(X, eps=eps, min_samples=3)
        n_cl, _, npct = _label_stats(labels)
        if n_cl < 2 or npct > 80:
            continue
        tag = f"DBSCAN (eps={eps:.2f})"
        r = _make_result(tag, labels, f"eps={eps:.3f}")
        if do_stability:
            r.stability = bootstrap_stability(
                fns, X,
                lambda x, _e=eps: _run_dbscan(x, eps=_e),
            )
        if best_dbscan is None or (
            not math.isnan(r.silhouette)
            and r.silhouette > (
                best_dbscan.silhouette
                if not math.isnan(best_dbscan.silhouette)
                else -999
            )
        ):
            best_dbscan = r
    if best_dbscan:
        results.append(best_dbscan)

    # ── 5. OPTICS ────────────────────────────────────────────────────────
    _step("OPTICS")
    try:
        labels = _run_optics(X, min_samples=3)
        n_cl, _, _ = _label_stats(labels)
        if n_cl >= 2:
            r = _make_result("OPTICS (ms=3)", labels, "min_samples=3")
            if do_stability:
                r.stability = bootstrap_stability(
                    fns, X,
                    lambda x: _run_optics(x, min_samples=3),
                )
            results.append(r)
    except Exception as e:
        if verbose:
            print(f"    OPTICS failed: {e}")

    # ── 6–8. Agglomerative — three linkage strategies ────────────────────
    k = max(n_beats_clusters, 2)
    for linkage in ["ward", "complete", "average"]:
        tag = f"Agglom-{linkage.title()} (k={k})"
        _step(tag)
        try:
            labels = _run_agglom(X, linkage=linkage, n_clusters=k)
            r = _make_result(tag, labels, f"linkage={linkage},k={k}")
            if do_stability:
                r.stability = bootstrap_stability(
                    fns, X,
                    lambda x, _l=linkage, _k=k: _run_agglom(
                        x, linkage=_l, n_clusters=_k
                    ),
                )
            results.append(r)
        except Exception as e:
            if verbose:
                print(f"    {tag} failed: {e}")

    # ── 9. Spectral ──────────────────────────────────────────────────────
    if k >= 2:
        tag = f"Spectral (k={k})"
        _step(tag)
        try:
            labels = _run_spectral(X, n_clusters=k)
            r = _make_result(tag, labels, f"n_clusters={k}")
            results.append(r)
        except Exception as e:
            if verbose:
                print(f"    Spectral failed: {e}")

    # ── 10. K-Means baseline ─────────────────────────────────────────────
    if k >= 2:
        tag = f"K-Means (k={k})"
        _step(tag)
        try:
            labels = _run_kmeans(X, n_clusters=k)
            r = _make_result(tag, labels, f"n_clusters={k}")
            results.append(r)
        except Exception as e:
            if verbose:
                print(f"    K-Means failed: {e}")

    # ── 11. Affinity Propagation ─────────────────────────────────────────
    _step("Affinity Propagation")
    try:
        labels = _run_affprop(X)
        n_cl, _, _ = _label_stats(labels)
        if n_cl >= 2:
            r = _make_result("Affinity Prop", labels)
            results.append(r)
    except Exception as e:
        if verbose:
            print(f"    AffinityProp failed: {e}")

    if verbose:
        print(f"  done — {len(results)} results")

    return results


# ═══════════════════════════════════════════════════════════════════════════════
#  Reporting
# ═══════════════════════════════════════════════════════════════════════════════

def print_table(
    results: list[AlgoResult], beats_labels: np.ndarray
) -> None:
    """Print a formatted comparison table to stdout."""
    header = (
        f"{'Algorithm':<28} {'Clusters':>9} {'Noise%':>7} "
        f"{'Silhouette':>11} {'ImpCoh':>7} {'CallCoh':>8} "
        f"{'Stability':>10} {'ARI':>6} {'OutF1':>6}"
    )
    sep = "─" * len(header)

    print(f"\n{sep}")
    print(header)
    print(sep)

    for r in results:
        def fmt(v):
            return f"{v:.3f}" if not math.isnan(v) else "  —"

        print(
            f"{r.name:<28} {r.n_clusters:>9} {r.noise_pct:>6.1f}% "
            f"{fmt(r.silhouette):>11} {fmt(r.coherence):>7} "
            f"{fmt(r.call_coherence):>8} "
            f"{fmt(r.stability):>10} {fmt(r.ari_vs_beats):>6} "
            f"{fmt(r.outlier_f1):>6}"
        )

    print(sep)

    # ── Outlier agreement detail ─────────────────────────────────────────
    print(f"\nOutlier Detection Agreement (Beats outliers as reference):")
    print(f"{'Algorithm':<28} {'Precision':>10} {'Recall':>8} {'F1':>6}")
    print("─" * 56)
    for r in results:
        if "Beats" in r.name:
            continue
        oa = outlier_agreement(beats_labels, r.labels)
        print(
            f"{r.name:<28} {oa['precision']:>10.3f} "
            f"{oa['recall']:>8.3f} {oa['f1']:>6.3f}"
        )
    print()


def print_ranking_verdict(results: list[AlgoResult]) -> None:
    """Print the multi-criteria ranking and winner determination."""
    print("=" * 70)
    print("RANKING FRAMEWORK")
    print("=" * 70)
    print("""
Each algorithm received the SAME raw data (AST token sequences, imports,
call targets) vectorised into a combined feature matrix, and used its
OWN clustering mechanism — no part of Beats' similarity formula
(Levenshtein, Jaccard, geometric mean) was shared with competitors.

Feature representation:
  • Token bag-of-words + bigram counts  (structural shape)
  • IDF-weighted import indicators      (dependency context)
  • IDF-weighted call-target indicators  (behavioural context)
  • All groups equalised + L2-normalised (Euclidean ≈ cosine)

Criteria:

  1. SILHOUETTE  — Are clusters well-separated in the feature space?
  2. DOMAIN COHERENCE — Do cluster members share imports/call targets?
     (Evaluated post-hoc via Jaccard, not used as clustering input.)
  3. STABILITY  — Do clusters survive 80%-resampling?  (--stability)
  4. OUTLIER F1 — Agreement on noise labels vs Beats.
  5. PARAMETER-FREE — Does the algorithm work without per-repo tuning?
     Beats, HDBSCAN, OPTICS, Affinity Prop are auto; the rest need k.

Beats wins when:
  • Its coherence is >= the best alternative
  • No alternative beats it on BOTH silhouette AND coherence
  • It does this without per-codebase parameter tuning

An alternative wins when:
  • It beats Beats on silhouette AND coherence simultaneously
  • With sane noise ratio (<= 50%) or proven stability (>= 0.7)
""")

    beats = next((r for r in results if "Beats" in r.name), None)
    if not beats:
        return

    challengers = [r for r in results if "Beats" not in r.name]
    threats = []
    for c in challengers:
        sil_better = (
            not math.isnan(c.silhouette)
        ) and c.silhouette > beats.silhouette + 0.01
        coh_better = (
            not math.isnan(c.coherence)
        ) and c.coherence > beats.coherence + 0.01
        noise_sane = c.noise_pct <= 50 or (
            not math.isnan(c.stability) and c.stability >= 0.7
        )
        if sil_better and coh_better and noise_sane:
            threats.append(c.name)

    if not threats:
        print(
            "VERDICT: Beats' approach holds — no alternative dominates on both"
        )
        print("         silhouette AND domain coherence simultaneously.")
    else:
        print(f"VERDICT: Worth investigating: {', '.join(threats)}")
        print(
            "         These beat Beats on both silhouette and coherence."
        )
        print(
            "         Check if they achieve this without per-repo "
            "parameter tuning."
        )


# ═══════════════════════════════════════════════════════════════════════════════
#  Main
# ═══════════════════════════════════════════════════════════════════════════════

# ═══════════════════════════════════════════════════════════════════════════════
#  Held-out edge prediction (coherence circularity check)
#
#  Beats' own coherence metric is partly circular: imports/call targets are
#  used both as a clustering signal AND as the post-hoc quality metric. This
#  section scores a DIFFERENT question: if a fraction of a function's
#  imports/calls were hidden BEFORE clustering (see pkg/ast/holdout.go on the
#  Go side, `beats init --holdout-frac`), do that function's clustermates
#  still "know" the hidden fact via their own TRUE (unredacted) data? A
#  cluster that recovers held-out edges reflects real transferable structure,
#  not just an echo of its own visible input.
#
#  This only produces meaningful numbers when export_json was built from a
#  `beats init` run that had --holdout-frac set — otherwise the truth file
#  will have zero records and every score below is NaN.
# ═══════════════════════════════════════════════════════════════════════════════

def func_id_for(fn: "FuncRecord") -> str:
    """Must exactly match Go's ast.FuncID() format — this is the join key
    between the truth file (written by beats) and the export JSON (read
    here)."""
    return f"{fn.package}|{fn.name}|{fn.file}|{fn.line}"


def load_holdout_truth(path: str) -> tuple[dict[str, dict], dict]:
    """Loads the ground-truth sidecar written by `beats init --holdout-frac`.
    Returns (func_id -> {full_imports, full_calls, held_out_imports,
    held_out_calls} as sets, raw config dict)."""
    d = json.load(open(path))
    truth: dict[str, dict] = {}
    for rec in d.get("records", []):
        fid = rec["func_id"]
        if fid in truth:
            # FuncID is (package|name|file|line). A collision means two
            # DISTINCT functions produced the same key -- silently keeping
            # the second would corrupt scoring for both (wrong full/held-out
            # sets used for whichever one we drop). Fail loud instead: this
            # indicates a broken assumption in the join key, not something
            # safe to paper over.
            raise ValueError(
                f"duplicate func_id in holdout truth file: {fid!r} "
                f"(two functions collided on package|name|file|line -- "
                f"the truth file at {path} cannot be trusted for scoring)"
            )
        truth[fid] = {
            "full_imports": set(rec.get("full_imports") or []),
            "full_calls": set(rec.get("full_calls") or []),
            "held_out_imports": set(rec.get("held_out_imports") or []),
            "held_out_calls": set(rec.get("held_out_calls") or []),
        }
    return truth, d.get("config", {})


def score_holdout_for_labels(
    fns: list["FuncRecord"],
    labels: np.ndarray,
    truth: dict[str, dict],
) -> dict:
    """For every held-out import/call, check whether ANY other member of the
    function's assigned cluster has that item in their TRUE (unredacted)
    set — not their visible/possibly-also-redacted set, since a clustermate
    may have had the exact same item hidden from IT too. Functions with no
    truth record (nothing was redacted from them) contribute their exported
    imports/calls as their "true" set, since nothing was hidden.

    Noise-labeled functions (-1) are excluded on both sides: a noise-labeled
    function was never assigned anywhere to test recovery against, and it
    can't vouch for a clustermate either (it isn't one).
    """
    from collections import defaultdict

    cluster_members: dict = defaultdict(list)
    for i, lbl in enumerate(labels):
        if lbl == -1:
            continue
        cluster_members[lbl].append(i)

    def true_imports(i: int):
        rec = truth.get(func_id_for(fns[i]))
        return rec["full_imports"] if rec else fns[i].imports

    def true_calls(i: int):
        rec = truth.get(func_id_for(fns[i]))
        return rec["full_calls"] if rec else fns[i].calls

    tp = 0
    total = 0
    n_truth_functions = 0
    n_truth_functions_noise = 0

    for i, fn in enumerate(fns):
        rec = truth.get(func_id_for(fn))
        if rec is None:
            continue
        n_truth_functions += 1
        lbl = labels[i]
        if lbl == -1:
            n_truth_functions_noise += 1
            continue
        mates = [j for j in cluster_members[lbl] if j != i]
        if not mates:
            continue
        for item in rec["held_out_imports"]:
            total += 1
            if any(item in true_imports(j) for j in mates):
                tp += 1
        for item in rec["held_out_calls"]:
            total += 1
            if any(item in true_calls(j) for j in mates):
                tp += 1

    hit_rate = tp / total if total else float("nan")
    return {
        "held_out_hits": tp,
        "held_out_total": total,
        "held_out_hit_rate": hit_rate,
        "held_out_truth_functions": n_truth_functions,
        "held_out_truth_functions_noise": n_truth_functions_noise,
    }


def random_baseline_holdout(
    fns: list["FuncRecord"],
    labels: np.ndarray,
    truth: dict[str, dict],
    n_trials: int = 20,
    seed: int = 0,
) -> dict:
    """Shuffles WHICH function gets which label (same cluster-count/size/
    noise-rate distribution as the real assignment) and rescoes, n_trials
    times. This is the null model: a hit rate at or below this is not
    evidence the clustering found anything — it's what random grouping with
    the same shape would already produce (e.g. from near-universal imports
    that show up in most clusters regardless)."""
    rng = np.random.default_rng(seed)
    rates = []
    for _ in range(n_trials):
        shuffled = labels.copy()
        rng.shuffle(shuffled)
        r = score_holdout_for_labels(fns, shuffled, truth)
        if not math.isnan(r["held_out_hit_rate"]):
            rates.append(r["held_out_hit_rate"])
    return {
        "random_baseline_mean": float(np.mean(rates)) if rates else float("nan"),
        "random_baseline_std": float(np.std(rates)) if rates else float("nan"),
        "random_baseline_trials": len(rates),
    }


def print_holdout_table(fns, results, truth, truth_config) -> list[dict]:
    print("\n" + "=" * 70)
    print("HELD-OUT EDGE PREDICTION")
    print("=" * 70)
    n_records = len(truth)
    if n_records == 0:
        print("Truth file has 0 records — was this export built from a "
              "`beats init --holdout-frac` run? Nothing to score.")
        return []
    cfg_frac = truth_config.get("Fraction", "?")
    cfg_seed = truth_config.get("Seed", "?")
    print(f"{n_records} functions had imports/calls hidden before "
          f"clustering (fraction={cfg_frac}, seed={cfg_seed}).")
    print("For each hidden edge: does the function's cluster contain a "
          "member whose TRUE data has that edge?\n")
    print(f"{'Algorithm':<28}{'Hit rate':>10}{'Random bl.':>14}{'Lift':>10}"
          f"{'Edges':>10}{'Noise-excl':>12}")
    rows = []
    for r in results:
        s = score_holdout_for_labels(fns, r.labels, truth)
        b = random_baseline_holdout(fns, r.labels, truth)
        lift = (
            s["held_out_hit_rate"] - b["random_baseline_mean"]
            if not (math.isnan(s["held_out_hit_rate"]) or math.isnan(b["random_baseline_mean"]))
            else float("nan")
        )
        def _clean(v):
            return None if (isinstance(v, float) and math.isnan(v)) else v

        row = {
            "algorithm": r.name,
            "held_out_hits": s["held_out_hits"],
            "held_out_total": s["held_out_total"],
            "held_out_hit_rate": _clean(round(s["held_out_hit_rate"], 4)) if not math.isnan(s["held_out_hit_rate"]) else None,
            "held_out_truth_functions": s["held_out_truth_functions"],
            "held_out_truth_functions_noise": s["held_out_truth_functions_noise"],
            "random_baseline_mean": _clean(round(b["random_baseline_mean"], 4)) if not math.isnan(b["random_baseline_mean"]) else None,
            "random_baseline_std": _clean(round(b["random_baseline_std"], 4)) if not math.isnan(b["random_baseline_std"]) else None,
            "lift": _clean(round(lift, 4)) if not math.isnan(lift) else None,
        }
        rows.append(row)

        def fmt_pct(v):
            return f"{v * 100:.1f}%" if not math.isnan(v) else "n/a"

        print(
            f"{r.name:<28}"
            f"{fmt_pct(s['held_out_hit_rate']):>10}"
            f"{fmt_pct(b['random_baseline_mean']):>14}"
            f"{(f'{lift*100:+.1f}%' if not math.isnan(lift) else 'n/a'):>10}"
            f"{s['held_out_total']:>10}"
            f"{s['held_out_truth_functions_noise']:>12}"
        )
    print(
        "\nHit rate: fraction of hidden imports/calls recovered via cluster "
        "membership.\nRandom bl.: expected hit rate from the SAME cluster "
        "size/noise distribution, shuffled.\nLift: hit rate above the random "
        "baseline — the part attributable to real clustering structure, not "
        "just background\nfrequency of common imports or coverage.\n"
    )
    return rows


def main():
    parser = argparse.ArgumentParser(
        description=(
            "Benchmark clustering algorithms against Beats' structural "
            "clustering.  Each algorithm clusters on raw feature vectors "
            "(token counts, bigrams, IDF-weighted imports/calls) — no "
            "Beats similarity formula is shared."
        ),
        epilog="Run:  beats export --repo /path/to/repo > export.json  first.",
    )
    parser.add_argument(
        "export_json", help="Path to beats export JSON file"
    )
    parser.add_argument(
        "--sample", type=int, default=0,
        help="Randomly sample N functions (0 = use all)",
    )
    parser.add_argument(
        "--stability", action="store_true",
        help="Run bootstrap stability analysis (adds ~10× runtime)",
    )
    parser.add_argument(
        "--json-out", type=str, default="",
        help="Write structured results as JSON to this path "
             "(for cross-repo aggregation)",
    )
    parser.add_argument(
        "--quiet", action="store_true", help="Suppress progress output"
    )
    parser.add_argument(
        "--holdout-truth", type=str, default="",
        help="Path to the holdout ground-truth JSON written by "
             "`beats init --holdout-frac` for this same repo/run. When set, "
             "scores held-out edge prediction per algorithm against a "
             "shuffled-label random baseline.",
    )
    args = parser.parse_args()

    verbose = not args.quiet

    # ── Load ─────────────────────────────────────────────────────────────
    if verbose:
        print(f"Loading {args.export_json} ...")
    fns, meta = load_functions(args.export_json)
    if verbose:
        print(
            f"  repo:      {meta['repo']}\n"
            f"  functions: {meta['n_functions']}  "
            f"(in {meta['n_beats_clusters']} clusters "
            f"+ {meta['n_orphans']} orphans)"
        )

    if len(fns) < 10:
        print(
            "Too few functions for meaningful clustering comparison.",
            file=sys.stderr,
        )
        sys.exit(1)

    # ── Sample ───────────────────────────────────────────────────────────
    if args.sample and args.sample < len(fns):
        rng = np.random.RandomState(42)
        indices = np.sort(rng.choice(len(fns), args.sample, replace=False))
        fns = [fns[i] for i in indices]
        if verbose:
            print(f"  sampled:   {len(fns)} functions")

    # ── Feature matrix ───────────────────────────────────────────────────
    if verbose:
        print(f"\nBuilding feature matrix from raw data ...")
    X, feat_info = build_feature_matrix(fns, verbose=verbose)

    # ── Beats reference labels ───────────────────────────────────────────
    beats_labels = np.array([fn.beats_cluster for fn in fns])

    # ── Run algorithms ───────────────────────────────────────────────────
    if verbose:
        print(
            f"\nRunning clustering algorithms on "
            f"{X.shape[0]}×{X.shape[1]} feature matrix ..."
        )
    results = run_all(
        fns, X, beats_labels,
        n_beats_clusters=meta["n_beats_clusters"],
        do_stability=args.stability,
        verbose=verbose,
    )

    # ── Report ───────────────────────────────────────────────────────────
    print_table(results, beats_labels)
    print_ranking_verdict(results)

    holdout_rows = []
    if args.holdout_truth:
        try:
            truth, truth_config = load_holdout_truth(args.holdout_truth)
            holdout_rows = print_holdout_table(fns, results, truth, truth_config)
        except (OSError, ValueError, KeyError) as e:
            # Don't let a bad/missing truth path or a corrupt truth file
            # (e.g. the collision check above) discard the clustering
            # comparison results already computed and printed above --
            # holdout scoring is a bonus, not load-bearing for the rest of
            # this run's output.
            print(
                f"\nWARNING: holdout scoring skipped -- could not load "
                f"{args.holdout_truth!r}: {e}",
                file=sys.stderr,
            )

    # ── JSON output (for cross-repo aggregation) ─────────────────────────
    if args.json_out:
        json_results = []
        for r in results:
            oa = (
                outlier_agreement(beats_labels, r.labels)
                if "Beats" not in r.name
                else {"precision": 1, "recall": 1, "f1": 1}
            )
            json_results.append({
                "algorithm": r.name,
                "params": r.params,
                "n_clusters": r.n_clusters,
                "noise_count": r.noise_count,
                "noise_pct": round(r.noise_pct, 2),
                "silhouette": (
                    round(r.silhouette, 4)
                    if not math.isnan(r.silhouette) else None
                ),
                "coherence": (
                    round(r.coherence, 4)
                    if not math.isnan(r.coherence) else None
                ),
                "call_coherence": (
                    round(r.call_coherence, 4)
                    if not math.isnan(r.call_coherence) else None
                ),
                "stability": (
                    round(r.stability, 4)
                    if not math.isnan(r.stability) else None
                ),
                "ari_vs_beats": (
                    round(r.ari_vs_beats, 4)
                    if not math.isnan(r.ari_vs_beats) else None
                ),
                "nmi_vs_beats": (
                    round(r.nmi_vs_beats, 4)
                    if not math.isnan(r.nmi_vs_beats) else None
                ),
                "outlier_f1": round(oa["f1"], 4),
            })
        payload = {
            "repo": meta["repo"],
            "n_functions": meta["n_functions"],
            "n_beats_clusters": meta["n_beats_clusters"],
            "features": feat_info,
            "results": json_results,
            "holdout": holdout_rows,
        }
        with open(args.json_out, "w") as f:
            json.dump(payload, f, indent=2)
        if verbose:
            print(f"\nJSON results saved to {args.json_out}")


if __name__ == "__main__":
    main()
