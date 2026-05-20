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

// BuildClusters groups fns by identical token sequence, then computes
// per-cluster profile and coherence. Returns clusters sorted by size desc.
//
// Filters applied:
//   - Size < 4                → too small to be a pattern
//   - len(TokenSeq) < 4       → structural stop-word (trivial sequences)
//   - Size >= 5% of corpus    → structural stop-word; pattern is so universal
//     it carries no information (e.g. every model has a fetch-by-ID function).
//     These are dropped entirely rather than flagged — they pollute reports and
//     CollapseToFamilies without contributing meaningful signal.
func BuildClusters(fns []ds.FunctionMeta) []ds.Cluster {
	totalDocs := len(fns)
	primitiveThreshold := float64(totalDocs) * 0.05

	// group by token sequence
	bySeq := make(map[string][]ds.FunctionMeta)
	for _, fn := range fns {
		k := seqKey(fn.TokenSeq)
		bySeq[k] = append(bySeq[k], fn)
	}

	clusters := make([]ds.Cluster, 0, len(bySeq))
	for key, members := range bySeq {
		if len(members) < 4 {
			continue
		}
		seq := members[0].TokenSeq
		if len(seq) < 4 {
			continue
		}
		if float64(len(members)) >= primitiveThreshold {
			continue // structural stop-word — too common to be meaningful
		}

		c := ds.Cluster{
			SeqKey:    key,
			ShapeHash: shapeHash(key),
			TokenSeq:  seq,
			Members:   members,
			Size:      len(members),
		}
		c.Profile = computeProfile(members)
		c.Coherence = computeCoherence(members)
		c.CallCoherence = computeCallCoherence(members)
		clusters = append(clusters, c)
	}

	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Size > clusters[j].Size
	})
	return clusters
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

// ── Labelable filter ──────────────────────────────────────────────────────────

// Labelable returns the subset of clusters worth sending to Claude for labelling.
//
// Default thresholds (callers should use these unless they have a reason not to):
//
//	minCoherence = 0.50  — below this, imports are too mixed to label reliably
//	minSize      = 4     — fewer than 4 members is coincidence, not a pattern
//	maxShapeLen  = 20    — shapes longer than this are overfit; not reusable patterns
//
// For large repos (corpus > 20k functions) consider raising minCoherence to 0.60
// to keep the output file under ~50 clusters and within one LLM context window.
func Labelable(clusters []ds.Cluster, minCoherence float64, minSize int) []ds.Cluster {
	const maxShapeLen = 20
	out := make([]ds.Cluster, 0, len(clusters))
	for _, c := range clusters {
		if c.IsPrimitive {
			continue
		}
		if c.Size < minSize {
			continue
		}
		if c.Coherence < minCoherence {
			continue
		}
		if len(c.TokenSeq) > maxShapeLen {
			continue
		}
		out = append(out, c)
	}
	return out
}

