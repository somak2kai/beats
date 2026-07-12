// Package calibrate provides threshold calibration for the beats clustering
// pipeline.
//
// The core idea: beats has ~11 hardcoded thresholds. Without ground-truth labels,
// we use proxy quality metrics (temporal stability, coherence, coverage,
// size-distribution health) to evaluate whether a threshold configuration is
// good. A grid search over the 3 primary knobs, scored by these proxies across
// multiple release snapshots, finds the optimal config.
package calibrate

import (
	"fmt"
)

// ClusterConfig holds all tuneable thresholds for the clustering pipeline.
// Pass this to IdentifyClustersWithConfig instead of relying on package constants.
type ClusterConfig struct {
	// PRIMARY — grid-searched
	IdentifyThreshold      float64 // min ∛(seqS×impS×callS) for pair to be a candidate (default 0.75)
	SeqSimilarityThreshold float64 // fast-reject: min seq similarity (default 0.40)
	MaxTrigranBucket       int     // trigram stop-word cutoff (default 300)

	// SECONDARY — moderate impact
	MinTokenSeqLen     int     // min tokens for a function to participate (default 5)
	IdentifyMinSize    int     // min cluster members to survive (default 3)
	PrimitiveThreshold float64 // max cluster size as fraction of corpus (default 0.05)

	// DERIVED — auto-set from primaries and corpus
	SeqFastReject     float64 // orphan medoid gate (default: SeqSimilarityThreshold × 0.75)
	ClusterToleranceK float64 // multiplier for μ−k×σ dynamic admission (default 1.5)
	TierHighBound     float64 // std < this → high tier (default 0.05)
	TierMediumBound   float64 // std < this → medium tier (default 0.12)
}

// DefaultConfig returns the current hardcoded values as a ClusterConfig.
func DefaultConfig() ClusterConfig {
	return ClusterConfig{
		IdentifyThreshold:      0.75,
		SeqSimilarityThreshold: 0.40,
		MaxTrigranBucket:       300,
		MinTokenSeqLen:         5,
		IdentifyMinSize:        3,
		PrimitiveThreshold:     0.05,
		SeqFastReject:          0.30,
		ClusterToleranceK:      1.5,
		TierHighBound:          0.05,
		TierMediumBound:        0.12,
	}
}

// GridPoints returns all threshold configurations in the search grid.
// 6 × 6 × 5 = 180 configurations.
func GridPoints() []ClusterConfig {
	identifyRange := []float64{0.60, 0.65, 0.70, 0.75, 0.80, 0.85}
	seqSimRange := []float64{0.25, 0.30, 0.35, 0.40, 0.45, 0.50}
	bucketRange := []int{100, 200, 300, 400, 500}

	configs := make([]ClusterConfig, 0, len(identifyRange)*len(seqSimRange)*len(bucketRange))
	for _, it := range identifyRange {
		for _, ss := range seqSimRange {
			for _, mb := range bucketRange {
				c := DefaultConfig()
				c.IdentifyThreshold = it
				c.SeqSimilarityThreshold = ss
				c.MaxTrigranBucket = mb
				// auto-derive seqFastReject from the seq threshold
				c.SeqFastReject = ss * 0.75
				configs = append(configs, c)
			}
		}
	}
	return configs
}

// String returns a compact representation for CSV headers / log lines.
func (c ClusterConfig) String() string {
	return fmt.Sprintf("IT=%.2f|SS=%.2f|MB=%d", c.IdentifyThreshold, c.SeqSimilarityThreshold, c.MaxTrigranBucket)
}
