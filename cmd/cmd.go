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
	"slices"
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

type execute func(s *State) error
type stage struct {
	exec execute
	// any database interaction gets skipped in dry run mode.
	isSkipInDryRun bool
}

type Beats struct {
	IsDryRun bool
}
type State struct {
	PkgToFileMetadata ds.PkgToFileMeta
	FunctionMetadata  []*ds.FunctionMeta
	IdentifiedCluster []*ds.Cluster
	OrphanMetas       []*ds.FunctionMeta     // functions that did not join any cluster
	OrphanedFunctions []*ds.OrphanedFunction // orphans with Z-score candidates, after analysis
	RepositoryPath    string
	Index             *ds.Index
	Java              *StateJava
}

type StateJava struct {
	FunctionMetadata  []*ds.FunctionMeta
	IdentifiedCluster []*ds.Cluster
	OrphanMetas       []*ds.FunctionMeta     // functions that did not join any cluster
	OrphanedFunctions []*ds.OrphanedFunction // orphans with Z-score candidates, after analysis
	Index             *ds.Index
}

func getPipeline() []stage {
	return []stage{
		{exec: resetDb, isSkipInDryRun: true},               // 1. wipes off existing data
		{exec: getFileMetadata},                             // 2. identifies files to scan
		{exec: getGolangFunctionMetadata},                   // 3. runs ast parser to get go function metadata
		{exec: getJavaFunctionMetadata},                     // 4. runs jbeats ast parser to get java function metadata
		{exec: perfomClustering},                            // 5. performs clustering on function metadata
		{exec: classifyClusters},                            // 6. runs classification on clusters identified into high, medium
		{exec: saveClusterInDb, isSkipInDryRun: true},       // 7. saves cluster information to badgerdb
		{exec: identifyOrphans},                             //8. runs analysis to identify functions which did not join clusters
		{exec: saveOrphansInDB, isSkipInDryRun: true},       // 9. saves orphans information to badger db
		{exec: populateFunctionIndex},                       // 10. gets and populates additional metadata for function metadata
		{exec: saveFunctionIndexInDb, isSkipInDryRun: true}, // 11. writes function index metadata to badger
		{exec: writeOutlierHtmlFile, isSkipInDryRun: true},  // 12. creates the .beats/outlier.md files for skills to read.
		{exec: writeBeatsHtmlFile, isSkipInDryRun: true},    //13. creates the .beats/report.html files
	}
}

func (b *Beats) run(repo string) error {

	s := &State{
		RepositoryPath:    repo,
		FunctionMetadata:  make([]*ds.FunctionMeta, 0),
		PkgToFileMetadata: make(ds.PkgToFileMeta),
		Java:              &StateJava{},
	}
	for _, cmd := range getPipeline() {
		if b.IsDryRun {
			if cmd.isSkipInDryRun {
				slog.Info("skipping (dry-run)")
				continue
			}
		}
		if err := cmd.exec(s); err != nil {
			slog.Error("stage halted...", slog.Any("error", err))
			return err
		}
	}
	return nil
}

// removes the BadgerDB directory for the repository, if exists, before the
// pipeline runs.
func resetDb(s *State) error {
	dbPath, err := beatsDBPath(s.RepositoryPath)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dbPath); err != nil {
		slog.Error("failed to clear badger db", slog.String("path", dbPath), slog.Any("error", err))
		return err
	}
	slog.Info("cleared existing db", slog.String("path", dbPath))
	return nil
}

// gets the list of files to scan further in repository.
func getFileMetadata(s *State) error {

	files, err := util.GetFileMetadata(s.RepositoryPath)
	if err != nil {
		slog.Error("failed to get file metadata", slog.Any("error", err))
		return err
	}
	s.PkgToFileMetadata = files
	return nil
}

