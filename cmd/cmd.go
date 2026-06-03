package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
	"github.com/somak2kai/beats/pkg/util"
	"golang.org/x/sync/errgroup"
)

var (
	_ command   = (*dbCleaner)(nil)
	_ skippable = (*dbCleaner)(nil)
	_ command   = (*fileMetadata)(nil)
	_ command   = (*functionMetadata)(nil)
	_ command   = (*indexCommand)(nil)
	_ command   = (*functionMetadataWriter)(nil)
	_ skippable = (*functionMetadataWriter)(nil)
	_ command   = (*indexMetadataWriter)(nil)
	_ skippable = (*indexMetadataWriter)(nil)
	_ command   = (*indexPersistor)(nil)
	_ skippable = (*indexPersistor)(nil)
	_ command   = (*beatsLabelWriter)(nil)
	_ command   = (*identifyCluster)(nil)
	_ command   = (*identifyClusterPersistor)(nil)
	_ skippable = (*identifyClusterPersistor)(nil)
	_ command   = (*identifyClusterWriter)(nil)
	_ skippable = (*identifyClusterWriter)(nil)
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
type functionMetadataWriter struct{ state *State }
type indexMetadataWriter struct{ state *State }
type indexPersistor struct{ state *State }
type beatsLabelWriter struct{ state *State }
type identifyCluster struct{ state *State }
type identifyClusterPersistor struct{ state *State }
type identifyClusterWriter struct{ state *State }
type analyzer struct{ state *State }
type orphanAnalyzer struct{ state *State }
type orphanPersistor struct{ state *State }

type Beats struct {
	IsDryRun bool
}
type State struct {
	PkgToFileMetadata ds.PkgToFileMeta
	FunctionMetadata  []ds.FunctionMeta
	OriginalCluster   []ds.Cluster
	CollapsedCluster  []ds.Cluster
	LabelableCluster  []ds.Cluster
	IdentifiedCluster []ds.Cluster
	MemberScores      []ds.MemberScore
	OrphanMetas       []ds.FunctionMeta     // functions that did not join any cluster
	OrphanedFunctions []ds.OrphanedFunction // orphans with Z-score candidates, after analysis
	RepositoryPath    string
	Index             ds.Index
}

// dbCleaner removes the BadgerDB directory for the repository before the
// pipeline writes anything. Without this, re-running beats init accumulates
// stale keys from previous runs — old shape hashes coexist with new ones and
// ScanClusters returns both, inflating all counts.
func (d *dbCleaner) execute() error {
	dbPath := filepath.Join(os.TempDir(), "badger", d.state.RepositoryPath)
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
		for _, m := range val {
			m := m
			g.Go(func() error {
				meta, err := ast.ParseFile(m)
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				f.state.FunctionMetadata = append(f.state.FunctionMetadata, meta...)
				return nil
			})
		}
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

func (w *functionMetadataWriter) execute() error {

	tmp := filepath.Join(os.TempDir(), "funcMeta", uuid.NewString(), filepath.Base(w.state.RepositoryPath), "func_meta.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, fn := range w.state.FunctionMetadata {
		if err := enc.Encode(fn); err != nil {
			slog.Error("unable to write function metadata", slog.String("function_metadata_path", tmp))
			return err
		}
	}
	slog.Info("function metadata written", slog.String("function_metadata_path", tmp))
	return nil
}
func (w *functionMetadataWriter) skipInDryRun() bool {
	return true
}

func (w *indexMetadataWriter) execute() error {

	tmp := filepath.Join(os.TempDir(), "indexMeta", uuid.NewString(), filepath.Base(w.state.RepositoryPath), "index.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	if err := json.NewEncoder(f).Encode(w.state.Index); err != nil {
		slog.Error("unable to write index metadata", slog.String("index_metadata_path", tmp))
		return err
	}
	slog.Info("index metadata written", slog.String("index_metadata_path", tmp))
	return nil
}
func (w *indexMetadataWriter) skipInDryRun() bool {
	return true
}

func (w *indexPersistor) execute() error {

	tmp := filepath.Join(os.TempDir(), "badger", w.state.RepositoryPath)
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

func (b *beatsLabelWriter) execute() error {

	beatsDir := filepath.Join(b.state.RepositoryPath, ".beats")
	if err := os.MkdirAll(beatsDir, 0755); err != nil {
		slog.Error("unable to create .beats directory", slog.Any("error", err))
		return err
	}

	base := filepath.Base(b.state.RepositoryPath)
	labelFile := filepath.Join(beatsDir, "beats_label_"+base+".md")

	if err := b.createClusterLabels(b.state.LabelableCluster, labelFile, base); err != nil {
		slog.Error("unable to create cluster labels ", slog.Any("error", err))
		return err
	}
	slog.Info("beats label wrote", slog.String("path", labelFile))
	return nil
}

func (b *beatsLabelWriter) createClusterLabels(cls []ds.Cluster, path, repo string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	return ast.WriteClusters(f, repo, cls)
}
func (a *analyzer) execute() error {
	return runAnalyze(a.state.RepositoryPath)
}

func (m *analyzer) skipInDryRun() bool { return true }

func (c *identifyCluster) execute() error {
	clusters, orphans := ast.IdentifyClusters(c.state.FunctionMetadata)
	c.state.IdentifiedCluster = clusters
	c.state.OrphanMetas = orphans
	slog.Info("identified clusters",
		slog.Int("count", len(clusters)),
		slog.Int("orphans", len(orphans)),
	)
	return nil
}

func (c *identifyClusterPersistor) execute() error {
	tmp := filepath.Join(os.TempDir(), "badger", c.state.RepositoryPath)
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck

	for idx, cl := range c.state.IdentifiedCluster {
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

func (w *identifyClusterWriter) execute() error {
	tmp := filepath.Join(os.TempDir(), "identifiedCluster", uuid.NewString(), filepath.Base(w.state.RepositoryPath), "cluster.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, cl := range w.state.IdentifiedCluster {
		if err := enc.Encode(cl); err != nil {
			slog.Error("unable to write identified cluster", slog.String("path", tmp))
			return err
		}
	}
	slog.Info("identified clusters written", slog.String("path", tmp))
	return nil
}

func (w *identifyClusterWriter) skipInDryRun() bool { return true }

// orphanAnalyzer scores each orphaned function against every non-primitive
// cluster and computes a Z-score relative to that cluster's internal score
// distribution.
//
// Scoring: the orphan is scored against ALL cluster members (not just the
// medoid) using arithmetic mean (seqS+impS+callS)/3. The mean of those scores
// is then compared to the cluster's own internal mean pairwise score.
//
// Z = (arith_orphan − mean_C) / max(std_C, 0.05)
//
// Z > 0:  orphan scores above the cluster mean → strong candidate, likely missed member.
// Z > 2:  very strong fit — more than 2 std devs above the internal mean.
// Z < −2: too far below the cluster mean → filtered out, not meaningful.
//
// Fast-reject: seqS against the medoid must be ≥ 0.30 before full member scoring.
// For each orphan, up to 5 candidates with Z ≥ −2.0 are kept, sorted by Z
// descending (highest Z = best fit first).
func (o *orphanAnalyzer) execute() error {
	if len(o.state.OrphanMetas) == 0 || len(o.state.IdentifiedCluster) == 0 {
		slog.Info("orphan analysis skipped", slog.String("reason", "no orphans or no clusters"))
		return nil
	}

	const (
		zFloor        = -2.0 // discard candidates more than 2 std devs below cluster mean
		arithMinScore = 0.25 // minimum mean-member arith score to surface a candidate
		seqFastReject = 0.30 // seq similarity against medoid — cheap gate before full scoring
		maxCandidates = 5
	)

	result := make([]ds.OrphanedFunction, 0, len(o.state.OrphanMetas))

	for _, orphan := range o.state.OrphanMetas {
		var candidates []ds.ClusterCandidate

		for idx, cl := range o.state.IdentifiedCluster {
			if cl.IsPrimitive || len(cl.Stats.Top3) == 0 || len(cl.Members) == 0 {
				continue
			}

			// Fast-reject: check seq similarity against the medoid before
			// scoring the orphan against every member.
			medoidSeqS, _, _, _ := ast.OrphanScore(orphan, cl.Stats.Top3[0].Meta)
			if medoidSeqS < seqFastReject {
				continue
			}

			// Score orphan against every cluster member, collect per-dimension sums.
			var seqSum, impSum, callSum float64
			for _, m := range cl.Members {
				s, i, c, _ := ast.OrphanScore(orphan, m)
				seqSum += s
				impSum += i
				callSum += c
			}
			n := float64(len(cl.Members))
			seqS := seqSum / n
			impS := impSum / n
			callS := callSum / n
			arith := (seqS + impS + callS) / 3.0

			if arith < arithMinScore {
				continue
			}

			// Z > 0: orphan beats the cluster mean → probable missed member.
			// Z < −2: too distant → discard.
			z := (arith - cl.Stats.MeanScore) / math.Max(cl.Stats.StdScore, 0.05)
			if z < zFloor {
				continue
			}

			candidates = append(candidates, ds.ClusterCandidate{
				ClusterIdx: idx,
				ShapeHash:  cl.ShapeHash,
				SeqScore:   seqS,
				ImpScore:   impS,
				CallScore:  callS,
				ArithScore: arith,
				ZScore:     z,
				Idiom:      cl.SemanticIdiom,
			})
		}

		if len(candidates) == 0 {
			continue
		}

		// Sort descending: highest Z (strongest fit) first.
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].ZScore > candidates[j].ZScore
		})
		if len(candidates) > maxCandidates {
			candidates = candidates[:maxCandidates]
		}

		result = append(result, ds.OrphanedFunction{
			Meta:       orphan,
			Candidates: candidates,
		})
	}

	o.state.OrphanedFunctions = result
	slog.Info("orphan analysis complete",
		slog.Int("orphans_analysed", len(o.state.OrphanMetas)),
		slog.Int("orphans_with_candidates", len(result)),
	)
	return nil
}

func (o *orphanAnalyzer) skipInDryRun() bool { return false }

func (p *orphanPersistor) execute() error {
	if len(p.state.OrphanedFunctions) == 0 {
		return nil
	}
	tmp := filepath.Join(os.TempDir(), "badger", p.state.RepositoryPath)
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
		OriginalCluster:   make([]ds.Cluster, 0),
		CollapsedCluster:  make([]ds.Cluster, 0),
		LabelableCluster:  make([]ds.Cluster, 0),
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
		&functionMetadataWriter{state: s},
		&identifyCluster{state: s}, // also populates OrphanMetas
		&identifyClusterPersistor{state: s},
		&identifyClusterWriter{state: s},
		&orphanAnalyzer{state: s},  // Z-score analysis against cluster stats
		&orphanPersistor{state: s}, // persist OrphanedFunctions to DB
		&indexCommand{state: s},
		&indexMetadataWriter{state: s},
		&indexPersistor{state: s},
		&analyzer{state: s},
	}
}

//nolint:unused
func (b *Beats) query() error {

	// TODO not yet...
	return nil
}
