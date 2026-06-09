package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
)

// default
var Version = "dev"

const queryPageSize = 10

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "query":
		runQuery(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "analyze":
		runAnalyzeCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("beats %s\n", Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: beats <command> [flags]")
	fmt.Fprintln(os.Stderr, "  beats init    --repo <path> [--dry-run]")
	fmt.Fprintln(os.Stderr, "  beats query   cluster <page> --repo <path> [--format compact|text]")
	fmt.Fprintln(os.Stderr, "  beats update  cluster <idx> --repo <path> --idiom <str> --verdict <str> --canonical <pkg/Fn> --action <str> --confidence <high|medium|low> --questions <q1|q2|...>")
	fmt.Fprintln(os.Stderr, "  beats update  page --repo <path> --file <tsv>  (batch: one DB open for the whole page)")
	fmt.Fprintln(os.Stderr, "  beats analyze --repo <path>")
	fmt.Fprintln(os.Stderr, "  beats version")
}

// runAnalyzeCmd regenerates the HTML report from whatever is currently in the
// database — including any LLM enrichment written by beats update cluster.
// It is intentionally separate from beats init so the report can be refreshed
// after enrichment without re-indexing the whole repository.
func runAnalyzeCmd(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	repo := fs.String("repo", "", "Path to the repository (same value passed to beats init)")
	_ = fs.Parse(args)

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats analyze: --repo is required")
		os.Exit(1)
	}

	if err := runAnalyze(*repo); err != nil {
		slog.Error("beats analyze failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	prj := fs.String("repo", "", "Path to the repository to index")
	isDryRun := fs.Bool("dry-run", false, "Execute beats in dry run mode, no persistence to file or db")
	_ = fs.Parse(args)

	if *prj == "" {
		fmt.Fprintln(os.Stderr, "beats init: --repo is required")
		os.Exit(1)
	}

	b := &Beats{IsDryRun: *isDryRun}
	if err := b.run(*prj); err != nil {
		slog.Error("unable to create beats index", slog.String("repo", *prj), slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("successfully created beats index and cluster", slog.String("repo", *prj))
}

// runQuery handles: beats query cluster <page> --repo <path> [--format compact|text]
func runQuery(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: beats query cluster <page> --repo <path> [--format compact|text]")
		fmt.Fprintln(os.Stderr, "  page is 0-based; each page shows up to 10 clusters")
		os.Exit(1)
	}

	if args[0] != "cluster" {
		fmt.Fprintf(os.Stderr, "unknown query target %q — only 'cluster' is supported\n", args[0])
		os.Exit(1)
	}

	page, err := strconv.Atoi(args[1])
	if err != nil || page < 0 {
		fmt.Fprintln(os.Stderr, "beats query cluster: <page> must be a non-negative integer")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("query", flag.ExitOnError)
	repo := fs.String("repo", "", "Path to the repository (same value passed to beats init)")
	format := fs.String("format", "text", "Output format: text or compact")
	_ = fs.Parse(args[2:])
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats query cluster: --repo is required")
		os.Exit(1)
	}

	dbPath := filepath.Join(os.TempDir(), "badger", *repo)
	bDb := db.NewBadgerXDb(dbPath)
	defer bDb.Close() //nolint:errcheck

	total, err := bDb.LoadClusterCount(db.TierIdentified)
	if err != nil || total == 0 {
		total, err = bDb.LoadClusterCount(db.TierCollapsed)
		if err != nil || total == 0 {
			fmt.Fprintln(os.Stderr, "no beats index found — run 'beats init --repo <path>' first")
			os.Exit(1)
		}
	}

	start := page * queryPageSize
	if start >= total {
		fmt.Fprintf(os.Stderr, "page %d is out of range — %d clusters total (%d pages)\n",
			page, total, (total+queryPageSize-1)/queryPageSize)
		os.Exit(1)
	}
	end := start + queryPageSize
	if end > total {
		end = total
	}

	for i := start; i < end; i++ {
		cl, err := bDb.LoadClusterByIndex(db.TierIdentified, i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading cluster %d: %v\n", i, err)
			continue
		}
		commonSeq := ast.CommonSubsequence(cl.Members)
		shape := ast.SeqString(commonSeq)
		if shape == "" {
			shape = "none"
		}
		score := clusterAvgScore(cl)

		if *format == "compact" {
			printClusterCompact(i, cl, shape, score)
		} else {
			printClusterText(i, cl, shape, score)
		}
	}

	totalPages := (total + queryPageSize - 1) / queryPageSize
	if *format != "compact" {
		if page+1 < totalPages {
			fmt.Printf("→  next page: beats query cluster %d --repo %s\n", page+1, *repo)
		} else {
			fmt.Printf("(end of index — %d clusters total)\n", total)
		}
	} else {
		// compact: emit a machine-readable trailer the skill can detect
		fmt.Printf("TOTAL:%d PAGES:%d PAGE:%d\n", total, totalPages, page)
	}
}

// printClusterCompact emits the minimum fields an LLM needs to analyse a cluster.
// Format (3+ lines per cluster, terminated by ---):
//
//	IDX:<n> SIZE:<n> SCORE:<f> QUAD:<hh|hl|lh|ll>
//	SHAPE:<token sequence>
//	M:<name>|<package>|<absolute_file_path>|<start_line>
//	---
//
// At most compactMaxMembers members are printed to keep token usage bounded;
// the SIZE field always reflects the true cluster size.
const compactMaxMembers = 3

func printClusterCompact(idx int, cl ds.Cluster, shape string, score float64) {
	hiImp := cl.Coherence >= 0.60
	hiCall := cl.CallCoherence >= 0.60
	var quad string
	switch {
	case hiImp && hiCall:
		quad = "HH"
	case hiImp && !hiCall:
		quad = "HL"
	case !hiImp && hiCall:
		quad = "LH"
	default:
		quad = "LL"
	}
	fmt.Printf("IDX:%d SIZE:%d SCORE:%.3f QUAD:%s\n", idx, cl.Size, score, quad)
	fmt.Printf("SHAPE:%s\n", shape)
	shown := 0
	for _, m := range cl.Members {
		if shown >= compactMaxMembers {
			break
		}
		fmt.Printf("M:%s|%s|%s|%d\n", m.Name, m.Package, m.FileMeta.Path, m.Start_line)
		shown++
	}
	fmt.Println("---")
}

// printClusterText emits a human-readable cluster block.
func printClusterText(idx int, cl ds.Cluster, shape string, score float64) {
	pkgSet := make(map[string]struct{})
	for _, m := range cl.Members {
		pkgSet[m.Package] = struct{}{}
	}
	pkgs := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}

	fmt.Printf("─── cluster %d ─────────────────────────────────────────────\n", idx)
	fmt.Printf("  hash     : %s\n", cl.ShapeHash)
	fmt.Printf("  size     : %d\n", cl.Size)
	fmt.Printf("  score    : %.3f\n", score)
	fmt.Printf("  import   : %.2f   call: %.2f\n", cl.Coherence, cl.CallCoherence)
	fmt.Printf("  shape    : %s\n", shape)
	fmt.Printf("  packages : %s\n", strings.Join(pkgs, ", "))
	fmt.Printf("  members  :\n")
	for _, m := range cl.Members {
		fmt.Printf("      %s/%s  (%s:%d)\n", m.Package, m.Name, m.FileMeta.Path, m.Start_line)
	}
	fmt.Println()
}

// runUpdate dispatches beats update cluster | page.
func runUpdate(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: beats update cluster <idx> --repo <path> [enrichment flags]")
		fmt.Fprintln(os.Stderr, "       beats update page --repo <path> --file <tsv>")
		os.Exit(1)
	}
	if args[0] == "page" {
		runUpdatePage(args[1:])
		return
	}
	if args[0] != "cluster" {
		fmt.Fprintf(os.Stderr, "unknown update target %q — only 'cluster' or 'page' is supported\n", args[0])
		os.Exit(1)
	}

	idx, err := strconv.Atoi(args[1])
	if err != nil || idx < 0 {
		fmt.Fprintln(os.Stderr, "beats update cluster: <idx> must be a non-negative integer")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("update", flag.ExitOnError)
	repo := fs.String("repo", "", "Path to the repository (same value passed to beats init)")
	idiom := fs.String("idiom", "", "3–6 word structural idiom name")
	verdict := fs.String("verdict", "", "One-sentence description of what the cluster represents")
	canonical := fs.String("canonical", "", "Canonical member in pkg/FuncName form")
	action := fs.String("action", "", "Suggested action or 'none'")
	confidence := fs.String("confidence", "", "Confidence level: high, medium, or low")
	questions := fs.String("questions", "", "Pipe-separated developer search questions, e.g. 'How are X built?|Where is Y wired?'")
	_ = fs.Parse(args[2:])

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "beats update cluster: --repo is required")
		os.Exit(1)
	}

	dbPath := filepath.Join(os.TempDir(), "badger", *repo)
	bDb := db.NewBadgerXDb(dbPath)
	defer bDb.Close() //nolint:errcheck

	cl, err := bDb.LoadClusterByIndex(db.TierIdentified, idx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "beats update cluster: cannot load cluster %d: %v\n", idx, err)
		os.Exit(1)
	}

	// apply only the fields that were supplied — leave existing values untouched
	if *idiom != "" {
		cl.SemanticIdiom = *idiom
	}
	if *verdict != "" {
		cl.Verdict = *verdict
	}
	if *canonical != "" {
		cl.CanonicalMember = *canonical
	}
	if *action != "" {
		cl.SuggestedAction = *action
	}
	if *confidence != "" {
		cl.Confidence = *confidence
	}
	if *questions != "" {
		qs := strings.Split(*questions, "|")
		filtered := qs[:0]
		for _, q := range qs {
			q = strings.TrimSpace(q)
			if q != "" {
				filtered = append(filtered, q)
			}
		}
		cl.SearchQuestions = filtered
	}

	if err := bDb.StoreClusterByIndex(db.TierIdentified, idx, cl); err != nil {
		fmt.Fprintf(os.Stderr, "beats update cluster: cannot write cluster %d: %v\n", idx, err)
		os.Exit(1)
	}

	fmt.Printf("cluster %d updated\n", idx)
}

