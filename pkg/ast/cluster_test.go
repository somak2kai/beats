package ast

import (
	"testing"

	"github.com/somak2kai/beats/pkg/hash"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeFn constructs a FunctionMeta with a computed TokenSeqHash from the given
// token sequence. DirectImports and CallTargets drive the Jaccard terms of the
// three-term similarity score.
func makeFn(seq []int, imports, calls []string) ds.FunctionMeta {
	return ds.FunctionMeta{
		TokenSeq:      seq,
		TokenSeqHash:  hash.ComputeWindowHash(seq),
		DirectImports: imports,
		CallTargets:   calls,
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// makeNoiseFns creates n functions with unique token sequences that share no
// trigrams with seq5 or with each other. Each sequence uses large unique
// integers (10000+i*100 ...) that are extremely unlikely to hash-collide with
// the test sequences or with each other.
//
// Purpose: IdentifyClusters drops clusters whose size ≥ 5% of the total corpus
// (primitiveThreshold). Tests that form small clusters (size 2-3) must pad the
// corpus to at least 20× the cluster size so the cluster is not mis-classified
// as a structural stop-word. Passing 98 noise functions gives a 100-function
// corpus where the threshold is 5, safely above any cluster in these tests.
func makeNoiseFns(n int) []ds.FunctionMeta {
	fns := make([]ds.FunctionMeta, n)
	for i := range fns {
		base := 10000 + i*100
		seq := []int{base, base + 1, base + 2, base + 3, base + 4}
		fns[i] = makeFn(seq, nil, nil)
	}
	return fns
}

// ── buildTrigramMap ───────────────────────────────────────────────────────────

func TestBuildTrigramMap_ExcludesShortSeqs(t *testing.T) {
	// len=2 < minTokenSeqLen=4 → no entries in the trigram map.
	fns := []ds.FunctionMeta{makeFn([]int{1, 2}, nil, nil)}
	m := buildTrigramMap(fns)
	if len(m) != 0 {
		t.Fatalf("expected empty map for short seq, got %d entries", len(m))
	}
}

func TestBuildTrigramMap_IndexesFunction(t *testing.T) {
	seq := []int{TK_IF, TK_FOR, TK_CALL, TK_RETURN, TK_RETURN}
	fns := []ds.FunctionMeta{makeFn(seq, nil, nil)}
	m := buildTrigramMap(fns)
	if len(m) == 0 {
		t.Fatal("expected non-empty trigram map for valid seq")
	}
	for _, indices := range m {
		for _, idx := range indices {
			if idx != 0 {
				t.Fatalf("expected function index 0, got %d", idx)
			}
		}
	}
}

func TestBuildTrigramMap_TwoIdenticalFunctionsShareBuckets(t *testing.T) {
	seq := []int{TK_IF, TK_FOR, TK_CALL, TK_RETURN, TK_RETURN}
	fns := []ds.FunctionMeta{
		makeFn(seq, nil, nil),
		makeFn(seq, nil, nil),
	}
	m := buildTrigramMap(fns)
	// Identical seqs produce identical hashes → both indices land in the same buckets.
	for h, indices := range m {
		if len(indices) != 2 {
			t.Fatalf("bucket %d: expected 2 indices, got %d", h, len(indices))
		}
	}
}

func TestBuildTrigramMap_DisjointSeqsNeverShareBuckets(t *testing.T) {
	// Two sequences with completely different token values → no shared hashes.
	seq1 := []int{1, 2, 3, 4, 5}
	seq2 := []int{100, 200, 300, 400, 500}
	fns := []ds.FunctionMeta{
		makeFn(seq1, nil, nil),
		makeFn(seq2, nil, nil),
	}
	m := buildTrigramMap(fns)
	for _, indices := range m {
		if len(indices) > 1 {
			t.Fatal("disjoint sequences should never share a trigram bucket")
		}
	}
}

// ── countSharedTrigrams ───────────────────────────────────────────────────────

func TestCountSharedTrigrams_IdenticalSeqs(t *testing.T) {
	seq := []int{TK_IF, TK_FOR, TK_CALL, TK_RETURN, TK_RETURN}
	fns := []ds.FunctionMeta{
		makeFn(seq, nil, nil),
		makeFn(seq, nil, nil),
	}
	m := buildTrigramMap(fns)
	shared := countSharedTrigrams(m)
	if shared[pairKey{0, 1}] == 0 {
		t.Fatal("expected pair (0,1) to share trigrams for identical sequences")
	}
}

func TestCountSharedTrigrams_SkipsLargeBuckets(t *testing.T) {
	// A bucket with more than maxTrigranBucket entries is a stop-word — skip it.
	hugeBucket := make([]int, maxTrigranBucket+1)
	for i := range hugeBucket {
		hugeBucket[i] = i
	}
	m := map[int64][]int{999: hugeBucket}
	shared := countSharedTrigrams(m)
	if len(shared) != 0 {
		t.Fatalf("stop-word bucket should be skipped, got %d pairs", len(shared))
	}
}

func TestCountSharedTrigrams_PairsAlwaysOrdered(t *testing.T) {
	// All pairKey{i,j} in the result must satisfy i < j.
	seq := []int{TK_IF, TK_FOR, TK_CALL, TK_RETURN, TK_RETURN}
	fns := []ds.FunctionMeta{
		makeFn(seq, nil, nil),
		makeFn(seq, nil, nil),
		makeFn(seq, nil, nil),
	}
	m := buildTrigramMap(fns)
	for pk := range countSharedTrigrams(m) {
		if pk.i >= pk.j {
			t.Fatalf("pairKey not ordered: i=%d j=%d", pk.i, pk.j)
		}
	}
}

// ── jaccard ───────────────────────────────────────────────────────────────────

func TestJaccard_IdenticalSets(t *testing.T) {
	a := map[string]bool{"fmt": true, "os": true}
	if got := jaccard(a, a); got != 1.0 {
		t.Fatalf("expected 1.0 for identical sets, got %f", got)
	}
}

func TestJaccard_DisjointSets(t *testing.T) {
	a := map[string]bool{"fmt": true}
	b := map[string]bool{"os": true}
	if got := jaccard(a, b); got != 0.0 {
		t.Fatalf("expected 0.0 for disjoint sets, got %f", got)
	}
}

func TestJaccard_BothEmpty(t *testing.T) {
	// Both empty → no shared vocabulary; returns 0 not 1.
	if got := jaccard(nil, nil); got != 0.0 {
		t.Fatalf("expected 0.0 for both-empty, got %f", got)
	}
}

func TestJaccard_PartialOverlap(t *testing.T) {
	a := map[string]bool{"fmt": true, "os": true}
	b := map[string]bool{"os": true, "io": true}
	// intersection={os}=1, union={fmt,os,io}=3 → 1/3
	got := jaccard(a, b)
	want := 1.0 / 3.0
	if absFloat(got-want) > 1e-9 {
		t.Fatalf("expected %f, got %f", want, got)
	}
}

func TestJaccard_OneEmpty(t *testing.T) {
	a := map[string]bool{"fmt": true}
	// union = len(a)+len(b)-intersection = 1+0-0 = 1, intersection = 0 → 0.0
	if got := jaccard(a, nil); got != 0.0 {
		t.Fatalf("expected 0.0 when one set is empty, got %f", got)
	}
}

// ── seqSimilarity & editDistance ─────────────────────────────────────────────

func TestSeqSimilarity_Identical(t *testing.T) {
	a := []int{1, 2, 3, 4}
	if got := seqSimilarity(a, a); got != 1.0 {
		t.Fatalf("expected 1.0 for identical sequences, got %f", got)
	}
}

func TestSeqSimilarity_BothEmpty(t *testing.T) {
	if got := seqSimilarity(nil, nil); got != 1.0 {
		t.Fatalf("expected 1.0 for both-empty sequences, got %f", got)
	}
}

func TestSeqSimilarity_CompletelyDifferent(t *testing.T) {
	a := []int{1, 1, 1, 1}
	b := []int{9, 9, 9, 9}
	if got := seqSimilarity(a, b); got >= 0.5 {
		t.Fatalf("expected low similarity for completely different sequences, got %f", got)
	}
}

func TestSeqSimilarity_OneInsertion(t *testing.T) {
	a := []int{1, 2, 3}
	b := []int{1, 2, 3, 4}
	// editDist=1, maxLen=4 → sim = 1 - 1/4 = 0.75
	got := seqSimilarity(a, b)
	want := 0.75
	if absFloat(got-want) > 1e-9 {
		t.Fatalf("expected %f, got %f", want, got)
	}
}

func TestEditDistance_Identical(t *testing.T) {
	a := []int{1, 2, 3}
	if got := editDistance(a, a); got != 0 {
		t.Fatalf("expected 0 for identical sequences, got %d", got)
	}
}

func TestEditDistance_SingleInsertion(t *testing.T) {
	if got := editDistance([]int{1, 2, 3}, []int{1, 2, 3, 4}); got != 1 {
		t.Fatalf("expected edit distance 1 (insertion), got %d", got)
	}
}

func TestEditDistance_SingleDeletion(t *testing.T) {
	if got := editDistance([]int{1, 2, 3, 4}, []int{1, 2, 3}); got != 1 {
		t.Fatalf("expected edit distance 1 (deletion), got %d", got)
	}
}

func TestEditDistance_SingleSubstitution(t *testing.T) {
	if got := editDistance([]int{1, 2, 3}, []int{1, 9, 3}); got != 1 {
		t.Fatalf("expected edit distance 1 (substitution), got %d", got)
	}
}

func TestEditDistance_EmptyVsNonEmpty(t *testing.T) {
	if got := editDistance(nil, []int{1, 2, 3}); got != 3 {
		t.Fatalf("expected edit distance 3 (insert all), got %d", got)
	}
}

func TestEditDistance_Symmetric(t *testing.T) {
	a := []int{1, 2, 3, 4}
	b := []int{2, 3, 5}
	if editDistance(a, b) != editDistance(b, a) {
		t.Fatal("edit distance must be symmetric")
	}
}

// ── completeLinkageCheck ──────────────────────────────────────────────────────

func TestCompleteLinkageCheck_AllPairsAboveThreshold(t *testing.T) {
	scores := map[pairKey]float64{
		{0, 2}: 0.80,
		{0, 3}: 0.90,
		{1, 2}: 0.70,
		{1, 3}: 0.60,
	}
	if !completeLinkageCheck([]int{0, 1}, []int{2, 3}, scores) {
		t.Fatal("expected true — all cross-cluster pairs are above threshold")
	}
}

func TestCompleteLinkageCheck_MissingPairFails(t *testing.T) {
	scores := map[pairKey]float64{
		{0, 2}: 0.80,
		// (0,3) missing → treated as score 0 → reject
		{1, 2}: 0.70,
		{1, 3}: 0.60,
	}
	if completeLinkageCheck([]int{0, 1}, []int{2, 3}, scores) {
		t.Fatal("expected false — missing pair is treated as score 0")
	}
}

func TestCompleteLinkageCheck_OnePairBelowThreshold(t *testing.T) {
	scores := map[pairKey]float64{
		{0, 2}: 0.80,
		{0, 3}: 0.50, // below identifyThreshold=0.55
		{1, 2}: 0.70,
		{1, 3}: 0.60,
	}
	if completeLinkageCheck([]int{0, 1}, []int{2, 3}, scores) {
		t.Fatal("expected false — one cross-cluster pair is below threshold")
	}
}

func TestCompleteLinkageCheck_NormalisesIndexOrder(t *testing.T) {
	// membA=[3], membB=[0]: a=3, b=0 → after swap i=0, j=3.
	// Score is stored under key {0,3} — the check must normalise before lookup.
	scores := map[pairKey]float64{{0, 3}: 0.80}
	if !completeLinkageCheck([]int{3}, []int{0}, scores) {
		t.Fatal("expected true — check must normalise (a,b) to (min,max) before lookup")
	}
}

func TestCompleteLinkageCheck_SingletonClusters(t *testing.T) {
	scores := map[pairKey]float64{{0, 1}: 0.80}
	if !completeLinkageCheck([]int{0}, []int{1}, scores) {
		t.Fatal("expected true for singleton clusters with a passing score")
	}
}

// ── assembleCluster ───────────────────────────────────────────────────────────

func TestAssembleCluster_PicksModalSeq(t *testing.T) {
	seqA := []int{TK_IF, TK_FOR, TK_RETURN, TK_RETURN}
	seqB := []int{TK_RANGE, TK_CALL, TK_RETURN, TK_RETURN}
	metas := []ds.FunctionMeta{
		{TokenSeq: seqA},
		{TokenSeq: seqA}, // seqA appears twice → modal
		{TokenSeq: seqB},
	}
	c := assembleCluster(metas)
	if len(c.TokenSeq) != len(seqA) {
		t.Fatalf("expected modal seq length %d, got %d", len(seqA), len(c.TokenSeq))
	}
	for i, tok := range seqA {
		if c.TokenSeq[i] != tok {
			t.Fatalf("token[%d]: expected %d, got %d", i, tok, c.TokenSeq[i])
		}
	}
}

func TestAssembleCluster_SetsSize(t *testing.T) {
	seq := []int{1, 2, 3, 4}
	metas := []ds.FunctionMeta{
		{TokenSeq: seq},
		{TokenSeq: seq},
		{TokenSeq: seq},
	}
	if c := assembleCluster(metas); c.Size != 3 {
		t.Fatalf("expected Size=3, got %d", c.Size)
	}
}

func TestAssembleCluster_PopulatesShapeHash(t *testing.T) {
	metas := []ds.FunctionMeta{{TokenSeq: []int{1, 2, 3, 4}}}
	c := assembleCluster(metas)
	if len(c.ShapeHash) == 0 {
		t.Fatal("expected non-empty ShapeHash")
	}
}

// ── disambiguateShapeHashes ───────────────────────────────────────────────────

func TestDisambiguateShapeHashes_NoCollision(t *testing.T) {
	clusters := []ds.Cluster{{ShapeHash: "aaa"}, {ShapeHash: "bbb"}}
	disambiguateShapeHashes(clusters)
	if clusters[0].ShapeHash != "aaa" || clusters[1].ShapeHash != "bbb" {
		t.Fatal("hashes should be unchanged when there is no collision")
	}
}

func TestDisambiguateShapeHashes_CollisionGetsSuffix(t *testing.T) {
	clusters := []ds.Cluster{
		{ShapeHash: "aaa"},
		{ShapeHash: "aaa"},
		{ShapeHash: "aaa"},
	}
	disambiguateShapeHashes(clusters)
	if clusters[0].ShapeHash != "aaa" {
		t.Fatalf("first occurrence should keep original hash, got %q", clusters[0].ShapeHash)
	}
	if clusters[1].ShapeHash != "aaa-1" {
		t.Fatalf("second occurrence should be aaa-1, got %q", clusters[1].ShapeHash)
	}
	if clusters[2].ShapeHash != "aaa-2" {
		t.Fatalf("third occurrence should be aaa-2, got %q", clusters[2].ShapeHash)
	}
}

func TestDisambiguateShapeHashes_IndependentCollisionGroups(t *testing.T) {
	clusters := []ds.Cluster{
		{ShapeHash: "aaa"},
		{ShapeHash: "bbb"},
		{ShapeHash: "aaa"},
		{ShapeHash: "bbb"},
	}
	disambiguateShapeHashes(clusters)
	if clusters[0].ShapeHash != "aaa" || clusters[2].ShapeHash != "aaa-1" {
		t.Fatalf("aaa collision not handled correctly: %q, %q", clusters[0].ShapeHash, clusters[2].ShapeHash)
	}
	if clusters[1].ShapeHash != "bbb" || clusters[3].ShapeHash != "bbb-1" {
		t.Fatalf("bbb collision not handled correctly: %q, %q", clusters[1].ShapeHash, clusters[3].ShapeHash)
	}
}

// ── isTestingCluster ──────────────────────────────────────────────────────────

func TestIsTestingCluster_MajorityTestImports(t *testing.T) {
	members := []ds.FunctionMeta{
		{DirectImports: []string{"testing"}},
		{DirectImports: []string{"testing"}},
		{DirectImports: []string{"fmt"}},
	}
	if !isTestingCluster(members) {
		t.Fatal("expected true — majority import testing")
	}
}

func TestIsTestingCluster_MinorityTestImports(t *testing.T) {
	members := []ds.FunctionMeta{
		{DirectImports: []string{"testing"}},
		{DirectImports: []string{"fmt"}},
		{DirectImports: []string{"os"}},
	}
	if isTestingCluster(members) {
		t.Fatal("expected false — only minority import testing")
	}
}

func TestIsTestingCluster_TestifyImport(t *testing.T) {
	members := []ds.FunctionMeta{
		{DirectImports: []string{"github.com/stretchr/testify/require"}},
		{DirectImports: []string{"github.com/stretchr/testify/require"}},
	}
	if !isTestingCluster(members) {
		t.Fatal("expected true — testify/require is a test import")
	}
}

func TestIsTestingCluster_Empty(t *testing.T) {
	if isTestingCluster(nil) {
		t.Fatal("expected false for empty members")
	}
}

// ── isInitCluster ─────────────────────────────────────────────────────────────

func TestIsInitCluster_AllInit(t *testing.T) {
	members := []ds.FunctionMeta{{Name: "init"}, {Name: "init"}}
	if !isInitCluster(members) {
		t.Fatal("expected true — all functions are init")
	}
}

func TestIsInitCluster_Mixed(t *testing.T) {
	members := []ds.FunctionMeta{{Name: "init"}, {Name: "Setup"}}
	if isInitCluster(members) {
		t.Fatal("expected false — not all functions are init")
	}
}

func TestIsInitCluster_Empty(t *testing.T) {
	if isInitCluster(nil) {
		t.Fatal("expected false for empty members")
	}
}

// ── IdentifyClusters (end-to-end) ────────────────────────────────────────────

// seq5 is a 5-token sequence that produces 2 trigram hashes, satisfying the
// minShared=2 requirement for pairs where both functions have ≥2 hashes.
var seq5 = []int{TK_IF, TK_FOR, TK_CALL, TK_RETURN, TK_RETURN}

func TestIdentifyClusters_NilInput(t *testing.T) {
	if got := IdentifyClusters(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %v", got)
	}
}

func TestIdentifyClusters_IdenticalFunctionsClustered(t *testing.T) {
	imp := []string{"fmt"}
	calls := []string{"fmt.Println"}
	// 2 target functions + 98 noise = 100 total.
	// primitiveThreshold = 100 * 0.05 = 5 → a cluster of 2 survives (2 < 5).
	fns := append(
		[]ds.FunctionMeta{makeFn(seq5, imp, calls), makeFn(seq5, imp, calls)},
		makeNoiseFns(98)...,
	)
	clusters := IdentifyClusters(fns)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster for identical functions, got %d", len(clusters))
	}
	if clusters[0].Size != 2 {
		t.Fatalf("expected cluster size 2, got %d", clusters[0].Size)
	}
}

func TestIdentifyClusters_StructurallyDifferentNotClustered(t *testing.T) {
	// Disjoint token sequences → no shared trigrams → no candidates → no cluster.
	f0 := makeFn([]int{TK_IF, TK_FOR, TK_CALL, TK_RETURN, TK_RETURN}, []string{"fmt"}, nil)
	f1 := makeFn([]int{TK_RANGE, TK_DEFER, TK_CALL_PKG, TK_ASSIGN, TK_GO}, []string{"os"}, nil)
	if clusters := IdentifyClusters([]ds.FunctionMeta{f0, f1}); len(clusters) != 0 {
		t.Fatalf("expected 0 clusters for structurally different functions, got %d", len(clusters))
	}
}

func TestIdentifyClusters_CompleteLinkagePreventsChaining(t *testing.T) {
	// F0 ~ F1 (score≈0.75), F1 ~ F2 (score≈0.67), but F0 !~ F2 (score=0.50 < 0.55).
	// Complete linkage must block {F0,F1,F2} from merging; only {F0,F1} forms.
	//
	// Score breakdown for F0-F2:
	//   seqSim=1.0, importJaccard=0 ({"fmt"}∩{"os","io"}=∅), callJaccard=0
	//   → 0.5×1.0 + 0.3×0 + 0.2×0 = 0.50 < 0.55 → not in pairScores
	f0 := makeFn(seq5, []string{"fmt"}, []string{"fmt.Println"})
	f1 := makeFn(seq5, []string{"fmt", "os"}, []string{"fmt.Println", "os.Exit"})
	f2 := makeFn(seq5, []string{"os", "io"}, []string{"os.Exit", "io.Close"})

	// 3 target + 97 noise = 100 total → primitiveThreshold = 5 → cluster of 2 survives.
	fns := append([]ds.FunctionMeta{f0, f1, f2}, makeNoiseFns(97)...)
	clusters := IdentifyClusters(fns)
	// F2 is a singleton → dropped (identifyMinSize=2). Only {F0,F1} survives.
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster (complete linkage blocks 3-way merge), got %d", len(clusters))
	}
	if clusters[0].Size != 2 {
		t.Fatalf("expected cluster size 2 (F0+F1 only), got %d", clusters[0].Size)
	}
}

func TestIdentifyClusters_SortedBySizeDescending(t *testing.T) {
	seq2 := []int{TK_RANGE, TK_CALL, TK_ASSIGN, TK_RETURN, TK_DEFER}
	imp1, calls1 := []string{"fmt"}, []string{"fmt.Println"}
	imp2, calls2 := []string{"os"}, []string{"os.Exit"}

	// Three functions sharing seq2 (larger cluster) + two sharing seq5 (smaller).
	// 5 target + 95 noise = 100 total → primitiveThreshold = 5 → cluster of 3 (3<5)
	// and cluster of 2 (2<5) both survive.
	fns := append(
		[]ds.FunctionMeta{
			makeFn(seq2, imp2, calls2),
			makeFn(seq2, imp2, calls2),
			makeFn(seq2, imp2, calls2),
			makeFn(seq5, imp1, calls1),
			makeFn(seq5, imp1, calls1),
		},
		makeNoiseFns(95)...,
	)
	clusters := IdentifyClusters(fns)
	if len(clusters) < 2 {
		t.Fatalf("expected ≥2 clusters, got %d", len(clusters))
	}
	if clusters[0].Size < clusters[1].Size {
		t.Fatalf("clusters must be sorted descending by size: got %d then %d",
			clusters[0].Size, clusters[1].Size)
	}
}

func TestIdentifyClusters_SingletonDropped(t *testing.T) {
	// One function alone cannot form a cluster (identifyMinSize=2).
	fns := []ds.FunctionMeta{makeFn(seq5, []string{"fmt"}, nil)}
	if clusters := IdentifyClusters(fns); len(clusters) != 0 {
		t.Fatalf("expected 0 clusters for a single function, got %d", len(clusters))
	}
}

func TestIdentifyClusters_InitClusterDropped(t *testing.T) {
	imp := []string{"fmt"}
	calls := []string{"fmt.Println"}
	fn := makeFn(seq5, imp, calls)
	fn.Name = "init"
	fn2 := fn
	// 2 init functions + 98 noise = 100 total → primitiveThreshold = 5.
	// The cluster of 2 would survive the stop-word check (2 < 5) but must be
	// dropped by isInitCluster — this tests that filter specifically.
	fns := append([]ds.FunctionMeta{fn, fn2}, makeNoiseFns(98)...)
	if clusters := IdentifyClusters(fns); len(clusters) != 0 {
		t.Fatalf("expected init cluster to be dropped, got %d clusters", len(clusters))
	}
}
