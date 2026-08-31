// Package sample holds a handful of small, self-contained Go functions used
// purely as mutation-testing fodder (see scripts/mutation/run.sh). They are
// deliberately trivial and cover a mix of comparisons, boundaries, loops,
// and early returns so that gremlins' default mutators (arithmetic-base,
// conditionals-boundary, conditionals-negation, increment-decrement,
// invert-negatives) and go-mutesting's branch/expression/statement mutators
// all have something to bite into.
package sample

import "errors"

// Max returns the larger of a and b.
func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Min returns the smaller of a and b.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Clamp restricts v to the inclusive range [lo, hi].
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// IsEven reports whether n is even.
func IsEven(n int) bool {
	return n%2 == 0
}

// IsPositive reports whether n is strictly greater than zero.
func IsPositive(n int) bool {
	return n > 0
}

// Sum returns the sum of nums.
func Sum(nums []int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}

// Contains reports whether target appears anywhere in nums.
func Contains(nums []int, target int) bool {
	for _, n := range nums {
		if n == target {
			return true
		}
	}
	return false
}

// Reverse returns s with its runes in reverse order.
func Reverse(s string) string {
	runes := []rune(s)
	result := make([]rune, len(runes))
	for i, r := range runes {
		result[len(runes)-1-i] = r
	}
	return string(result)
}

// CountVowels returns the number of ASCII vowels (a, e, i, o, u — either
// case) in s.
func CountVowels(s string) int {
	count := 0
	for _, r := range s {
		switch r {
		case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
			count++
		}
	}
	return count
}

// Filter returns the subset of nums that are strictly greater than
// threshold, preserving order.
func Filter(nums []int, threshold int) []int {
	out := make([]int, 0, len(nums))
	for _, n := range nums {
		if n > threshold {
			out = append(out, n)
		}
	}
	return out
}

// SafeDivide divides a by b, returning an error instead of panicking or
// producing +/-Inf when b is zero.
func SafeDivide(a, b float64) (float64, error) {
	if b == 0 {
		return 0, errors.New("sample: division by zero")
	}
	return a / b, nil
}
