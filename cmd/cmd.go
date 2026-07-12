package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	"github.com/somak2kai/beats/pkg/hash"
	ds "github.com/somak2kai/beats/pkg/types"
	"github.com/somak2kai/beats/pkg/util"
	"golang.org/x/sync/errgroup"
)

// beatsDBPath returns the stable BadgerDB path for a given repo.
// Uses ~/.beats/badger/<repo> so the path is consistent across all shell contexts.
func beatsDBPath(repo string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir() // fallback, should never happen
	}
	return filepath.Join(home, ".beats", "badger", repo)
}

var (
	_ command   = (*dbCleaner)(nil)
	_ skippable = (*dbCleaner)(nil)
	_ command   = (*fileMetadata)(nil)
	_ command   = (*functionMetadata)(nil)
	_ command   = (*indexCommand)(nil)
	_ command   = (*indexPersistor)(nil)
	_ skippable = (*indexPersistor)(nil)
	_ command   = (*identifyCluster)(nil)
	_ command   = (*clusterClassifier)(nil)
	_ command   = (*identifyClusterPersistor)(nil)
	_ skippable = (*identifyClusterPersistor)(nil)
	_ command   = (*analyzer)(nil)
	_ skippable = (*analyzer)(nil)
	_ command   = (*orphanAnalyzer)(nil)
	_ skippable = (*orphanAnalyzer)(nil)
	_ command   = (*orphanPersistor)(nil)
	_ skippable = (*orphanPersistor)(nil)
	_ command   = (*outlierWriter)(nil)
	_ skippable = (*outlierWriter)(nil)
	_ command   = (*javafunctionMetadata)(nil)
)

type command interface{ execute() error }
type skippable interface{ skipInDryRun() bool }
type dbCleaner struct{ state *State }
type fileMetadata struct{ state *State }
type functionMetadata struct{ state *State }
type javafunctionMetadata struct{ state *State }
type indexCommand struct{ state *State }
type indexPersistor struct{ state *State }
type identifyCluster struct{ state *State }
type clusterClassifier struct{ state *State }
type identifyClusterPersistor struct{ state *State }
type analyzer struct{ state *State }
type orphanAnalyzer struct{ state *State }
type orphanPersistor struct{ state *State }
type outlierWriter struct{ state *State }

type Beats struct {
	IsDryRun bool
}
type State struct {
	PkgToFileMetadata ds.PkgToFileMeta
	FunctionMetadata  []ds.FunctionMeta
	IdentifiedCluster []ds.Cluster
	OrphanMetas       []ds.FunctionMeta     // functions that did not join any cluster
	OrphanedFunctions []ds.OrphanedFunction // orphans with Z-score candidates, after analysis
	RepositoryPath    string
	Index             ds.Index
}

// dbCleaner removes the BadgerDB directory for the repository, if exists, before the
// pipeline runs.
func (d *dbCleaner) execute() error {
	dbPath := beatsDBPath(d.state.RepositoryPath)
	if err := os.RemoveAll(dbPath); err != nil {
		slog.Error("failed to clear badger db", slog.String("path", dbPath), slog.Any("error", err))
		return err
	}
	slog.Info("cleared existing db", slog.String("path", dbPath))
	return nil
}

func (d *dbCleaner) skipInDryRun() bool { return true }

func (f *fileMetadata) execute() error {
	files, err := util.GetFileMetadata(f.state.RepositoryPath)
	if err != nil {
		slog.Error("failed to get file metadata", slog.Any("error", err))
		return err
	}
	f.state.PkgToFileMetadata = files
	return nil
}

