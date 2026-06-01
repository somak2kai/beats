package ast

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	ds "github.com/somak2kai/beats/pkg/types"
)

// ── Single-pass agglomerative clustering ─────────────────────────────────────

// identifyThreshold is the minimum combined similarity score for two functions
// to be considered members of the same cluster.
//
// Score = 0.5×seqSim + 0.3×importJaccard + 0.2×callJaccard
//
// At 0.55 the gate sits between:
//   - identical sequence + mismatched imports (~0.515) → reject
//   - identical sequence + moderate import overlap (~0.65+) → accept
const (
	identifyThreshold = 0.55
	identifyMinSize   = 2

	// maxTrigranBucket caps how many functions a single trigram may appear in
	// before it is treated as a structural stop-word and skipped during candidate
	// pair generation. A trigram shared by 300+ functions (≈2.7% of an 11k
	// corpus) carries no discriminating signal and generates O(300²)=45k pairs
	// that are almost all rejected in the scoring step anyway.
	maxTrigranBucket = 300
	// anything below this many number of token sequences for a function,
	// we wont have enough data to make a judicious clustering.
	minTokenSeqLen = 4
)

// pairKey is a canonical ordered (i < j) pair of function indices into the fns slice.
type pairKey struct {
	i, j int
}

// scoredPair is a candidate pair with its combined similarity score.
type scoredPair struct {
	i, j  int
	score float64 // indicates scoring of possibility of belonging in same cluster for the function metadata pair.
}

// IdentifyClusters builds clusters in a single agglomerative pass over all
// functions. Import and call target similarity are part of the grouping decision
// from the start — not a post-hoc merge step — which prevents contaminated
// clusters from ever forming.
//
// Algorithm:
//  1. Build a trigram map from each function's TokenSeqHash. Functions sharing a trigram are structural candidates.
//  2. For each candidate pair, compute the combined score. Fast-reject pairs
//     with seqSim < 0.40 before touching import/call sets.
//  3. Perform simple union find algoritm to define member and agglomerative clustering at identifyThreshold: two groups
//     merge only when every cross-group pair exceeds the threshold, preventing
//     chaining artefacts.
//  4. Drop clusters below identifyMinSize and structural stop-words (≥ 5% of
//     corpus).
func IdentifyClusters(fns []ds.FunctionMeta) []ds.Cluster {
	if len(fns) == 0 {
		return nil
	}
	primitiveThreshold := float64(len(fns)) * 0.05

	trigramMap := buildTrigramMap(fns)
	sharedCounts := countSharedTrigrams(trigramMap)
	candidates, pairScores := scorePairs(fns, sharedCounts)
	clusterMembers := agglomerate(fns, candidates, pairScores)
	return buildClusters(fns, clusterMembers, primitiveThreshold)
}

// buildTrigramMap maps each trigram hash to the indices of functions that contain it.
// Functions with fewer than minTokenSeqLen tokens are excluded — not enough
// structure to be discriminating.
func buildTrigramMap(fns []ds.FunctionMeta) map[int64][]int {
	trigramMap := make(map[int64][]int, len(fns))
	for i, fn := range fns {
		if len(fn.TokenSeq) < minTokenSeqLen {
			continue
		}
		if fn.GeneratedCode { // ignored auto generated code gen.
			continue
		}
		for _, h := range fn.TokenSeqHash {
			trigramMap[h] = append(trigramMap[h], i)
		}
	}
	return trigramMap
}

// countSharedTrigrams counts how many trigram hashes each (i,j) candidate pair
// shares. Buckets larger than maxTrigranBucket are skipped — that trigram is a
// structural stop-word and would generate O(n²) noise pairs.
func countSharedTrigrams(trigramMap map[int64][]int) map[pairKey]int {
	shared := make(map[pairKey]int)
	for _, bucket := range trigramMap {
		if len(bucket) > maxTrigranBucket {
			continue // stop-word trigram — too common to discriminate
		}
		for a := range bucket {
			for b := a + 1; b < len(bucket); b++ {
				shared[pairKey{bucket[a], bucket[b]}]++
			}
		}
	}
	return shared
}

