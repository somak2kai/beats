package ast

import (
	"crypto/sha256"
	"fmt"
	"math"
	"strconv"

	ds "github.com/somak2kai/beats/pkg/types"
)

// ComputeMemberScores scores every member of every non-primitive collapsed
// cluster against all non-primitive clusters using:
//
//	score(f, C_i) = shape_match(f, C_i)
//	              + Jaccard(f.DirectImports,  all_member_imports(C_i))
//	              + Jaccard(f.CallTargets,    all_member_call_targets(C_i))
//
// Raw scores are converted to probabilities via softmax. Each function is
// scored exactly once even if it appears in multiple clusters after family
// collapse.
func ComputeMemberScores(clusters []ds.Cluster) []ds.MemberScore {
	// work only with non-primitive clusters — primitives are structural stop-words
	active := make([]ds.Cluster, 0, len(clusters))
	for _, c := range clusters {
		if !c.IsPrimitive {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return nil
	}

	// precompute full import and call sets per cluster once — reused for every
	// function scored against that cluster
	type clusterSets struct {
		imports map[string]bool
		calls   map[string]bool
	}
	sets := make([]clusterSets, len(active))
	for i, c := range active {
		imp := make(map[string]bool)
		cal := make(map[string]bool)
		for _, m := range c.Members {
			for _, x := range m.DirectImports {
				imp[x] = true
			}
			for _, x := range m.CallTargets {
				cal[x] = true
			}
		}
		sets[i] = clusterSets{imp, cal}
	}

	// score every member, deduplicating functions that appear in more than one
	// cluster after family collapse
	seen := make(map[string]bool)
	var scores []ds.MemberScore

	for _, c := range active {
		for _, fn := range c.Members {
			id := FunctionID(fn)
			if seen[id] {
				continue
			}
			seen[id] = true

			// raw score against relevant clusters only.
			// A cluster is relevant only if shape_match fired (score >= 1.0).
			// Clusters with no shape match produce near-zero scores from import/call
			// Jaccard alone — including them inflates the softmax denominator with
			// noise and compresses entropy to a useless near-uniform distribution.
			raw := make(map[string]float64, 8)
			for i, ci := range active {
				s := scoreFunction(fn, ci, sets[i].imports, sets[i].calls)
				if s >= 1.0 {
					raw[ci.ShapeHash] = s
				}
			}

			// if no cluster matched on shape, this function sits outside all known
			// patterns — skip it rather than writing a meaningless uniform distribution
			if len(raw) == 0 {
				continue
			}

			probs := softmax(raw)
			scores = append(scores, ds.MemberScore{
				FunctionID: id,
				Package:    fn.Package,
				Name:       fn.Name,
				FilePath:   fn.FileMeta.Path,
				Line:       fn.Start_line,
				Probs:      probs,
				WinnerID:   winnerCluster(probs),
				Entropy:    shannonEntropy(probs),
			})
		}
	}
	return scores
}

// scoreFunction computes the raw three-term score for fn against cluster c.
// Maximum possible score is 3.0 (perfect shape match + Jaccard 1.0 on both sets).
func scoreFunction(fn ds.FunctionMeta, c ds.Cluster, memberImports, memberCalls map[string]bool) float64 {
	return shapeMatchScore(fn.TokenSeq, c.TokenSeq, c.ShapeVariants) +
		jaccard(toSet(fn.DirectImports), memberImports) +
		jaccard(toSet(fn.CallTargets), memberCalls)
}

// shapeMatchScore returns 1.0 if fnSeq matches the cluster's primary shape or
// any absorbed shape variant, 0.0 otherwise.
func shapeMatchScore(fnSeq, primary []int, variants [][]int) float64 {
	k := seqKey(fnSeq)
	if k == seqKey(primary) {
		return 1.0
	}
	for _, v := range variants {
		if k == seqKey(v) {
			return 1.0
		}
	}
	return 0.0
}

// softmax converts raw scores to a probability distribution that sums to 1.
// Subtracts the max score before exp() for numerical stability.
func softmax(scores map[string]float64) map[string]float64 {
	var maxScore float64
	for _, s := range scores {
		if s > maxScore {
			maxScore = s
		}
	}
	probs := make(map[string]float64, len(scores))
	var sum float64
	for k, s := range scores {
		probs[k] = math.Exp(s - maxScore)
		sum += probs[k]
	}
	for k := range probs {
		probs[k] /= sum
	}
	return probs
}

// shannonEntropy computes H = -Σ p·log(p) over the probability distribution.
// High entropy → function fits multiple clusters equally (boundary candidate).
// Low entropy  → function clearly belongs to one cluster (conforming member).
func shannonEntropy(probs map[string]float64) float64 {
	var h float64
	for _, p := range probs {
		if p > 0 {
			h -= p * math.Log(p)
		}
	}
	return h
}

// winnerCluster returns the shapeHash of the cluster with the highest
// probability. Ties broken alphabetically by shapeHash for determinism.
func winnerCluster(probs map[string]float64) string {
	var best string
	var bestP float64
	for k, p := range probs {
		if p > bestP || (p == bestP && k < best) {
			bestP = p
			best = k
		}
	}
	return best
}

// toSet converts a string slice to a boolean set map.
func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// FunctionID returns a stable 16-hex-char identity for a function derived from
// its package, name, file path, and start line. Stable across runs as long as
// the function's location does not change.
func FunctionID(fn ds.FunctionMeta) string {
	raw := fn.Package + "." + fn.Name + "@" + fn.FileMeta.Path + ":" + strconv.Itoa(fn.Start_line)
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%016x", h[:8])
}
