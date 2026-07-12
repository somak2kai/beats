package calibrate

import (
	"fmt"
	"math"
	"math/rand"
)

// ── The problem with "composite quality scores" ──────────────────────────────
//
// There's no ground truth. We can't say threshold 0.75 is "better" than 0.70
// because we don't know which functions SHOULD cluster together. Any single
// number that claims to rank configurations is smuggling in assumptions
// (arbitrary weights, arbitrary coverage targets).
//
// What we CAN do:
//   1. Show the TRADEOFF CURVE as you sweep a threshold — coherence goes up,
//      coverage goes down. The user picks their point.
//   2. BOOTSTRAP STABILITY — resample functions, re-cluster, see which clusters
//      survive perturbation. This is objective: "at threshold X, Y% of clusters
//      are reproducible under subsampling."
//   3. Find the KNEE — where lowering the threshold stops producing stable new
//      clusters and starts producing noise.

// ── Tradeoff point — one row in the sweep ────────────────────────────────────

// TradeoffPoint captures what happens at a single threshold value.
type TradeoffPoint struct {
	IdentifyThreshold float64

	// What you get
	NumClusters  int
	NumClustered int // functions in clusters
	NumOrphans   int
	NumEligible  int     // total eligible functions
	Coverage     float64 // NumClustered / NumEligible

	// How tight the clusters are
	MeanCoherence     float64 // mean import Jaccard across clusters
	MeanCallCoherence float64 // mean call-target Jaccard
	MeanPairwiseScore float64 // mean ∛(seqS×impS×callS) within clusters

	// How stable under perturbation (bootstrap)
	BootstrapStability float64 // fraction of clusters that appear in ≥70% of resamples
	StableClusters     int     // count of stable clusters
	NoiseClusters      int     // count that appear in <70% of resamples

	// Size distribution
	MeanClusterSize   float64
	MedianClusterSize int
	MaxClusterSize    int
}

// SweepHeader returns the CSV header for a threshold sweep.
func SweepHeader() string {
	return "threshold,clusters,clustered,coverage,coherence,callCoherence," +
		"meanScore,bootstrapStability,stableClusters,noiseClusters," +
		"meanSize,medianSize,maxSize"
}

// SweepRow formats a TradeoffPoint as CSV.
func SweepRow(p TradeoffPoint) string {
	return fmt.Sprintf("%.2f,%d,%d,%.3f,%.3f,%.3f,%.3f,%.3f,%d,%d,%.1f,%d,%d",
		p.IdentifyThreshold,
		p.NumClusters,
		p.NumClustered,
		p.Coverage,
		p.MeanCoherence,
		p.MeanCallCoherence,
		p.MeanPairwiseScore,
		p.BootstrapStability,
		p.StableClusters,
		p.NoiseClusters,
		p.MeanClusterSize,
		p.MedianClusterSize,
		p.MaxClusterSize,
	)
}

// ── Bootstrap stability ──────────────────────────────────────────────────────
//
// The only metric here that's NOT subjective. It asks:
// "If I randomly drop 20% of functions, do I get the same clusters?"
//
// A cluster that survives 18/20 resamples is capturing real structure.
// A cluster that appears in 3/20 is noise that happens to pass the threshold.

// BootstrapResult holds the outcome of one bootstrap resample.
type BootstrapResult struct {
	// ClusterFingerprints: for each cluster, a sorted set of member qualified names.
	// Two clusters "match" if their Jaccard overlap > 0.5.
	ClusterFingerprints []map[string]bool
}

// Subsample returns a random 80% subset of indices into fns.
func Subsample(n int, fraction float64, rng *rand.Rand) []int {
	perm := rng.Perm(n)
	k := int(float64(n) * fraction)
	if k < 3 {
		k = 3
	}
	return perm[:k]
}

// MatchClusters computes how many clusters from the full run appear in a
// bootstrap resample. A full cluster "appears" if any bootstrap cluster
// has Jaccard overlap > 0.5 with it.
func MatchClusters(fullClusters []map[string]bool, bootClusters []map[string]bool) int {
	matched := 0
	for _, full := range fullClusters {
		for _, boot := range bootClusters {
			if setJaccard(full, boot) > 0.5 {
				matched++
				break
			}
		}
	}
	return matched
}

// ComputeBootstrapStability runs B resamples and returns the fraction of
// full-run clusters that appear in ≥ survivalThreshold fraction of resamples.
//
// fullClusters: fingerprints from the full clustering run
// appearances:  for each full cluster index, count of resamples it appeared in
func ComputeBootstrapStability(appearances []int, B int, survivalThreshold float64) (stability float64, stable, noise int) {
	if len(appearances) == 0 {
		return 0, 0, 0
	}
	minAppearances := int(float64(B) * survivalThreshold)
	for _, count := range appearances {
		if count >= minAppearances {
			stable++
		} else {
			noise++
		}
	}
	stability = float64(stable) / float64(len(appearances))
	return
}

// ── Knee detection ───────────────────────────────────────────────────────────
//
// As you lower the threshold, bootstrap stability eventually drops sharply.
// The knee is where:
//   stability(t) - stability(t - step)  is most negative
//
// Below the knee, you're adding noise clusters faster than stable ones.

// FindKnee returns the threshold value at the knee of the stability curve.
// Points must be sorted by threshold descending (tight → loose).
func FindKnee(points []TradeoffPoint) (kneeThreshold float64, found bool) {
	if len(points) < 3 {
		return 0, false
	}
	maxDrop := 0.0
	kneeIdx := -1
	for i := 1; i < len(points); i++ {
		drop := points[i-1].BootstrapStability - points[i].BootstrapStability
		if drop > maxDrop {
			maxDrop = drop
			kneeIdx = i
		}
	}
	if kneeIdx < 0 || maxDrop < 0.05 {
		return 0, false // no significant knee
	}
	return points[kneeIdx].IdentifyThreshold, true
}

// ── helpers ──────────────────────────────────────────────────────────────────

func setJaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ── Sensitivity verdict (kept from prior version) ────────────────────────────

func ClassifySensitivity(deltaQ float64) string {
	switch {
	case deltaQ < 0.02:
		return "insensitive"
	case deltaQ < 0.05:
		return "moderate"
	default:
		return "critical"
	}
}

// suppress unused import warnings
var _ = math.Sqrt