// scorePairs filters and scores candidate pairs using the three-term similarity
// formula: 0.5×seqSim + 0.3×importJaccard + 0.2×callJaccard.
//
// Returns:
//   - candidates: pairs above identifyThreshold, sorted descending by score
//   - pairScores: lookup map used by the complete-linkage gate
func scorePairs(fns []ds.FunctionMeta, sharedCounts map[pairKey]int) ([]scoredPair, map[pairKey]float64) {
	// pre-compute per-function keys and sets once — reused across all pair lookups.
	keys := make([]string, len(fns))
	importSets := make([]map[string]bool, len(fns))
	callSets := make([]map[string]bool, len(fns))
	for i, fn := range fns {
		keys[i] = seqKey(fn.TokenSeq)
		importSets[i] = toStringSet(fn.DirectImports)
		callSets[i] = toStringSet(fn.CallTargets)
	}

	var candidates []scoredPair
	pairScores := make(map[pairKey]float64, len(sharedCounts))

	for pk, cnt := range sharedCounts {
		// adaptive minimum: require ≥2 shared trigrams when both functions have
		// enough trigrams to be discriminating; 1 is sufficient for short seqs.
		minShared := 1
		if len(fns[pk.i].TokenSeqHash) >= 2 && len(fns[pk.j].TokenSeqHash) >= 2 {
			minShared = 2
		}
		if cnt < minShared {
			continue
		}

		// identical sequence → seqSim = 1.0, no edit distance needed.
		var seqS float64
		if keys[pk.i] == keys[pk.j] {
			seqS = 1.0
		} else {
			seqS = seqSimilarity(fns[pk.i].TokenSeq, fns[pk.j].TokenSeq)
			if seqS < 0.40 {
				continue // fast reject — structurally too different
			}
		}

		impS := jaccard(importSets[pk.i], importSets[pk.j])
		callS := jaccard(callSets[pk.i], callSets[pk.j])
		score := math.Cbrt(seqS * impS * callS)

		if score < identifyThreshold {
			continue // pre-filter: no point passing to agglomerate if completeLinkageCheck would reject it anyway
		}

		candidates = append(candidates, scoredPair{pk.i, pk.j, score})
		pairScores[pk] = score
	}

	// sort descending so we process highest-confidence merges first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	return candidates, pairScores
}

// agglomerate runs complete-linkage agglomerative clustering over the scored
// pairs using Union-Find for efficient membership tracking.
// Returns clusterMembers: root index → all member indices in that cluster.
func agglomerate(fns []ds.FunctionMeta, candidates []scoredPair, pairScores map[pairKey]float64) map[int][]int {
	parent := make([]int, len(fns))
	for i := range parent {
		parent[i] = i
	}
	clusterMembers := make(map[int][]int, len(fns))
	for i := range fns {
		clusterMembers[i] = []int{i}
	}

	findRoot := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path compression
			x = parent[x]
		}
		return x
	}

	for _, cp := range candidates {
		if cp.score < identifyThreshold {
			break // sorted descending — everything below is under threshold
		}

		ri := findRoot(cp.i)
		rj := findRoot(cp.j)
		if ri == rj {
			continue // already in the same cluster
		}

		if !completeLinkageCheck(clusterMembers[ri], clusterMembers[rj], pairScores) {
			continue
		}

		// merge smaller group into larger to keep clusterMembers lookups cheap.
		if len(clusterMembers[ri]) < len(clusterMembers[rj]) {
			ri, rj = rj, ri
		}
		parent[rj] = ri
		clusterMembers[ri] = append(clusterMembers[ri], clusterMembers[rj]...)
		delete(clusterMembers, rj)
	}

	return clusterMembers
}

// completeLinkageCheck returns true only when every cross-cluster pair (a, b)
// has a recorded score above identifyThreshold. A missing pair is treated as
// score 0 — if two functions were never candidates they cannot bridge clusters.
func completeLinkageCheck(membA, membB []int, pairScores map[pairKey]float64) bool {
	for _, a := range membA {
		for _, b := range membB {
			i, j := a, b
			if i > j {
				i, j = j, i // normalise to (smaller, larger) to match pairScores key order
			}
			s, ok := pairScores[pairKey{i, j}]
			if !ok || s < identifyThreshold {
				return false
			}
		}
	}
	return true
}

