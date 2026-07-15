package main

import (
	"math"
	"testing"

	"github.com/somak2kai/beats/pkg/ast"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ---------------------------------------------------------------------------
// clusterTier
// ---------------------------------------------------------------------------

func TestClusterTier(t *testing.T) {
	cases := []struct {
		std  float64
		want string
	}{
		// high: std < 0.05
		{0.00, "high"},
		{0.02, "high"},
		{0.049, "high"},

		// boundary: exactly 0.05 → medium
		{0.05, "medium"},

		// medium: 0.05 ≤ std < 0.12
		{0.07, "medium"},
		{0.10, "medium"},
		{0.119, "medium"},

		// boundary: exactly 0.12 → low
		{0.12, "low"},

		// low: std ≥ 0.12
		{0.15, "low"},
		{0.20, "low"},
		{0.50, "low"},
		{1.00, "low"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()
			got := clusterTier(tc.std)
			if got != tc.want {
				t.Errorf("clusterTier(%v) = %q, want %q", tc.std, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ClusterCompositeScore
// Formula: ln(size) × ln(numPackages+1) × confidence(tier) × meanScore² × (importCoh+callCoh)/2
// ---------------------------------------------------------------------------

func TestClusterCompositeScore_SizeGuard(t *testing.T) {
	// size ≤ 1 must always return 0 regardless of other params.
	for _, size := range []int{0, 1} {
		for _, tier := range []string{"high", "medium", "low"} {
			got := clusterCompositeScore(size, 3, tier, 0.9, 1.0, 1.0)
			if got != 0 {
				t.Errorf("clusterCompositeScore(%d, 3, %q, 0.9, 1.0, 1.0) = %f, want 0", size, tier, got)
			}
		}
	}
}

func TestClusterCompositeScore_Formula(t *testing.T) {
	const eps = 1e-9
	cases := []struct {
		size    int
		numPkgs int
		tier    string
		mean    float64
		imp     float64
		call    float64
		conf    float64 // expected confidence weight
	}{
		{10, 1, "high", 0.90, 1.0, 1.0, 1.0},
		{20, 3, "high", 0.95, 0.8, 1.0, 1.0},
		{10, 2, "medium", 0.80, 0.6, 0.8, 0.6},
		{50, 5, "medium", 0.70, 1.0, 1.0, 0.6},
		{10, 1, "low", 0.60, 0.5, 0.5, 0.3},
		{100, 4, "low", 0.50, 1.0, 1.0, 0.3},
		// unknown tier → default branch (0.3)
		{10, 2, "unknown", 0.70, 0.7, 0.9, 0.3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()
			cohFactor := (tc.imp + tc.call) / 2.0
			want := math.Log(float64(tc.size)) * math.Log1p(float64(tc.numPkgs)) * tc.conf * tc.mean * tc.mean * cohFactor
			got := clusterCompositeScore(tc.size, tc.numPkgs, tc.tier, tc.mean, tc.imp, tc.call)
			if math.Abs(got-want) > eps {
				t.Errorf("clusterCompositeScore(%d, %d, %q, %v, %v, %v) = %f, want %f",
					tc.size, tc.numPkgs, tc.tier, tc.mean, tc.imp, tc.call, got, want)
			}
		})
	}
}

func TestClusterCompositeScore_ZeroMean(t *testing.T) {
	// mean=0 → score=0 (0² = 0).
	got := clusterCompositeScore(100, 3, "high", 0, 1.0, 1.0)
	if got != 0 {
		t.Errorf("expected 0 for mean=0, got %f", got)
	}
}

func TestClusterCompositeScore_ZeroCoherence(t *testing.T) {
	// imp=0 and call=0 → cohFactor=0 → score=0.
	got := clusterCompositeScore(100, 3, "high", 0.9, 0, 0)
	if got != 0 {
		t.Errorf("expected 0 for zero coherence, got %f", got)
	}
}

func TestClusterCompositeScore_HighScoresHigherThanLow(t *testing.T) {
	// Equal size, numPackages, mean, coherence — high-tier confidence (1.0) must beat low-tier (0.3).
	high := clusterCompositeScore(20, 3, "high", 0.80, 0.9, 0.9)
	low := clusterCompositeScore(20, 3, "low", 0.80, 0.9, 0.9)
	if high <= low {
		t.Errorf("high-tier score (%f) should exceed low-tier score (%f)", high, low)
	}
}

func TestClusterCompositeScore_CoherencePenalty(t *testing.T) {
	// Same size and mean; perfect coherence must beat poor import coherence.
	perfect := clusterCompositeScore(10, 3, "high", 0.85, 1.0, 1.0)
	poor := clusterCompositeScore(10, 3, "high", 0.85, 0.5, 1.0)
	if perfect <= poor {
		t.Errorf("perfect coherence (%f) should exceed poor import coherence (%f)", perfect, poor)
	}
}

func TestClusterCompositeScore_PackageSpread(t *testing.T) {
	// Cross-package clusters must score higher than same-package clusters, all else equal.
	crossPkg := clusterCompositeScore(10, 5, "high", 0.85, 0.9, 0.9)
	samePkg := clusterCompositeScore(10, 1, "high", 0.85, 0.9, 0.9)
	if crossPkg <= samePkg {
		t.Errorf("cross-package score (%f) should exceed single-package score (%f)", crossPkg, samePkg)
	}
}

// ---------------------------------------------------------------------------
// shortImport
// ---------------------------------------------------------------------------

func TestShortImport(t *testing.T) {
	cases := []struct {
		imp  string
		want string
	}{
		{"fmt", "fmt"},
		{"os", "os"},
		{"github.com/somak2kai/beats/pkg/ast", "ast"},
		{"golang.org/x/sync/errgroup", "errgroup"},
		{"", ""},
	}
	for _, tc := range cases {
		got := shortImport(tc.imp)
		if got != tc.want {
			t.Errorf("shortImport(%q) = %q, want %q", tc.imp, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// shortPath
// ---------------------------------------------------------------------------

func TestShortPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/a/b/c/d/e", "c/d/e"},
		{"/a/b/c", "a/b/c"}, // leading slash produces 4 parts → last 3 joined without slash
		{"/a/b", "/a/b"},    // fewer than 3 → return original
		{"a", "a"},          // single segment
		{"", ""},
	}
	for _, tc := range cases {
		got := shortPath(tc.path)
		if got != tc.want {
			t.Errorf("shortPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// stringSetDiff
// ---------------------------------------------------------------------------

func TestStringSetDiff(t *testing.T) {
	cases := []struct {
		a, b         []string
		wantA, wantB []string
	}{
		{
			a: []string{"fmt", "os"}, b: []string{"os", "io"},
			wantA: []string{"fmt"}, wantB: []string{"io"},
		},
		{
			a: []string{"fmt"}, b: []string{"fmt"},
			wantA: nil, wantB: nil,
		},
		{
			a: nil, b: nil,
			wantA: nil, wantB: nil,
		},
		{
			a: []string{"a", "b", "c"}, b: nil,
			wantA: []string{"a", "b", "c"}, wantB: nil,
		},
		{
			a: nil, b: []string{"x", "y"},
			wantA: nil, wantB: []string{"x", "y"},
		},
	}
	eqSlice := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for _, tc := range cases {
		gotA, gotB := stringSetDiff(tc.a, tc.b)
		if !eqSlice(gotA, tc.wantA) {
			t.Errorf("stringSetDiff(%v,%v) onlyA=%v, want %v", tc.a, tc.b, gotA, tc.wantA)
		}
		if !eqSlice(gotB, tc.wantB) {
			t.Errorf("stringSetDiff(%v,%v) onlyB=%v, want %v", tc.a, tc.b, gotB, tc.wantB)
		}
	}
}

func TestStringSetDiff_Sorted(t *testing.T) {
	// Output must be sorted regardless of input order.
	gotA, gotB := stringSetDiff([]string{"z", "a", "m"}, []string{"b", "z"})
	for i := 1; i < len(gotA); i++ {
		if gotA[i] < gotA[i-1] {
			t.Errorf("onlyA not sorted: %v", gotA)
		}
	}
	for i := 1; i < len(gotB); i++ {
		if gotB[i] < gotB[i-1] {
			t.Errorf("onlyB not sorted: %v", gotB)
		}
	}
}

// ---------------------------------------------------------------------------
// tokenSetDiff
// ---------------------------------------------------------------------------

func TestTokenSetDiff_DisjointSets(t *testing.T) {
	// orphan has IF, cluster LCS has FOR — fully disjoint.
	orphan := []int{ast.TK_IF}
	lcs := []int{ast.TK_FOR}
	added, removed := tokenSetDiff(orphan, lcs)
	if len(added) != 1 || added[0] != ast.TokenName(ast.TK_IF) {
		t.Errorf("added: expected [%q], got %v", ast.TokenName(ast.TK_IF), added)
	}
	if len(removed) != 1 || removed[0] != ast.TokenName(ast.TK_FOR) {
		t.Errorf("removed: expected [%q], got %v", ast.TokenName(ast.TK_FOR), removed)
	}
}

func TestTokenSetDiff_IdenticalSets(t *testing.T) {
	// Orphan and LCS share the same token types → no diff.
	seq := []int{ast.TK_IF, ast.TK_RETURN}
	added, removed := tokenSetDiff(seq, seq)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("expected empty diff for identical sets, got added=%v removed=%v", added, removed)
	}
}

func TestTokenSetDiff_BothEmpty(t *testing.T) {
	added, removed := tokenSetDiff(nil, nil)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("expected empty diff for both-nil inputs, got added=%v removed=%v", added, removed)
	}
}

func TestTokenSetDiff_OrphanHasExtra(t *testing.T) {
	// Orphan has IF+FOR+RETURN, LCS has IF+RETURN → FOR is added, nothing removed.
	orphan := []int{ast.TK_IF, ast.TK_FOR, ast.TK_RETURN}
	lcs := []int{ast.TK_IF, ast.TK_RETURN}
	added, removed := tokenSetDiff(orphan, lcs)
	if len(added) != 1 || added[0] != ast.TokenName(ast.TK_FOR) {
		t.Errorf("added: expected [%q], got %v", ast.TokenName(ast.TK_FOR), added)
	}
	if len(removed) != 0 {
		t.Errorf("removed: expected empty, got %v", removed)
	}
}

func TestTokenSetDiff_Sorted(t *testing.T) {
	// Output slices must be sorted.
	orphan := []int{ast.TK_RETURN, ast.TK_IF, ast.TK_FOR}
	lcs := []int{ast.TK_CALL}
	added, removed := tokenSetDiff(orphan, lcs)
	for i := 1; i < len(added); i++ {
		if added[i] < added[i-1] {
			t.Errorf("added not sorted: %v", added)
		}
	}
	for i := 1; i < len(removed); i++ {
		if removed[i] < removed[i-1] {
			t.Errorf("removed not sorted: %v", removed)
		}
	}
}

func TestTokenSetDiff_DuplicateTokenInLCS(t *testing.T) {
	// Regression: orphan has CATCH once, cluster LCS has CATCH twice (two catch blocks).
	// A pure type-presence (set) diff would report nothing because CATCH ∈ both.
	// Multiset diff must report one CATCH as removed.
	orphan := []int{ast.TK_CATCH}
	lcs := []int{ast.TK_CATCH, ast.TK_CATCH}
	added, removed := tokenSetDiff(orphan, lcs)
	if len(added) != 0 {
		t.Errorf("added: expected empty, got %v", added)
	}
	catchName := ast.TokenName(ast.TK_CATCH)
	if len(removed) != 1 || removed[0] != catchName {
		t.Errorf("removed: expected [%q], got %v", catchName, removed)
	}
}

func TestTokenSetDiff_DuplicateTokenInOrphan(t *testing.T) {
	// Symmetric case: orphan has CATCH twice, cluster LCS has it once → one CATCH added.
	orphan := []int{ast.TK_CATCH, ast.TK_CATCH}
	lcs := []int{ast.TK_CATCH}
	added, removed := tokenSetDiff(orphan, lcs)
	catchName := ast.TokenName(ast.TK_CATCH)
	if len(added) != 1 || added[0] != catchName {
		t.Errorf("added: expected [%q], got %v", catchName, added)
	}
	if len(removed) != 0 {
		t.Errorf("removed: expected empty, got %v", removed)
	}
}

// ---------------------------------------------------------------------------
// coherenceBadgeClass
// ---------------------------------------------------------------------------

func TestCoherenceBadgeClass(t *testing.T) {
	cases := []struct {
		c    float64
		want string
	}{
		{0.00, "badge-red"},
		{0.39, "badge-red"},
		{0.40, "badge-yellow"},
		{0.59, "badge-yellow"},
		{0.60, "badge-green"},
		{1.00, "badge-green"},
	}
	for _, tc := range cases {
		if got := coherenceBadgeClass(tc.c); got != tc.want {
			t.Errorf("coherenceBadgeClass(%v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// scoreBadgeClass
// ---------------------------------------------------------------------------

func TestScoreBadgeClass(t *testing.T) {
	cases := []struct {
		s    float64
		want string
	}{
		{0.00, "badge-red"},
		{0.54, "badge-red"},
		{0.55, "badge-yellow"},
		{0.74, "badge-yellow"},
		{0.75, "badge-green"},
		{1.00, "badge-green"},
	}
	for _, tc := range cases {
		if got := scoreBadgeClass(tc.s); got != tc.want {
			t.Errorf("scoreBadgeClass(%v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// pct
// ---------------------------------------------------------------------------

func TestPct(t *testing.T) {
	cases := []struct {
		a, b int
		want string
	}{
		{0, 0, "0%"},
		{0, 10, "0%"},
		{1, 2, "50%"},
		{1, 3, "33%"},
		{2, 3, "67%"},
		{100, 100, "100%"},
		{1, 100, "1%"},
	}
	for _, tc := range cases {
		if got := pct(tc.a, tc.b); got != tc.want {
			t.Errorf("pct(%d,%d) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// labelOrHash
// ---------------------------------------------------------------------------

func TestLabelOrHash(t *testing.T) {
	withLabel := ClusterRow{Label: "HTTP handler pattern", ShapeHash: "abc123"}
	if got := labelOrHash(withLabel); got != "HTTP handler pattern" {
		t.Errorf("expected label, got %q", got)
	}

	noLabel := ClusterRow{Label: "", ShapeHash: "abc123"}
	if got := labelOrHash(noLabel); got != "abc123" {
		t.Errorf("expected hash fallback, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// buildOutlierGroups
// ---------------------------------------------------------------------------

func TestBuildOutlierGroups_EmptyInputs(t *testing.T) {
	groups := buildOutlierGroups(nil, nil)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for nil inputs, got %d", len(groups))
	}
}

func TestBuildOutlierGroups_OrphanWithNoCandidate(t *testing.T) {
	// Orphans with no candidates must be silently skipped.
	orphans := []ds.OrphanedFunction{{Meta: ds.FunctionMeta{Name: "foo"}, Candidates: nil}}
	groups := buildOutlierGroups(nil, orphans)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for orphan without candidates, got %d", len(groups))
	}
}

func TestBuildOutlierGroups_UnknownShapeHashSkipped(t *testing.T) {
	// Candidate ShapeHash not present in any cluster → group silently dropped.
	orphans := []ds.OrphanedFunction{{
		Meta:       ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{{ShapeHash: "doesnotexist"}},
	}}
	groups := buildOutlierGroups([]ds.Cluster{}, orphans)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for unknown hash, got %d", len(groups))
	}
}

func TestBuildOutlierGroups_PrimitiveClusterSkipped(t *testing.T) {
	// Orphan whose best candidate resolves to a primitive cluster must be skipped.
	cl := ds.Cluster{ShapeHash: "abc", IsPrimitive: true}
	orphans := []ds.OrphanedFunction{{
		Meta:       ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{{ShapeHash: "abc"}},
	}}
	groups := buildOutlierGroups([]ds.Cluster{cl}, orphans)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for primitive cluster, got %d", len(groups))
	}
}

func TestBuildOutlierGroups_ValidGroupProduced(t *testing.T) {
	cl := ds.Cluster{
		ShapeHash:   "abc123",
		CommonSeq:   []int{ast.TK_IF, ast.TK_RETURN},
		IsPrimitive: false,
	}
	cl.Profile.TopImports = []string{"fmt"}
	cl.Profile.TopCallTargets = []string{"fmt.Println"}

	orphan := ds.OrphanedFunction{
		Meta: ds.FunctionMeta{
			Name:          "myFunc",
			TokenSeq:      []int{ast.TK_IF, ast.TK_FOR, ast.TK_RETURN},
			DirectImports: []string{"os"},
			CallTargets:   []string{"os.Exit"},
		},
		Candidates: []ds.ClusterCandidate{{ShapeHash: "abc123"}},
	}

	groups := buildOutlierGroups([]ds.Cluster{cl}, []ds.OrphanedFunction{orphan})
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].Outliers) != 1 {
		t.Fatalf("expected 1 outlier, got %d", len(groups[0].Outliers))
	}
	if groups[0].Outliers[0].Name != "myFunc" {
		t.Errorf("outlier name: expected %q, got %q", "myFunc", groups[0].Outliers[0].Name)
	}
}
