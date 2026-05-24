package hash

import "testing"

// windowSize=3, step=2 — kept in sync with the constants in hash.go.

func TestComputeWindowHash_NilInput(t *testing.T) {
	// nil is shorter than windowSize → falls into the short-seq branch → one hash.
	result := ComputeWindowHash(nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 hash for nil input, got %d", len(result))
	}
}

func TestComputeWindowHash_ShorterThanWindow(t *testing.T) {
	result := ComputeWindowHash([]int{1, 2})
	if len(result) != 1 {
		t.Fatalf("expected 1 hash for len-2 input (< windowSize 3), got %d", len(result))
	}
}

func TestComputeWindowHash_ExactWindowSize(t *testing.T) {
	result := ComputeWindowHash([]int{1, 2, 3})
	if len(result) != 1 {
		t.Fatalf("expected 1 hash for len-3 input (== windowSize), got %d", len(result))
	}
}

func TestComputeWindowHash_WindowCount(t *testing.T) {
	// len=7, windowSize=3, step=2 → windows start at 0, 2, 4 (start+3 ≤ 7).
	// start=6: 6+3=9 > 7, stops. Expected 3 windows.
	result := ComputeWindowHash([]int{1, 2, 3, 4, 5, 6, 7})
	if len(result) != 3 {
		t.Fatalf("expected 3 hashes for len-7 input, got %d", len(result))
	}
}

func TestComputeWindowHash_LenFive_TwoWindows(t *testing.T) {
	// len=5, windowSize=3, step=2 → windows start at 0 (0+3=3≤5) and 2 (2+3=5≤5).
	// start=4: 4+3=7 > 5, stops. Expected 2 windows.
	result := ComputeWindowHash([]int{10, 20, 30, 40, 50})
	if len(result) != 2 {
		t.Fatalf("expected 2 hashes for len-5 input, got %d", len(result))
	}
}

func TestComputeWindowHash_Deterministic(t *testing.T) {
	seq := []int{1, 2, 3, 4, 5, 6}
	h1 := ComputeWindowHash(seq)
	h2 := ComputeWindowHash(seq)
	if len(h1) != len(h2) {
		t.Fatalf("hash lengths differ between runs: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Fatalf("hash[%d] differs between identical runs: %d vs %d", i, h1[i], h2[i])
		}
	}
}

func TestComputeWindowHash_OrderSensitive(t *testing.T) {
	// Reversing the sequence must produce at least one different hash.
	fwd := ComputeWindowHash([]int{1, 2, 3, 4, 5})
	rev := ComputeWindowHash([]int{5, 4, 3, 2, 1})
	if len(fwd) != len(rev) {
		return // different lengths → trivially different
	}
	for i := range fwd {
		if fwd[i] != rev[i] {
			return // found a difference — pass
		}
	}
	t.Error("reversed sequence produced identical hashes — hash must be order-sensitive")
}

func TestComputeWindowHash_NonNegative(t *testing.T) {
	// Polynomial hashing mod _rabinMod must always yield non-negative values.
	seq := []int{100, 200, 300, 400, 500, 600, 700}
	for i, h := range ComputeWindowHash(seq) {
		if h < 0 {
			t.Fatalf("hash[%d] is negative (%d) — modular reduction broken", i, h)
		}
	}
}

func TestComputeWindowHash_DistinctWindowsProduceDistinctHashes(t *testing.T) {
	// [1,2,3,X] vs [1,2,3,Y] — the second window differs → second hash should differ.
	h1 := ComputeWindowHash([]int{1, 2, 3, 4, 5})
	h2 := ComputeWindowHash([]int{1, 2, 3, 99, 5})
	// Both produce 2 hashes; first window [1,2,3] is identical so h[0] must match.
	if h1[0] != h2[0] {
		t.Errorf("first window is identical but produced different hashes: %d vs %d", h1[0], h2[0])
	}
	// Second window differs: [3,4,5] vs [3,99,5] → hashes should differ.
	if h1[1] == h2[1] {
		t.Errorf("second window differs but produced the same hash: %d", h1[1])
	}
}