// buildClusters converts raw cluster membership data into Cluster objects.
// Filters out singletons, structural stop-words, test clusters, and init clusters.
// Disambiguates ShapeHash collisions and sorts by size descending.
func buildClusters(fns []ds.FunctionMeta, clusterMembers map[int][]int, primitiveThreshold float64) []ds.Cluster {
	var clusters []ds.Cluster

	for _, idxs := range clusterMembers {
		if len(idxs) < identifyMinSize {
			continue
		}
		if float64(len(idxs)) >= primitiveThreshold {
			continue // structural stop-word — too common to carry signal
		}

		metas := make([]ds.FunctionMeta, len(idxs))
		for k, idx := range idxs {
			metas[k] = fns[idx]
		}

		// ignore clusters formed due to scanning any test files.
		// init function in golang are ignored, typically come up with best coherence
		// but hardly identifies any meaningful pattern. so we drop them.
		// TODO might be users discretions.
		if isTestingCluster(metas) || isInitCluster(metas) {
			continue
		}

		clusters = append(clusters, assembleCluster(metas))
	}

	disambiguateShapeHashes(clusters)

	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Size > clusters[j].Size
	})
	return clusters
}

// assembleCluster builds a single Cluster value from its member FunctionMeta slice.
// Picks the modal token sequence as the cluster representative — members may have
// slightly different sequences when merged via near-identity.
func assembleCluster(metas []ds.FunctionMeta) ds.Cluster {
	seqFreq := make(map[string]int)
	seqForKey := make(map[string][]int)
	for _, m := range metas {
		k := seqKey(m.TokenSeq)
		seqFreq[k]++
		seqForKey[k] = m.TokenSeq
	}
	bestKey := ""
	for k, cnt := range seqFreq {
		if bestKey == "" || cnt > seqFreq[bestKey] {
			bestKey = k
		}
	}

	c := ds.Cluster{
		SeqKey:    bestKey,
		ShapeHash: shapeHash(bestKey),
		TokenSeq:  seqForKey[bestKey],
		Members:   metas,
		Size:      len(metas),
	}
	c.Profile = computeProfile(metas)
	c.Coherence = computeCoherence(metas)
	c.CallCoherence = computeCallCoherence(metas)
	return c
}

// disambiguateShapeHashes appends a numeric suffix to ShapeHash when two clusters
// share the same modal token sequence, keeping DB keys unique across runs.
func disambiguateShapeHashes(clusters []ds.Cluster) {
	hashCount := make(map[string]int)
	for i := range clusters {
		h := clusters[i].ShapeHash
		if hashCount[h] > 0 {
			clusters[i].ShapeHash = fmt.Sprintf("%s-%d", h, hashCount[h])
		}
		hashCount[h]++
	}
}

// toStringSet converts a string slice to a set map for Jaccard computation.
func toStringSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}

// testingImports is the set of import paths that mark a function as belonging
// to test infrastructure. A cluster whose members are majority test-imports is
// noise in structural analysis and should be dropped.
var testingImports = map[string]bool{
	"testing":                             true,
	"github.com/stretchr/testify/require": true,
	"github.com/stretchr/testify/assert":  true,
	"github.com/stretchr/testify/suite":   true,
	"github.com/stretchr/testify/mock":    true,
}

// isTestingCluster returns true when a majority of members import one or more
// test infrastructure packages. "Majority" is defined as > 50% of members —
// a loose threshold so that test-helper packages (e.g. storetest, searchtest)
// that mix a few non-test imports still get caught.
func isTestingCluster(members []ds.FunctionMeta) bool {
	if len(members) == 0 {
		return false
	}
	count := 0
	for _, m := range members {
		for _, imp := range m.DirectImports {
			if testingImports[imp] {
				count++
				break // count this member once even if it has multiple test imports
			}
		}
	}
	return count > len(members)/2
}

// isInitCluster returns true when every member is a Go init() function.
// init() is runtime-invoked and exists solely for side-effect registration
// (model registration, provider registration, flag init, etc.). Structural
// similarity across init() functions is a language artifact — all init()
// bodies do registration — not an architectural signal worth surfacing.
// Note: short init() bodies (< 4 tokens) are already dropped by the
// minTokenSeqLen guard; this catches longer ones that share the same pattern.
func isInitCluster(members []ds.FunctionMeta) bool {
	for _, m := range members {
		if m.Name != "init" {
			return false
		}
	}
	return len(members) > 0
}

