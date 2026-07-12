package main

import (
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/calibrate"
	"github.com/somak2kai/beats/pkg/hash"
	ds "github.com/somak2kai/beats/pkg/types"
	"github.com/somak2kai/beats/pkg/util"
)

// runCalibrate is the entry point for "beats calibrate".
//
// Works on a single checkout — no git tag switching needed.
//
// Usage:
//
//	beats calibrate --repo /path/to/repo
//	beats calibrate --repo /path/to/repo --bootstraps 30
//
// What it does:
//  1. Parses all functions from current HEAD (same as beats init).
//  2. Sweeps identifyThreshold from 0.60 → 0.90 in 0.05 steps.
//  3. At each threshold, runs full clustering + bootstrap stability (20 resamples).
//  4. Prints the tradeoff curve and identifies the knee.
//
// Output: .beats/calibration.csv + console tradeoff table.
func runCalibrate(args []string) {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	repo := fs.String("repo", "", "Path to the repository")
	bootstraps := fs.Int("bootstraps", 20, "Number of bootstrap resamples for stability")
	_ = fs.Parse(args)

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats calibrate: --repo is required")
		os.Exit(1)
	}

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		slog.Error("resolve path", slog.Any("error", err))
		os.Exit(1)
	}

	start := time.Now()

	// Step 1: parse
	slog.Info("parsing functions...")
	fns, err := parseCurrentHead(absRepo)
	if err != nil {
		slog.Error("parse failed", slog.Any("error", err))
		os.Exit(1)
	}
	eligible := countEligible(fns)
	slog.Info("parsed", slog.Int("functions", len(fns)), slog.Int("eligible", eligible))

	if eligible < 10 {
		fmt.Fprintln(os.Stderr, "too few eligible functions for meaningful calibration")
		os.Exit(1)
	}

	// Step 2: sweep identifyThreshold
	thresholds := []float64{0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90}

	// Also sweep seqSimilarityThreshold at a few values to see if it matters
	seqSimValues := []float64{0.30, 0.40, 0.50}

	fmt.Printf("\n=== Threshold Sweep ===\n")
	fmt.Printf("Corpus: %d eligible functions\n", eligible)
	fmt.Printf("Bootstrap resamples: %d (80%% subsample each)\n\n", *bootstraps)

	// Primary sweep: vary identifyThreshold, hold seqSim at default (0.40)
	points := sweepThreshold(fns, eligible, thresholds, 0.40, *bootstraps)

	printTradeoffTable(points)

	// Find knee
	knee, found := calibrate.FindKnee(points)
	if found {
		fmt.Printf("\n→ Knee at identifyThreshold=%.2f — below this, new clusters are mostly noise.\n", knee)
	} else {
		fmt.Printf("\n→ No sharp knee found — stability degrades gradually across the range.\n")
	}

	// Secondary sweep: does seqSimilarityThreshold matter?
	fmt.Printf("\n=== seqSimilarityThreshold Sensitivity ===\n")
	fmt.Printf("(holding identifyThreshold at 0.75, varying seqSim)\n\n")
	fmt.Printf("%-8s  %7s  %7s  %7s  %10s\n", "seqSim", "clust", "cover", "coher", "stability")
	for _, ss := range seqSimValues {
		pts := sweepThreshold(fns, eligible, []float64{0.75}, ss, *bootstraps)
		if len(pts) > 0 {
			p := pts[0]
			fmt.Printf("%-8.2f  %7d  %6.1f%%  %7.3f  %9.1f%%\n",
				ss, p.NumClusters, p.Coverage*100, p.MeanCoherence, p.BootstrapStability*100)
		}
	}

	// Write CSV
	beatsDir := filepath.Join(absRepo, ".beats")
	_ = os.MkdirAll(beatsDir, 0755)
	csvPath := filepath.Join(beatsDir, "calibration.csv")
	writeCSV(csvPath, points)
	slog.Info("csv written", slog.String("path", csvPath))

	fmt.Printf("\nElapsed: %s\n", time.Since(start).Round(time.Millisecond))
}

// ── core sweep ───────────────────────────────────────────────────────────────

