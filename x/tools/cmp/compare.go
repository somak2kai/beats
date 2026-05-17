package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	ds "github.com/somak2kai/beats/pkg/types"
)

// ClusterResult is the full comparison output for one beats cluster.
//
// The core question answered here: beats discovered this cluster without any
// query. If you gave a vocabulary-aware tool (SCIP) the equivalent query,
// would it find the same functions?
//
// Precision and Recall answer this:
//
//	Precision = |beats ∩ SCIP| / |SCIP|
//	           fraction of SCIP's results that beats also found
//	           LOW → SCIP over-includes; structural filter beats applies is doing real work
//
//	Recall    = |beats ∩ SCIP| / |beats|
//	           fraction of beats cluster that SCIP's query also finds
//	           LOW → beats found functions the reference query misses entirely
//	                 (same structure, different vocabulary — the novel signal)
type ClusterResult struct {
	ClusterNum int
	Label      string
	Size       int
	ShapeHash  string

	// beats-side coherence (from cluster data)
	BeatsCoherence   float64 // mean pairwise Jaccard of DirectImports
	BeatsCallJaccard float64 // mean pairwise Jaccard of raw CallTargets

	// SCIP-side coherence (within-cluster, sanity check)
	SCIPRefJaccard float64
	SCIPMatchRate  float64 // fraction of members found in SCIP index

	// Discovery comparison — the main result
	ConsensusRefs  []string // SCIP symbols shared by ≥50% of matched members
	SCIPQuerySize  int      // how many corpus functions the consensus query returns
	IntersectCount int      // |beats ∩ SCIP query|
	Precision      float64  // |beats ∩ SCIP| / |SCIP query| — SCIP noise rate
	Recall         float64  // |beats ∩ SCIP| / |beats|       — beats unique finds
	F1             float64

	// Members in beats but NOT in SCIP query result — structural pattern invisible to reference search
	BeatsOnly []string
	// Members in SCIP query but NOT in beats cluster — over-inclusion by reference search
	SCIPOnly []string
}

// compareCluster runs the full comparison for one beats cluster.
//
// allFunctionRefs is the global corpus map: FunctionKey → SCIP outbound refs,
// built once upfront across all clusters. This is what lets us ask "what would
// SCIP return for this query across the whole codebase?"
func compareCluster(
	n int,
	c ds.Cluster,
	idx *SCIPIndex,
	repoRoot string,
	allFunctionRefs map[FunctionKey][]string,
	quorum float64, // fraction of consensus refs a function must match to count as a query hit
) ClusterResult {

	root := strings.TrimRight(repoRoot, "/") + "/"

	// --- beats call-target Jaccard (from beats data, no SCIP needed) ---
	callSets := make([][]string, len(c.Members))
	for i, m := range c.Members {
		callSets[i] = m.CallTargets
	}
	beatsCallJaccard := meanPairwiseJaccard(callSets)

	// --- Per-member SCIP refs and within-cluster coherence ---
	type memberWithRefs struct {
		key  FunctionKey
		name string
		refs []string
	}
	var matched []memberWithRefs
	for _, m := range c.Members {
		relPath := strings.TrimPrefix(m.FileMeta.Path, root)
		key := FunctionKey{RelPath: relPath, StartLine: m.Start_line}
		refs := idx.OutboundRefs(relPath, m.Start_line, m.End_line)
		if len(refs) > 0 {
			matched = append(matched, memberWithRefs{key, qualifiedName(m), refs})
		}
	}

	scipMatchRate := 0.0
	if len(c.Members) > 0 {
		scipMatchRate = float64(len(matched)) / float64(len(c.Members))
	}

	scipSets := make([][]string, len(matched))
	for i, mr := range matched {
		scipSets[i] = mr.refs
	}
	scipRefJaccard := meanPairwiseJaccard(scipSets)

	// --- Consensus refs: symbols shared by ≥50% of matched members ---
	// These become the "translated SCIP query" for this beats cluster.
	consensusRefs := topSharedRefs(scipSets, 0.50)

	// --- Discovery comparison ---
	// Build the beats cluster as a key set for O(1) lookup.
	beatsKeySet := make(map[FunctionKey]string, len(c.Members))
	for _, m := range c.Members {
		relPath := strings.TrimPrefix(m.FileMeta.Path, root)
		key := FunctionKey{RelPath: relPath, StartLine: m.Start_line}
		beatsKeySet[key] = qualifiedName(m)
	}

	var precision, recall, f1 float64
	var scipQueryHits []FunctionKey
	var beatsOnlyNames, scipOnlyNames []string

	if len(consensusRefs) > 0 {
		// Query the global corpus: find all functions that reference ≥quorum
		// fraction of the consensus symbols.
		scipQueryHits = queryCorpus(allFunctionRefs, consensusRefs, quorum)

		// Build SCIP query result as a key set.
		scipKeySet := make(map[FunctionKey]struct{}, len(scipQueryHits))
		for _, k := range scipQueryHits {
			scipKeySet[k] = struct{}{}
		}

		// Intersection
		intersect := 0
		for k := range beatsKeySet {
			if _, ok := scipKeySet[k]; ok {
				intersect++
			}
		}

		if len(scipQueryHits) > 0 {
			precision = float64(intersect) / float64(len(scipQueryHits))
		}
		if len(beatsKeySet) > 0 {
			recall = float64(intersect) / float64(len(beatsKeySet))
		}
		if precision+recall > 0 {
			f1 = 2 * precision * recall / (precision + recall)
		}

		// beats-only: in beats cluster, NOT in SCIP query result
		// These are the functions beats found purely through structural fingerprinting
		// that a vocabulary-aware query misses entirely.
		for k, name := range beatsKeySet {
			if _, ok := scipKeySet[k]; !ok {
				beatsOnlyNames = append(beatsOnlyNames, name)
			}
		}
		sort.Strings(beatsOnlyNames)

		// SCIP-only: in SCIP query result, NOT in beats cluster
		// These are false positives from the reference query — functions that call
		// the same things but have a different structural shape.
		for _, k := range scipQueryHits {
			if _, ok := beatsKeySet[k]; !ok {
				// Look up the name from matched entries if available
				scipOnlyNames = append(scipOnlyNames, k.RelPath+":"+itoa(k.StartLine))
			}
		}
		sort.Strings(scipOnlyNames)
	}

	return ClusterResult{
		ClusterNum:       n,
		Label:            c.Label,
		Size:             c.Size,
		ShapeHash:        c.ShapeHash,
		BeatsCoherence:   c.Coherence,
		BeatsCallJaccard: beatsCallJaccard,
		SCIPRefJaccard:   scipRefJaccard,
		SCIPMatchRate:    scipMatchRate,
		ConsensusRefs:    consensusRefs,
		SCIPQuerySize:    len(scipQueryHits),
		IntersectCount:   int(math.Round(precision * float64(len(scipQueryHits)))),
		Precision:        precision,
		Recall:           recall,
		F1:               f1,
		BeatsOnly:        beatsOnlyNames,
		SCIPOnly:         scipOnlyNames,
	}
}