// ── Profile ───────────────────────────────────────────────────────────────────

func computeProfile(members []ds.FunctionMeta) ds.ClusterProfile {
	n := float64(len(members))
	var p ds.ClusterProfile

	// initialise min/max sentinels
	p.CycloMin = math.MaxInt32
	p.CallsMin = math.MaxInt32

	importFreq := make(map[string]int)
	callFreq := make(map[string]int)

	// accumulators for std dev and percentile computation
	cycloVals := make([]float64, len(members))
	nestingVals := make([]float64, len(members))
	callVals := make([]float64, len(members))
	earlyReturnVals := make([]float64, len(members))
	deferCountVals := make([]float64, len(members))

	for i, fn := range members {
		f := fn.Features

		// cyclomatic
		if f.CyclomaticComplexity < p.CycloMin {
			p.CycloMin = f.CyclomaticComplexity
		}
		if f.CyclomaticComplexity > p.CycloMax {
			p.CycloMax = f.CyclomaticComplexity
		}
		p.CycloMean += float64(f.CyclomaticComplexity)
		cycloVals[i] = float64(f.CyclomaticComplexity)

		// nesting
		if f.NestingDepth > p.NestingMax {
			p.NestingMax = f.NestingDepth
		}
		nestingVals[i] = float64(f.NestingDepth)

		// outbound calls
		if f.OutboundCalls < p.CallsMin {
			p.CallsMin = f.OutboundCalls
		}
		if f.OutboundCalls > p.CallsMax {
			p.CallsMax = f.OutboundCalls
		}
		p.CallsMean += float64(f.OutboundCalls)
		callVals[i] = float64(f.OutboundCalls)

		// early returns and defer (raw counts for percentiles)
		earlyReturnVals[i] = float64(f.EarlyReturns)
		deferCountVals[i] = float64(f.ControlFlow.Defer)

		// rates (boolean / count → fraction)
		if f.ControlFlow.Defer > 0 {
			p.DeferRate++
		}
		if fn.Features.EarlyReturns > 0 {
			p.EarlyReturnRate++
		}
		if f.HasContextParam {
			p.ContextParamRate++
		}
		if f.HasErrorReturn {
			p.ErrorReturnRate++
		}
		if f.GoroutineSpawns > 0 {
			p.GoroutineRate++
		}

		// frequency maps
		for _, imp := range fn.DirectImports {
			importFreq[imp]++
		}
		for _, ct := range fn.CallTargets {
			callFreq[ct]++
		}
	}

	// normalise means
	p.CycloMean /= n
	p.CallsMean /= n
	p.DeferRate /= n
	p.EarlyReturnRate /= n
	p.ContextParamRate /= n
	p.ErrorReturnRate /= n
	p.GoroutineRate /= n

	// cyclo std dev (second pass — before sort)
	var variance float64
	for _, v := range cycloVals {
		d := v - p.CycloMean
		variance += d * d
	}
	p.CycloStd = math.Sqrt(variance / n)

	// sort all distributions then compute percentiles
	sort.Float64s(cycloVals)
	sort.Float64s(nestingVals)
	sort.Float64s(callVals)
	sort.Float64s(earlyReturnVals)
	sort.Float64s(deferCountVals)

	p.CycloP50 = percentileF(cycloVals, 0.50)
	p.CycloP75 = percentileF(cycloVals, 0.75)
	p.CycloP95 = percentileF(cycloVals, 0.95)

	p.NestingP50 = percentileF(nestingVals, 0.50)
	p.NestingP75 = percentileF(nestingVals, 0.75)
	p.NestingP95 = percentileF(nestingVals, 0.95)

	p.CallsP50 = percentileF(callVals, 0.50)
	p.CallsP75 = percentileF(callVals, 0.75)
	p.CallsP95 = percentileF(callVals, 0.95)

	p.EarlyReturnsP50 = percentileF(earlyReturnVals, 0.50)
	p.EarlyReturnsP75 = percentileF(earlyReturnVals, 0.75)
	p.EarlyReturnsP95 = percentileF(earlyReturnVals, 0.95)

	p.DeferCountP50 = percentileF(deferCountVals, 0.50)
	p.DeferCountP75 = percentileF(deferCountVals, 0.75)
	p.DeferCountP95 = percentileF(deferCountVals, 0.95)

	// top N by frequency
	p.TopImports = topNKeys(importFreq, 5)
	p.TopCallTargets = topNKeys(callFreq, 5)

	return p
}

