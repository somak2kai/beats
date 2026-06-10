package main

import (
	"strings"
	"testing"

	"github.com/somak2kai/beats/pkg/ast"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeCluster builds a minimal Cluster for use in tests.
func makeCluster(hash, tier string, size int, commonSeq []int) ds.Cluster {
	return ds.Cluster{
		ShapeHash: hash,
		Tier:      tier,
		Size:      size,
		CommonSeq: commonSeq,
	}
}

// makeOrphan builds a minimal OrphanedFunction for use in tests.
func makeOrphan(name string, candidates []ds.ClusterCandidate) ds.OrphanedFunction {
	return ds.OrphanedFunction{
		Meta:       ds.FunctionMeta{Name: name},
		Candidates: candidates,
	}
}

// ── buildOutlierResults ───────────────────────────────────────────────────────

func TestBuildOutlierResults_NilInputs(t *testing.T) {
	results := buildOutlierResults(nil, nil)
	if len(results) != 0 {
		t.Errorf("expected 0 results for nil inputs, got %d", len(results))
	}
}

func TestBuildOutlierResults_SkipsOrphanWithNoCandidates(t *testing.T) {
	orphans := []ds.OrphanedFunction{makeOrphan("foo", nil)}
	results := buildOutlierResults(orphans, map[string]ds.Cluster{})
	if len(results) != 0 {
		t.Errorf("expected 0 results for orphan with no candidates, got %d", len(results))
	}
}

func TestBuildOutlierResults_MetaFieldsTransferred(t *testing.T) {
	orphans := []ds.OrphanedFunction{{
		Meta: ds.FunctionMeta{
			Name:       "myFunc",
			Package:    "mypkg",
			FileMeta:   ds.FileMeta{Path: "/a/b/c.go"},
			Start_line: 42,
			Body:       "func myFunc() {}",
		},
		Candidates: []ds.ClusterCandidate{{ShapeHash: "abc"}},
	}}
	results := buildOutlierResults(orphans, map[string]ds.Cluster{})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Func != "myFunc" {
		t.Errorf("Func: want %q, got %q", "myFunc", r.Func)
	}
	if r.Package != "mypkg" {
		t.Errorf("Package: want %q, got %q", "mypkg", r.Package)
	}
	if r.File != "/a/b/c.go" {
		t.Errorf("File: want %q, got %q", "/a/b/c.go", r.File)
	}
	if r.Line != 42 {
		t.Errorf("Line: want 42, got %d", r.Line)
	}
	if r.Body != "func myFunc() {}" {
		t.Errorf("Body: want %q, got %q", "func myFunc() {}", r.Body)
	}
}

func TestBuildOutlierResults_UnknownCluster_EmptyDeltas(t *testing.T) {
	// Candidate cluster not in map → result still produced, deltas are all empty.
	orphans := []ds.OrphanedFunction{{
		Meta: ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{{
			ShapeHash:  "missing",
			ArithScore: 0.75,
		}},
	}}
	results := buildOutlierResults(orphans, map[string]ds.Cluster{})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if len(r.TokenDelta.Added) != 0 || len(r.TokenDelta.Removed) != 0 {
		t.Errorf("expected empty TokenDelta, got %+v", r.TokenDelta)
	}
	if len(r.ImportDelta.Added) != 0 || len(r.ImportDelta.Removed) != 0 {
		t.Errorf("expected empty ImportDelta, got %+v", r.ImportDelta)
	}
	if len(r.CallDelta.Added) != 0 || len(r.CallDelta.Removed) != 0 {
		t.Errorf("expected empty CallDelta, got %+v", r.CallDelta)
	}
}

func TestBuildOutlierResults_DeltasComputedFromCluster(t *testing.T) {
	cl := makeCluster("abc", "high", 10, []int{ast.TK_IF, ast.TK_RETURN})
	cl.Profile.TopImports = []string{"fmt"}
	cl.Profile.TopCallTargets = []string{"fmt.Println"}

	orphans := []ds.OrphanedFunction{{
		Meta: ds.FunctionMeta{
			Name:          "foo",
			TokenSeq:      []int{ast.TK_IF, ast.TK_FOR, ast.TK_RETURN}, // FOR is extra vs LCS
			DirectImports: []string{"os"},                              // os extra, fmt removed
			CallTargets:   []string{"os.Exit"},                         // os.Exit extra, fmt.Println removed
		},
		Candidates: []ds.ClusterCandidate{{ShapeHash: "abc", ArithScore: 0.80, CycloDelta: 2.5}},
	}}

	results := buildOutlierResults(orphans, map[string]ds.Cluster{"abc": cl})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]

	// TK_FOR is in orphan but not in cluster LCS → Added
	forName := ast.TokenName(ast.TK_FOR)
	found := false
	for _, a := range r.TokenDelta.Added {
		if a == forName {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in TokenDelta.Added, got %v", forName, r.TokenDelta.Added)
	}

	// import delta: orphan has "os", cluster top has "fmt"
	if len(r.ImportDelta.Added) != 1 || r.ImportDelta.Added[0] != "os" {
		t.Errorf("ImportDelta.Added: want [os], got %v", r.ImportDelta.Added)
	}
	if len(r.ImportDelta.Removed) != 1 || r.ImportDelta.Removed[0] != "fmt" {
		t.Errorf("ImportDelta.Removed: want [fmt], got %v", r.ImportDelta.Removed)
	}

	// call delta: orphan has "os.Exit", cluster top has "fmt.Println"
	if len(r.CallDelta.Added) != 1 || r.CallDelta.Added[0] != "os.Exit" {
		t.Errorf("CallDelta.Added: want [os.Exit], got %v", r.CallDelta.Added)
	}
	if len(r.CallDelta.Removed) != 1 || r.CallDelta.Removed[0] != "fmt.Println" {
		t.Errorf("CallDelta.Removed: want [fmt.Println], got %v", r.CallDelta.Removed)
	}

	// CycloDelta carried from candidate
	if r.CycloDelta != 2.5 {
		t.Errorf("CycloDelta: want 2.5, got %f", r.CycloDelta)
	}
}

func TestBuildOutlierResults_CandidateEntryFields(t *testing.T) {
	// Tier, Size, CommonShape, Idiom must be populated on the CandidateEntry.
	cl := makeCluster("abc123", "high", 14, []int{ast.TK_IF, ast.TK_RETURN})

	orphans := []ds.OrphanedFunction{{
		Meta: ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{{
			ShapeHash:  "abc123",
			ArithScore: 0.82,
			Idiom:      "error wrapper",
		}},
	}}

	results := buildOutlierResults(orphans, map[string]ds.Cluster{"abc123": cl})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	c := results[0].Candidates[0]
	if c.Tier != "high" {
		t.Errorf("Tier: want %q, got %q", "high", c.Tier)
	}
	if c.Size != 14 {
		t.Errorf("Size: want 14, got %d", c.Size)
	}
	if c.Idiom != "error wrapper" {
		t.Errorf("Idiom: want %q, got %q", "error wrapper", c.Idiom)
	}
	if c.CommonShape == "" {
		t.Errorf("CommonShape should not be empty for a known cluster with a non-nil CommonSeq")
	}
	// Score is carried through
	if c.Score != 0.82 {
		t.Errorf("Score: want 0.82, got %f", c.Score)
	}
}

func TestBuildOutlierResults_ClusterIDTruncatedTo6Chars(t *testing.T) {
	hash := "abcdefghij0123456789" // longer than 6 chars
	cl := makeCluster(hash, "low", 3, nil)

	orphans := []ds.OrphanedFunction{{
		Meta:       ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{{ShapeHash: hash}},
	}}

	results := buildOutlierResults(orphans, map[string]ds.Cluster{hash: cl})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Candidates[0].ClusterID != "abcdefghij0123456789" {
		t.Errorf("ClusterID: want %q, got %q", "abcdefghij0123456789", results[0].Candidates[0].ClusterID)
	}
}

func TestBuildOutlierResults_ClusterIDShortHashUnchanged(t *testing.T) {
	// Hashes shorter than 6 chars should not be trimmed.
	hash := "abc"
	cl := makeCluster(hash, "low", 2, nil)

	orphans := []ds.OrphanedFunction{{
		Meta:       ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{{ShapeHash: hash}},
	}}

	results := buildOutlierResults(orphans, map[string]ds.Cluster{hash: cl})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Candidates[0].ClusterID != "abc" {
		t.Errorf("ClusterID: want %q, got %q", "abc", results[0].Candidates[0].ClusterID)
	}
}

func TestBuildOutlierResults_MultipleCandidatesPreserved(t *testing.T) {
	// All candidates (not just the top one) must appear in the result.
	cl1 := makeCluster("aaa", "high", 10, nil)
	cl2 := makeCluster("bbb", "medium", 5, nil)

	orphans := []ds.OrphanedFunction{{
		Meta: ds.FunctionMeta{Name: "foo"},
		Candidates: []ds.ClusterCandidate{
			{ShapeHash: "aaa", ArithScore: 0.82},
			{ShapeHash: "bbb", ArithScore: 0.71},
		},
	}}

	results := buildOutlierResults(orphans, map[string]ds.Cluster{"aaa": cl1, "bbb": cl2})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].Candidates) != 2 {
		t.Errorf("expected 2 candidates, got %d", len(results[0].Candidates))
	}
}

