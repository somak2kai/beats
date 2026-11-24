package hash

const (
	_windowSize = 3
	_windowStep = 2 // ~ 50% overlap
	_rabinBase  = 131
)

// ComputeWindowHash runs sliding window hash against the token sequence of the function.
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

	// 1,2,3 -> 1*131^2 + 2*131^1 + 3*131^0 or 3*131^0 + 2*131^1 + 1*131^2
	var h, mult int64 = 0, 1
	for i := len(tokens) - 1; i >= 0; i-- {
		h += int64(tokens[i]) * mult
		mult = mult * _rabinBase
	}
	return h
}
