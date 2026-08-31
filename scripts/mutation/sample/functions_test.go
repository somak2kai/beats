package sample

import "testing"

// Minimal, non-exhaustive table tests — just enough coverage that gremlins
// and go-mutesting have live tests to run mutants against, instead of every
// mutant coming back NOT COVERED.

func TestMax(t *testing.T) {
	if got := Max(3, 5); got != 5 {
		t.Errorf("Max(3,5) = %d, want 5", got)
	}
	if got := Max(5, 3); got != 5 {
		t.Errorf("Max(5,3) = %d, want 5", got)
	}
}

func TestMin(t *testing.T) {
	if got := Min(3, 5); got != 3 {
		t.Errorf("Min(3,5) = %d, want 3", got)
	}
}

func TestClamp(t *testing.T) {
	cases := []struct{ v, lo, hi, want int }{
		{5, 0, 10, 5},
		{-5, 0, 10, 0},
		{15, 0, 10, 10},
	}
	for _, c := range cases {
		if got := Clamp(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("Clamp(%d,%d,%d) = %d, want %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}

func TestIsEven(t *testing.T) {
	if !IsEven(4) {
		t.Error("IsEven(4) = false, want true")
	}
	if IsEven(3) {
		t.Error("IsEven(3) = true, want false")
	}
}

func TestIsPositive(t *testing.T) {
	if !IsPositive(1) {
		t.Error("IsPositive(1) = false, want true")
	}
	if IsPositive(0) {
		t.Error("IsPositive(0) = true, want false")
	}
}

func TestSum(t *testing.T) {
	if got := Sum([]int{1, 2, 3}); got != 6 {
		t.Errorf("Sum = %d, want 6", got)
	}
}

func TestContains(t *testing.T) {
	if !Contains([]int{1, 2, 3}, 2) {
		t.Error("Contains = false, want true")
	}
	if Contains([]int{1, 2, 3}, 9) {
		t.Error("Contains = true, want false")
	}
}

func TestReverse(t *testing.T) {
	if got := Reverse("abc"); got != "cba" {
		t.Errorf("Reverse = %q, want %q", got, "cba")
	}
}

func TestCountVowels(t *testing.T) {
	if got := CountVowels("Hello World"); got != 3 {
		t.Errorf("CountVowels = %d, want 3", got)
	}
}

func TestFilter(t *testing.T) {
	got := Filter([]int{1, 5, 10, 2}, 3)
	if len(got) != 2 || got[0] != 5 || got[1] != 10 {
		t.Errorf("Filter = %v, want [5 10]", got)
	}
}

func TestSafeDivide(t *testing.T) {
	if got, err := SafeDivide(10, 2); err != nil || got != 5 {
		t.Errorf("SafeDivide(10,2) = (%v, %v), want (5, nil)", got, err)
	}
	if _, err := SafeDivide(10, 0); err == nil {
		t.Error("SafeDivide(10,0) err = nil, want error")
	}
}