// runUpdatePage handles: beats update page --repo <path> --file <tsv>
//
// TSV format (one cluster per line, tab-separated, no header):
//
//	IDX\tIDIOM\tVERDICT\tCANONICAL\tACTION\tCONFIDENCE\tQUESTIONS
//
// QUESTIONS is pipe-separated within the field.
// Opens BadgerDB exactly once for the whole batch — much faster than calling
// beats update cluster N times (avoids N process-starts and N DB open/close cycles).
func runUpdatePage(args []string) {
	fs := flag.NewFlagSet("update-page", flag.ExitOnError)
	repo := fs.String("repo", "", "Path to the repository (same value passed to beats init)")
	file := fs.String("file", "", "Path to the TSV enrichment file")
	_ = fs.Parse(args)

	if *repo == "" || *file == "" {
		fmt.Fprintln(os.Stderr, "beats update page: --repo and --file are required")
		os.Exit(1)
	}

	f, err := os.Open(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "beats update page: cannot open %s: %v\n", *file, err)
		os.Exit(1)
	}
	defer f.Close() //nolint:errcheck

	dbPath := filepath.Join(os.TempDir(), "badger", *repo)
	bDb := db.NewBadgerXDb(dbPath)
	defer bDb.Close() //nolint:errcheck

	updated, failed := 0, 0
	scanner := newTSVScanner(f)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 7)
		if len(fields) < 7 || fields[0] == "" {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			fmt.Fprintf(os.Stderr, "beats update page: invalid idx %q, skipping\n", fields[0])
			failed++
			continue
		}

		cl, err := bDb.LoadClusterByIndex(db.TierIdentified, idx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "beats update page: cannot load cluster %d: %v\n", idx, err)
			failed++
			continue
		}

		applyEnrichment(&cl, fields[1], fields[2], fields[3], fields[4], fields[5], fields[6])

		if err := bDb.StoreClusterByIndex(db.TierIdentified, idx, cl); err != nil {
			fmt.Fprintf(os.Stderr, "beats update page: cannot write cluster %d: %v\n", idx, err)
			failed++
			continue
		}
		updated++
	}

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d update(s) failed\n", failed)
	}
	fmt.Printf("%d cluster(s) updated\n", updated)
	if failed > 0 {
		os.Exit(1)
	}
}