// computeCoherence returns mean pairwise Jaccard similarity of DirectImports.
// 1.0 = all members share the same imports (tight domain)
// 0.0 = members import completely different things (heterogeneous)
func computeCoherence(members []ds.FunctionMeta) float64 {
	if len(members) < 2 {
		return 1.0
	}

	sets := make([]map[string]bool, len(members))
	for i, fn := range members {
		s := make(map[string]bool, len(fn.DirectImports))
		for _, imp := range fn.DirectImports {
			s[imp] = true
		}
		sets[i] = s
	}

	return meanPairwiseJaccard(sets)
}

// computeCallCoherence returns mean pairwise Jaccard similarity of CallTargets.
// 1.0 = all members call the same external functions (tight structural role)
// 0.0 = members call completely different things (cross-cutting structural shape)
func computeCallCoherence(members []ds.FunctionMeta) float64 {
	if len(members) < 2 {
		return 1.0
	}

	sets := make([]map[string]bool, len(members))
	for i, fn := range members {
		s := make(map[string]bool, len(fn.CallTargets))
		for _, ct := range fn.CallTargets {
			s[ct] = true
		}
		sets[i] = s
	}

	return meanPairwiseJaccard(sets)
}

// meanPairwiseJaccard computes the mean pairwise Jaccard similarity over a
// slice of sets. Stride-samples down to 50 when the slice is large so that
// O(n²) comparisons remain cheap. Sampling is deterministic (no randomness).
func meanPairwiseJaccard(sets []map[string]bool) float64 {
	// cap pairwise comparisons for large clusters (O(n²) gets expensive).
	// Stride-sample so the selection is deterministic across runs and evenly
	// distributed across the full member slice — neither the first-N bias
	// (samples one cohort if the cluster is heterogeneous) nor the random-shuffle
	// non-determinism (coherence changes on every run, making Labelable unstable).
	if len(sets) > 50 {
		stride := len(sets) / 50
		sampled := make([]map[string]bool, 0, 50)
		for i := 0; i < len(sets); i += stride {
			sampled = append(sampled, sets[i])
		}
		sets = sampled
	}

	var total float64
	var pairs int
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			total += jaccard(sets[i], sets[j])
			pairs++
		}
	}
	if pairs == 0 {
		return 0.0
	}
	return total / float64(pairs)
}

// Representatives returns the n members closest to the cluster centroid
// in [cyclomatic, nesting, outboundCalls] space. Use these as LLM examples.
func Representatives(c ds.Cluster, n int) []ds.FunctionMeta {
	if len(c.Members) <= n {
		return c.Members
	}

	centroid := [3]float64{
		c.Profile.CycloMean,
		float64(c.Profile.NestingMax) / 2,
		c.Profile.CallsMean,
	}

	type scored struct {
		fn   ds.FunctionMeta
		dist float64
	}
	scores := make([]scored, len(c.Members))
	for i, fn := range c.Members {
		f := fn.Features
		v := [3]float64{
			float64(f.CyclomaticComplexity),
			float64(f.NestingDepth),
			float64(f.OutboundCalls),
		}
		var dist float64
		for k := range centroid {
			d := centroid[k] - v[k]
			dist += d * d
		}
		scores[i] = scored{fn, dist}
	}
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].dist < scores[j].dist
	})

	result := make([]ds.FunctionMeta, n)
	for i := range result {
		result[i] = scores[i].fn
	}
	return result
}

// seqSimilarity returns 1 − (editDistance / maxLen), clamped to [0, 1].
func seqSimilarity(a, b []int) float64 {
	la, lb := len(a), len(b)
	if la == 0 && lb == 0 {
		return 1.0
	}
	maxLen := max(la, lb)
	dist := editDistance(a, b)
	return 1.0 - float64(dist)/float64(maxLen)
}

