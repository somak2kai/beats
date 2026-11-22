package sample

import (
	"errors"
	"testing"
	"time"
)

func TestValidateEmail(t *testing.T) {
	if err := ValidateEmail("a@b.com"); err != nil {
		t.Errorf("ValidateEmail(valid) = %v, want nil", err)
	}
	cases := []string{"", "no-at-sign", "@b.com", "a@", "a@b", "a b@c.com"}
	for _, c := range cases {
		if err := ValidateEmail(c); err == nil {
			t.Errorf("ValidateEmail(%q) = nil, want error", c)
		}
	}
}

func TestMaskSensitive(t *testing.T) {
	if got := MaskSensitive("4111111111111111", 4); got != "************1111" {
		t.Errorf("MaskSensitive = %q", got)
	}
	if got := MaskSensitive("abc", 10); got != "abc" {
		t.Errorf("MaskSensitive(short) = %q, want unchanged", got)
	}
}

func TestDeduplicate(t *testing.T) {
	got := Deduplicate([]string{"a", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Deduplicate = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Deduplicate[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestChunkSlice(t *testing.T) {
	chunks, err := ChunkSlice([]int{1, 2, 3, 4, 5}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 || len(chunks[2]) != 1 {
		t.Errorf("ChunkSlice = %v", chunks)
	}
	if _, err := ChunkSlice([]int{1}, 0); err == nil {
		t.Error("ChunkSlice(size=0) err = nil, want error")
	}
}

func TestPaginate(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	got, err := Paginate(items, 2, 2)
	if err != nil || len(got) != 2 || got[0] != "c" {
		t.Errorf("Paginate(page2) = %v, %v", got, err)
	}
	got, err = Paginate(items, 3, 2)
	if err != nil || len(got) != 1 || got[0] != "e" {
		t.Errorf("Paginate(last page) = %v, %v", got, err)
	}
	if _, err := Paginate(items, 0, 2); err == nil {
		t.Error("Paginate(page=0) err = nil, want error")
	}
}

func TestMergeConfigs(t *testing.T) {
	base := map[string]string{"a": "1", "b": "2"}
	override := map[string]string{"b": "override", "c": "3"}
	got := MergeConfigs(base, override)
	if got["a"] != "1" || got["b"] != "override" || got["c"] != "3" {
		t.Errorf("MergeConfigs = %v", got)
	}
	if base["b"] != "2" {
		t.Error("MergeConfigs mutated base")
	}
}

func TestTruncateWithEllipsis(t *testing.T) {
	if got := TruncateWithEllipsis("hello world", 8); got != "hello..." {
		t.Errorf("TruncateWithEllipsis = %q, want %q", got, "hello...")
	}
	if got := TruncateWithEllipsis("hi", 8); got != "hi" {
		t.Errorf("TruncateWithEllipsis(short) = %q, want unchanged", got)
	}
}

func TestValidatePasswordStrength(t *testing.T) {
	if got := ValidatePasswordStrength("Aa1!aaaa"); len(got) != 0 {
		t.Errorf("ValidatePasswordStrength(strong) = %v, want none", got)
	}
	got := ValidatePasswordStrength("weak")
	if len(got) == 0 {
		t.Error("ValidatePasswordStrength(weak) = none, want failures")
	}
}

func TestRetryWithBackoff(t *testing.T) {
	attempts := 0
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }

	err := RetryWithBackoff(func() error {
		attempts++
		if attempts < 3 {
			return errors.New("not yet")
		}
		return nil
	}, 5, sleep)

	if err != nil {
		t.Fatalf("RetryWithBackoff = %v, want nil", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if len(slept) != 2 {
		t.Errorf("sleep called %d times, want 2", len(slept))
	}

	err = RetryWithBackoff(func() error { return errors.New("always fails") }, 2, sleep)
	if err == nil {
		t.Error("RetryWithBackoff(always fails) = nil, want error")
	}
}

func TestCircuitBreakerAllow(t *testing.T) {
	now := time.Now()
	if !CircuitBreakerAllow(1, 3, now.Add(-time.Minute), now, time.Minute) {
		t.Error("below threshold should allow")
	}
	if CircuitBreakerAllow(3, 3, now, now, time.Minute) {
		t.Error("just tripped, within cooldown should NOT allow")
	}
	if !CircuitBreakerAllow(3, 3, now.Add(-2*time.Minute), now, time.Minute) {
		t.Error("past cooldown should allow again")
	}
}
