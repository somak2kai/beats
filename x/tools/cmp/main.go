// cmp compares beats structural clusters against a SCIP reference-graph index.
//
// For each beats cluster it answers: if you gave SCIP an equivalent reference query
// (derived from the consensus symbols beats' own members share), would SCIP find the
// same functions?
//
//   Precision = |beats ∩ SCIP query| / |SCIP query|
//               fraction of SCIP results also in beats  → LOW means SCIP over-includes
//
//   Recall    = |beats ∩ SCIP query| / |beats|
//               fraction of beats cluster SCIP confirms → LOW means beats found novel functions
//               invisible to reference-graph search
//
// Usage:
//
//	# 1. Generate SCIP index (requires scip-go)
//	cd /path/to/repo && scip-go --output index.scip
//
//	# 2. Run beats to produce cluster JSON
//	go run /path/to/beats/cmd/main.go --repo=/path/to/repo
//
//	# 3. Run cmp
//	go run . \
//	  --scip=/path/to/repo/index.scip \
//	  --clusters=/tmp/beats/fmeta/cluster_<reponame>.json \
//	  --repo=/path/to/repo \
//	  [--quorum=0.6] [--min-size=4] [--top=0]
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	ds "github.com/somak2kai/beats/pkg/types"
)

func main() {
	scipFile    := flag.String("scip", "", "path to SCIP index file (index.scip)")
	clusterFile := flag.String("clusters", "", "path to beats cluster JSONL file (use --badger instead for IdentifyCluster output)")
	badgerPath  := flag.String("badger", "", "repo path to load TierIdentified clusters from beats BadgerDB (same value as --repo)")
	repoRoot    := flag.String("repo", "", "absolute path to repo root")
	minSize     := flag.Int("min-size", 4, "skip clusters smaller than this")
	topN        := flag.Int("top", 0, "limit report to top N clusters by size (0 = all)")
	showPaths   := flag.Bool("show-paths", false, "print 20 sample SCIP paths and first-cluster member paths, then exit (path alignment diagnostic)")
	quorum      := flag.Float64("quorum", 0.6,
		"fraction of consensus refs a corpus function must match to count as a SCIP query hit")
	flag.Parse()

	if *scipFile == "" || *repoRoot == "" || (*clusterFile == "" && *badgerPath == "") {
		fmt.Fprintln(os.Stderr, "usage: cmp --scip=index.scip --repo=/path/to/repo (--clusters=cluster.json | --badger=/path/to/repo)")
		fmt.Fprintln(os.Stderr, "       --badger       preferred: repo path — DB location resolved via os.TempDir() automatically")
		fmt.Fprintln(os.Stderr, "       --clusters     legacy: load clusters from a JSONL file written by ClusterWriter")
		fmt.Fprintln(os.Stderr, "       --quorum=0.6   consensus-ref match threshold (default 0.6)")
		fmt.Fprintln(os.Stderr, "       --min-size=4   skip clusters smaller than N (default 4)")
		fmt.Fprintln(os.Stderr, "       --top=0        show only top N clusters by size (0 = all)")
		os.Exit(1)
	}

	// Load beats clusters — prefer BadgerDB (TierIdentified) over flat JSONL file
	var clusters []ds.Cluster
	var clusterSource string
	if *badgerPath != "" {
		var err error
		clusters, err = loadClustersFromBadger(*badgerPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading clusters from badger: %v\n", err)
			os.Exit(1)
		}
		clusterSource = badgerPathForRepo(*badgerPath) + " (TierIdentified)"
	} else {
		var err error
		clusters, err = loadClusters(*clusterFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading clusters: %v\n", err)
			os.Exit(1)
		}
		clusterSource = *clusterFile
	}
	fmt.Fprintf(os.Stderr, "loaded %d beats clusters from %s\n", len(clusters), clusterSource)

	// Load SCIP index
	fmt.Fprintf(os.Stderr, "loading SCIP index %s …\n", *scipFile)
	idx, err := loadSCIPIndex(*scipFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading SCIP index: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "SCIP index loaded: %d documents, %d symbols\n\n", idx.DocCount, idx.SymbolCount)

	// Path alignment diagnostic — run with --show-paths to debug mismatches
	if *showPaths {
		fmt.Println("=== SCIP sample paths (first 20) ===")
		for _, p := range idx.SamplePaths(20) {
			fmt.Println(" ", p)
		}
		root := strings.TrimRight(*repoRoot, "/") + "/"
		fmt.Println("\n=== beats member paths from first cluster (stripped with --repo) ===")
		if len(clusters) > 0 {
			for i, m := range clusters[0].Members {
				if i >= 10 {
					fmt.Printf("  … (%d more)\n", len(clusters[0].Members)-10)
					break
				}
				rel := strings.TrimPrefix(m.FileMeta.Path, root)
				inSCIP := idx.HasDocument(rel)
				fmt.Printf("  raw:     %s\n  rel:     %s\n  in SCIP: %v\n\n", m.FileMeta.Path, rel, inSCIP)
			}
		}
		os.Exit(0)
	}

	// Build global corpus map: FunctionKey → SCIP outbound refs
	// This covers every function beats knows about, across all clusters.
	fmt.Fprintln(os.Stderr, "building global function ref map …")
	allFunctionRefs, _ := idx.BuildFunctionRefMap(clusters, *repoRoot)
	fmt.Fprintf(os.Stderr, "corpus: %d unique functions with SCIP ref data\n\n", len(allFunctionRefs))

	// Run comparison per cluster
	var results []ClusterResult
	for i, c := range clusters {
		if c.Size < *minSize {
			continue
		}
		r := compareCluster(i+1, c, idx, *repoRoot, allFunctionRefs, *quorum)
		results = append(results, r)
	}

	// Sort by size descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Size > results[j].Size
	})
	if *topN > 0 && len(results) > *topN {
		results = results[:*topN]
	}

	printReport(results, clusterSource, idx, *quorum)
}