func TestBuildOutlierResults_MultipleOrphansAllIncluded(t *testing.T) {
	cl := makeCluster("abc", "high", 5, nil)

	orphans := []ds.OrphanedFunction{
		makeOrphan("alpha", []ds.ClusterCandidate{{ShapeHash: "abc"}}),
		makeOrphan("beta", []ds.ClusterCandidate{{ShapeHash: "abc"}}),
		makeOrphan("nocandidate", nil), // must be skipped
	}

	results := buildOutlierResults(orphans, map[string]ds.Cluster{"abc": cl})
	if len(results) != 2 {
		t.Errorf("expected 2 results (nocandidate skipped), got %d", len(results))
	}
}

// ── buildClusterResult ────────────────────────────────────────────────────────

func TestBuildClusterResult_TopLevelFields(t *testing.T) {
	cl := &ds.Cluster{
		ShapeHash: "abc123",
		Tier:      "high",
		Size:      7,
		CommonSeq: []int{ast.TK_IF, ast.TK_RETURN},
	}
	r := buildClusterResult(cl)
	if r.ShapeHash != "abc123" {
		t.Errorf("ShapeHash: want %q, got %q", "abc123", r.ShapeHash)
	}
	if r.Tier != "high" {
		t.Errorf("Tier: want %q, got %q", "high", r.Tier)
	}
	if r.Size != 7 {
		t.Errorf("Size: want 7, got %d", r.Size)
	}
	if r.CommonShape == "" {
		t.Errorf("CommonShape should not be empty for non-nil CommonSeq")
	}
}