// applyEnrichment writes non-empty enrichment fields onto a cluster in place.
func applyEnrichment(cl *ds.Cluster, idiom, verdict, canonical, action, confidence, questions string) {
	if idiom != "" {
		cl.SemanticIdiom = idiom
	}
	if verdict != "" {
		cl.Verdict = verdict
	}
	if canonical != "" {
		cl.CanonicalMember = canonical
	}
	if action != "" {
		cl.SuggestedAction = action
	}
	if confidence != "" {
		cl.Confidence = confidence
	}
	if questions != "" {
		qs := strings.Split(questions, "|")
		filtered := qs[:0]
		for _, q := range qs {
			q = strings.TrimSpace(q)
			if q != "" {
				filtered = append(filtered, q)
			}
		}
		cl.SearchQuestions = filtered
	}
}

// newTSVScanner returns a line scanner with a large buffer to handle long question fields.
func newTSVScanner(f *os.File) *bufio.Scanner {
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 256*1024), 256*1024)
	return s
}

// clusterAvgScore returns the mean per-member pairwise ∛(seq×imp×call) score.
func clusterAvgScore(cl ds.Cluster) float64 {
	scores := ast.MemberPairwiseScores(cl.Members)
	if len(scores) == 0 {
		return 0
	}
	var sum float64
	for _, s := range scores {
		sum += s
	}
	return sum / float64(len(scores))
}
