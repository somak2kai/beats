package sample

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ValidateEmail does a minimal structural check (not full RFC 5322) — good
// enough to exercise realistic branching without pulling in regexp.
func ValidateEmail(email string) error {
	if email == "" {
		return errors.New("sample: email is empty")
	}
	if strings.ContainsAny(email, " \t\n") {
		return errors.New("sample: email contains whitespace")
	}
	at := strings.Index(email, "@")
	if at <= 0 || at == len(email)-1 {
		return errors.New("sample: email missing a valid '@'")
	}
	domain := email[at+1:]
	dot := strings.LastIndex(domain, ".")
	if dot <= 0 || dot == len(domain)-1 {
		return errors.New("sample: email domain missing a valid '.'")
	}
	return nil
}

// MaskSensitive replaces everything but the last `visible` characters of s
// with '*', the way you'd mask a card number or token in a log line.
func MaskSensitive(s string, visible int) string {
	if visible < 0 {
		visible = 0
	}
	if visible >= len(s) {
		return s
	}
	return strings.Repeat("*", len(s)-visible) + s[len(s)-visible:]
}

// Deduplicate returns items with duplicates removed, preserving first-seen
// order.
func Deduplicate(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// ChunkSlice splits items into consecutive chunks of at most size elements.
func ChunkSlice(items []int, size int) ([][]int, error) {
	if size <= 0 {
		return nil, errors.New("sample: chunk size must be positive")
	}
	var chunks [][]int
	for i := 0; i < len(items); i += size {
		end := i + size
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[i:end])
	}
	return chunks, nil
}

// Paginate returns the 1-indexed page of items for the given page size.
func Paginate(items []string, page, pageSize int) ([]string, error) {
	if page < 1 {
		return nil, errors.New("sample: page must be >= 1")
	}
	if pageSize < 1 {
		return nil, errors.New("sample: pageSize must be >= 1")
	}
	start := (page - 1) * pageSize
	if start >= len(items) {
		return []string{}, nil
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], nil
}

// MergeConfigs merges override on top of base, without mutating either
// input map.
func MergeConfigs(base, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// TruncateWithEllipsis shortens s to at most maxLen bytes, appending "..."
// when truncation actually happens.
func TruncateWithEllipsis(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// hasUpper, hasDigit, and hasSpecial are small helpers that
// ValidatePasswordStrength calls — deliberately factored out so there's a
// real call site for gopls's "inline function call" refactor to work on.
func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func hasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func hasSpecial(s string) bool {
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return true
		}
	}
	return false
}

// ValidatePasswordStrength returns a human-readable reason for every
// strength rule pw fails; an empty slice means pw passes all of them.
func ValidatePasswordStrength(pw string) []string {
	var problems []string
	if len(pw) < 8 {
		problems = append(problems, "must be at least 8 characters")
	}
	if !hasUpper(pw) {
		problems = append(problems, "must contain an uppercase letter")
	}
	if !hasDigit(pw) {
		problems = append(problems, "must contain a digit")
	}
	if !hasSpecial(pw) {
		problems = append(problems, "must contain a special character")
	}
	return problems
}

// RetryWithBackoff calls fn until it succeeds or maxAttempts is reached,
// calling sleep between attempts (injected so tests don't need real time.Sleep).
func RetryWithBackoff(fn func() error, maxAttempts int, sleep func(time.Duration)) error {
	if maxAttempts < 1 {
		return errors.New("sample: maxAttempts must be >= 1")
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt < maxAttempts {
			sleep(time.Duration(attempt) * time.Millisecond)
		}
	}
	return fmt.Errorf("sample: all %d attempts failed: %w", maxAttempts, lastErr)
}

// CircuitBreakerAllow reports whether a call should be allowed through,
// given how many consecutive failures have happened, the failure
// threshold, when the last failure was, and how long the breaker stays
// open (cooldown) once tripped.
func CircuitBreakerAllow(consecutiveFailures, threshold int, lastFailureAt, now time.Time, cooldown time.Duration) bool {
	if consecutiveFailures < threshold {
		return true
	}
	return now.Sub(lastFailureAt) >= cooldown
}
