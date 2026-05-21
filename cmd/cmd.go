package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/google/uuid"
	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
	"github.com/somak2kai/beats/pkg/util"
	"golang.org/x/sync/errgroup"
)

var (
	_ Command   = (*DbCleaner)(nil)
	_ Skippable = (*DbCleaner)(nil)
	_ Command   = (*FileMetadata)(nil)
	_ Command   = (*FunctionMetadata)(nil)
	_ Command   = (*IndexCommand)(nil)
	_ Command   = (*FunctionMetadataWriter)(nil)
	_ Skippable = (*FunctionMetadataWriter)(nil)
	_ Command   = (*IndexMetadataWriter)(nil)
	_ Skippable = (*IndexMetadataWriter)(nil)
	_ Command   = (*IndexPersistor)(nil)
	_ Skippable = (*IndexPersistor)(nil)
	_ Command   = (*BeatsLabelWriter)(nil)
	_ Command   = (*MemberScorer)(nil)
	_ Command   = (*MemberScoreWriter)(nil)
	_ Skippable = (*MemberScoreWriter)(nil)
	_ Command   = (*MemberScorePersistor)(nil)
	_ Skippable = (*MemberScorePersistor)(nil)
	_ Command   = (*IdentifyCluster)(nil)
	_ Command   = (*IdentifyClusterPersistor)(nil)
	_ Skippable = (*IdentifyClusterPersistor)(nil)
	_ Command   = (*IdentifyClusterWriter)(nil)
	_ Skippable = (*IdentifyClusterWriter)(nil)
)

type Command interface{ Execute() error }
type Skippable interface{ SkipInDryRun() bool }
type DbCleaner struct{ State *State }
type FileMetadata struct{ State *State }
type FunctionMetadata struct{ State *State }
type IndexCommand struct{ State *State }
type ClusterMetadata struct{ State *State }
type FunctionMetadataWriter struct{ State *State }
type IndexMetadataWriter struct{ State *State }
type IndexPersistor struct{ State *State }
type CollapseClusterToFamily struct{ State *State }
type LabelCluster struct{ State *State }
type BeatsLabelWriter struct{ State *State }
type ClusterWriter struct{ State *State }
type ClusterPersistor struct{ State *State }
type MemberScorer struct{ State *State }
type MemberScoreWriter struct{ State *State }
type MemberScorePersistor struct{ State *State }
type IdentifyCluster struct{ State *State }
type IdentifyClusterPersistor struct{ State *State }
type IdentifyClusterWriter struct{ State *State }

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
	RepositoryPath    string
	Index             ds.Index
}

// DbCleaner removes the BadgerDB directory for the repository before the
// pipeline writes anything. Without this, re-running beats init accumulates
// stale keys from previous runs — old shape hashes coexist with new ones and
// ScanClusters returns both, inflating all counts.
func (d *DbCleaner) Execute() error {
	dbPath := filepath.Join(os.TempDir(), "badger", d.State.RepositoryPath)
	if err := os.RemoveAll(dbPath); err != nil {
		slog.Error("failed to clear badger db", slog.String("path", dbPath), slog.Any("error", err))
		return err
	}
	slog.Info("cleared existing db", slog.String("path", dbPath))
	return nil
}

func (d *DbCleaner) SkipInDryRun() bool { return true }

func (f *FileMetadata) Execute() error {
	files, err := util.GetFileMetadata(f.State.RepositoryPath)
	if err != nil {
		slog.Error("failed to get file metadata", slog.Any("error", err))
		return err
	}
	f.State.PkgToFileMetadata = files
	return nil
}