func TestBuildClusterResult_EmptyCluster(t *testing.T) {
	cl := &ds.Cluster{ShapeHash: "empty", Tier: "low"}
	r := buildClusterResult(cl)
	if len(r.Members) != 0 {
		t.Errorf("expected 0 members for empty cluster, got %d", len(r.Members))
	}
}

func TestBuildClusterResult_MemberMetaTransferred(t *testing.T) {
	cl := &ds.Cluster{
		ShapeHash: "x",
		Members: []ds.FunctionMeta{{
			Name:       "funcA",
			Package:    "pkgA",
			FileMeta:   ds.FileMeta{Path: "/x/y/z.go"},
			Start_line: 99,
			TokenSeq:   []int{ast.TK_IF, ast.TK_RETURN},
			Body:       "func funcA() {}",
		}},
	}
	r := buildClusterResult(cl)
	if len(r.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(r.Members))
	}
	m := r.Members[0]
	if m.Func != "funcA" {
		t.Errorf("Func: want %q, got %q", "funcA", m.Func)
	}
	if m.Package != "pkgA" {
		t.Errorf("Package: want %q, got %q", "pkgA", m.Package)
	}
	if m.File != "/x/y/z.go" {
		t.Errorf("File: want %q, got %q", "/x/y/z.go", m.File)
	}
	if m.Line != 99 {
		t.Errorf("Line: want 99, got %d", m.Line)
	}
	if m.Body != "func funcA() {}" {
		t.Errorf("Body: want %q, got %q", "func funcA() {}", m.Body)
	}
	if m.Tokens == "" {
		t.Errorf("Tokens should not be empty for non-nil TokenSeq")
	}
}

func TestBuildClusterResult_ImportsSorted(t *testing.T) {
	cl := &ds.Cluster{
		ShapeHash: "x",
		Members: []ds.FunctionMeta{{
			DirectImports: []string{"os", "fmt", "context"},
		}},
	}
	r := buildClusterResult(cl)
	if len(r.Members) != 1 {
		t.Fatalf("expected 1 member")
	}
	imports := r.Members[0].Imports
	for i := 1; i < len(imports); i++ {
		if imports[i] < imports[i-1] {
			t.Errorf("imports not sorted at index %d: %v", i, imports)
		}
	}
	if imports[0] != "context" || imports[1] != "fmt" || imports[2] != "os" {
		t.Errorf("imports sorted incorrectly: %v", imports)
	}
}

