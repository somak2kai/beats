package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ── entry point ───────────────────────────────────────────────────────────────

func runQuery(args []string) {
	if len(args) == 0 {
		printQueryUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "outlier":
		runQueryOutlier(args[1:])
	case "cluster":
		// expects: cluster shape <hash> --repo=<path> [--format text|json]
		if len(args) < 3 || args[1] != "shape" {
			fmt.Fprintln(os.Stderr, "usage: beats query cluster shape <hash> --repo <path> [--format text|json]")
			os.Exit(1)
		}
		runQueryCluster(args[2], args[3:])
	default:
		fmt.Fprintf(os.Stderr, "beats query: unknown sub-command %q\n", args[0])
		printQueryUsage()
		os.Exit(1)
	}
}

func printQueryUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  beats query outlier              --repo <path> [--format text|json]")
	fmt.Fprintln(os.Stderr, "  beats query cluster shape <hash> --repo <path> [--format text|json]")
}

// ── result types (shared by text and JSON renderers) ─────────────────────────

type DeltaStrings struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

type CandidateEntry struct {
	ClusterID   string  `json:"cluster_id"`
	Score       float64 `json:"score"`
	Tier        string  `json:"tier"`
	Size        int     `json:"size"`
	CommonShape string  `json:"common_shape"`
	Idiom       string  `json:"idiom,omitempty"`
}

type OutlierResult struct {
	Func        string           `json:"func"`
	Package     string           `json:"package"`
	File        string           `json:"file"`
	Line        int              `json:"line"`
	Body        string           `json:"body"`
	TokenDelta  DeltaStrings     `json:"token_delta"`
	ImportDelta DeltaStrings     `json:"import_delta"`
	CallDelta   DeltaStrings     `json:"call_delta"`
	CycloDelta  float64          `json:"cyclo_delta"`
	Candidates  []CandidateEntry `json:"candidates"`
}

type MemberDetail struct {
	Func    string   `json:"func"`
	Package string   `json:"package"`
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Tokens  string   `json:"tokens"`
	Imports []string `json:"imports"`
	Calls   []string `json:"calls"`
	Body    string   `json:"body"`
}

type ClusterResult struct {
	ShapeHash   string         `json:"shape_hash"`
	Tier        string         `json:"tier"`
	Size        int            `json:"size"`
	CommonShape string         `json:"common_shape"`
	Members     []MemberDetail `json:"members"`
}

// ── outlier query ─────────────────────────────────────────────────────────────

func runQueryOutlier(args []string) {
	fs := flag.NewFlagSet("query outlier", flag.ExitOnError)
	repo := fs.String("repo", "", "path to repository")
	format := fs.String("format", "text", "output format: text|json")
	_ = fs.Parse(args)

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats query outlier: --repo is required")
		os.Exit(1)
	}

	bDb := openQueryDB(*repo)
	defer bDb.Close() //nolint:errcheck

	goOrphans, err := bDb.LoadOrphanedFunctions(ds.Language_GOLANG)
	javaOrphans, _ := bDb.LoadOrphanedFunctions(ds.Language_JAVA)
	orphans := append(goOrphans, javaOrphans...)
	if err != nil || len(orphans) == 0 {
		fmt.Fprintln(os.Stderr, "no orphaned functions found — run beats init first")
		return
	}

	clusters, _ := queryScanClusters(bDb)
	clusterByHash := make(map[string]ds.Cluster, len(clusters))
	for _, cl := range clusters {
		clusterByHash[cl.ShapeHash] = cl
	}

	results := buildOutlierResults(orphans, clusterByHash)

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	default:
		printOutliersText(results)
	}
}

func buildOutlierResults(orphans []ds.OrphanedFunction, clusterByHash map[string]ds.Cluster) []OutlierResult {
	results := make([]OutlierResult, 0, len(orphans))
	for _, o := range orphans {
		if len(o.Candidates) == 0 {
			continue
		}
		top := o.Candidates[0]
		cl, ok := clusterByHash[top.ShapeHash]

		var tokAdded, tokRemoved, impAdded, impRemoved, callAdded, callRemoved []string
		if ok {
			topImports := make([]string, len(cl.Profile.TopImports))
			for i, imp := range cl.Profile.TopImports {
				topImports[i] = shortImport(imp)
			}
			orphanImports := make([]string, len(o.Meta.DirectImports))
			for i, imp := range o.Meta.DirectImports {
				orphanImports[i] = shortImport(imp)
			}
			tokAdded, tokRemoved = tokenSetDiff(o.Meta.TokenSeq, cl.CommonSeq)
			impAdded, impRemoved = stringSetDiff(orphanImports, topImports)
			callAdded, callRemoved = stringSetDiff(o.Meta.CallTargets, cl.Profile.TopCallTargets)
		}

		candidates := make([]CandidateEntry, 0, len(o.Candidates))
		for _, c := range o.Candidates {
			entry := CandidateEntry{
				ClusterID: c.ShapeHash,
				Score:     c.ArithScore,
				Idiom:     c.Idiom,
			}
			if cl, ok := clusterByHash[c.ShapeHash]; ok {
				entry.Tier = cl.Tier
				entry.Size = cl.Size
				entry.CommonShape = ast.SeqString(cl.CommonSeq)
			}
			candidates = append(candidates, entry)
		}

		results = append(results, OutlierResult{
			Func:        o.Meta.Name,
			Package:     o.Meta.Package,
			File:        o.Meta.FileMeta.Path,
			Line:        o.Meta.Start_line,
			Body:        o.Meta.Body,
			TokenDelta:  DeltaStrings{Added: tokAdded, Removed: tokRemoved},
			ImportDelta: DeltaStrings{Added: impAdded, Removed: impRemoved},
			CallDelta:   DeltaStrings{Added: callAdded, Removed: callRemoved},
			CycloDelta:  top.CycloDelta,
			Candidates:  candidates,
		})
	}
	return results
}