// queryCorpus returns all functions in the corpus whose SCIP outbound refs
// contain at least `quorum` fraction of the query symbols.
//
// quorum=1.0 → function must reference ALL query symbols (strict intersection)
// quorum=0.5 → function must reference at least half (more inclusive)
func queryCorpus(
	allFunctionRefs map[FunctionKey][]string,
	query []string,
	quorum float64,
) []FunctionKey {

	if len(query) == 0 {
		return nil
	}

	querySet := make(map[string]struct{}, len(query))
	for _, q := range query {
		querySet[q] = struct{}{}
	}
	threshold := int(math.Ceil(float64(len(query)) * quorum))
	if threshold < 1 {
		threshold = 1
	}

	var hits []FunctionKey
	for key, refs := range allFunctionRefs {
		count := 0
		for _, r := range refs {
			if _, ok := querySet[r]; ok {
				count++
			}
		}
		if count >= threshold {
			hits = append(hits, key)
		}
	}
	return hits
}

// jaccardStrings computes Jaccard similarity of two string slices treated as sets.
func jaccardStrings(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	setA := make(map[string]struct{}, len(a))
	for _, s := range a {
		setA[s] = struct{}{}
	}
	intersection := 0
	setB := make(map[string]struct{}, len(b))
	for _, s := range b {
		setB[s] = struct{}{}
		if _, ok := setA[s]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// meanPairwiseJaccard computes mean Jaccard over all pairs in a set collection.
func meanPairwiseJaccard(sets [][]string) float64 {
	if len(sets) < 2 {
		return 0
	}
	total := 0.0
	count := 0
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			total += jaccardStrings(sets[i], sets[j])
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

// topSharedRefs returns full SCIP symbols present in at least minFraction of
// the sets, sorted by frequency descending. Full symbols are returned so they
// can be used directly in queryCorpus without mismatch. Use trimSCIPSymbol
// when displaying them to a human.
func topSharedRefs(sets [][]string, minFraction float64) []string {
	if len(sets) == 0 {
		return nil
	}
	freq := make(map[string]int)
	for _, refs := range sets {
		seen := make(map[string]struct{})
		for _, r := range refs {
			if _, already := seen[r]; !already {
				freq[r]++
				seen[r] = struct{}{}
			}
		}
	}
	threshold := int(float64(len(sets)) * minFraction)
	if threshold < 1 {
		threshold = 1
	}
	var result []string
	for sym, cnt := range freq {
		if cnt >= threshold {
			result = append(result, sym)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		fi, fj := freq[result[i]], freq[result[j]]
		if fi != fj {
			return fi > fj
		}
		return result[i] < result[j]
	})
	return result
}

// trimSCIPSymbol extracts the human-readable descriptor from a full SCIP symbol.
// SCIP Go symbols are space-separated: "scip-go gomod <pkg> <version> <descriptor>"
// e.g. "scip-go gomod code.gitea.io/gitea v0.0.1 `models/db`/GetEngine()."
// → "`models/db`/GetEngine()."
func trimSCIPSymbol(s string) string {
	parts := strings.Fields(s)
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return s
}

func meanFloat(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