// editDistance computes the Levenshtein edit distance between two int slices.
func editDistance(a, b []int) int {
	la, lb := len(a), len(b)

	// dp[j] = edit distance between a[:current row] and b[:j]
	// use two rolling rows to keep allocations small
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1]
			} else {
				curr[j] = 1 + min(prev[j], min(curr[j-1], prev[j-1]))
			}
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// WriteIndex writes a compact markdown index of labelled clusters.
// One line per cluster header, followed by file:line locations of every member.
// Designed to be read by an LLM as a lookup table: "which functions implement
// pattern X, and exactly where are they?"
//
// Only clusters with a non-empty Label are written. Call this after the
// labelling pass has populated Cluster.Label.
func WriteIndex(w io.Writer, repo string, clusters []ds.Cluster) error {
	labelled := 0
	for _, c := range clusters {
		if c.Label != "" {
			labelled++
		}
	}

	fmt.Fprintf(w, "# beats index — %s\n\n", repo) //nolint:errcheck
	fmt.Fprintf(w, "%d patterns\n\n", labelled)    //nolint:errcheck
	fmt.Fprintf(w, "---\n\n")                      //nolint:errcheck

	for _, c := range clusters {
		if c.Label == "" {
			continue
		}
		fmt.Fprintf(w, "## %s\n", c.Label) //nolint:errcheck
		//nolint:errcheck
		fmt.Fprintf(w, "id:%s  size:%d  coherence:%.2f  shape:%s\n\n",
			c.ShapeHash, c.Size, c.Coherence, seqString(c.TokenSeq))

		for _, m := range c.Members {
			fmt.Fprintf(w, "- %s.%s  %s:%d\n", m.Package, m.Name, m.FileMeta.Path, m.Start_line) //nolint:errcheck
		}
		fmt.Fprintf(w, "\n") //nolint:errcheck
	}
	return nil
}

// shapeHash returns a stable 16-hex-char identity for a token sequence key.
// Uses the first 8 bytes of SHA-256 — collision probability ~1/2^64 per pair.
// Stable across runs: same token sequence always produces the same hash.
func shapeHash(seqKey string) string {
	h := sha256.Sum256([]byte(seqKey))
	return fmt.Sprintf("%016x", h[:8])
}

// percentileF returns the p-th percentile of a pre-sorted slice using linear
// interpolation. p must be in [0, 1]. Caller is responsible for sorting.
func percentileF(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := p * float64(n-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// seqKey converts a token sequence to a stable string map key.
func seqKey(tokens []int) string {
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = fmt.Sprintf("%d", t)
	}
	return strings.Join(parts, ",")
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0.0 // both empty — no shared vocabulary, not "perfectly similar"
	}
	var intersection int
	for k := range a {
		if b[k] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0.0
	}
	return float64(intersection) / float64(union)
}

// topNKeys returns the n most frequent keys from freq, sorted by count desc.
// Ties are broken alphabetically for determinism — this is a display field
// (used only in the LLM label file), not a clustering signal.
func topNKeys(freq map[string]int, n int) []string {
	type kv struct {
		key   string
		count int
	}
	ranked := make([]kv, 0, len(freq))
	for k, v := range freq {
		ranked = append(ranked, kv{k, v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].key < ranked[j].key
	})
	out := make([]string, 0, n)
	for i := 0; i < n && i < len(ranked); i++ {
		out = append(out, ranked[i].key)
	}
	return out
}

var tokenNames = []string{
	"IF", "FOR", "RANGE", "SWITCH", "CASE", "SELECT", "COMM",
	"RETURN", "GO", "SEND", "DEFER", "CONTINUE", "BREAK", "GOTO",
	"CALL", "FUNCLIT", "ASSIGN", "CALL_PKG", "CALL_METHOD",
}

func tokenName(t int) string {
	if t >= 0 && t < len(tokenNames) {
		return tokenNames[t]
	}
	return fmt.Sprintf("T%d", t)
}

func seqString(seq []int) string {
	parts := make([]string, len(seq))
	for i, t := range seq {
		parts[i] = tokenName(t)
	}
	return strings.Join(parts, " ")
}