func printOutliersText(results []OutlierResult) {
	sep := strings.Repeat("─", 72)
	for i, r := range results {
		if i > 0 {
			fmt.Println(sep)
		}
		fmt.Printf("func:    %s\n", r.Func)
		fmt.Printf("package: %s\n", r.Package)
		fmt.Printf("file:    %s\n", r.File)
		fmt.Printf("line:    %d\n", r.Line)
		if r.Body != "" {
			fmt.Println("body:")
			for _, line := range strings.Split(strings.TrimRight(r.Body, "\n"), "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
		fmt.Println("deviation:")
		fmt.Printf("  token delta:   %s\n", formatDeltaText(r.TokenDelta))
		fmt.Printf("  import delta:  %s\n", formatDeltaText(r.ImportDelta))
		fmt.Printf("  call delta:    %s\n", formatDeltaText(r.CallDelta))
		fmt.Printf("  cyclo delta:   %s\n", signedDelta(r.CycloDelta))
		fmt.Println("candidates:")
		for j, c := range r.Candidates {
			idiom := ""
			if c.Idiom != "" {
				idiom = "  " + c.Idiom
			}
			short := c.ClusterID
			if len(short) > 6 {
				short = short[:6]
			}
			fmt.Printf("  %d. %s  score=%.3f%s\n", j+1, short, c.Score, idiom)
		}
	}
}

func formatDeltaText(d DeltaStrings) string {
	var parts []string
	for _, a := range d.Added {
		parts = append(parts, "+"+a)
	}
	for _, r := range d.Removed {
		parts = append(parts, "-"+r)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "  ")
}

// ── cluster query ─────────────────────────────────────────────────────────────

func runQueryCluster(hashPrefix string, args []string) {
	fs := flag.NewFlagSet("query cluster", flag.ExitOnError)
	repo := fs.String("repo", "", "path to repository")
	format := fs.String("format", "text", "output format: text|json")
	_ = fs.Parse(args)

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats query cluster: --repo is required")
		os.Exit(1)
	}

	bDb := openQueryDB(*repo)
	defer bDb.Close() //nolint:errcheck

	clusters, _ := queryScanClusters(bDb)

	var found *ds.Cluster
	for i := range clusters {
		if clusters[i].ShapeHash == hashPrefix {
			found = &clusters[i]
			break
		}
	}
	if found == nil {
		fmt.Fprintf(os.Stderr, "no cluster found with shape hash %q\n", hashPrefix)
		os.Exit(1)
	}

	result := buildClusterResult(found)

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
	default:
		printClusterText(result)
	}
}

func buildClusterResult(cl *ds.Cluster) ClusterResult {
	members := make([]MemberDetail, 0, len(cl.Members))
	for _, m := range cl.Members {
		imports := make([]string, len(m.DirectImports))
		copy(imports, m.DirectImports)
		sort.Strings(imports)

		calls := make([]string, len(m.CallTargets))
		copy(calls, m.CallTargets)
		sort.Strings(calls)

		members = append(members, MemberDetail{
			Func:    m.Name,
			Package: m.Package,
			File:    m.FileMeta.Path,
			Line:    m.Start_line,
			Tokens:  ast.SeqString(m.TokenSeq),
			Imports: imports,
			Calls:   calls,
			Body:    m.Body,
		})
	}
	return ClusterResult{
		ShapeHash:   cl.ShapeHash,
		Tier:        cl.Tier,
		Size:        cl.Size,
		CommonShape: ast.SeqString(cl.CommonSeq),
		Members:     members,
	}
}

func printClusterText(r ClusterResult) {
	fmt.Printf("cluster: %s\n", r.ShapeHash)
	fmt.Printf("tier:    %s\n", r.Tier)
	fmt.Printf("size:    %d\n", r.Size)
	fmt.Printf("shape:   %s\n", r.CommonShape)
	sep := strings.Repeat("─", 72)
	for i, m := range r.Members {
		fmt.Println()
		fmt.Printf("member %d/%d: %s\n", i+1, len(r.Members), m.Func)
		fmt.Printf("  package: %s\n", m.Package)
		fmt.Printf("  file:    %s\n", m.File)
		fmt.Printf("  line:    %d\n", m.Line)
		fmt.Printf("  tokens:  %s\n", m.Tokens)
		if len(m.Imports) > 0 {
			fmt.Printf("  imports: %s\n", strings.Join(m.Imports, ", "))
		}
		if len(m.Calls) > 0 {
			fmt.Printf("  calls:   %s\n", strings.Join(m.Calls, ", "))
		}
		if m.Body != "" {
			fmt.Println("  body:")
			for _, line := range strings.Split(strings.TrimRight(m.Body, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		if i < len(r.Members)-1 {
			fmt.Println(sep)
		}
	}
}

// ── shared helpers ────────────────────────────────────────────────────────────

func openQueryDB(repo string) *db.BadgerXDb {
	return db.NewBadgerXDb(beatsDBPath(repo))
}

// queryScanClusters tries TierIdentified first, falls back to TierCollapsed.
func queryScanClusters(bDb *db.BadgerXDb) ([]ds.Cluster, string) {
	clusters, err := bDb.ScanClusters(db.TierIdentified)
	if err == nil && len(clusters) > 0 {
		return clusters, db.TierIdentified
	}
	clusters, _ = bDb.ScanClusters(db.TierCollapsed)
	return clusters, db.TierCollapsed
}