// gets ast token sequences for golang functions.
func getGolangFunctionMetadata(s *State) error {

	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	var mu sync.Mutex
	for _, val := range s.PkgToFileMetadata {
		val := val
		g.Go(func() error {
			var localFncM []*ds.FunctionMeta
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
			s.FunctionMetadata = append(s.FunctionMetadata, localFncM...)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		slog.Error("unable to capture file metadata record", slog.Any("error", err))
		return err
	}
	slog.Info("parsed golang functions", slog.Int("total", len(s.FunctionMetadata)))
	return nil
}

// gets and populates additional metadata for function metadata
func populateFunctionIndex(s *State) error {
	s.Index = ds.PopulateIndex(s.FunctionMetadata)
	s.Java.Index = ds.PopulateIndex(s.Java.FunctionMetadata)
	return nil
}

// saves function index created in badger db.
func saveFunctionIndexInDb(s *State) error {

	tmp, err := beatsDBPath(s.RepositoryPath)
	if err != nil {
		return err
	}
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck

	store := func(index *ds.Index, lang ds.Language) error {
		for k, v := range index.Postings {
			if err := bDb.StorePostings(k, lang, v); err != nil {
				slog.Error("unable to save inverted index", slog.Any("error", err))
				return err
			}
		}

		for k, v := range index.DocFreq {
			if err := bDb.StoreDocFreq(k, lang, v); err != nil {
				slog.Error("unable to save document frequency", slog.Any("error", err))
				return err
			}
		}

		for k, v := range index.FuncMeta {
			if err := bDb.StoreFunctionMeta(k, lang, v); err != nil {
				slog.Error("unable to save function metadata", slog.Any("error", err))
				return err
			}
		}
		return nil
	}
	if err := store(s.Index, ds.Language_GOLANG); err != nil {
		return err
	}
	if err := store(s.Java.Index, ds.Language_JAVA); err != nil {
		return err
	}
	return nil
}

// writes out .beats/report.html file.
func writeBeatsHtmlFile(s *State) error {
	goHasData := len(s.IdentifiedCluster) > 0 && len(s.OrphanMetas) > 0
	javaHasData := len(s.Java.IdentifiedCluster) > 0 && len(s.Java.OrphanMetas) > 0
	if !goHasData && !javaHasData {
		return nil
	}
	return runAnalyze(s.RepositoryPath)
}

// based on function metadata created so far , performs clustering.
func perfomClustering(s *State) error {

	cluster := func(fMeta []*ds.FunctionMeta) ([]*ds.Cluster, []*ds.FunctionMeta, error) {
		clusters, orphans, err := ast.IdentifyClustersWithConfig(fMeta, ast.DefaultClusterConfig())
		if err != nil {
			return nil, nil, err
		}
		return clusters, orphans, nil
	}

	cl, o, err := cluster(s.FunctionMetadata)
	if err != nil {
		return err
	}
	s.IdentifiedCluster = cl
	s.OrphanMetas = o

	clj, oj, err := cluster(s.Java.FunctionMetadata)
	if err != nil {
		return err
	}
	s.Java.IdentifiedCluster = clj
	s.Java.OrphanMetas = oj

	slog.Info("identified golang clusters",
		slog.Int("count", len(s.IdentifiedCluster)),
		slog.Int("orphans", len(s.OrphanMetas)),
	)
	slog.Info("identified java clusters",
		slog.Int("count", len(s.Java.IdentifiedCluster)),
		slog.Int("orphans", len(s.Java.OrphanMetas)),
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

// classifies clusters as high medium low for clsuters identified.
func classifyClusters(s *State) error {

	classify := func(cluster []*ds.Cluster, lang ds.Language) {

		var high, medium, low int
		for _, cl := range cluster {
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
		if lang == ds.Language_GOLANG {
			slog.Info("golang clusters classified",
				slog.Int("high", high),
				slog.Int("medium", medium),
				slog.Int("low", low),
			)
		} else {
			slog.Info("java clusters classified",
				slog.Int("high", high),
				slog.Int("medium", medium),
				slog.Int("low", low),
			)
		}
	}
	classify(s.IdentifiedCluster, ds.Language_GOLANG)
	classify(s.Java.IdentifiedCluster, ds.Language_JAVA)

	return nil
}

// saves cluster information to badgerdb.
func saveClusterInDb(s *State) error {

	tmp, err := beatsDBPath(s.RepositoryPath)
	if err != nil {
		return err
	}
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck

	persist := func(clusters []*ds.Cluster, lang ds.Language) error {

		for idx, cl := range clusters {
			if cl.IsPrimitive {
				continue
			}
			if err := bDb.StoreClusterByIndex(db.TierIdentified, idx, string(lang), cl); err != nil {
				slog.Error("unable to save identified cluster",
					slog.Int("index", idx),
					slog.String("shape_hash", cl.ShapeHash),
					slog.Any("error", err),
				)
				return err
			}
		}
		return nil
	}
	if err := persist(s.IdentifiedCluster, ds.Language_GOLANG); err != nil {
		return err
	}
	if err := persist(s.Java.IdentifiedCluster, ds.Language_JAVA); err != nil {
		return err
	}
	return nil
}

// identifies orphans, which are potential outliers to clusters identified.
func identifyOrphans(s *State) error {

	analyze := func(cltrs []*ds.Cluster, orphans []*ds.FunctionMeta) ([]*ds.OrphanedFunction, error) {

		if len(cltrs) == 0 || len(orphans) == 0 {
			return nil, nil
		}
		result, err := ast.IdentifyOrphans(orphans, cltrs)
		if err != nil {
			return nil, err
		}
		return result, nil
	}

	r, err := analyze(s.IdentifiedCluster, s.OrphanMetas)
	if err != nil {
		return err
	}
	s.OrphanedFunctions = r

	rj, err := analyze(s.Java.IdentifiedCluster, s.Java.OrphanMetas)
	if err != nil {
		return err
	}
	s.Java.OrphanedFunctions = rj

	return nil
}

// saves outlier information to db.
func saveOrphansInDB(s *State) error {

	tmp, err := beatsDBPath(s.RepositoryPath)
	if err != nil {
		return err
	}
	bDb := db.NewBadgerXDb(tmp)
	defer bDb.Close() //nolint:errcheck
	store := func(orphans []*ds.OrphanedFunction, lang ds.Language) error {
		if len(orphans) == 0 {
			return nil
		}
		if err := bDb.StoreOrphanedFunctions(orphans, lang); err != nil {
			slog.Error("unable to persist orphaned functions", slog.String("language", string(lang)), slog.Any("error", err))
			return err
		}
		return nil
	}
	if err := store(s.OrphanedFunctions, ds.Language_GOLANG); err != nil {
		return err
	}
	if err := store(s.Java.OrphanedFunctions, ds.Language_JAVA); err != nil {
		return err
	}

	return nil
}

// writes out .beats/outlier.md for skills to use further.
func writeOutlierHtmlFile(s *State) error {

	allOrphans := slices.Concat(s.OrphanedFunctions, s.Java.OrphanedFunctions)
	if len(allOrphans) == 0 {
		slog.Info("no orphaned functions — skipping outlier.md")
		return nil
	}

	// Index non-primitive clusters by ShapeHash (both Go and Java).
	allClusters := slices.Concat(s.IdentifiedCluster, s.Java.IdentifiedCluster)
	clusterByHash := make(map[string]*ds.Cluster, len(allClusters))
	for _, cl := range allClusters {
		if !cl.IsPrimitive && cl.ShapeHash != "" {
			clusterByHash[cl.ShapeHash] = cl
		}
	}

	// Build the same OutlierResult slice the query command produces.
	results := buildOutlierResults(allOrphans, clusterByHash)
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
	fmt.Fprintf(&sb, "repo: %s\n", s.RepositoryPath)
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

		cr := buildClusterResult(cl)
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

	beatsDir := filepath.Join(s.RepositoryPath, ".beats")
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

// gets ast token sequences for java functions.
func getJavaFunctionMetadata(s *State) error {

	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	var mu sync.Mutex
	s.Java = &StateJava{FunctionMetadata: make([]*ds.FunctionMeta, 0)}
	for _, val := range s.PkgToFileMetadata {
		val := val
		g.Go(func() error {
			var localFncM []*ds.FunctionMeta
			for _, m := range val {
				if m.Lang != ds.Language_JAVA {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/Users/admin/ws/java/jbeats/target/jbeats",
					fmt.Sprintf("--inp=%s", m.Path),
					fmt.Sprintf("--repo=%s", s.RepositoryPath),
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
			s.Java.FunctionMetadata = append(s.Java.FunctionMetadata, localFncM...)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		slog.Error("unable to capture file metadata record", slog.Any("error", err))
		return err
	}
	slog.Info("total number of java functions", slog.Int("count", len(s.Java.FunctionMetadata)))
	return nil
}

// beatsDBPath returns the stable BadgerDB path for a given repo.
func beatsDBPath(repo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir() // fallback, should never happen
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo path %q: %w", repo, err)
	}
	if runtime.GOOS == "windows" {
		abs = strings.ReplaceAll(abs, ":", "") // ':' is not allowed in a Windows folder name
	}
	name := strings.ReplaceAll(abs, string(filepath.Separator), "_")
	if strings.Trim(name, "_") == "" {
		return "", fmt.Errorf("refusing to use %q as a repo path", repo)
	}
	return filepath.Join(home, ".beats", "badger", name), nil
}

//nolint:unused
func (b *Beats) query() error {

	// TODO not yet...
	return nil
}