// CollapseToFamilies groups clusters that represent the same structural pattern
// at different arities (Type-3 clone detection). Two clusters are considered
// family members when their combined similarity score exceeds familyThreshold.
//
// Similarity is weighted across three independent signals:
//
//	0.5 × token sequence similarity  (1 − normalised edit distance)
//	0.3 × import set Jaccard         (across all members, not just top-N)
//	0.2 × call target Jaccard        (across all members)
//
// Transitive grouping is handled by union-find. The largest cluster in each
// group is elected primary; absorbed clusters contribute their token sequences
// to ShapeVariants and their members to Members. Profile and Coherence are
// recomputed on the merged member set.
func CollapseToFamilies(clusters []ds.Cluster) []ds.Cluster {
	const familyThreshold = 0.70

	n := len(clusters)
	if n == 0 {
		return clusters
	}

	// build full import and call sets from every member (more accurate than TopN)
	importSets := make([]map[string]bool, n)
	callSets := make([]map[string]bool, n)
	for i, c := range clusters {
		importSets[i] = make(map[string]bool)
		callSets[i] = make(map[string]bool)
		for _, m := range c.Members {
			for _, imp := range m.DirectImports {
				importSets[i][imp] = true
			}
			for _, ct := range m.CallTargets {
				callSets[i][ct] = true
			}
		}
	}

	// precompute full NxN pairwise similarity matrix (upper triangle only)
	sims := make([][]float64, n)
	for i := range sims {
		sims[i] = make([]float64, n)
		sims[i][i] = 1.0
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			s := clusterSimilarity(
				clusters[i].TokenSeq, clusters[j].TokenSeq,
				importSets[i], importSets[j],
				callSets[i], callSets[j],
			)
			sims[i][j] = s
			sims[j][i] = s
		}
	}

	// complete-linkage agglomerative clustering.
	//
	// Two groups merge only when EVERY cross-group pair exceeds familyThreshold.
	// This eliminates chaining: unlike union-find (single-linkage), a 6-token
	// cluster cannot absorb a 40-token cluster via a chain of intermediate
	// clusters each individually within ratio bounds.
	//
	// Complexity: O(n³) worst case — fine for n ≤ ~150.
	groups := make([][]int, n)
	for i := range groups {
		groups[i] = []int{i}
	}

	for {
		merged := false
		for i := 0; i < len(groups) && !merged; i++ {
			for j := i + 1; j < len(groups) && !merged; j++ {
				if minGroupSim(groups[i], groups[j], sims) >= familyThreshold {
					groups[i] = append(groups[i], groups[j]...)
					groups = append(groups[:j], groups[j+1:]...)
					merged = true
				}
			}
		}
		if !merged {
			break
		}
	}

	// merge each group into one cluster
	result := make([]ds.Cluster, 0, len(groups))
	for _, idxs := range groups {
		if len(idxs) == 1 {
			result = append(result, clusters[idxs[0]])
			continue
		}

		// elect primary = largest cluster
		sort.Slice(idxs, func(a, b int) bool {
			return clusters[idxs[a]].Size > clusters[idxs[b]].Size
		})
		primary := clusters[idxs[0]]

		// absorb remaining clusters
		seen := map[string]bool{primary.SeqKey: true}
		for _, idx := range idxs[1:] {
			absorbed := clusters[idx]
			if !seen[absorbed.SeqKey] {
				primary.ShapeVariants = append(primary.ShapeVariants, absorbed.TokenSeq)
				seen[absorbed.SeqKey] = true
			}
			primary.Members = append(primary.Members, absorbed.Members...)
		}

		primary.Size = len(primary.Members)
		primary.Profile = computeProfile(primary.Members)
		primary.Coherence = computeCoherence(primary.Members)
		primary.CallCoherence = computeCallCoherence(primary.Members)
		result = append(result, primary)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Size > result[j].Size
	})
	return result
}

// minGroupSim returns the minimum pairwise similarity across all cross-group
// pairs. Used by complete-linkage clustering to decide whether two groups can
// merge: they merge only when every pair is above the threshold, preventing the
// chaining artefacts that single-linkage (union-find) produces.
func minGroupSim(g1, g2 []int, sims [][]float64) float64 {
	min := math.MaxFloat64
	for _, i := range g1 {
		for _, j := range g2 {
			if sims[i][j] < min {
				min = sims[i][j]
			}
		}
	}
	return min
}

// clusterSimilarity returns a weighted similarity score in [0, 1] between two
// clusters using token sequence edit distance, import Jaccard, and call Jaccard.
//
// Three hard gates are applied before the weighted score:
//
//  1. Length ratio: if the longer sequence is more than 2.5× the shorter, the
//     clusters cannot be arity variants — return 0 immediately. This prevents
//     monorepo chaining where a 6-token shape absorbs a 40-token shape via
//     intermediate clusters with high import overlap.
//
//  2. Short sequence exact match: if either sequence is shorter than 6 tokens,
//     the token sequences must be identical. For short functions a single token
//     substitution (e.g. ASSIGN → IF) represents a fundamental structural
//     difference — not a minor arity variant — and edit-distance similarity
//     inflates to 0.80 on a 5-token sequence, making collapse too aggressive.
//     Sequences of length ≥ 6 have enough tokens that a 1-2 edit is genuinely
//     minor and the weighted score can be trusted.
//
//  3. Sequence similarity floor: seqSim must be ≥ 0.40. Below this the shapes
//     are structurally too different to be the same pattern. Without this floor,
//     high importSim (common in monorepos that share a small set of packages)
//     can push the weighted score over threshold even for unrelated shapes.
const shortSeqThreshold = 6