func (f *FunctionMetadata) Execute() error {

	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	var mu sync.Mutex
	for _, val := range f.State.PkgToFileMetadata {
		for _, m := range val {
			m := m
			g.Go(func() error {
				meta, err := ast.ParseFile(m)
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				f.State.FunctionMetadata = append(f.State.FunctionMetadata, meta...)
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

func (i *IndexCommand) Execute() error {
	i.State.Index = ds.PopulateIndex(i.State.FunctionMetadata)
	return nil
}

func (w *FunctionMetadataWriter) Execute() error {

	tmp := filepath.Join(os.TempDir(), "funcMeta", uuid.NewString(), filepath.Base(w.State.RepositoryPath), "func_meta.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, fn := range w.State.FunctionMetadata {
		if err := enc.Encode(fn); err != nil {
			slog.Error("unable to write function metadata", slog.String("function_metadata_path", tmp))
			return err
		}
	}
	slog.Info("function metadata written", slog.String("function_metadata_path", tmp))
	return nil
}
func (w *FunctionMetadataWriter) SkipInDryRun() bool {
	return true
}

func (w *IndexMetadataWriter) Execute() error {

	tmp := filepath.Join(os.TempDir(), "indexMeta", uuid.NewString(), filepath.Base(w.State.RepositoryPath), "index.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	if err := json.NewEncoder(f).Encode(w.State.Index); err != nil {
		slog.Error("unable to write index metadata", slog.String("index_metadata_path", tmp))
		return err
	}
	slog.Info("index metadata written", slog.String("index_metadata_path", tmp))
	return nil
}
func (w *IndexMetadataWriter) SkipInDryRun() bool {
	return true
}

func (w *IndexPersistor) Execute() error {

	tmp := filepath.Join(os.TempDir(), "badger", w.State.RepositoryPath)
	bDb := db.NewDb(tmp)
	defer bDb.Close() //nolint:errcheck
	for k, v := range w.State.Index.Postings {
		if err := bDb.StorePostings(k, v); err != nil {
			slog.Error("unable to save inverted index", slog.Any("error", err))
			return err
		}
	}

	for k, v := range w.State.Index.DocFreq {
		if err := bDb.StoreDocFreq(k, v); err != nil {
			slog.Error("unable to save document frequency", slog.Any("error", err))
			return err
		}
	}

	for k, v := range w.State.Index.FuncMeta {
		if err := bDb.StoreFunctionMeta(k, v); err != nil {
			slog.Error("unable to save function metadata", slog.Any("error", err))
			return err
		}
	}
	return nil
}
func (w *IndexPersistor) SkipInDryRun() bool {
	return true
}

func (b *BeatsLabelWriter) Execute() error {

	beatsDir := filepath.Join(b.State.RepositoryPath, ".beats")
	if err := os.MkdirAll(beatsDir, 0755); err != nil {
		slog.Error("unable to create .beats directory", slog.Any("error", err))
		return err
	}

	base := filepath.Base(b.State.RepositoryPath)
	labelFile := filepath.Join(beatsDir, "beats_label_"+base+".md")

	if err := b.createClusterLabels(b.State.LabelableCluster, labelFile, base); err != nil {
		slog.Error("unable to create cluster labels ", slog.Any("error", err))
		return err
	}
	slog.Info("beats label wrote", slog.String("path", labelFile))
	return nil
}

func (b *BeatsLabelWriter) createClusterLabels(cls []ds.Cluster, path, repo string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	return ast.WriteClusters(f, repo, cls)
}

func (m *MemberScorer) Execute() error {
	m.State.MemberScores = ast.ComputeMemberScores(m.State.CollapsedCluster)
	slog.Info("member scores computed", slog.Int("count", len(m.State.MemberScores)))
	return nil
}

func (m *MemberScoreWriter) Execute() error {
	tmp := filepath.Join(os.TempDir(), "memberScore", uuid.NewString(), filepath.Base(m.State.RepositoryPath), "member_score.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, ms := range m.State.MemberScores {
		if err := enc.Encode(ms); err != nil {
			slog.Error("unable to write member score", slog.String("path", tmp))
			return err
		}
	}
	slog.Info("member scores written", slog.String("path", tmp))
	return nil
}

func (m *MemberScoreWriter) SkipInDryRun() bool { return true }

func (m *MemberScorePersistor) Execute() error {
	tmp := filepath.Join(os.TempDir(), "badger", m.State.RepositoryPath)
	bDb := db.NewDb(tmp)
	defer bDb.Close() //nolint:errcheck
	for _, ms := range m.State.MemberScores {
		if err := bDb.StoreMemberScore(ms.FunctionID, ms); err != nil {
			slog.Error("unable to save member score",
				slog.String("function_id", ms.FunctionID),
				slog.Any("error", err),
			)
			return err
		}
	}
	slog.Info("member scores persisted", slog.Int("count", len(m.State.MemberScores)))
	return nil
}

func (m *MemberScorePersistor) SkipInDryRun() bool { return true }

func (c *IdentifyCluster) Execute() error {
	c.State.IdentifiedCluster = ast.IdentifyClusters(c.State.FunctionMetadata)
	slog.Info("identified clusters", slog.Int("count", len(c.State.IdentifiedCluster)))
	return nil
}

func (c *IdentifyClusterPersistor) Execute() error {
	tmp := filepath.Join(os.TempDir(), "badger", c.State.RepositoryPath)
	bDb := db.NewDb(tmp)
	defer bDb.Close() //nolint:errcheck

	for _, cl := range c.State.IdentifiedCluster {
		if err := bDb.StoreCluster(db.TierIdentified, cl.ShapeHash, cl); err != nil {
			slog.Error("unable to save identified cluster",
				slog.String("shape_hash", cl.ShapeHash),
				slog.Any("error", err),
			)
			return err
		}
	}
	slog.Info("identified clusters persisted", slog.Int("count", len(c.State.IdentifiedCluster)))
	return nil
}

func (c *IdentifyClusterPersistor) SkipInDryRun() bool { return true }

func (w *IdentifyClusterWriter) Execute() error {
	tmp := filepath.Join(os.TempDir(), "identifiedCluster", uuid.NewString(), filepath.Base(w.State.RepositoryPath), "cluster.json")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, cl := range w.State.IdentifiedCluster {
		if err := enc.Encode(cl); err != nil {
			slog.Error("unable to write identified cluster", slog.String("path", tmp))
			return err
		}
	}
	slog.Info("identified clusters written", slog.String("path", tmp))
	return nil
}

func (w *IdentifyClusterWriter) SkipInDryRun() bool { return true }

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
			if c, ok := cmd.(Skippable); ok && c.SkipInDryRun() {
				slog.Info("skipping (dry-run)")
				continue
			}
		}
		if err := cmd.Execute(); err != nil {
			slog.Error("stage halted...", slog.Any("error", err))
			return err
		}
	}
	return nil
}

func getCommands(s *State) []Command {
	return []Command{
		&DbCleaner{State: s},
		&FileMetadata{State: s},
		&FunctionMetadata{State: s},
		&FunctionMetadataWriter{State: s},
		&IdentifyCluster{State: s},
		&IdentifyClusterPersistor{State: s},
		&IdentifyClusterWriter{State: s},
		&IndexCommand{State: s},
		&IndexMetadataWriter{State: s},
		&IndexPersistor{State: s},
		// TODO membership scoring is brittle at the moment.
		// &MemberScorer{State: s},
		// &MemberScoreWriter{State: s},
		// &MemberScorePersistor{State: s},
		&BeatsLabelWriter{State: s},
	}
}

//nolint:unused
func (b *Beats) query() error {

	// TODO not yet...
	return nil
}
