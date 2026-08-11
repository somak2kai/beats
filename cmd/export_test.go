package main

import (
	"encoding/json"
	"testing"

	ds "github.com/somak2kai/beats/pkg/types"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeClusterWithStats builds a Cluster with Stats + Coherence populated —
// enough for buildExportPayload to serialise it fully.
func makeClusterWithStats(hash string, size int, mean, std float64) ds.Cluster {
	return ds.Cluster{
		ShapeHash: hash,
		Tier:      "high",
		Size:      size,
		CommonSeq: []int{1, 2, 3},
		Stats: ds.ClusterStats{
			MeanScore: mean,
			StdScore:  std,
		},
		Coherence:      0.5,
		CallCoherence:  0.6,
		CompositeScore: 1.23,
		Members: []ds.FunctionMeta{
			{Name: "m1", Package: "pkg", FileMeta: ds.FileMeta{Path: "a.go"}, Start_line: 1},
		},
	}
}

func makeOrphanFull(name string, imp, calls []string, cands []ds.ClusterCandidate) ds.OrphanedFunction {
	return ds.OrphanedFunction{
		Meta: ds.FunctionMeta{
			Name:          name,
			Package:       "pkg",
			FileMeta:      ds.FileMeta{Path: "b.go"},
			Start_line:    99,
			DirectImports: imp,
			CallTargets:   calls,
			Body:          "func " + name + "() {}",
		},
		Candidates: cands,
	}
}

// ── buildExportPayload ────────────────────────────────────────────────────────

func TestBuildExportPayload_EmptyInputs(t *testing.T) {
	p := buildExportPayload("some/repo", nil, nil, 0)
	if p.Repo != "some/repo" {
		t.Errorf("Repo: want %q, got %q", "some/repo", p.Repo)
	}
	if p.CorpusSize != 0 {
		t.Errorf("CorpusSize: want 0, got %d", p.CorpusSize)
	}
	if p.UnclusteredCount != 0 {
		t.Errorf("UnclusteredCount: want 0, got %d", p.UnclusteredCount)
	}
	if len(p.Clusters) != 0 {
		t.Errorf("Clusters: want empty, got %d", len(p.Clusters))
	}
	if len(p.Orphans) != 0 {
		t.Errorf("Orphans: want empty, got %d", len(p.Orphans))
	}
}

func TestBuildExportPayload_CorpusSizeIncludesClusterAndOrphanCounts(t *testing.T) {
	clusters := []ds.Cluster{
		makeClusterWithStats("h1", 5, 0.9, 0.02),
		makeClusterWithStats("h2", 3, 0.8, 0.05),
	}
	orphans := []ds.OrphanedFunction{
		makeOrphanFull("o1", nil, nil, nil),
		makeOrphanFull("o2", nil, nil, nil),
	}
	p := buildExportPayload("r", clusters, orphans, 0)
	// 5 + 3 + 2 = 10
	if p.CorpusSize != 10 {
		t.Errorf("CorpusSize: want 10, got %d", p.CorpusSize)
	}
	if p.UnclusteredCount != 2 {
		t.Errorf("UnclusteredCount: want 2, got %d", p.UnclusteredCount)
	}
}

func TestBuildExportPayload_MinScoreFiltersClustersNotCorpus(t *testing.T) {
	clusters := []ds.Cluster{
		makeClusterWithStats("h1", 5, 0.95, 0.01), // keep
		makeClusterWithStats("h2", 3, 0.80, 0.02), // drop
		makeClusterWithStats("h3", 4, 0.92, 0.01), // keep (== threshold)
	}
	p := buildExportPayload("r", clusters, nil, 0.92)

	if len(p.Clusters) != 2 {
		t.Fatalf("Clusters after filter: want 2, got %d", len(p.Clusters))
	}
	// h1 and h3 survive
	got := map[string]bool{}
	for _, c := range p.Clusters {
		got[c.ShapeHash] = true
	}
	if !got["h1"] || !got["h3"] {
		t.Errorf("kept wrong clusters: %v", got)
	}
	// Corpus size is unaffected by min-score filter.
	if p.CorpusSize != 12 {
		t.Errorf("CorpusSize: want 12, got %d", p.CorpusSize)
	}
}

func TestBuildExportPayload_SkipsPrimitiveClusters(t *testing.T) {
	prim := makeClusterWithStats("prim", 100, 0.99, 0.001)
	prim.IsPrimitive = true
	clusters := []ds.Cluster{
		prim,
		makeClusterWithStats("h1", 3, 0.9, 0.02),
	}
	p := buildExportPayload("r", clusters, nil, 0)
	if len(p.Clusters) != 1 {
		t.Fatalf("Clusters: want 1 (primitive dropped), got %d", len(p.Clusters))
	}
	if p.Clusters[0].ShapeHash != "h1" {
		t.Errorf("wrong survivor: %s", p.Clusters[0].ShapeHash)
	}
	// Corpus size DOES include primitive-cluster members — total code count.
	if p.CorpusSize != 103 {
		t.Errorf("CorpusSize: want 103, got %d", p.CorpusSize)
	}
}

func TestBuildExportPayload_ClusterFieldMapping(t *testing.T) {
	cl := makeClusterWithStats("h1", 5, 0.94, 0.03)
	cl.Coherence = 0.82
	cl.CallCoherence = 0.75
	cl.CompositeScore = 4.2

	p := buildExportPayload("r", []ds.Cluster{cl}, nil, 0)
	if len(p.Clusters) != 1 {
		t.Fatal("expected 1 cluster")
	}
	c := p.Clusters[0]
	if c.MeanScore != 0.94 {
		t.Errorf("MeanScore: want 0.94, got %v", c.MeanScore)
	}
	if c.StdScore != 0.03 {
		t.Errorf("StdScore: want 0.03, got %v", c.StdScore)
	}
	if c.Coherence != 0.82 {
		t.Errorf("Coherence: want 0.82, got %v", c.Coherence)
	}
	if c.CallCoherence != 0.75 {
		t.Errorf("CallCoherence: want 0.75, got %v", c.CallCoherence)
	}
	if c.CompositeScore != 4.2 {
		t.Errorf("CompositeScore: want 4.2, got %v", c.CompositeScore)
	}
	if c.Tier != "high" {
		t.Errorf("Tier: want high, got %q", c.Tier)
	}
	if c.Size != 5 {
		t.Errorf("Size: want 5, got %d", c.Size)
	}
	if c.CommonShape == "" {
		t.Errorf("CommonShape should be populated from CommonSeq")
	}
	if len(c.Members) != 1 || c.Members[0].Func != "m1" {
		t.Errorf("Members not serialised correctly: %+v", c.Members)
	}
}

func TestBuildExportPayload_OrphanCandidateFieldMapping(t *testing.T) {
	cands := []ds.ClusterCandidate{{
		ShapeHash:  "cl1",
		SeqScore:   0.7,
		ImpScore:   0.5,
		CallScore:  0.6,
		ArithScore: 0.6,
		CycloDelta: -1.5,
		Idiom:      "webhook handler",
	}}
	orphans := []ds.OrphanedFunction{
		makeOrphanFull("o1", []string{"fmt", "os"}, []string{"fmt.Println"}, cands),
	}

	p := buildExportPayload("r", nil, orphans, 0)
	if len(p.Orphans) != 1 {
		t.Fatal("expected 1 orphan")
	}
	o := p.Orphans[0]
	if o.Func != "o1" || o.Package != "pkg" || o.File != "b.go" || o.Line != 99 {
		t.Errorf("orphan meta wrong: %+v", o)
	}
	if len(o.Imports) != 2 || o.Imports[0] != "fmt" || o.Imports[1] != "os" {
		t.Errorf("Imports (should be sorted): %v", o.Imports)
	}
	if len(o.Calls) != 1 || o.Calls[0] != "fmt.Println" {
		t.Errorf("Calls: %v", o.Calls)
	}
	if len(o.Candidates) != 1 {
		t.Fatalf("Candidates: want 1, got %d", len(o.Candidates))
	}
	c := o.Candidates[0]
	if c.ClusterHash != "cl1" {
		t.Errorf("ClusterHash: %q", c.ClusterHash)
	}
	if c.ArithScore != 0.6 {
		t.Errorf("ArithScore: %v", c.ArithScore)
	}
	if c.SeqScore != 0.7 || c.ImpScore != 0.5 || c.CallScore != 0.6 {
		t.Errorf("sub-scores wrong: %+v", c)
	}
	if c.CycloDelta != -1.5 {
		t.Errorf("CycloDelta: %v", c.CycloDelta)
	}
	if c.Idiom != "webhook handler" {
		t.Errorf("Idiom: %q", c.Idiom)
	}
}

func TestBuildExportPayload_OrphanImportsSorted(t *testing.T) {
	orphans := []ds.OrphanedFunction{
		makeOrphanFull("o1", []string{"z", "a", "m"}, []string{"z.F", "a.F"}, nil),
	}
	p := buildExportPayload("r", nil, orphans, 0)
	imp := p.Orphans[0].Imports
	if len(imp) != 3 || imp[0] != "a" || imp[1] != "m" || imp[2] != "z" {
		t.Errorf("Imports not sorted: %v", imp)
	}
	calls := p.Orphans[0].Calls
	if len(calls) != 2 || calls[0] != "a.F" || calls[1] != "z.F" {
		t.Errorf("Calls not sorted: %v", calls)
	}
}

// ── JSON round-trip: ensure field names match what gt-start's jq expects ─────

func TestExportPayload_JSONFieldNames(t *testing.T) {
	p := ExportPayload{
		Repo:             "r",
		Commit:           "abc",
		CorpusSize:       10,
		UnclusteredCount: 3,
		Clusters: []ExportCluster{{
			ShapeHash:      "h",
			Tier:           "high",
			Size:           2,
			MeanScore:      0.9,
			StdScore:       0.01,
			Coherence:      0.5,
			CallCoherence:  0.6,
			CompositeScore: 1.0,
			CommonShape:    "IF",
			Members:        nil,
		}},
		Orphans: []ExportOrphan{{
			Func: "f",
			Candidates: []ExportOrphanCandidate{{
				ClusterHash: "h",
				ArithScore:  0.8,
				SeqScore:    0.7,
				ImpScore:    0.6,
				CallScore:   0.5,
				CycloDelta:  1.0,
			}},
		}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	// Field-name contract with gt-start. Break these and the pipeline breaks.
	for _, want := range []string{
		`"repo":"r"`,
		`"commit":"abc"`,
		`"corpus_size":10`,
		`"unclustered_count":3`,
		`"shape_hash":"h"`,
		`"mean_score":0.9`,
		`"std_score":0.01`,
		`"call_coherence":0.6`,
		`"common_shape":"IF"`,
		`"composite_score":1`,
		`"cluster_hash":"h"`,
		`"arith_score":0.8`,
		`"seq_score":0.7`,
		`"imp_score":0.6`,
		`"call_score":0.5`,
		`"cyclo_delta":1`,
	} {
		if !containsSubstring(s, want) {
			t.Errorf("JSON missing %q\n  got: %s", want, s)
		}
	}
	// These names must NOT appear — plan had them, source doesn't have them.
	for _, banned := range []string{`avg_pairwise_score`, `geo_score`} {
		if containsSubstring(s, banned) {
			t.Errorf("JSON contains forbidden field %q", banned)
		}
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
