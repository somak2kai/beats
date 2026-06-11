package main

import (
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
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
)

type command interface{ execute() error }
type skippable interface{ skipInDryRun() bool }
type dbCleaner struct{ state *State }
type fileMetadata struct{ state *State }
type functionMetadata struct{ state *State }
type indexCommand struct{ state *State }
type indexPersistor struct{ state *State }

type identifyCluster struct{ state *State }
type clusterClassifier struct{ state *State }
type identifyClusterPersistor struct{ state *State }
type analyzer struct{ state *State }
type orphanAnalyzer struct{ state *State }
type orphanPersistor struct{ state *State }

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
	clusters, orphans, err := ast.IdentifyClusters(c.state.FunctionMetadata)
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
		&identifyCluster{state: s},
		&clusterClassifier{state: s},
		&identifyClusterPersistor{state: s},
		&orphanAnalyzer{state: s},
		&orphanPersistor{state: s},
		&indexCommand{state: s},
		&indexPersistor{state: s},
		&analyzer{state: s},
	}
}

//nolint:unused
func (b *Beats) query() error {

	// TODO not yet...
	return nil
}
