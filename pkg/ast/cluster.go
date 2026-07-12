package ast

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"

	ds "github.com/somak2kai/beats/pkg/types"
	"golang.org/x/sync/errgroup"
)

// ── default constants (kept for backward compat — use ClusterConfig instead) ─

const (
	identifyThreshold      = 0.75
	identifyMinSize        = 3
	maxTrigranBucket       = 300
	minTokenSeqLen         = 5
	seqSimilarityThreshold = 0.40
)

// ClusterConfig holds all tuneable thresholds for the clustering pipeline.
// Pass to IdentifyClustersWithConfig. IdentifyClusters uses DefaultClusterConfig.
type ClusterConfig struct {
	IdentifyThreshold      float64 // min ∛(seqS×impS×callS) to be a candidate (default 0.75)
	SeqSimilarityThreshold float64 // fast-reject on seq similarity (default 0.40)
	MaxTrigranBucket       int     // trigram stop-word cutoff (default 300)
	MinTokenSeqLen         int     // min tokens to participate (default 5)
	IdentifyMinSize        int     // min cluster members (default 3)
	PrimitiveThreshold     float64 // max cluster as fraction of corpus (default 0.05)
	SeqFastReject          float64 // orphan medoid gate (default 0.30)
	ClusterToleranceK      float64 // multiplier for μ−k×σ dynamic admission (default 1.5).
	// ToleranceShrinkK is the pseudo-count for empirical-Bayes shrinkage of the
	// orphan-admission bar.
	ToleranceShrinkK float64
}

// DefaultClusterConfig returns the current hardcoded defaults.
func DefaultClusterConfig() ClusterConfig {
	return ClusterConfig{
		IdentifyThreshold:      identifyThreshold,
		SeqSimilarityThreshold: seqSimilarityThreshold,
		MaxTrigranBucket:       maxTrigranBucket,
		MinTokenSeqLen:         minTokenSeqLen,
		IdentifyMinSize:        identifyMinSize,
		PrimitiveThreshold:     0.05,
		SeqFastReject:          0.30,
		// we take 1.5 as value since as part of Bells curve , that means 7percentile , as in give me score
		// where bar admits anything that scores at least as well as the weakest ~7% of members would.
		ClusterToleranceK: 1.5,
		ToleranceShrinkK:  5.0,
	}
}

// pairKey is a canonical ordered (i < j) pair of function indices into the fns slice.
type pairKey struct {
	i, j int
}

// scoredPair is a candidate pair with its combined similarity score.
type scoredPair struct {
	i, j  int
	score float64 // indicates scoring of possibility of belonging in same cluster for the function metadata pair.
}

// IdentifyOrphans uses DefaultClusterConfig. See IdentifyOrphansWithConfig.
func IdentifyOrphans(orphanMetas []ds.FunctionMeta, cluster []ds.Cluster) ([]ds.OrphanedFunction, error) {
	return IdentifyOrphansWithConfig(orphanMetas, cluster, DefaultClusterConfig())
}