func (f *functionMetadata) execute() error {

	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	var mu sync.Mutex
	for _, val := range f.state.PkgToFileMetadata {
		val := val
		g.Go(func() error {
			var localFncM []ds.FunctionMeta
			for _, m := range val {
				if m.Lang != ds.Language_GOLANG {
					continue
				}
				meta, err := ast.ParseFile(m)
				if err != nil {
					slog.Error("unable to parse file", slog.String("file", m.Path))
					continue
				}
				localFncM = append(localFncM, meta...)
			}
			mu.Lock()
			f.state.FunctionMetadata = append(f.state.FunctionMetadata, localFncM...)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		slog.Error("unable to capture file metadata record", slog.Any("error", err))
		return err
	}
	slog.Info("parsed golang functions", slog.Int("total", len(f.state.FunctionMetadata)))
	return nil
}

func (i *indexCommand) execute() error {
	i.state.Index = ds.PopulateIndex(i.state.FunctionMetadata)
	return nil
}

func (w *indexPersistor) execute() error {

	tmp := beatsDBPath(w.state.RepositoryPath)
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck
	for k, v := range w.state.Index.Postings {
		if err := bDb.StorePostings(k, v); err != nil {
			slog.Error("unable to save inverted index", slog.Any("error", err))
			return err
		}
	}

	for k, v := range w.state.Index.DocFreq {
		if err := bDb.StoreDocFreq(k, v); err != nil {
			slog.Error("unable to save document frequency", slog.Any("error", err))
			return err
		}
	}

	for k, v := range w.state.Index.FuncMeta {
		if err := bDb.StoreFunctionMeta(k, v); err != nil {
			slog.Error("unable to save function metadata", slog.Any("error", err))
			return err
		}
	}
	return nil
}
func (w *indexPersistor) skipInDryRun() bool {
	return true
}

func (a *analyzer) execute() error {
	if len(a.state.IdentifiedCluster) == 0 || len(a.state.OrphanMetas) == 0 {
		return nil
	}
	return runAnalyze(a.state.RepositoryPath)
}

func (m *analyzer) skipInDryRun() bool { return true }

func (c *identifyCluster) execute() error {
	cfg := ast.DefaultClusterConfig()

	clusters, orphans, err := ast.IdentifyClustersWithConfig(c.state.FunctionMetadata, cfg)
	if err != nil {
		return err
	}
	c.state.IdentifiedCluster = clusters
	c.state.OrphanMetas = orphans
	slog.Info("identified clusters",
		slog.Int("count", len(clusters)),
		slog.Int("orphans", len(orphans)),
	)
	return nil
}

// clusterTier returns the conformity tier for a cluster based on the standard
// deviation of its internal pairwise arithmetic scores.
//
//	High   (std < 0.20) — members are structurally near-identical
//	Medium (0.20 ≤ std < 0.40) — consistent pattern with variation
//	Low    (std ≥ 0.40) — broad structural family
//
// clusterTier assigns a conformity tier based on the sample std deviation of
// intra-cluster pairwise arithmetic scores. Thresholds are calibrated to the
// natural spread of structural clusters (std devs typically 0.00–0.15):
//
//	"high"   std < 0.05 — members are near-identical structurally
//	"medium" 0.05 ≤ std < 0.12 — consistent pattern with variation
//	"low"    std ≥ 0.12 — broad structural family, higher internal variation
func clusterTier(stdScore float64) string {
	switch {
	case stdScore < 0.05:
		return "high"
	case stdScore < 0.12:
		return "medium"
	default:
		return "low"
	}
}

// clusterCompositeScore computes the attention score for a cluster:
//
//	ln(size) × ln(numPackages+1) × confidence(tier) × meanScore² × (importCoh + callCoh) / 2
//
// Squaring meanScore penalises looser clusters disproportionately.
// The coherence factor rewards clusters where members share both import domain
// and call vocabulary — the strongest signal of a "real" settled convention.
// The ln(numPackages+1) factor boosts patterns that recur across multiple packages,
// reflecting that cross-package spread is a stronger signal of an established convention.
// confidence weights: high=1.0 / medium=0.6 / low=0.3
// Returns 0 for size ≤ 1 (ln(1)=0, ln(0) is undefined).
func clusterCompositeScore(size, numPackages int, tier string, meanScore, importCoh, callCoh float64) float64 {
	if size <= 1 {
		return 0
	}
	var confidence float64
	switch tier {
	case "high":
		confidence = 1.0
	case "medium":
		confidence = 0.6
	default: // "low"
		confidence = 0.3
	}
	cohFactor := (importCoh + callCoh) / 2.0
	return math.Log(float64(size)) * math.Log1p(float64(numPackages)) * confidence * meanScore * meanScore * cohFactor
}

// clusterClassifier stamps Tier and AttentionScore on every non-primitive
// cluster in State after IdentifyClusters has run. It must execute before
// identifyClusterPersistor so the values are saved to the DB.
func (c *clusterClassifier) execute() error {
	var high, medium, low int
	for i := range c.state.IdentifiedCluster {
		cl := &c.state.IdentifiedCluster[i]
		if cl.IsPrimitive {
			continue
		}
		cl.Tier = clusterTier(cl.Stats.StdScore)
		pkgSet := make(map[string]struct{}, len(cl.Members))
		for _, m := range cl.Members {
			pkgSet[m.Package] = struct{}{}
		}
		cl.CompositeScore = clusterCompositeScore(cl.Size, len(pkgSet), cl.Tier, cl.Stats.MeanScore, cl.Coherence, cl.CallCoherence)
		switch cl.Tier {
		case "high":
			high++
		case "medium":
			medium++
		default:
			low++
		}
	}
	slog.Info("clusters classified",
		slog.Int("high", high),
		slog.Int("medium", medium),
		slog.Int("low", low),
	)
	return nil
}

func (c *identifyClusterPersistor) execute() error {
	tmp := beatsDBPath(c.state.RepositoryPath)
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck

	for idx, cl := range c.state.IdentifiedCluster {
		if cl.IsPrimitive {
			continue
		}
		if err := bDb.StoreClusterByIndex(db.TierIdentified, idx, cl); err != nil {
			slog.Error("unable to save identified cluster",
				slog.Int("index", idx),
				slog.String("shape_hash", cl.ShapeHash),
				slog.Any("error", err),
			)
			return err
		}
	}
	count := len(c.state.IdentifiedCluster)
	if err := bDb.StoreClusterCount(db.TierIdentified, count); err != nil {
		slog.Error("unable to save cluster count", slog.Any("error", err))
		return err
	}
	slog.Info("identified clusters persisted", slog.Int("count", count))
	return nil
}

func (c *identifyClusterPersistor) skipInDryRun() bool { return true }

func (o *orphanAnalyzer) execute() error {

	if len(o.state.IdentifiedCluster) == 0 || len(o.state.OrphanMetas) == 0 {
		return nil
	}
	result, err := ast.IdentifyOrphans(o.state.OrphanMetas, o.state.IdentifiedCluster)
	if err != nil {
		return err
	}
	o.state.OrphanedFunctions = result
	return nil
}

func (o *orphanAnalyzer) skipInDryRun() bool { return false }

func (p *orphanPersistor) execute() error {
	if len(p.state.OrphanedFunctions) == 0 {
		return nil
	}
	tmp := beatsDBPath(p.state.RepositoryPath)
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck

	if err := bDb.StoreOrphanedFunctions(p.state.OrphanedFunctions); err != nil {
		slog.Error("unable to persist orphaned functions", slog.Any("error", err))
		return err
	}
	slog.Info("orphaned functions persisted", slog.Int("count", len(p.state.OrphanedFunctions)))
	return nil
}

func (p *orphanPersistor) skipInDryRun() bool { return true }

// outlierWriter writes .beats/outlier.md — a pre-computed, self-contained
// document containing every outlier and its closest cluster's peer bodies in
// the exact format the beats-analyze LLM skill expects as its user message.
func (o *outlierWriter) execute() error {
	if len(o.state.OrphanedFunctions) == 0 {
		slog.Info("no orphaned functions — skipping outlier.md")
		return nil
	}

	// Index non-primitive clusters by ShapeHash.
	clusterByHash := make(map[string]ds.Cluster, len(o.state.IdentifiedCluster))
	for _, cl := range o.state.IdentifiedCluster {
		if !cl.IsPrimitive && cl.ShapeHash != "" {
			clusterByHash[cl.ShapeHash] = cl
		}
	}

	// Build the same OutlierResult slice the query command produces.
	results := buildOutlierResults(o.state.OrphanedFunctions, clusterByHash)
	if len(results) == 0 {
		slog.Info("no outlier results with candidates — skipping outlier.md")
		return nil
	}

	// Collect unique top-candidate cluster hashes in first-appearance order.
	seen := make(map[string]bool)
	var orderedHashes []string
	for _, r := range results {
		if len(r.Candidates) == 0 {
			continue
		}
		h := r.Candidates[0].ClusterID
		if !seen[h] {
			seen[h] = true
			orderedHashes = append(orderedHashes, h)
		}
	}

	var sb strings.Builder

	fmt.Fprintf(&sb, "# beats outlier analysis\n")
	fmt.Fprintf(&sb, "repo: %s\n", o.state.RepositoryPath)
	fmt.Fprintf(&sb, "generated: %s\n", time.Now().Format("2006-01-02T15:04:05"))
	fmt.Fprintf(&sb, "outliers: %d  clusters: %d\n", len(results), len(orderedHashes))
	fmt.Fprintf(&sb, "\n")

	fmt.Fprintf(&sb, "=== Established Patterns ===\n\n")
	for _, hash := range orderedHashes {
		cl, ok := clusterByHash[hash]
		if !ok {
			continue
		}
		tier := cl.Tier
		if tier == "" {
			tier = "low"
		}
		idiom := cl.SemanticIdiom
		commonSubseq := ast.SeqString(cl.CommonSeq)

		fmt.Fprintf(&sb, "[%s] tier: %s  size: %d  idiom: %q\n", hash, tier, cl.Size, idiom)
		fmt.Fprintf(&sb, "  common subsequence of cluster: %s\n", commonSubseq)
		fmt.Fprintf(&sb, "  peers:\n")

		cr := buildClusterResult(&cl)
		for _, m := range cr.Members {
			fmt.Fprintf(&sb, "    %s/%s (%s:%d)\n", m.Package, m.Func, m.File, m.Line)
			if m.Body != "" {
				for _, line := range strings.Split(strings.TrimRight(m.Body, "\n"), "\n") {
					fmt.Fprintf(&sb, "      %s\n", line)
				}
			}
			fmt.Fprintf(&sb, "\n")
		}
	}

	fmt.Fprintf(&sb, "=== Outlier Functions ===\n\n")
	for i, r := range results {
		fmt.Fprintf(&sb, "--- [%d] %s/%s (%s:%d) ---\n", i+1, r.Package, r.Func, r.File, r.Line)
		if len(r.Candidates) > 0 {
			c := r.Candidates[0]
			fmt.Fprintf(&sb, "closest cluster: %s  score: %.3f  cyclo delta: %s\n",
				c.ClusterID, c.Score, signedDelta(r.CycloDelta))
		}
		fmt.Fprintf(&sb, "token delta:  %s\n", formatDeltaText(r.TokenDelta))
		fmt.Fprintf(&sb, "import delta: %s\n", formatDeltaText(r.ImportDelta))
		fmt.Fprintf(&sb, "call delta:   %s\n", formatDeltaText(r.CallDelta))
		fmt.Fprintf(&sb, "\n")
		if r.Body != "" {
			fmt.Fprintf(&sb, "%s\n", strings.TrimRight(r.Body, "\n"))
		}
		fmt.Fprintf(&sb, "\n")
	}

	beatsDir := filepath.Join(o.state.RepositoryPath, ".beats")
	if err := os.MkdirAll(beatsDir, 0755); err != nil {
		return fmt.Errorf("create .beats dir: %w", err)
	}
	outPath := filepath.Join(beatsDir, "outlier.md")
	if err := os.WriteFile(outPath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("write outlier.md: %w", err)
	}
	slog.Info("outlier.md written", slog.String("path", outPath), slog.Int("outliers", len(results)))
	return nil
}

func (o *outlierWriter) skipInDryRun() bool { return true }

// performs the parsing of java files via jbeats. code errors is jbeats is not available and installed in path.
func (j *javafunctionMetadata) execute() error {

	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	var mu sync.Mutex
	for _, val := range j.state.PkgToFileMetadata {
		val := val
		g.Go(func() error {
			var localFncM []ds.FunctionMeta
			for _, m := range val {
				if m.Lang != ds.Language_JAVA {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/Users/admin/ws/java/jbeats/target/jbeats",
					fmt.Sprintf("--inp=%s", m.Path),
					fmt.Sprintf("--repo=%s", j.state.RepositoryPath),
				)

				out, err := cmd.Output()
				if err != nil {
					if perr, ok := errors.AsType[*exec.ExitError](err); ok {
						slog.Error("unable to execute beats ", slog.String("path", m.Path), slog.Any("error", perr), slog.String("stderr", strings.TrimSpace(string(perr.Stderr))))
						continue
					} else {
						return fmt.Errorf("jbeats failed for %s: %w", m.Path, err)
					}

				}
				if len(out) == 0 {
					return fmt.Errorf("no output was generated for %s", m.Path)
				}
				jsonPath := strings.TrimSpace(string(out))
				if _, err := os.Stat(jsonPath); errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("output file does not exist for %s %s", m.Path, string(out))
				}
				data, err := os.ReadFile(jsonPath)
				if err != nil {
					return fmt.Errorf("read jbeats output for %s: %w", m.Path, err)
				}
				var fileResult ds.JBeatsFileResult
				if err := json.Unmarshal(data, &fileResult); err != nil {
					return fmt.Errorf("parse jbeats output for %s: %w", m.Path, err)
				}
				for _, fn := range fileResult.Functions {
					meta := ds.ToFunctionMeta(fn)
					if meta.TestCode || meta.GeneratedCode {
						continue
					}
					meta.TokenSeqHash = hash.ComputeWindowHash(meta.TokenSeq)
					localFncM = append(localFncM, meta)
				}
				os.Remove(jsonPath) //nolint errcheck
			}
			mu.Lock()
			j.state.FunctionMetadata = append(j.state.FunctionMetadata, localFncM...)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		slog.Error("unable to capture file metadata record", slog.Any("error", err))
		return err
	}
	return nil
}

func (b *Beats) run(repo string) error {

	s := &State{
		RepositoryPath:    repo,
		FunctionMetadata:  make([]ds.FunctionMeta, 0),
		PkgToFileMetadata: make(ds.PkgToFileMeta),
	}
	for _, cmd := range getCommands(s) {
		if b.IsDryRun {
			if c, ok := cmd.(skippable); ok && c.skipInDryRun() {
				slog.Info("skipping (dry-run)")
				continue
			}
		}
		if err := cmd.execute(); err != nil {
			slog.Error("stage halted...", slog.Any("error", err))
			return err
		}
	}
	return nil
}

func getCommands(s *State) []command {
	return []command{
		&dbCleaner{state: s},
		&fileMetadata{state: s},
		&functionMetadata{state: s},
		&javafunctionMetadata{state: s},
		&identifyCluster{state: s},
		&clusterClassifier{state: s},
		&identifyClusterPersistor{state: s},
		&orphanAnalyzer{state: s},
		&orphanPersistor{state: s},
		&indexCommand{state: s},
		&indexPersistor{state: s},
		&outlierWriter{state: s},
		&analyzer{state: s},
	}
}

//nolint:unused
func (b *Beats) query() error {

	// TODO not yet...
	return nil
}