// WriteClusters writes a compact, LLM-readable context file from a slice of
// labellable clusters (non-primitive, size >= 4, seq length >= 3).
// Pass repo name and total corpus size for the header.
func WriteClusters(w io.Writer, repo string, clusters []ds.Cluster) error {
	fmt.Fprintf(w, "repo: %s\n", repo)                         //nolint:errcheck
	fmt.Fprintf(w, "labellable_clusters: %d\n", len(clusters)) //nolint:errcheck
	fmt.Fprintf(w, "\n")                                       //nolint:errcheck

	for i, cl := range clusters {
		fmt.Fprintf(w, "---\n")                               //nolint:errcheck
		fmt.Fprintf(w, "cluster: %d\n", i+1)                  //nolint:errcheck
		fmt.Fprintf(w, "id: %s\n", cl.ShapeHash)              //nolint:errcheck
		fmt.Fprintf(w, "size: %d\n", cl.Size)                 //nolint:errcheck
		fmt.Fprintf(w, "coherence: %.2f\n", cl.Coherence)     //nolint:errcheck
		fmt.Fprintf(w, "shape: %s\n", seqString(cl.TokenSeq)) //nolint:errcheck
		for _, v := range cl.ShapeVariants {
			fmt.Fprintf(w, "shape_variant: %s\n", seqString(v)) //nolint:errcheck
		}

		p := cl.Profile

		// rates — only emit non-zero ones to save tokens
		var rates []string
		if p.ContextParamRate > 0 {
			rates = append(rates, fmt.Sprintf("ctx=%.0f%%", p.ContextParamRate*100))
		}
		if p.ErrorReturnRate > 0 {
			rates = append(rates, fmt.Sprintf("err=%.0f%%", p.ErrorReturnRate*100))
		}
		if p.DeferRate > 0 {
			rates = append(rates, fmt.Sprintf("defer=%.0f%%", p.DeferRate*100))
		}
		if p.GoroutineRate > 0 {
			rates = append(rates, fmt.Sprintf("go=%.0f%%", p.GoroutineRate*100))
		}
		if len(rates) > 0 {
			fmt.Fprintf(w, "rates: %s\n", strings.Join(rates, " ")) //nolint:errcheck
		}

		//nolint:errcheck
		fmt.Fprintf(w, "cyclo: %d-%d (mean %.1f p75 %.0f p95 %.0f)\n",
			p.CycloMin, p.CycloMax, p.CycloMean, p.CycloP75, p.CycloP95)
		//nolint:errcheck
		fmt.Fprintf(w, "nesting: p50 %.0f p75 %.0f p95 %.0f\n",
			p.NestingP50, p.NestingP75, p.NestingP95)
		//nolint:errcheck
		fmt.Fprintf(w, "calls: p50 %.0f p75 %.0f p95 %.0f\n",
			p.CallsP50, p.CallsP75, p.CallsP95)
		if p.EarlyReturnsP50 > 0 || p.EarlyReturnsP75 > 0 {
			//nolint:errcheck
			fmt.Fprintf(w, "early_returns: p50 %.0f p75 %.0f p95 %.0f\n",
				p.EarlyReturnsP50, p.EarlyReturnsP75, p.EarlyReturnsP95)
		}
		if p.DeferCountP50 > 0 || p.DeferCountP75 > 0 {
			//nolint:errcheck
			fmt.Fprintf(w, "defer_count: p50 %.0f p75 %.0f p95 %.0f\n",
				p.DeferCountP50, p.DeferCountP75, p.DeferCountP95)
		}

		if len(p.TopImports) > 0 {
			fmt.Fprintf(w, "imports: %s\n", strings.Join(p.TopImports, ", ")) //nolint:errcheck
		}
		if len(p.TopCallTargets) > 0 {
			fmt.Fprintf(w, "top_calls: %s\n", strings.Join(p.TopCallTargets, ", ")) //nolint:errcheck
		}

		// representatives — package.Name  file:line
		if len(cl.Members) > 0 {
			fmt.Fprintf(w, "representatives:\n") //nolint:errcheck
			reps := Representatives(cl, 3)
			for _, m := range reps {
				fmt.Fprintf(w, "  %s.%s  %s:%d\n", m.Package, m.Name, m.FileMeta.Path, m.Start_line) //nolint:errcheck
			}
		}

		fmt.Fprintf(w, "label: {}\n") //nolint:errcheck
		fmt.Fprintf(w, "\n")          //nolint:errcheck
	}

	return nil
}
