package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/somak2kai/beats/pkg/ast"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ── JSON payload types ────────────────────────────────────────────────────────
//
// beats export dumps the full clustered + orphan state of a repo as one JSON
// blob. It's the read path used by ground-truth harvesting: a caller (jq, a
// script, another tool) can slice clusters by score, list orphans, or walk
// members without needing to know cluster hashes up front.
//
// Field names deliberately mirror the underlying ds.Cluster / ClusterStats /
// ClusterCandidate structs — no invented aliases. In particular:
//   - mean_score  ← Stats.MeanScore (arithmetic mean of intra-cluster pairwise arith scores)
//   - arith_score ← ClusterCandidate.ArithScore (orphan→cluster arithmetic mean)
// There is no geo_score anywhere in the beats codebase, and no separate
// avg_pairwise_score — mean_score is that value.

type ExportCluster struct {
	ShapeHash      string         `json:"shape_hash"`
	Tier           string         `json:"tier"`
	Size           int            `json:"size"`
	MeanScore      float64        `json:"mean_score"`
	StdScore       float64        `json:"std_score"`
	Coherence      float64        `json:"coherence"`
	CallCoherence  float64        `json:"call_coherence"`
	CompositeScore float64        `json:"composite_score"`
	CommonShape    string         `json:"common_shape"`
	Members        []MemberDetail `json:"members"`
}

type ExportOrphanCandidate struct {
	ClusterHash string  `json:"cluster_hash"`
	ArithScore  float64 `json:"arith_score"`
	SeqScore    float64 `json:"seq_score"`
	ImpScore    float64 `json:"imp_score"`
	CallScore   float64 `json:"call_score"`
	CycloDelta  float64 `json:"cyclo_delta"`
	Idiom       string  `json:"idiom,omitempty"`
}

type ExportOrphan struct {
	Func       string                  `json:"func"`
	Package    string                  `json:"package"`
	File       string                  `json:"file"`
	Line       int                     `json:"line"`
	Tokens     string                  `json:"tokens"`
	Imports    []string                `json:"imports"`
	Calls      []string                `json:"calls"`
	Body       string                  `json:"body"`
	Candidates []ExportOrphanCandidate `json:"candidates"`
}

type ExportPayload struct {
	Repo             string          `json:"repo"`
	Commit           string          `json:"commit,omitempty"`
	CorpusSize       int             `json:"corpus_size"`
	UnclusteredCount int             `json:"unclustered_count"`
	Clusters         []ExportCluster `json:"clusters"`
	Orphans          []ExportOrphan  `json:"orphans"`
}

// ── entry point ───────────────────────────────────────────────────────────────

func runExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	repo := fs.String("repo", "", "path to repository")
	format := fs.String("format", "json", "output format: json (only)")
	minScore := fs.Float64("min-score", 0, "only include clusters with mean_score >= this value")
	_ = fs.Parse(args)

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats export: --repo is required")
		os.Exit(1)
	}
	if *format != "json" {
		fmt.Fprintln(os.Stderr, "beats export: only --format json is supported")
		os.Exit(1)
	}

	bDb := openQueryDB(*repo)
	defer bDb.Close() //nolint:errcheck

	clusters, _ := queryScanClusters(bDb)

	goOrphans, _ := bDb.LoadOrphanedFunctions(ds.Language_GOLANG)
	javaOrphans, _ := bDb.LoadOrphanedFunctions(ds.Language_JAVA)
	orphans := append(goOrphans, javaOrphans...)

	payload := buildExportPayload(*repo, clusters, orphans, *minScore)
	payload.Commit = readGitCommit(*repo)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintln(os.Stderr, "beats export:", err)
		os.Exit(1)
	}
}

// ── payload assembly (pure, testable) ─────────────────────────────────────────

// buildExportPayload materialises the JSON export from an in-memory slice of
// clusters and orphans. Kept side-effect-free so tests don't need BadgerDB.
//
// minScore filters clusters by Stats.MeanScore (inclusive lower bound). It
// does NOT affect corpus_size — corpus_size is the total function count
// across ALL clusters (including primitives if present) plus orphans, so the
// number stays stable regardless of what the caller filters on the read side.
func buildExportPayload(repo string, clusters []ds.Cluster, orphans []ds.OrphanedFunction, minScore float64) ExportPayload {
	corpus := 0
	exportClusters := make([]ExportCluster, 0, len(clusters))
	for _, cl := range clusters {
		corpus += cl.Size
		if cl.IsPrimitive {
			continue
		}
		if cl.Stats.MeanScore < minScore {
			continue
		}
		exportClusters = append(exportClusters, toExportCluster(cl))
	}
	corpus += len(orphans)

	exportOrphans := make([]ExportOrphan, 0, len(orphans))
	for _, o := range orphans {
		exportOrphans = append(exportOrphans, toExportOrphan(o))
	}

	return ExportPayload{
		Repo:             repo,
		CorpusSize:       corpus,
		UnclusteredCount: len(orphans),
		Clusters:         exportClusters,
		Orphans:          exportOrphans,
	}
}

// toExportCluster reuses buildClusterResult (from query.go) so member
// serialisation is identical to `beats query cluster shape`.
func toExportCluster(cl ds.Cluster) ExportCluster {
	cr := buildClusterResult(&cl)
	return ExportCluster{
		ShapeHash:      cl.ShapeHash,
		Tier:           cl.Tier,
		Size:           cl.Size,
		MeanScore:      cl.Stats.MeanScore,
		StdScore:       cl.Stats.StdScore,
		Coherence:      cl.Coherence,
		CallCoherence:  cl.CallCoherence,
		CompositeScore: cl.CompositeScore,
		CommonShape:    cr.CommonShape,
		Members:        cr.Members,
	}
}

func toExportOrphan(o ds.OrphanedFunction) ExportOrphan {
	imports := make([]string, len(o.Meta.DirectImports))
	copy(imports, o.Meta.DirectImports)
	sort.Strings(imports)

	calls := make([]string, len(o.Meta.CallTargets))
	copy(calls, o.Meta.CallTargets)
	sort.Strings(calls)

	cands := make([]ExportOrphanCandidate, 0, len(o.Candidates))
	for _, c := range o.Candidates {
		cands = append(cands, ExportOrphanCandidate{
			ClusterHash: c.ShapeHash,
			ArithScore:  c.ArithScore,
			SeqScore:    c.SeqScore,
			ImpScore:    c.ImpScore,
			CallScore:   c.CallScore,
			CycloDelta:  c.CycloDelta,
			Idiom:       c.Idiom,
		})
	}
	return ExportOrphan{
		Func:       o.Meta.Name,
		Package:    o.Meta.Package,
		File:       o.Meta.FileMeta.Path,
		Line:       o.Meta.Start_line,
		Tokens:     ast.SeqString(o.Meta.TokenSeq),
		Imports:    imports,
		Calls:      calls,
		Body:       o.Meta.Body,
		Candidates: cands,
	}
}

// readGitCommit returns the HEAD SHA of repo, or "" if git is unavailable or
// the path isn't a git checkout. Never fails the export — commit metadata is
// nice-to-have, not required.
func readGitCommit(repo string) string {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