func sweepThreshold(fns []ds.FunctionMeta, eligible int, thresholds []float64, seqSim float64, B int) []calibrate.TradeoffPoint {
	points := make([]calibrate.TradeoffPoint, len(thresholds))

	for ti, thresh := range thresholds {
		tStart := time.Now()
		fmt.Printf("  [%d/%d] threshold=%.2f — clustering...", ti+1, len(thresholds), thresh)

		cfg := ast.DefaultClusterConfig()
		cfg.IdentifyThreshold = thresh
		cfg.SeqSimilarityThreshold = seqSim
		cfg.SeqFastReject = seqSim * 0.75

		// Full clustering
		clusters, orphans, err := ast.IdentifyClustersWithConfig(fns, cfg)
		if err != nil {
			fmt.Printf(" FAILED\n")
			slog.Warn("clustering failed", slog.Float64("threshold", thresh))
			continue
		}

		// Compute cluster fingerprints for bootstrap matching
		fullFingerprints := clusterFingerprints(clusters)

		fmt.Printf(" %d clusters. bootstrapping...", len(fullFingerprints))

		// Basic stats
		numClustered := 0
		var cohSum, callCohSum, scoreSum float64
		sizes := make([]int, 0, len(clusters))
		for _, cl := range clusters {
			if cl.IsPrimitive {
				continue
			}
			numClustered += cl.Size
			cohSum += cl.Coherence
			callCohSum += cl.CallCoherence
			scoreSum += cl.Stats.MeanScore
			sizes = append(sizes, cl.Size)
		}
		nc := len(sizes)
		meanCoh, meanCallCoh, meanScore := 0.0, 0.0, 0.0
		if nc > 0 {
			meanCoh = cohSum / float64(nc)
			meanCallCoh = callCohSum / float64(nc)
			meanScore = scoreSum / float64(nc)
		}

		// Size stats
		sort.Ints(sizes)
		medianSize, maxSize := 0, 0
		meanSize := 0.0
		if len(sizes) > 0 {
			medianSize = sizes[len(sizes)/2]
			maxSize = sizes[len(sizes)-1]
			s := 0
			for _, sz := range sizes {
				s += sz
			}
			meanSize = float64(s) / float64(len(sizes))
		}

		// Bootstrap stability — parallel resamples
		appearances := make([]int, len(fullFingerprints))
		var appMu sync.Mutex

		// Pre-generate all subsample indices (deterministic)
		rng := rand.New(rand.NewSource(42))
		allIndices := make([][]int, B)
		for b := range B {
			allIndices[b] = calibrate.Subsample(len(fns), 0.80, rng)
		}

		workers := runtime.GOMAXPROCS(0)
		if workers > B {
			workers = B
		}
		var bootWg sync.WaitGroup
		bootSem := make(chan struct{}, workers)

		for b := 0; b < B; b++ {
			b := b
			bootWg.Add(1)
			go func() {
				defer bootWg.Done()
				bootSem <- struct{}{}
				defer func() { <-bootSem }()

				subFns := make([]ds.FunctionMeta, len(allIndices[b]))
				for i, idx := range allIndices[b] {
					subFns[i] = fns[idx]
				}

				bootClusters, _, _ := ast.IdentifyClustersWithConfig(subFns, cfg)
				bootFP := clusterFingerprints(bootClusters)

				// Match each full cluster against bootstrap clusters
				localHits := make([]bool, len(fullFingerprints))
				for fi, fullFP := range fullFingerprints {
					for _, bootFPItem := range bootFP {
						if setJaccard(fullFP, bootFPItem) > 0.5 {
							localHits[fi] = true
							break
						}
					}
				}

				appMu.Lock()
				for fi, hit := range localHits {
					if hit {
						appearances[fi]++
					}
				}
				appMu.Unlock()
			}()
		}
		bootWg.Wait()

		stability, stable, noise := calibrate.ComputeBootstrapStability(appearances, B, 0.70)

		fmt.Printf(" done (%s)\n", time.Since(tStart).Round(time.Millisecond))

		points[ti] = calibrate.TradeoffPoint{
			IdentifyThreshold:  thresh,
			NumClusters:        nc,
			NumClustered:       numClustered,
			NumOrphans:         len(orphans),
			NumEligible:        eligible,
			Coverage:           float64(numClustered) / math.Max(1, float64(eligible)),
			MeanCoherence:      meanCoh,
			MeanCallCoherence:  meanCallCoh,
			MeanPairwiseScore:  meanScore,
			BootstrapStability: stability,
			StableClusters:     stable,
			NoiseClusters:      noise,
			MeanClusterSize:    meanSize,
			MedianClusterSize:  medianSize,
			MaxClusterSize:     maxSize,
		}
	}

	return points
}