func printReport(results []ClusterResult, clusterFile string, idx *SCIPIndex, quorum float64) {
	fmt.Printf("beats × SCIP comparison\n")
	fmt.Printf("cluster file : %s\n", clusterFile)
	fmt.Printf("SCIP index   : %d documents, %d symbols\n", idx.DocCount, idx.SymbolCount)
	fmt.Printf("clusters     : %d (after size filter)\n", len(results))
	fmt.Printf("quorum       : %.0f%% of consensus refs must match\n\n", quorum*100)
	fmt.Println(strings.Repeat("━", 80))

	// Per-cluster detail
	for _, r := range results {
		label := r.Label
		if label == "" {
			label = "(unlabelled)"
		}
		fmt.Printf("\nCluster %-3d  %s\n", r.ClusterNum, label)
		fmt.Printf("  id: %s   size: %d\n", r.ShapeHash, r.Size)
		fmt.Printf("  beats coherence (import Jaccard) : %.2f\n", r.BeatsCoherence)
		fmt.Printf("  beats call-target Jaccard        : %.2f\n", r.BeatsCallJaccard)
		fmt.Printf("  SCIP within-cluster Jaccard      : %.2f  (match rate %.0f%%)\n",
			r.SCIPRefJaccard, r.SCIPMatchRate*100)

		if len(r.ConsensusRefs) > 0 {
			shown := r.ConsensusRefs
			if len(shown) > 8 {
				shown = shown[:8]
			}
			displayRefs := make([]string, len(shown))
			for i, s := range shown {
				displayRefs[i] = trimSCIPSymbol(s)
			}
			fmt.Printf("  consensus SCIP refs (query)      : %s\n", strings.Join(displayRefs, ", "))
			fmt.Printf("  SCIP query result size           : %d\n", r.SCIPQuerySize)
			fmt.Printf("  intersection                     : %d\n", r.IntersectCount)
			fmt.Printf("  precision (SCIP noise rate)      : %.2f  [want HIGH — means SCIP matches beats]\n", r.Precision)
			fmt.Printf("  recall    (beats coverage)       : %.2f  [want HIGH — means SCIP confirms beats]\n", r.Recall)
			fmt.Printf("  F1                               : %.2f\n", r.F1)
		} else {
			fmt.Printf("  consensus SCIP refs              : (none — no members found in SCIP index)\n")
		}

		if len(r.BeatsOnly) > 0 {
			shown := r.BeatsOnly
			if len(shown) > 5 {
				shown = append(shown[:5], fmt.Sprintf("… +%d more", len(r.BeatsOnly)-5))
			}
			fmt.Printf("  beats-only (novel, SCIP misses)  : %s\n", strings.Join(shown, ", "))
		}
		if len(r.SCIPOnly) > 0 {
			shown := r.SCIPOnly
			if len(shown) > 5 {
				shown = append(shown[:5], fmt.Sprintf("… +%d more", len(r.SCIPOnly)-5))
			}
			fmt.Printf("  SCIP-only (over-included)        : %s\n", strings.Join(shown, ", "))
		}
	}

	// Summary
	fmt.Printf("\n%s\n", strings.Repeat("━", 80))
	fmt.Println("\nSummary")

	total := len(results)
	var precisions, recalls, f1s []float64
	highF1, midF1, lowF1, noData := 0, 0, 0, 0
	for _, r := range results {
		if len(r.ConsensusRefs) == 0 {
			noData++
			continue
		}
		precisions = append(precisions, r.Precision)
		recalls = append(recalls, r.Recall)
		f1s = append(f1s, r.F1)
		switch {
		case r.F1 >= 0.7:
			highF1++
		case r.F1 >= 0.4:
			midF1++
		default:
			lowF1++
		}
	}

	fmt.Printf("  Total clusters compared : %d\n", total)
	pct := func(n int) string {
		if total == 0 {
			return "0%"
		}
		return fmt.Sprintf("%d (%.0f%%)", n, float64(n)/float64(total)*100)
	}
	fmt.Printf("  F1 ≥ 0.7 (strong match) : %s  ← SCIP reproduces beats cluster well\n", pct(highF1))
	fmt.Printf("  F1 0.4–0.7 (partial)    : %s\n", pct(midF1))
	fmt.Printf("  F1 < 0.4 (novel/noisy)  : %s  ← beats found something SCIP cannot reproduce\n", pct(lowF1))
	fmt.Printf("  No SCIP data            : %s\n", pct(noData))

	fmt.Printf("\n  Mean precision          : %.2f\n", meanFloat(precisions))
	fmt.Printf("  Mean recall             : %.2f\n", meanFloat(recalls))
	fmt.Printf("  Mean F1                 : %.2f\n", meanFloat(f1s))

	// Highlight low-recall clusters — these are the most interesting:
	// beats found a structural pattern that SCIP cannot reproduce via reference query.
	var novelClusters []ClusterResult
	for _, r := range results {
		if len(r.ConsensusRefs) > 0 && r.Recall < 0.5 && r.BeatsCoherence > 0.3 {
			novelClusters = append(novelClusters, r)
		}
	}
	if len(novelClusters) > 0 {
		fmt.Printf("\nHigh-coherence beats clusters with low SCIP recall — structural patterns SCIP cannot see:\n")
		for _, r := range novelClusters {
			label := r.Label
			if label == "" {
				label = "(unlabelled)"
			}
			fmt.Printf("  [%s]  beats=%.2f  recall=%.2f  f1=%.2f  beats-only=%d  %s\n",
				r.ShapeHash, r.BeatsCoherence, r.Recall, r.F1, len(r.BeatsOnly), label)
		}
	}

	// Highlight high-F1 clusters — these validate that beats and SCIP agree.
	var validatedClusters []ClusterResult
	for _, r := range results {
		if r.F1 >= 0.7 {
			validatedClusters = append(validatedClusters, r)
		}
	}
	if len(validatedClusters) > 0 {
		fmt.Printf("\nValidated clusters — beats and SCIP agree (F1 ≥ 0.7):\n")
		for _, r := range validatedClusters {
			label := r.Label
			if label == "" {
				label = "(unlabelled)"
			}
			fmt.Printf("  [%s]  f1=%.2f  precision=%.2f  recall=%.2f  %s\n",
				r.ShapeHash, r.F1, r.Precision, r.Recall, label)
		}
	}
}
