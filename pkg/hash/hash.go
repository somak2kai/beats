package hash

const (
	_windowSize = 3
	_windowStep = 2 // ~ 50% overlap
	_rabinBase  = 131
	_rabinMod   = 1_000_000_007
)

// ComputeWindowHash runs rabin karp sliding window hash against the token sequence of the function.
func ComputeWindowHash(tokenSeq []int) []int64 {
	if len(tokenSeq) < _windowSize {
		return []int64{hashWindow(tokenSeq)}
	}
	hashes := make([]int64, 0, len(tokenSeq))
	for start := 0; start+_windowSize <= len(tokenSeq); start += _windowStep {
		hashes = append(hashes, hashWindow(tokenSeq[start:start+_windowSize]))
	}
	return hashes
}

func hashWindow(tokens []int) int64 {
	var h int64
	for _, t := range tokens {
		h = (h*_rabinBase + int64(t)) % _rabinMod
	}
	return h
}