func clusterSimilarity(seqA, seqB []int, importsA, importsB, callsA, callsB map[string]bool) float64 {
	la, lb := len(seqA), len(seqB)

	// gate 1 — length ratio
	lo, hi := la, lb
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo == 0 || float64(hi)/float64(lo) > 2.5 {
		return 0.0
	}

	// gate 2 — short sequences must match exactly
	if la < shortSeqThreshold || lb < shortSeqThreshold {
		if seqSimilarity(seqA, seqB) < 1.0 {
			return 0.0
		}
	}

	// gate 3 — sequence similarity floor
	seqSim := seqSimilarity(seqA, seqB)
	if seqSim < 0.40 {
		return 0.0
	}

	importSim := jaccard(importsA, importsB)
	callSim := jaccard(callsA, callsB)
	return 0.5*seqSim + 0.3*importSim + 0.2*callSim
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

	fmt.Fprintf(w, "# beats index — %s\n\n", repo)
	fmt.Fprintf(w, "%d patterns\n\n", labelled)
	fmt.Fprintf(w, "---\n\n")

	for _, c := range clusters {
		if c.Label == "" {
			continue
		}
		fmt.Fprintf(w, "## %s\n", c.Label)
		fmt.Fprintf(w, "id:%s  size:%d  coherence:%.2f  shape:%s\n\n",
			c.ShapeHash, c.Size, c.Coherence, seqString(c.TokenSeq))

		for _, m := range c.Members {
			fmt.Fprintf(w, "- %s.%s  %s:%d\n", m.Package, m.Name, m.FileMeta.Path, m.Start_line)
		}
		fmt.Fprintf(w, "\n")
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
	fmt.Fprintf(w, "repo: %s\n", repo)
	fmt.Fprintf(w, "labellable_clusters: %d\n", len(clusters))
	fmt.Fprintf(w, "\n")

	for i, cl := range clusters {
		fmt.Fprintf(w, "---\n")
		fmt.Fprintf(w, "cluster: %d\n", i+1)
		fmt.Fprintf(w, "id: %s\n", cl.ShapeHash)
		fmt.Fprintf(w, "size: %d\n", cl.Size)
		fmt.Fprintf(w, "coherence: %.2f\n", cl.Coherence)
		fmt.Fprintf(w, "shape: %s\n", seqString(cl.TokenSeq))
		for _, v := range cl.ShapeVariants {
			fmt.Fprintf(w, "shape_variant: %s\n", seqString(v))
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
			fmt.Fprintf(w, "rates: %s\n", strings.Join(rates, " "))
		}

		fmt.Fprintf(w, "cyclo: %d-%d (mean %.1f p75 %.0f p95 %.0f)\n",
			p.CycloMin, p.CycloMax, p.CycloMean, p.CycloP75, p.CycloP95)
		fmt.Fprintf(w, "nesting: p50 %.0f p75 %.0f p95 %.0f\n",
			p.NestingP50, p.NestingP75, p.NestingP95)
		fmt.Fprintf(w, "calls: p50 %.0f p75 %.0f p95 %.0f\n",
			p.CallsP50, p.CallsP75, p.CallsP95)
		if p.EarlyReturnsP50 > 0 || p.EarlyReturnsP75 > 0 {
			fmt.Fprintf(w, "early_returns: p50 %.0f p75 %.0f p95 %.0f\n",
				p.EarlyReturnsP50, p.EarlyReturnsP75, p.EarlyReturnsP95)
		}
		if p.DeferCountP50 > 0 || p.DeferCountP75 > 0 {
			fmt.Fprintf(w, "defer_count: p50 %.0f p75 %.0f p95 %.0f\n",
				p.DeferCountP50, p.DeferCountP75, p.DeferCountP95)
		}

		if len(p.TopImports) > 0 {
			fmt.Fprintf(w, "imports: %s\n", strings.Join(p.TopImports, ", "))
		}
		if len(p.TopCallTargets) > 0 {
			fmt.Fprintf(w, "top_calls: %s\n", strings.Join(p.TopCallTargets, ", "))
		}

		// representatives — package.Name  file:line
		if len(cl.Members) > 0 {
			fmt.Fprintf(w, "representatives:\n")
			reps := Representatives(cl, 3)
			for _, m := range reps {
				fmt.Fprintf(w, "  %s.%s  %s:%d\n", m.Package, m.Name, m.FileMeta.Path, m.Start_line)
			}
		}

		fmt.Fprintf(w, "label: {}\n")
		fmt.Fprintf(w, "\n")
	}

	return nil
}