// IdentifyOrphansWithConfig scores each orphaned function against every
// non-primitive cluster to identify potential outlier functions.
//
// Scoring: the orphan is scored against ALL cluster members (not just the
// medoid) using arithmetic mean (seqS+impS+callS)/3.
//
// Fast-reject: seqS against the medoid must be ≥ cfg.SeqFastReject before full member scoring.
func IdentifyOrphansWithConfig(orphanMetas []ds.FunctionMeta, cluster []ds.Cluster, cfg ClusterConfig) ([]ds.OrphanedFunction, error) {

	if len(orphanMetas) == 0 || len(cluster) == 0 {
		slog.Info("orphan analysis skipped", slog.String("reason", "no orphans or no clusters"))
		return nil, nil
	}

	var g errgroup.Group
	var mu sync.Mutex
	result := make([]ds.OrphanedFunction, 0, len(orphanMetas))

	g.SetLimit(runtime.GOMAXPROCS(0))

	for _, orphan := range orphanMetas {
		orphan := orphan // shadow for closure
		g.Go(func() error {
			var candidates []ds.ClusterCandidate

			for idx, cl := range cluster {
				if cl.IsPrimitive || len(cl.Stats.Top3) == 0 || len(cl.Members) == 0 {
					continue
				}

				medoidSeqS, _, _, _ := OrphanScore(orphan, cl.Stats.Top3[0].Meta)
				if medoidSeqS < cfg.SeqFastReject {
					continue
				}

				var seqSum, impSum, callSum float64
				for _, mem := range cl.Members {
					s, i, c, _ := OrphanScore(orphan, mem)
					seqSum += s
					impSum += i
					callSum += c
				}
				n := float64(len(cl.Members))
				seqS := seqSum / n
				impS := impSum / n
				callS := callSum / n
				arith := (seqS + impS + callS) / 3.0

				// lets take an example of imdb ratings
				// say movie1 has 3 reviews all greater than 98, but godfather has 5000 reviews of different numbers
				// if we just select standard deviation to account for cluster tightness , movie1 would have greater
				// scores than godfather. We need to adjust to size of reviews as well.

				// i am using this formula
				// granted = Baseline + (TrustMeter * Claimed Greatness)
				// where baseline is 0.75 our cfg.IdentifyThreshold ( anything below this figure is not to be considered)
				// trust meter is n/(n+5) -- which would give me fraction of how much to trust my sample size. 3/8 and 10/15 .It will always result in a percentage between 0% and 100%.
				// Claimed Greatness is the original score of (cl.Stats.MeanScore - cfg.ClusterToleranceK*cl.Stats.StdScore)
				// This is Bayesian shrinkage ( given by gemini).

				rawTolerance := cl.Stats.MeanScore - cfg.ClusterToleranceK*cl.Stats.StdScore
				shrinkWeight := 1.0
				if cfg.ToleranceShrinkK > 0 {
					shrinkWeight = n / (n + cfg.ToleranceShrinkK)
				}
				clusterTolerance := cfg.IdentifyThreshold + shrinkWeight*math.Max(rawTolerance-cfg.IdentifyThreshold, 0)

				if arith < math.Max(clusterTolerance, cfg.IdentifyThreshold) {
					continue
				}

				candidates = append(candidates, ds.ClusterCandidate{
					ClusterIdx: idx,
					ShapeHash:  cl.ShapeHash,
					SeqScore:   seqS,
					ImpScore:   impS,
					CallScore:  callS,
					ArithScore: arith,
					CycloDelta: float64(orphan.Features.CyclomaticComplexity) - cl.Profile.CycloMean,
					Idiom:      cl.SemanticIdiom,
				})
			}

			if len(candidates) == 0 {
				return nil
			}
			sort.Slice(candidates, func(a, b int) bool {
				if candidates[a].ArithScore != candidates[b].ArithScore {
					return candidates[a].ArithScore > candidates[b].ArithScore
				}
				return candidates[a].ShapeHash < candidates[b].ShapeHash
			})
			mu.Lock()
			result = append(result, ds.OrphanedFunction{
				Meta:       orphan,
				Candidates: candidates,
			})
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("orphan analysis: %w", err)
	}
	return result, nil
}

// IdentifyClusters uses DefaultClusterConfig. See IdentifyClustersWithConfig.
func IdentifyClusters(fns []ds.FunctionMeta) ([]ds.Cluster, []ds.FunctionMeta, error) {
	return IdentifyClustersWithConfig(fns, DefaultClusterConfig())
}

// IdentifyClustersWithConfig builds clusters using the provided thresholds.
// Returns:
//   - clusters: all multi-member structural clusters (after filtering)
//   - orphans: functions that did not join any cluster and are eligible for orphan analysis
func IdentifyClustersWithConfig(fns []ds.FunctionMeta, cfg ClusterConfig) ([]ds.Cluster, []ds.FunctionMeta, error) {
	if len(fns) == 0 {
		return nil, nil, nil
	}
	sort.Slice(fns, func(a, b int) bool {
		ka := fns[a].Package + "/" + fns[a].Name
		kb := fns[b].Package + "/" + fns[b].Name
		return ka < kb
	})

	primitiveThreshold := float64(len(fns)) * cfg.PrimitiveThreshold
	trigramMap := buildTrigramMapCfg(fns, cfg)
	sharedCounts := countSharedTrigramsCfg(trigramMap, cfg)
	candidates, pairScores, scoredFns, err := scorePairsCfg(fns, sharedCounts, cfg)
	if err != nil {
		return nil, nil, err
	}

	clusterMembers := agglomerateCfg(fns, candidates, pairScores, cfg)
	cl, orph := buildClustersCfg(fns, clusterMembers, primitiveThreshold, scoredFns, cfg)
	return cl, orph, nil
}

func ignoreFunction(fn ds.FunctionMeta) bool {
	if len(fn.TokenSeq) < minTokenSeqLen {
		return true
	}
	if fn.GeneratedCode || fn.TestCode || fn.IsConstructor {
		return true
	}
	return false
}

func ignoreFunctionCfg(fn ds.FunctionMeta, cfg ClusterConfig) bool {
	if len(fn.TokenSeq) < cfg.MinTokenSeqLen {
		return true
	}
	if fn.GeneratedCode || fn.TestCode || fn.IsConstructor {
		return true
	}
	return false
}

// buildTrigramMap maps each trigram hash to the indices of functions that contain it.
// Functions with fewer than minTokenSeqLen tokens are excluded — not enough
// structure to be discriminating. Similarly any test or generated code is excluded.
func buildTrigramMap(fns []ds.FunctionMeta) map[int64][]int {
	trigramMap := make(map[int64][]int, len(fns))
	for i, fn := range fns {
		if ignoreFunction(fn) {
			continue
		}
		for _, h := range fn.TokenSeqHash {
			trigramMap[h] = append(trigramMap[h], i)
		}
	}
	return trigramMap
}

// BuildTrigramMap is the exported form for use by the calibrate package.
func BuildTrigramMap(fns []ds.FunctionMeta) map[int64][]int {
	return buildTrigramMap(fns)
}

func buildTrigramMapCfg(fns []ds.FunctionMeta, cfg ClusterConfig) map[int64][]int {
	trigramMap := make(map[int64][]int, len(fns))
	for i, fn := range fns {
		if ignoreFunctionCfg(fn, cfg) {
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
// structural stop-word and would generate noise pairs.
func countSharedTrigrams(trigramMap map[int64][]int) map[pairKey]int {
	shared := make(map[pairKey]int)
	for _, bucket := range trigramMap {
		if len(bucket) > maxTrigranBucket {
			continue
		}
		for a := range bucket {
			for b := a + 1; b < len(bucket); b++ {
				shared[pairKey{bucket[a], bucket[b]}]++
			}
		}
	}
	return shared
}

func countSharedTrigramsCfg(trigramMap map[int64][]int, cfg ClusterConfig) map[pairKey]int {
	shared := make(map[pairKey]int)
	for _, bucket := range trigramMap {
		if len(bucket) > cfg.MaxTrigranBucket {
			continue
		}
		for a := range bucket {
			for b := a + 1; b < len(bucket); b++ {
				shared[pairKey{bucket[a], bucket[b]}]++
			}
		}
	}
	return shared
}

// scorePairsCfg filters and scores candidate pairs using the three-term similarity
// formula: ∛(seqSim × importJaccard × callJaccard).
//
// Returns:
//   - candidates: pairs above identifyThreshold, sorted descending by score
//   - pairScores: lookup map used by the complete-linkage gate
//   - scoredFns: set of function indices that had at least one below-threshold
//     full score computed. Any function in this set that later remains a singleton
//     is a genuine orphan — it had structural affinity to something but every pair
//     still fell below identifyThreshold. Functions absent from this set either
//     never shared enough trigram structure to score against anything (structurally
//     unique) or scored above threshold and joined a cluster.
func scorePairsCfg(fns []ds.FunctionMeta, sharedCounts map[pairKey]int, cfg ClusterConfig) ([]scoredPair, map[pairKey]float64, map[int]bool, error) {
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
	potentialOrphans := make(map[int]bool)

	var g errgroup.Group
	var m sync.Mutex
	chunkSize := len(sharedCounts) / 1000
	if chunkSize == 0 {
		chunkSize = 30
	}
	g.SetLimit(runtime.GOMAXPROCS(0))
	chunk := make(map[int]map[pairKey]int, chunkSize)
	i := 0
	for pk, cnt := range sharedCounts {
		index := i % chunkSize
		if val, ok := chunk[index]; !ok {
			chunk[index] = map[pairKey]int{pk: cnt}
		} else {
			val[pk] = cnt
		}
		i++
	}
	for _, val := range chunk {
		val := val // variable shadow
		g.Go(func() error {

			localOrphans := make(map[int]bool)
			var localScore []scoredPair
			localPairScore := make(map[pairKey]float64)
			for pk, cnt := range val {
				// require ≥2 shared trigrams when both functions have
				// enough trigrams to be discriminating
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
					// TODO slowest piece in this function.
					seqS = seqSimilarity(fns[pk.i].TokenSeq, fns[pk.j].TokenSeq)
					if seqS < cfg.SeqSimilarityThreshold {
						continue // fast reject — structurally too different
					}
				}

				impS := jaccard(importSets[pk.i], importSets[pk.j])
				callS := jaccard(callSets[pk.i], callSets[pk.j])
				score := math.Cbrt(seqS * impS * callS)

				if score < cfg.IdentifyThreshold {
					// Had real affinity but didn't qualify — genuine orphan candidates if
					// they remain singletons. Functions scoring above threshold join clusters
					// and can never reach the singleton check, so marking them here is moot.
					localOrphans[pk.i] = true
					localOrphans[pk.j] = true
					continue // below threshold — not a clustering candidate
				}
				localScore = append(localScore, scoredPair{pk.i, pk.j, score})
				localPairScore[pk] = score
			}
			m.Lock()
			for t := range localOrphans {
				potentialOrphans[t] = true
			}
			candidates = append(candidates, localScore...)
			for k, v := range localPairScore {
				pairScores[k] = v
			}
			m.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, fmt.Errorf("unable to calculate scores between functons :%w", err)
	}

	// sort descending so we process highest-confidence merges first.
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].score != candidates[b].score {
			return candidates[a].score > candidates[b].score
		}
		if candidates[a].i != candidates[b].i {
			return candidates[a].i < candidates[b].i
		}
		return candidates[a].j < candidates[b].j
	})

	return candidates, pairScores, potentialOrphans, nil
}

// agglomerateCfg runs complete-linkage agglomerative clustering over the scored
// pairs using Union-Find for efficient membership tracking.
// Returns clusterMembers: root index → all member indices in that cluster.
func agglomerateCfg(fns []ds.FunctionMeta, candidates []scoredPair, pairScores map[pairKey]float64, cfg ClusterConfig) map[int][]int {
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

		if !completeLinkageCheckCfg(clusterMembers[ri], clusterMembers[rj], pairScores, cfg) {
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

// completeLinkageCheckCfg returns true only when every cross-cluster pair (a, b)
// has a recorded score above identifyThreshold. A missing pair is treated as
// score 0 — if two functions were never candidates they cannot bridge clusters.
func completeLinkageCheckCfg(membA, membB []int, pairScores map[pairKey]float64, cfg ClusterConfig) bool {
	for _, a := range membA {
		for _, b := range membB {
			i, j := a, b
			if i > j {
				i, j = j, i // normalise to (smaller, larger) to match pairScores key order
			}
			s, ok := pairScores[pairKey{i, j}]
			if !ok || s < cfg.IdentifyThreshold {
				return false
			}
		}
	}
	return true
}

// buildClusters converts raw cluster membership data into Cluster objects.
// Filters out clusters smaller than identifyMinSize (currently 3), structural stop-words, test clusters, and init clusters.
// Disambiguates ShapeHash collisions and sorts by size descending.
//
// Returns clusters and orphan metas. An orphan is a singleton that:
//   - appeared in scoredFns — it had at least one full pair score computed,
//     meaning it had structural affinity to something but every pair fell below
//     identifyThreshold. Functions that never shared enough trigrams to score
//     against anything are structurally unique, not orphaned.
//   - passes the standard eligibility guard (not generated, ≥4 tokens, not test, not init)
func buildClustersCfg(fns []ds.FunctionMeta, clusterMembers map[int][]int, primitiveThreshold float64, scoredFns map[int]bool, cfg ClusterConfig) ([]ds.Cluster, []ds.FunctionMeta) {
	var clusters []ds.Cluster
	var orphans []ds.FunctionMeta

	for _, idxs := range clusterMembers {
		if len(idxs) < cfg.IdentifyMinSize {
			// Clusters below identifyMinSize are dropped. Only true singletons (size 1)
			// are eligible as orphans — size-2 clusters are silently discarded.
			// An orphan must have had a real score computed (it almost joined something);
			// GeneratedCode and TestCode functions are excluded by buildTrigramMap and
			// never appear in scoredFns.
			if len(idxs) == 1 && scoredFns[idxs[0]] {
				fn := fns[idxs[0]]
				if fn.Name != "init" {
					orphans = append(orphans, fn)
				}
			}
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
	return clusters, orphans
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
		CommonSeq: CommonSubsequence(metas),
		Members:   metas,
		Size:      len(metas),
	}
	c.Profile = computeProfile(metas)
	c.Coherence = computeCoherence(metas)
	c.CallCoherence = computeCallCoherence(metas)
	c.Stats = computeClusterStats(metas)
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

// SeqSim is the exported form of seqSimilarity, for use by the calibrate package.
func SeqSim(a, b []int) float64 { return seqSimilarity(a, b) }

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

// ── Cross-cluster analysis helpers ───────────────────────────────────────────

// lcsInts returns the Longest Common Subsequence of two int slices.
func lcsInts(a, b []int) []int {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	length := dp[m][n]
	if length == 0 {
		return nil
	}
	result := make([]int, length)
	i, j, k := m, n, length-1
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			result[k] = a[i-1]
			i--
			j--
			k--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}
	return result
}

// CommonSubsequence returns the longest token subsequence shared by ALL
// members, computed by iterative pairwise LCS reduction starting from the
// first member's sequence.
//
// Note: two clusters can share the same common subsequence — the ShapeHash
// remains the unique cluster identifier. Returns nil when members is empty
// or no token is common to all members.
func CommonSubsequence(members []ds.FunctionMeta) []int {
	if len(members) == 0 {
		return nil
	}
	common := members[0].TokenSeq
	for _, m := range members[1:] {
		common = lcsInts(common, m.TokenSeq)
		if len(common) == 0 {
			return nil
		}
	}
	return common
}

// SeqString is the exported form of seqString, for use outside this package.
func SeqString(seq []int) string { return seqString(seq) }

// TokenName returns the human-readable name of a single token constant.
func TokenName(t int) string {
	if t >= 0 && t < len(tokenNames) {
		return tokenNames[t]
	}
	return fmt.Sprintf("tok%d", t)
}

// MemberPairwiseScores returns, for each member, its mean ∛(seqS×impS×callS)
// against all other members in the cluster. Index aligns with the members
// slice. Members in a singleton cluster get score 0.
func MemberPairwiseScores(members []ds.FunctionMeta) []float64 {
	n := len(members)
	scores := make([]float64, n)
	if n < 2 {
		return scores
	}
	importSets := make([]map[string]bool, n)
	callSets := make([]map[string]bool, n)
	for i, m := range members {
		importSets[i] = toStringSet(m.DirectImports)
		callSets[i] = toStringSet(m.CallTargets)
	}
	for i := 0; i < n; i++ {
		var total float64
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			seqS := seqSimilarity(members[i].TokenSeq, members[j].TokenSeq)
			impS := jaccard(importSets[i], importSets[j])
			callS := jaccard(callSets[i], callSets[j])
			total += math.Cbrt(seqS * impS * callS)
		}
		scores[i] = total / float64(n-1)
	}
	return scores
}

// computeClusterStats builds the ClusterStats for a cluster using arithmetic
// mean pairwise scores (seqS + impS + callS) / 3. Arithmetic mean is used here
// (rather than geometric) so that partial matches across dimensions stay visible
// and do not collapse to 0. This is what orphan Z-score analysis compares against.
func computeClusterStats(metas []ds.FunctionMeta) ds.ClusterStats {
	n := len(metas)
	if n < 2 {
		return ds.ClusterStats{}
	}
	importSets := make([]map[string]bool, n)
	callSets := make([]map[string]bool, n)
	for i, m := range metas {
		importSets[i] = toStringSet(m.DirectImports)
		callSets[i] = toStringSet(m.CallTargets)
	}

	// Per-member mean arithmetic pairwise score
	memberScores := make([]float64, n)
	for i := 0; i < n; i++ {
		var total float64
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			total += arithPairwiseScore(
				metas[i].TokenSeq, metas[j].TokenSeq,
				importSets[i], importSets[j],
				callSets[i], callSets[j],
			)
		}
		memberScores[i] = total / float64(n-1)
	}

	// Overall mean and sample std dev
	var mean float64
	for _, s := range memberScores {
		mean += s
	}
	mean /= float64(n)

	var variance float64
	for _, s := range memberScores {
		d := s - mean
		variance += d * d
	}
	std := math.Sqrt(variance / float64(n-1)) // sample std dev

	// Top-3 members by score (medoids)
	type idxScore struct {
		idx   int
		score float64
	}
	ranked := make([]idxScore, n)
	for i, s := range memberScores {
		ranked[i] = idxScore{i, s}
	}
	sort.Slice(ranked, func(a, b int) bool {
		return ranked[a].score > ranked[b].score
	})
	k := 3
	if n < k {
		k = n
	}
	top3 := make([]ds.RankedMember, k)
	for i := 0; i < k; i++ {
		top3[i] = ds.RankedMember{Meta: metas[ranked[i].idx], Score: ranked[i].score}
	}

	return ds.ClusterStats{
		MeanScore: mean,
		StdScore:  std,
		Top3:      top3,
	}
}

// arithPairwiseScore computes (seqS + impS + callS) / 3 for a pair of functions.
// Unlike the geometric mean used in clustering, this never collapses to 0 when
// one dimension is 0, making it suitable for orphan affinity analysis.
func arithPairwiseScore(seqA, seqB []int, impsA, impsB, callsA, callsB map[string]bool) float64 {
	seqS := seqSimilarity(seqA, seqB)
	impS := jaccard(impsA, impsB)
	callS := jaccard(callsA, callsB)
	return (seqS + impS + callS) / 3.0
}

// OrphanScore computes the three dimension scores and arithmetic mean between
// an orphaned function and a cluster medoid. Returns (seqS, impS, callS, arith).
// Used by the orphan analysis pass in cmd.go.
func OrphanScore(orphan, medoid ds.FunctionMeta) (seqS, impS, callS, arith float64) {
	impA := toStringSet(orphan.DirectImports)
	impB := toStringSet(medoid.DirectImports)
	callA := toStringSet(orphan.CallTargets)
	callB := toStringSet(medoid.CallTargets)
	seqS = seqSimilarity(orphan.TokenSeq, medoid.TokenSeq)
	impS = jaccard(impA, impB)
	callS = jaccard(callA, callB)
	arith = (seqS + impS + callS) / 3.0
	return
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
	"COMPOSITE_LIT", "BINARY_OP", "TYPE_ASSERT",
	// Java-specific (emitted by jbeats)
	"TRY", "CATCH", "THROW", "FINALLY", "SYNCHRONIZED",
	"WHILE", "DO_WHILE", "ASSERT_STMT",
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
