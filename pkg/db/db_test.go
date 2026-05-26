package db

import (
	"testing"

	badger "github.com/dgraph-io/badger/v4"
	ds "github.com/somak2kai/beats/pkg/types"
)

// newTestDb opens a real BadgerDB in the test's temp directory and registers a
// cleanup to close it when the test finishes.
func newTestDb(t *testing.T) *BadgerDb {
	t.Helper()
	opts := badger.DefaultOptions(t.TempDir()).WithLogger(nil)
	raw, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("open test BadgerDB: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &BadgerDb{db: raw}
}

// ── Save / Load ───────────────────────────────────────────────────────────────

func TestSaveLoad_RoundTrip(t *testing.T) {
	type payload struct {
		X int
		S string
	}
	want := payload{X: 42, S: "hello"}
	bdb := newTestDb(t)

	if err := bdb.Save("mykey", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var got payload
	if err := bdb.Load("mykey", &got); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: want %+v, got %+v", want, got)
	}
}

func TestSaveLoad_OverwritesExistingKey(t *testing.T) {
	bdb := newTestDb(t)
	if err := bdb.Save("k", 1); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	if err := bdb.Save("k", 99); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	var got int
	if err := bdb.Load("k", &got); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != 99 {
		t.Fatalf("expected 99 after overwrite, got %d", got)
	}
}

func TestLoad_MissingKeyReturnsError(t *testing.T) {
	bdb := newTestDb(t)
	var v int
	if err := bdb.Load("does-not-exist", &v); err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

// ── StoreFunctionMeta ─────────────────────────────────────────────────────────

func TestStoreFunctionMeta_RoundTrip(t *testing.T) {
	bdb := newTestDb(t)
	fn := ds.FunctionMeta{
		Name:    "MyFunc",
		Package: "mypkg",
		Features: ds.StructuralFeatures{
			CyclomaticComplexity: 3,
			ParamCount:           2,
			HasErrorReturn:       true,
		},
		DirectImports: []string{"fmt", "os"},
		CallTargets:   []string{"fmt.Println"},
	}
	if err := bdb.StoreFunctionMeta("fn-001", fn); err != nil {
		t.Fatalf("StoreFunctionMeta: %v", err)
	}

	// Load through the generic Load — key prefix matches StoreFunctionMeta's format.
	var got ds.FunctionMeta
	if err := bdb.Load("fncId:fn-001", &got); err != nil {
		t.Fatalf("Load FunctionMeta: %v", err)
	}
	if got.Name != fn.Name {
		t.Errorf("Name: want %q, got %q", fn.Name, got.Name)
	}
	if got.Package != fn.Package {
		t.Errorf("Package: want %q, got %q", fn.Package, got.Package)
	}
	if got.Features.CyclomaticComplexity != fn.Features.CyclomaticComplexity {
		t.Errorf("CyclomaticComplexity: want %d, got %d",
			fn.Features.CyclomaticComplexity, got.Features.CyclomaticComplexity)
	}
	if !got.Features.HasErrorReturn {
		t.Error("expected HasErrorReturn=true after round-trip")
	}
}

// ── StoreCluster / LoadCluster ────────────────────────────────────────────────

func TestStoreLoadCluster_RoundTrip(t *testing.T) {
	bdb := newTestDb(t)
	c := ds.Cluster{
		ShapeHash: "abc123",
		SeqKey:    "1,2,3",
		Size:      5,
		Members: []ds.FunctionMeta{
			{Name: "Fn1", Package: "pkg"},
			{Name: "Fn2", Package: "pkg"},
		},
		Profile: ds.ClusterProfile{
			CycloMin: 1,
			CycloMax: 4,
		},
	}
	if err := bdb.StoreCluster(TierRaw, "abc123", c); err != nil {
		t.Fatalf("StoreCluster: %v", err)
	}
	got, err := bdb.LoadCluster(TierRaw, "abc123")
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	if got.ShapeHash != c.ShapeHash {
		t.Errorf("ShapeHash: want %q, got %q", c.ShapeHash, got.ShapeHash)
	}
	if got.Size != c.Size {
		t.Errorf("Size: want %d, got %d", c.Size, got.Size)
	}
	if len(got.Members) != len(c.Members) {
		t.Errorf("Members count: want %d, got %d", len(c.Members), len(got.Members))
	}
	if got.Profile.CycloMax != c.Profile.CycloMax {
		t.Errorf("CycloMax: want %d, got %d", c.Profile.CycloMax, got.Profile.CycloMax)
	}
}

func TestLoadCluster_MissingKeyReturnsError(t *testing.T) {
	bdb := newTestDb(t)
	if _, err := bdb.LoadCluster(TierRaw, "nope"); err == nil {
		t.Fatal("expected error for missing cluster key, got nil")
	}
}

func TestStoreCluster_DifferentTiersSameHash(t *testing.T) {
	// Same shapeHash stored under two different tiers must be independent.
	bdb := newTestDb(t)
	raw := ds.Cluster{ShapeHash: "same", Size: 2}
	collapsed := ds.Cluster{ShapeHash: "same", Size: 10}

	if err := bdb.StoreCluster(TierRaw, "same", raw); err != nil {
		t.Fatal(err)
	}
	if err := bdb.StoreCluster(TierCollapsed, "same", collapsed); err != nil {
		t.Fatal(err)
	}

	gotRaw, err := bdb.LoadCluster(TierRaw, "same")
	if err != nil || gotRaw.Size != 2 {
		t.Fatalf("TierRaw cluster: want Size=2, got Size=%d err=%v", gotRaw.Size, err)
	}
	gotCollapsed, err := bdb.LoadCluster(TierCollapsed, "same")
	if err != nil || gotCollapsed.Size != 10 {
		t.Fatalf("TierCollapsed cluster: want Size=10, got Size=%d err=%v", gotCollapsed.Size, err)
	}
}

// ── StorePostings ─────────────────────────────────────────────────────────────

func TestStorePostings_NoError(t *testing.T) {
	bdb := newTestDb(t)
	ids := []string{"fn-001", "fn-002", "fn-003"}
	if err := bdb.StorePostings(12345, ids); err != nil {
		t.Fatalf("StorePostings: %v", err)
	}
}

func TestStorePostings_RoundTripViaLoad(t *testing.T) {
	bdb := newTestDb(t)
	ids := []string{"fn-001", "fn-002"}
	if err := bdb.StorePostings(99, ids); err != nil {
		t.Fatalf("StorePostings: %v", err)
	}
	// Key format mirrors StorePostings: "post:" + big-endian int64 bytes.
	key := string(append([]byte("post:"), int64ToBytes(99)...))
	var got []string
	if err := bdb.Load(key, &got); err != nil {
		t.Fatalf("Load postings: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("expected %d posting ids, got %d", len(ids), len(got))
	}
}

// ── StoreDocFreq ──────────────────────────────────────────────────────────────

func TestStoreDocFreq_NoError(t *testing.T) {
	bdb := newTestDb(t)
	if err := bdb.StoreDocFreq(77777, 42); err != nil {
		t.Fatalf("StoreDocFreq: %v", err)
	}
}

func TestStoreDocFreq_RoundTripViaLoad(t *testing.T) {
	bdb := newTestDb(t)
	if err := bdb.StoreDocFreq(88888, 17); err != nil {
		t.Fatalf("StoreDocFreq: %v", err)
	}
	key := string(append([]byte("freq:"), int64ToBytes(88888)...))
	var got int
	if err := bdb.Load(key, &got); err != nil {
		t.Fatalf("Load docfreq: %v", err)
	}
	if got != 17 {
		t.Fatalf("expected freq=17, got %d", got)
	}
}

// ── ScanClusters ──────────────────────────────────────────────────────────────

func TestScanClusters_EmptyTier(t *testing.T) {
	bdb := newTestDb(t)
	got, err := bdb.ScanClusters(TierRaw)
	if err != nil {
		t.Fatalf("ScanClusters: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 clusters for empty tier, got %d", len(got))
	}
}

func TestScanClusters_ReturnsAllForTier(t *testing.T) {
	bdb := newTestDb(t)
	hashes := []string{"h1", "h2", "h3"}
	for _, h := range hashes {
		if err := bdb.StoreCluster(TierRaw, h, ds.Cluster{ShapeHash: h, Size: 2}); err != nil {
			t.Fatalf("StoreCluster %s: %v", h, err)
		}
	}
	got, err := bdb.ScanClusters(TierRaw)
	if err != nil {
		t.Fatalf("ScanClusters: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 clusters, got %d", len(got))
	}
}

func TestScanClusters_TierIsolation(t *testing.T) {
	// Clusters stored under TierRaw must not appear in TierCollapsed and vice versa.
	bdb := newTestDb(t)
	if err := bdb.StoreCluster(TierRaw, "r1", ds.Cluster{ShapeHash: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := bdb.StoreCluster(TierCollapsed, "c1", ds.Cluster{ShapeHash: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := bdb.StoreCluster(TierLabel, "l1", ds.Cluster{ShapeHash: "l1"}); err != nil {
		t.Fatal(err)
	}

	raw, _ := bdb.ScanClusters(TierRaw)
	collapsed, _ := bdb.ScanClusters(TierCollapsed)
	label, _ := bdb.ScanClusters(TierLabel)

	if len(raw) != 1 {
		t.Errorf("TierRaw: expected 1 cluster, got %d", len(raw))
	}
	if len(collapsed) != 1 {
		t.Errorf("TierCollapsed: expected 1 cluster, got %d", len(collapsed))
	}
	if len(label) != 1 {
		t.Errorf("TierLabel: expected 1 cluster, got %d", len(label))
	}
}

func TestScanClusters_PreservesClusterFields(t *testing.T) {
	bdb := newTestDb(t)
	c := ds.Cluster{
		ShapeHash: "xyz",
		SeqKey:    "7,8,9",
		Size:      4,
		Label:     "validation-pattern",
	}
	if err := bdb.StoreCluster(TierLabel, "xyz", c); err != nil {
		t.Fatalf("StoreCluster: %v", err)
	}
	clusters, err := bdb.ScanClusters(TierLabel)
	if err != nil {
		t.Fatalf("ScanClusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	got := clusters[0]
	if got.Label != c.Label {
		t.Errorf("Label: want %q, got %q", c.Label, got.Label)
	}
	if got.Size != c.Size {
		t.Errorf("Size: want %d, got %d", c.Size, got.Size)
	}
}