func TestBuildClusterResult_CallsSorted(t *testing.T) {
	cl := &ds.Cluster{
		ShapeHash: "x",
		Members: []ds.FunctionMeta{{
			CallTargets: []string{"z.Do", "a.Run", "m.Get"},
		}},
	}
	r := buildClusterResult(cl)
	if len(r.Members) != 1 {
		t.Fatalf("expected 1 member")
	}
	calls := r.Members[0].Calls
	for i := 1; i < len(calls); i++ {
		if calls[i] < calls[i-1] {
			t.Errorf("calls not sorted at index %d: %v", i, calls)
		}
	}
	if calls[0] != "a.Run" || calls[1] != "m.Get" || calls[2] != "z.Do" {
		t.Errorf("calls sorted incorrectly: %v", calls)
	}
}

func TestBuildClusterResult_EmptyImportsAndCalls(t *testing.T) {
	cl := &ds.Cluster{
		ShapeHash: "x",
		Members:   []ds.FunctionMeta{{Name: "f"}},
	}
	r := buildClusterResult(cl)
	if len(r.Members) != 1 {
		t.Fatalf("expected 1 member")
	}
	if len(r.Members[0].Imports) != 0 {
		t.Errorf("expected empty imports, got %v", r.Members[0].Imports)
	}
	if len(r.Members[0].Calls) != 0 {
		t.Errorf("expected empty calls, got %v", r.Members[0].Calls)
	}
}

func TestBuildClusterResult_OriginalSlicesNotMutated(t *testing.T) {
	// buildClusterResult copies imports/calls — original FunctionMeta must not be sorted in-place.
	orig := []string{"z", "a", "m"}
	cl := &ds.Cluster{
		ShapeHash: "x",
		Members:   []ds.FunctionMeta{{DirectImports: orig}},
	}
	_ = buildClusterResult(cl)
	if cl.Members[0].DirectImports[0] != "z" {
		t.Errorf("original imports slice was mutated: %v", cl.Members[0].DirectImports)
	}
}

func TestBuildClusterResult_AllMembersIncluded(t *testing.T) {
	cl := &ds.Cluster{
		ShapeHash: "x",
		Members: []ds.FunctionMeta{
			{Name: "alpha"},
			{Name: "beta"},
			{Name: "gamma"},
		},
	}
	r := buildClusterResult(cl)
	if len(r.Members) != 3 {
		t.Errorf("expected 3 members, got %d", len(r.Members))
	}
}

// ── formatDeltaText ───────────────────────────────────────────────────────────

func TestFormatDeltaText_BothEmpty(t *testing.T) {
	got := formatDeltaText(DeltaStrings{})
	if got != "—" {
		t.Errorf("want %q, got %q", "—", got)
	}
}

func TestFormatDeltaText_AddedOnly(t *testing.T) {
	got := formatDeltaText(DeltaStrings{Added: []string{"IF", "FOR"}})
	if !strings.Contains(got, "+IF") {
		t.Errorf("expected +IF in %q", got)
	}
	if !strings.Contains(got, "+FOR") {
		t.Errorf("expected +FOR in %q", got)
	}
	if strings.Contains(got, "−") {
		t.Errorf("unexpected removal marker in %q", got)
	}
}

func TestFormatDeltaText_RemovedOnly(t *testing.T) {
	got := formatDeltaText(DeltaStrings{Removed: []string{"RETURN"}})
	if !strings.Contains(got, "-RETURN") {
		t.Errorf("expected -RETURN in %q", got)
	}
	if strings.Contains(got, "+") {
		t.Errorf("unexpected addition marker in %q", got)
	}
}

func TestFormatDeltaText_BothAddedAndRemoved(t *testing.T) {
	got := formatDeltaText(DeltaStrings{Added: []string{"SWITCH"}, Removed: []string{"IF"}})
	if !strings.Contains(got, "+SWITCH") {
		t.Errorf("expected +SWITCH in %q", got)
	}
	if !strings.Contains(got, "-IF") {
		t.Errorf("expected -IF in %q", got)
	}
}

func TestFormatDeltaText_NilSlicesEquivalentToEmpty(t *testing.T) {
	got := formatDeltaText(DeltaStrings{Added: nil, Removed: nil})
	if got != "—" {
		t.Errorf("nil slices should produce %q, got %q", "—", got)
	}
}