// ── display ──────────────────────────────────────────────────────────────────

func printTradeoffTable(points []calibrate.TradeoffPoint) {
	fmt.Printf("%-8s  %7s  %7s  %7s  %7s  %10s  %7s  %7s\n",
		"thresh", "clust", "cover", "coher", "score", "stability", "stable", "noise")
	fmt.Printf("%-8s  %7s  %7s  %7s  %7s  %10s  %7s  %7s\n",
		"------", "-----", "-----", "-----", "-----", "---------", "------", "-----")
	for _, p := range points {
		marker := "  "
		// Mark the sweet spot: highest stable cluster count
		fmt.Printf("%-8.2f  %7d  %6.1f%%  %7.3f  %7.3f  %9.1f%%  %7d  %7d%s\n",
			p.IdentifyThreshold,
			p.NumClusters,
			p.Coverage*100,
			p.MeanCoherence,
			p.MeanPairwiseScore,
			p.BootstrapStability*100,
			p.StableClusters,
			p.NoiseClusters,
			marker,
		)
	}

	// Show the tradeoff explicitly
	if len(points) >= 2 {
		lo := points[len(points)-1] // tightest (highest threshold)
		hi := points[0]             // loosest (lowest threshold)
		fmt.Printf("\nTradeoff: %.2f→%.2f gains %d clusters (+%.0f%% coverage) but coherence drops %.3f→%.3f\n",
			lo.IdentifyThreshold, hi.IdentifyThreshold,
			hi.NumClusters-lo.NumClusters,
			(hi.Coverage-lo.Coverage)*100,
			lo.MeanCoherence, hi.MeanCoherence,
		)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func clusterFingerprints(clusters []ds.Cluster) []map[string]bool {
	fps := make([]map[string]bool, 0, len(clusters))
	for _, cl := range clusters {
		if cl.IsPrimitive {
			continue
		}
		fp := make(map[string]bool, len(cl.Members))
		for _, m := range cl.Members {
			fp[m.Package+"/"+m.Name] = true
		}
		fps = append(fps, fp)
	}
	return fps
}

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

func parseCurrentHead(repo string) ([]ds.FunctionMeta, error) {
	files, err := util.GetFileMetadata(repo)
	if err != nil {
		return nil, err
	}
	var fns []ds.FunctionMeta
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for _, val := range files {
		val := val
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var local []ds.FunctionMeta
			for _, m := range val {
				if m.Lang != ds.Language_GOLANG {
					continue
				}
				meta, err := ast.ParseFile(m)
				if err != nil {
					continue
				}
				for i := range meta {
					meta[i].TokenSeqHash = hash.ComputeWindowHash(meta[i].TokenSeq)
				}
				local = append(local, meta...)
			}
			mu.Lock()
			fns = append(fns, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return fns, nil
}

func countEligible(fns []ds.FunctionMeta) int {
	count := 0
	for _, fn := range fns {
		if len(fn.TokenSeq) >= 3 && !fn.GeneratedCode && !fn.TestCode && !fn.IsConstructor {
			count++
		}
	}
	return count
}

func writeCSV(path string, points []calibrate.TradeoffPoint) {
	f, err := os.Create(path)
	if err != nil {
		slog.Error("create csv", slog.Any("error", err))
		return
	}
	defer f.Close() //nolint
	fmt.Println(f, calibrate.SweepHeader())
	for _, p := range points {
		fmt.Println(f, calibrate.SweepRow(p))
	}
}
