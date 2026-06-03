package main

import (
	"fmt"
	"html/template"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ── report data structures ────────────────────────────────────────────────────

type MemberRow struct {
	Package       string
	Name          string
	FilePath      string // full path
	Line          int
	PairwiseScore float64 // mean ∛(seqS×impS×callS) against all other cluster members
}

type ClusterRow struct {
	ShapeHash        string
	Label            string
	CommonShape      string  // human-readable LCS of all member token sequences
	AvgPairwiseScore float64 // mean cbrt score across all member pairs
	Size             int
	Coherence        float64 // mean pairwise Jaccard of DirectImports
	CallCoherence    float64 // mean pairwise Jaccard of CallTargets
	CycloMean        float64
	TopImports       []string
	Packages         []string    // unique package names, sorted
	Members          []MemberRow // sorted by descending pairwise score
	// LLM enrichment — empty until beats update cluster has been run
	SemanticIdiom   string
	Verdict         string
	CanonicalMember string
	SuggestedAction string
	Confidence      string
	SearchQuestions []string
	// Orphan candidates — functions that did not join this cluster but have
	// strong Z-score affinity to it. Each entry is "FuncName#shortpath#line".
	Potentials []string
}

// CandidateRow is one cluster candidate for an orphaned function, ready for template rendering.
type CandidateRow struct {
	ClusterIdx int
	ShapeHash  string
	SeqScore   float64
	ImpScore   float64
	CallScore  float64
	ArithScore float64
	ZScore     float64
	Idiom      string // SemanticIdiom if enriched, else ""
}

// OrphanRow holds display data for one orphaned function.
type OrphanRow struct {
	Package    string
	Name       string
	FilePath   string
	Line       int
	Candidates []CandidateRow
}

// DistBucket is one bar in a histogram — a label, a count, and a bar width
// (0–100) pre-scaled to the bucket with the highest count.
type DistBucket struct {
	Label string
	Count int
	Width int // 0–100, for CSS bar width %
}

type RepoReport struct {
	Repo                string
	GeneratedAt         string
	TotalClusters       int
	FunctionsInClusters int
	CorpusSize          int          // total functions analysed (including those not in any cluster)
	MeanCoherence       float64      // mean import Jaccard across clusters
	MeanCallCoherence   float64      // mean call target Jaccard across clusters
	MeanAvgScore        float64      // mean avg pairwise cbrt score across clusters
	QuadHH              int          // import >= 0.60 AND call >= 0.60
	QuadHL              int          // import >= 0.60 AND call <  0.60
	QuadLH              int          // import <  0.60 AND call >= 0.60
	QuadLL              int          // import <  0.60 AND call <  0.60
	ScoreDist           []DistBucket // score histogram: 0.55–0.65, 0.65–0.75, 0.75–0.85, 0.85–0.95, 0.95–1.00
	ScoreExplain        string       // data-driven interpretation of the score distribution shape
	SizeDist            []DistBucket // size histogram: 2, 3–4, 5–9, 10+
	SizeExplain         string       // data-driven interpretation of the size distribution shape
	Clusters            []ClusterRow // sorted by avg pairwise score desc
	Orphans             []OrphanRow  // functions that escaped clustering, with Z-score candidates
}

// ── entry point ───────────────────────────────────────────────────────────────

func runAnalyze(repo string) error {
	dbPath := filepath.Join(os.TempDir(), "badger", repo)
	bDb := db.NewBadgerXDb(dbPath)
	defer bDb.Close() //nolint:errcheck

	// prefer the single-pass identified tier; fall back to collapsed for indexes
	// built before IdentifyClusterCommand was added to the pipeline.
	tier := db.TierIdentified
	clusters, err := bDb.ScanClusters(tier)
	if err != nil {
		return fmt.Errorf("scan clusters (%s): %w", tier, err)
	}
	if len(clusters) == 0 {
		tier = db.TierCollapsed
		clusters, err = bDb.ScanClusters(tier)
		if err != nil {
			return fmt.Errorf("scan clusters (%s): %w", tier, err)
		}
	}
	if len(clusters) == 0 {
		return fmt.Errorf("no beats index found for %q — run 'beats init --repo %s' first", repo, repo)
	}

	slog.Info("loaded clusters", slog.Int("count", len(clusters)), slog.String("tier", tier))

	orphans, _ := bDb.LoadOrphanedFunctions() // best-effort; nil if not yet computed

	report := buildReport(repo, clusters, orphans)

	beatsDir := filepath.Join(repo, ".beats")
	if err := os.MkdirAll(beatsDir, 0755); err != nil {
		return fmt.Errorf("create .beats dir: %w", err)
	}

	outPath := filepath.Join(beatsDir, "report.html")
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if err := renderHTML(f, report); err != nil {
		return fmt.Errorf("render html: %w", err)
	}

	slog.Info("report written", slog.String("path", outPath))
	return nil
}

// ── report builder ────────────────────────────────────────────────────────────

func buildClusterRow(c ds.Cluster) ClusterRow {
	// per-member pairwise scores — computed once, used for both sort and display
	memberScores := ast.MemberPairwiseScores(c.Members)

	// avg pairwise score = mean of per-member scores
	var totalScore float64
	for _, s := range memberScores {
		totalScore += s
	}
	avgScore := 0.0
	if len(memberScores) > 0 {
		avgScore = totalScore / float64(len(memberScores))
	}

	// common token subsequence → human-readable shape
	commonSeq := ast.CommonSubsequence(c.Members)
	commonShape := ast.SeqString(commonSeq)

	pkgSet := make(map[string]bool)
	members := make([]MemberRow, 0, len(c.Members))
	for i, m := range c.Members {
		pkgSet[m.Package] = true
		members = append(members, MemberRow{
			Package:       m.Package,
			Name:          m.Name,
			FilePath:      m.FileMeta.Path,
			Line:          m.Start_line,
			PairwiseScore: memberScores[i],
		})
	}
	// sort members by pairwise score descending — most central members first
	sort.Slice(members, func(i, j int) bool {
		return members[i].PairwiseScore > members[j].PairwiseScore
	})

	pkgs := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	return ClusterRow{
		ShapeHash:        c.ShapeHash,
		Label:            c.Label,
		CommonShape:      commonShape,
		AvgPairwiseScore: avgScore,
		Size:             c.Size,
		Coherence:        c.Coherence,
		CallCoherence:    c.CallCoherence,
		CycloMean:        c.Profile.CycloMean,
		TopImports:       c.Profile.TopImports,
		Packages:         pkgs,
		Members:          members,
		SemanticIdiom:    c.SemanticIdiom,
		Verdict:          c.Verdict,
		CanonicalMember:  c.CanonicalMember,
		SuggestedAction:  c.SuggestedAction,
		Confidence:       c.Confidence,
		SearchQuestions:  c.SearchQuestions,
	}
}

// buildScoreExplain reads the five score buckets and returns a short
// data-driven interpretation of the shape.
//
// buckets: [0]=0.55–0.65  [1]=0.65–0.75  [2]=0.75–0.85  [3]=0.85–0.95  [4]=0.95–1.00
func buildScoreExplain(b [5]int) string {
	total := 0
	for _, c := range b {
		total += c
	}
	if total == 0 {
		return "No clusters to analyse."
	}

	body := b[0] + b[1] + b[2]   // 0.55–0.85
	clones := b[4]                // 0.95–1.00
	trough := b[3]                // 0.85–0.95
	clonePct := clones * 100 / total
	bodyPct := body * 100 / total

	// bimodal: body is large, there's a visible dip at [3], and [4] spikes back up
	bimodal := body > trough && clones > trough && trough < (body+clones)/3

	switch {
	case bimodal && clonePct >= 15:
		return fmt.Sprintf(
			"Bimodal shape: %d%% of clusters (0.55–0.85) are structural conventions — "+
				"functions following the same pattern with variation — and %d%% (0.95–1.00) are near-identical, "+
				"pointing to intentional twins or unabstracted copy-paste. "+
				"The dip at 0.85–0.95 means these are two distinct populations, not a continuum.",
			bodyPct, clonePct)
	case clonePct >= 25:
		return fmt.Sprintf(
			"Heavy clone concentration: %d%% of clusters score above 0.95, indicating a large number of "+
				"near-identical function pairs. These are strong candidates for abstraction or consolidation.",
			clonePct)
	case bodyPct >= 80:
		return fmt.Sprintf(
			"Most clusters (%d%%) sit in the 0.55–0.85 band — structural conventions with variation, "+
				"not near-clones. The codebase has a consistent structural vocabulary without excessive copy-paste. "+
				"Few clusters reach the 0.95+ near-identical threshold.",
			bodyPct)
	case b[0] >= b[1] && b[1] >= b[2] && b[2] >= b[3] && b[3] >= b[4]:
		return "Smoothly declining distribution: most clusters sit just above the similarity threshold " +
			"and count drops steadily toward 1.00. No dominant clone population; the codebase favours " +
			"loosely similar conventions over tight structural repetition."
	default:
		return fmt.Sprintf(
			"Score distribution is broadly spread across the 0.55–1.00 range (%d total clusters). "+
				"No single band dominates, suggesting a mix of loose conventions, tighter sub-idioms, "+
				"and isolated near-clones.",
			total)
	}
}

// buildSizeExplain reads the four size buckets and returns a short
// data-driven interpretation of the shape.
//
// buckets: [0]=2  [1]=3–4  [2]=5–9  [3]=10+
func buildSizeExplain(b [4]int) string {
	total := 0
	for _, c := range b {
		total += c
	}
	if total == 0 {
		return "No clusters to analyse."
	}

	pairPct := b[0] * 100 / total
	large := b[2] + b[3] // size ≥ 5

	switch {
	case b[3] >= 5:
		return fmt.Sprintf(
			"%d%% of clusters are pairs (size 2), which is expected — "+
				"truly identical functions get abstracted, so pairs are the most common structural echo. "+
				"Notably, %d clusters recur 10 or more times: these are genuine codebase-wide idioms "+
				"the team has organically converged on.",
			pairPct, b[3])
	case b[3] >= 1 && b[2] >= 3:
		return fmt.Sprintf(
			"%d%% of clusters are pairs (size 2). %d clusters recur 5 or more times — "+
				"a small but real set of broadly recurring conventions worth documenting as canonical idioms.",
			pairPct, large)
	case b[3] == 0 && b[2] <= 2:
		return fmt.Sprintf(
			"%d%% of clusters are pairs (size 2) and no cluster recurs 10 or more times. "+
				"The codebase has no single structural convention that appears broadly across the corpus — "+
				"patterns are localised rather than codebase-wide.",
			pairPct)
	case pairPct >= 85:
		return fmt.Sprintf(
			"Pair-dominated: %d%% of clusters have exactly 2 members. "+
				"Most findings are isolated echoes rather than recurring conventions. "+
				"Focus on the size 5+ clusters (%d total) for the most actionable patterns.",
			pairPct, large)
	default:
		return fmt.Sprintf(
			"%d%% of clusters are pairs (size 2), with %d clusters of size 5 or more. "+
				"A reasonable spread — the codebase has both local structural echoes and some "+
				"broader recurring conventions.",
			pairPct, large)
	}
}

func buildReport(repo string, clusters []ds.Cluster, orphanedFns []ds.OrphanedFunction) RepoReport {
	rows := make([]ClusterRow, 0, len(clusters))
	var totalCoherence, totalCallCoherence, totalAvgScore float64
	var functionsInClusters int
	var quadHH, quadHL, quadLH, quadLL int

	for _, c := range clusters {
		row := buildClusterRow(c)
		rows = append(rows, row)
		totalCoherence += c.Coherence
		totalCallCoherence += c.CallCoherence
		totalAvgScore += row.AvgPairwiseScore
		functionsInClusters += c.Size

		hiImport := c.Coherence >= 0.60
		hiCall := c.CallCoherence >= 0.60
		switch {
		case hiImport && hiCall:
			quadHH++
		case hiImport && !hiCall:
			quadHL++
		case !hiImport && hiCall:
			quadLH++
		default:
			quadLL++
		}
	}

	// sort by avg pairwise score descending — the most structurally coherent
	// clusters (highest geometric mean across all member pairs) appear first.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AvgPairwiseScore != rows[j].AvgPairwiseScore {
			return rows[i].AvgPairwiseScore > rows[j].AvgPairwiseScore
		}
		return rows[i].Size > rows[j].Size
	})

	n := len(clusters)
	meanCoherence, meanCallCoherence, meanAvgScore := 0.0, 0.0, 0.0
	if n > 0 {
		meanCoherence = totalCoherence / float64(n)
		meanCallCoherence = totalCallCoherence / float64(n)
		meanAvgScore = totalAvgScore / float64(n)
	}

	// ── score distribution ────────────────────────────────────────────────────
	scoreCounts := [5]int{} // buckets: <0.65, 0.65–0.75, 0.75–0.85, 0.85–0.95, 0.95+
	for _, r := range rows {
		s := r.AvgPairwiseScore
		switch {
		case s >= 0.95:
			scoreCounts[4]++
		case s >= 0.85:
			scoreCounts[3]++
		case s >= 0.75:
			scoreCounts[2]++
		case s >= 0.65:
			scoreCounts[1]++
		default:
			scoreCounts[0]++
		}
	}
	scoreLabels := [5]string{"0.55 – 0.65", "0.65 – 0.75", "0.75 – 0.85", "0.85 – 0.95", "0.95 – 1.00"}
	scoreMax := 1
	for _, c := range scoreCounts {
		if c > scoreMax {
			scoreMax = c
		}
	}
	scoreDist := make([]DistBucket, 5)
	for i, c := range scoreCounts {
		scoreDist[i] = DistBucket{Label: scoreLabels[i], Count: c, Width: c * 100 / scoreMax}
	}

	// ── size distribution ─────────────────────────────────────────────────────
	sizeCounts := [4]int{} // buckets: 2, 3–4, 5–9, 10+
	for _, r := range rows {
		switch {
		case r.Size >= 10:
			sizeCounts[3]++
		case r.Size >= 5:
			sizeCounts[2]++
		case r.Size >= 3:
			sizeCounts[1]++
		default:
			sizeCounts[0]++
		}
	}
	sizeLabels := [4]string{"2", "3 – 4", "5 – 9", "10+"}
	sizeMax := 1
	for _, c := range sizeCounts {
		if c > sizeMax {
			sizeMax = c
		}
	}
	sizeDist := make([]DistBucket, 4)
	for i, c := range sizeCounts {
		sizeDist[i] = DistBucket{Label: sizeLabels[i], Count: c, Width: c * 100 / sizeMax}
	}

	// ── orphan potentials reverse index: ClusterIdx → candidate entries ────────
	// Each entry is "FuncName#shortpath#line" for display in the cluster table.
	clusterPotentials := make(map[int][]string)
	for _, o := range orphanedFns {
		for _, c := range o.Candidates {
			entry := o.Meta.Name + "#" + shortPath(o.Meta.FileMeta.Path) + "#" + fmt.Sprintf("%d", o.Meta.Start_line)
			clusterPotentials[c.ClusterIdx] = append(clusterPotentials[c.ClusterIdx], entry)
		}
	}
	// Attach potentials to cluster rows (rows are already built, patch them in).
	for i := range rows {
		// rows[i] was built from clusters[i] (same order, buildClusterRow preserves index)
		// We need to find which original cluster index corresponds to rows[i].
		// Since rows were appended in cluster order before the sort, we match by ShapeHash.
		for clIdx, cl := range clusters {
			if cl.ShapeHash == rows[i].ShapeHash {
				if pots := clusterPotentials[clIdx]; len(pots) > 0 {
					rows[i].Potentials = pots
				}
				break
			}
		}
	}

	// ── orphan rows ────────────────────────────────────────────────────────────
	orphanRows := make([]OrphanRow, 0, len(orphanedFns))
	for _, o := range orphanedFns {
		cands := make([]CandidateRow, 0, len(o.Candidates))
		for _, c := range o.Candidates {
			cands = append(cands, CandidateRow{
				ClusterIdx: c.ClusterIdx,
				ShapeHash:  c.ShapeHash,
				SeqScore:   c.SeqScore,
				ImpScore:   c.ImpScore,
				CallScore:  c.CallScore,
				ArithScore: c.ArithScore,
				ZScore:     c.ZScore,
				Idiom:      c.Idiom,
			})
		}
		orphanRows = append(orphanRows, OrphanRow{
			Package:    o.Meta.Package,
			Name:       o.Meta.Name,
			FilePath:   o.Meta.FileMeta.Path,
			Line:       o.Meta.Start_line,
			Candidates: cands,
		})
	}

	return RepoReport{
		Repo:                filepath.Base(repo),
		GeneratedAt:         time.Now().Format("2006-01-02 15:04:05"),
		TotalClusters:       n,
		FunctionsInClusters: functionsInClusters,
		MeanCoherence:       meanCoherence,
		MeanCallCoherence:   meanCallCoherence,
		MeanAvgScore:        meanAvgScore,
		QuadHH:              quadHH,
		QuadHL:              quadHL,
		QuadLH:              quadLH,
		QuadLL:              quadLL,
		ScoreDist:           scoreDist,
		ScoreExplain:        buildScoreExplain(scoreCounts),
		SizeDist:            sizeDist,
		SizeExplain:         buildSizeExplain(sizeCounts),
		Clusters:            rows,
		Orphans:             orphanRows,
	}
}

// ── template helpers ──────────────────────────────────────────────────────────

func coherenceBadgeClass(c float64) string {
	switch {
	case c >= 0.60:
		return "badge-green"
	case c >= 0.40:
		return "badge-yellow"
	default:
		return "badge-red"
	}
}

func pct(a, b int) string {
	if b == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", int(float64(a)/float64(b)*100+0.5))
}

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
func f1(v float64) string { return fmt.Sprintf("%.1f", v) }

func joinComma(ss []string) string { return strings.Join(ss, ", ") }

// quadrantCode returns the two-letter quadrant code for a cluster based on its
// import and call coherence scores. Used as CSS class suffix and data attribute.
func quadrantCode(imp, call float64) string {
	hiImp := imp >= 0.60
	hiCall := call >= 0.60
	switch {
	case hiImp && hiCall:
		return "hh"
	case hiImp && !hiCall:
		return "hl"
	case !hiImp && hiCall:
		return "lh"
	default:
		return "ll"
	}
}

// quadrantLabel returns the display label for a quadrant.
func quadrantLabel(imp, call float64) string {
	switch quadrantCode(imp, call) {
	case "hh":
		return "HH"
	case "hl":
		return "HL"
	case "lh":
		return "LH"
	default:
		return "LL"
	}
}

func labelOrHash(cr ClusterRow) string {
	if cr.Label != "" {
		return cr.Label
	}
	return cr.ShapeHash
}

// shortPath returns the last 3 path segments of a file path for display.
// e.g. /home/user/project/pkg/store/sqlstore/user.go → pkg/store/sqlstore/user.go
func shortPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) <= 3 {
		return p
	}
	return strings.Join(parts[len(parts)-3:], "/")
}

// ── HTML renderer ─────────────────────────────────────────────────────────────

// scoreBadgeClass returns a CSS class based on a 0–1 pairwise score.
func scoreBadgeClass(s float64) string {
	switch {
	case s >= 0.75:
		return "badge-green"
	case s >= 0.55:
		return "badge-yellow"
	default:
		return "badge-red"
	}
}

// tokenChips renders a space-separated token string (e.g. "IF FOR RETURN")
// as a sequence of styled chips with arrows, safe for direct HTML embedding.
func tokenChips(shape string) template.HTML {
	if shape == "" {
		return template.HTML(`<span class="no-shape">no common tokens</span>`)
	}
	tokens := strings.Fields(shape)
	var sb strings.Builder
	sb.WriteString(`<span class="token-seq">`)
	for i, t := range tokens {
		if i > 0 {
			sb.WriteString(`<span class="token-arrow">→</span>`)
		}
		sb.WriteString(`<span class="token-chip">`)
		sb.WriteString(template.HTMLEscapeString(t))
		sb.WriteString(`</span>`)
	}
	sb.WriteString(`</span>`)
	return template.HTML(sb.String())
}

// scorePct converts a 0–1 score to an integer percentage for bar width.
func scorePct(s float64) int { return int(s * 100) }

func renderHTML(w *os.File, report RepoReport) error {
	funcMap := template.FuncMap{
		"badgeClass":    coherenceBadgeClass,
		"scoreBadge":    scoreBadgeClass,
		"tokenChips":    tokenChips,
		"scorePct":      scorePct,
		"pct":           pct,
		"f2":            f2,
		"f1":            f1,
		"f3":            func(v float64) string { return fmt.Sprintf("%.3f", v) },
		"joinComma":     joinComma,
		"labelOrHash":   labelOrHash,
		"shortPath":     shortPath,
		"quadrantCode":  quadrantCode,
		"quadrantLabel": quadrantLabel,
	}
	tmpl, err := template.New("report").Funcs(funcMap).Parse(reportTemplate)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, report)
}

// ── embedded HTML template ────────────────────────────────────────────────────

const reportTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<title>beats — {{.Repo}}</title>
<style>
  :root {
    --bg:#0f1117; --surface:#1a1d27; --surface2:#22263a; --surface3:#191c2a;
    --border:#2e3350; --text:#e0e4f0; --muted:#7a82a6; --muted2:#4a5070;
    --green:#34d399; --yellow:#fbbf24; --red:#f87171;
    --accent:#818cf8; --accent2:#60a5fa; --accent3:#c084fc;
  }
  *{box-sizing:border-box;margin:0;padding:0;}
  body{background:var(--bg);color:var(--text);font-family:'Inter','Segoe UI',system-ui,sans-serif;font-size:14px;line-height:1.6;}

  header{background:var(--surface);border-bottom:1px solid var(--border);padding:18px 32px;display:flex;align-items:center;justify-content:space-between;}
  .hdr-left h1{font-size:1.2rem;font-weight:700;color:var(--accent);letter-spacing:-.01em;}
  .hdr-left .repo{font-size:12px;color:var(--muted);margin-top:2px;}
  .hdr-right{font-size:11px;color:var(--muted2);text-align:right;}

  .container{max-width:1340px;margin:0 auto;padding:24px 32px;}

  /* summary cards */
  .cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px;margin-bottom:24px;}
  .card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:13px 16px;}
  .card .lbl{font-size:10px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin-bottom:3px;}
  .card .val{font-size:1.6rem;font-weight:700;color:var(--text);line-height:1.1;}
  .card .sub{font-size:11px;color:var(--muted);margin-top:3px;}

  /* legend */
  details.legend{background:var(--surface);border:1px solid var(--border);border-radius:10px;margin-bottom:20px;}
  details.legend summary{padding:11px 16px;cursor:pointer;font-size:11px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);font-weight:600;list-style:none;display:flex;align-items:center;gap:8px;}
  details.legend summary::before{content:'▶';font-size:9px;transition:transform .2s;}
  details.legend[open] summary::before{transform:rotate(90deg);}
  .legend-body{padding:4px 16px 14px;display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:9px;}
  .lg-item{display:flex;gap:9px;}
  .lg-term{min-width:110px;font-weight:600;color:var(--accent2);font-size:12px;flex-shrink:0;}
  .lg-def{color:var(--muted);font-size:12px;}
  .lg-def code{background:rgba(129,140,248,.12);color:var(--accent);border-radius:3px;padding:1px 5px;font-size:11px;font-family:'JetBrains Mono','Fira Code',monospace;}

  /* quadrant matrix */
  .quadrant-wrap{grid-column:1/-1;margin:4px 0 2px;}
  .quadrant-label{font-size:10px;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);margin-bottom:6px;font-weight:600;}
  .quadrant-grid{display:grid;grid-template-columns:110px 1fr 1fr;gap:2px;font-size:11px;}
  .qg-col-hdr{padding:5px 10px;text-align:center;font-weight:700;color:var(--muted);background:var(--surface2);border-radius:4px 4px 0 0;}
  .qg-row-hdr{padding:5px 10px;display:flex;align-items:center;font-weight:700;color:var(--muted);background:var(--surface2);border-radius:4px 0 0 4px;}
  .qg-cell{padding:7px 11px;border-radius:4px;}
  .qg-hh{background:rgba(52,211,153,.10);border:1px solid rgba(52,211,153,.25);}
  .qg-lh{background:rgba(96,165,250,.10);border:1px solid rgba(96,165,250,.25);}
  .qg-hl{background:rgba(251,191,36,.10);border:1px solid rgba(251,191,36,.20);}
  .qg-ll{background:rgba(248,113,113,.08);border:1px solid rgba(248,113,113,.20);}
  .qg-cell strong{display:block;margin-bottom:2px;font-size:11px;}
  .qg-hh strong{color:var(--green);}.qg-lh strong{color:var(--accent2);}
  .qg-hl strong{color:var(--yellow);}.qg-ll strong{color:var(--red);}

  .sec{font-size:.75rem;font-weight:600;color:var(--accent);text-transform:uppercase;letter-spacing:.08em;margin:0 0 10px;padding-bottom:5px;border-bottom:1px solid var(--border);}
  .sec-bar{display:flex;align-items:center;justify-content:space-between;margin-bottom:10px;}
  .sec-bar .sec{margin:0;border:none;padding:0;}

  /* search */
  .search-wrap{display:flex;align-items:center;gap:6px;}
  .search-icon{color:var(--muted);font-size:16px;line-height:1;}
  .search-input{background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);padding:5px 10px;font-size:13px;width:190px;outline:none;transition:border-color .15s;}
  .search-input:focus{border-color:var(--accent);}
  #file-search:focus{border-color:var(--accent2);}
  .search-clear{background:none;border:none;color:var(--muted);cursor:pointer;font-size:13px;padding:3px 5px;border-radius:4px;display:none;}
  .search-clear:hover{color:var(--text);background:var(--surface2);}
  .search-divider{color:var(--muted2);font-size:11px;padding:0 2px;}
  .search-count{font-size:11px;color:var(--muted);white-space:nowrap;min-width:80px;}

  /* quadrant filter */
  .quad-filter-bar{display:flex;align-items:center;gap:6px;flex-wrap:wrap;margin-bottom:10px;padding:9px 13px;background:var(--surface);border:1px solid var(--border);border-radius:8px;}
  .quad-filter-label{font-size:10px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);font-weight:600;margin-right:4px;}
  .quad-filter-item{display:flex;align-items:center;gap:5px;cursor:pointer;padding:3px 7px;border-radius:6px;border:1px solid transparent;transition:background .1s;user-select:none;}
  .quad-filter-item:hover{background:var(--surface2);}
  .quad-filter-item input[type=checkbox]{accent-color:var(--accent);width:13px;height:13px;cursor:pointer;}
  .quad-filter-item.inactive{opacity:0.4;}
  .quad-filter-divider{width:1px;height:18px;background:var(--border);margin:0 3px;}

  /* quadrant pills */
  .quad-pill{display:inline-block;padding:2px 7px;border-radius:4px;font-size:10px;font-weight:700;letter-spacing:.05em;white-space:nowrap;}
  .quad-hh{background:rgba(52,211,153,.15);color:var(--green);border:1px solid rgba(52,211,153,.30);}
  .quad-lh{background:rgba(96,165,250,.15);color:var(--accent2);border:1px solid rgba(96,165,250,.30);}
  .quad-hl{background:rgba(251,191,36,.15);color:var(--yellow);border:1px solid rgba(251,191,36,.25);}
  .quad-ll{background:rgba(248,113,113,.10);color:var(--red);border:1px solid rgba(248,113,113,.20);}

  /* cluster table */
  .tbl-wrap{overflow-x:auto;overflow-y:auto;max-height:calc(100vh - 240px);border:1px solid var(--border);border-radius:8px;}
  table.clusters{width:100%;border-collapse:collapse;}
  table.clusters thead th{text-align:left;padding:8px 12px;font-size:10px;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);background:var(--surface);border-bottom:2px solid var(--border);white-space:nowrap;cursor:pointer;user-select:none;position:sticky;top:0;z-index:10;box-shadow:0 1px 0 var(--border);}
  table.clusters thead th:hover{color:var(--text);}
  table.clusters thead th.sorted-asc::after{content:' ▲';color:var(--accent);}
  table.clusters thead th.sorted-desc::after{content:' ▼';color:var(--accent);}

  tr.cl-row{background:var(--surface);border-bottom:1px solid var(--border);cursor:pointer;transition:background .1s;}
  tr.cl-row:hover{background:var(--surface2);}
  tr.cl-row td{padding:10px 12px;vertical-align:top;}

  tr.cl-detail{display:none;background:var(--surface3);}
  tr.cl-detail.open{display:table-row;}
  tr.cl-detail td{padding:0;border-bottom:2px solid var(--border);}

  .caret{display:inline-block;font-size:9px;color:var(--muted);margin-right:5px;transition:transform .18s;vertical-align:middle;}
  tr.cl-row.open .caret{transform:rotate(90deg);}

  /* token sequence — the visual identity of each cluster */
  .token-seq{display:inline-flex;flex-wrap:wrap;gap:3px;align-items:center;margin-bottom:4px;}
  .token-chip{background:rgba(129,140,248,.14);color:var(--accent);border:1px solid rgba(129,140,248,.28);border-radius:3px;padding:1px 6px;font-size:10px;font-family:'JetBrains Mono','Fira Code',monospace;font-weight:600;letter-spacing:.02em;}
  .token-arrow{color:var(--muted2);font-size:9px;padding:0 1px;}
  .no-shape{color:var(--muted2);font-size:11px;font-style:italic;}
  .cl-hash{font-family:'JetBrains Mono','Fira Code',monospace;font-size:10px;color:var(--muted);display:block;margin-top:2px;}

  /* badges */
  .badge{display:inline-block;padding:2px 7px;border-radius:4px;font-size:11px;font-weight:600;}
  .badge-green{background:rgba(52,211,153,.15);color:var(--green);}
  .badge-yellow{background:rgba(251,191,36,.15);color:var(--yellow);}
  .badge-red{background:rgba(248,113,113,.15);color:var(--red);}

  .pkg-pills{display:flex;flex-wrap:wrap;gap:3px;}
  .pkg-pill{background:rgba(192,132,252,.10);color:var(--accent3);border-radius:3px;padding:1px 5px;font-size:10px;font-family:monospace;}

  .num{font-variant-numeric:tabular-nums;}
  .muted{color:var(--muted);}

  /* member detail panel */
  .detail-panel{padding:14px 20px 18px;}
  .detail-meta{display:flex;gap:16px;margin-bottom:12px;flex-wrap:wrap;}
  .detail-meta-item{font-size:11px;color:var(--muted);}
  .detail-meta-item strong{color:var(--text);font-weight:600;}
  table.members{width:100%;border-collapse:collapse;font-size:12px;}
  table.members th{text-align:left;padding:5px 10px;color:var(--muted);font-size:10px;text-transform:uppercase;letter-spacing:.06em;border-bottom:1px solid var(--border);}
  table.members td{padding:6px 10px;border-bottom:1px solid var(--border);vertical-align:middle;}
  table.members tr:last-child td{border-bottom:none;}
  .fn-name{font-weight:600;color:var(--accent2);font-family:monospace;font-size:12px;}
  .fn-pkg{color:var(--accent3);font-family:monospace;font-size:11px;}
  .fn-file{color:var(--muted);font-size:11px;font-family:monospace;}
  .fn-line{color:var(--muted2);font-size:11px;font-variant-numeric:tabular-nums;}

  /* score bar inside member table */
  .score-wrap{display:flex;align-items:center;gap:7px;min-width:110px;}
  .score-num{font-size:11px;font-variant-numeric:tabular-nums;font-weight:600;min-width:34px;text-align:right;}
  .score-num.score-high{color:var(--green);}
  .score-num.score-mid{color:var(--yellow);}
  .score-num.score-low{color:var(--red);}
  .score-bar-bg{flex:1;height:5px;background:var(--border);border-radius:3px;overflow:hidden;}
  .score-bar-fill{height:100%;border-radius:3px;transition:width .2s;}
  .score-bar-fill.score-high{background:var(--green);}
  .score-bar-fill.score-mid{background:var(--yellow);}
  .score-bar-fill.score-low{background:var(--red);}

  /* scroll indicator */
  .tbl-scroll-hint{display:flex;align-items:center;justify-content:flex-end;gap:5px;font-size:10px;color:var(--muted2);margin-bottom:4px;user-select:none;}
  .tbl-scroll-hint svg{opacity:.5;}

  /* LLM enrichment panel */
  .enrich-panel{margin-top:10px;padding:10px 12px;background:rgba(129,140,248,.06);border:1px solid rgba(129,140,248,.18);border-radius:6px;}
  .enrich-row{display:flex;gap:8px;margin-bottom:5px;font-size:12px;align-items:baseline;}
  .enrich-row:last-child{margin-bottom:0;}
  .enrich-key{min-width:100px;flex-shrink:0;font-size:10px;text-transform:uppercase;letter-spacing:.07em;color:var(--accent);font-weight:600;}
  .enrich-val{color:var(--text);line-height:1.5;}
  .conf-high{color:var(--green);font-weight:600;}
  .conf-medium{color:var(--yellow);font-weight:600;}
  .conf-low{color:var(--red);font-weight:600;}
  .action-none{color:var(--muted2);font-style:italic;}
  .action-attention{color:var(--yellow);}

  /* search questions */
  .sq-list{display:flex;flex-direction:column;gap:4px;margin-top:1px;}
  .sq-item{display:flex;align-items:baseline;gap:6px;font-size:11px;color:var(--muted);line-height:1.5;}
  .sq-item::before{content:'?';flex-shrink:0;font-size:10px;font-weight:700;color:var(--accent);opacity:.6;width:10px;text-align:center;}

  /* search state */
  tr.cl-row.search-hidden,tr.cl-detail.search-hidden{display:none;}
  tr.cl-row.quad-hidden,tr.cl-detail.quad-hidden{display:none;}
  table.members tr.member-hidden{display:none;}
  table.members tr.member-match td{background:rgba(129,140,248,.07);}
  .fn-name mark{background:rgba(129,140,248,.35);color:var(--text);border-radius:2px;padding:0 1px;}

  /* distribution histograms */
  .dist-section{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:16px 20px;margin-bottom:20px;}
  .dist-grid{display:grid;grid-template-columns:1fr 1fr;gap:24px;}
  .dist-title{font-size:10px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);font-weight:600;margin-bottom:10px;}
  .dist-row{display:flex;align-items:center;gap:9px;margin-bottom:5px;}
  .dist-label{font-size:11px;color:var(--muted);font-family:'JetBrains Mono','Fira Code',monospace;min-width:80px;flex-shrink:0;text-align:right;}
  .dist-bar-bg{flex:1;height:10px;background:var(--surface2);border-radius:3px;overflow:hidden;}
  .dist-bar-fill{height:100%;border-radius:3px;background:var(--accent);opacity:.7;transition:width .3s;}
  .dist-count{font-size:11px;color:var(--muted2);font-variant-numeric:tabular-nums;min-width:28px;}
  .dist-explain{font-size:11px;color:var(--muted);line-height:1.6;margin-top:10px;padding-top:9px;border-top:1px solid var(--border);}
  .dist-explain strong{color:var(--text);font-weight:600;}

  footer{text-align:center;padding:24px;color:var(--muted2);font-size:11px;border-top:1px solid var(--border);margin-top:36px;}
</style>
</head>
<body>

<header>
  <div class="hdr-left">
    <h1>beats analyze</h1>
    <div class="repo">{{.Repo}}</div>
  </div>
  <div class="hdr-right">
    structural fingerprinting · ∛(seq × import × call)<br/>
    {{.GeneratedAt}}
  </div>
</header>

<div class="container">

  <div class="cards">
    <div class="card">
      <div class="lbl">Clusters</div>
      <div class="val">{{.TotalClusters}}</div>
      <div class="sub">structural patterns found</div>
    </div>
    <div class="card">
      <div class="lbl">Functions Covered</div>
      <div class="val">{{.FunctionsInClusters}}</div>
      <div class="sub">across all clusters</div>
    </div>
    <div class="card">
      <div class="lbl">Mean Pairwise Score</div>
      <div class="val">{{f2 .MeanAvgScore}}</div>
      <div class="sub">mean ∛(seq×imp×call) across clusters</div>
    </div>
    <div class="card">
      <div class="lbl">Mean Import Coh.</div>
      <div class="val">{{f2 .MeanCoherence}}</div>
      <div class="sub">mean pairwise import Jaccard</div>
    </div>
    <div class="card">
      <div class="lbl">Mean Call Coh.</div>
      <div class="val">{{f2 .MeanCallCoherence}}</div>
      <div class="sub">mean pairwise call-target Jaccard</div>
    </div>
    <div class="card" style="border-color:rgba(52,211,153,.35);">
      <div class="lbl" style="color:var(--green);">HH</div>
      <div class="val" style="color:var(--green);">{{.QuadHH}}</div>
      <div class="sub">{{pct .QuadHH .TotalClusters}} tight domain-local</div>
    </div>
    <div class="card" style="border-color:rgba(96,165,250,.35);">
      <div class="lbl" style="color:var(--accent2);">LH</div>
      <div class="val" style="color:var(--accent2);">{{.QuadLH}}</div>
      <div class="sub">{{pct .QuadLH .TotalClusters}} cross-cutting</div>
    </div>
    <div class="card" style="border-color:rgba(251,191,36,.30);">
      <div class="lbl" style="color:var(--yellow);">HL</div>
      <div class="val" style="color:var(--yellow);">{{.QuadHL}}</div>
      <div class="sub">{{pct .QuadHL .TotalClusters}} domain-cohesive</div>
    </div>
    <div class="card" style="border-color:rgba(248,113,113,.25);">
      <div class="lbl" style="color:var(--red);">LL</div>
      <div class="val" style="color:var(--red);">{{.QuadLL}}</div>
      <div class="sub">{{pct .QuadLL .TotalClusters}} probably noise</div>
    </div>
  </div>

  <details class="legend">
    <summary>Metric Glossary</summary>
    <div class="legend-body">

      <div class="lg-item" style="grid-column:1/-1;padding-bottom:6px;margin-bottom:2px;border-bottom:1px solid var(--border);">
        <span class="lg-term" style="color:var(--accent);font-size:11px;text-transform:uppercase;letter-spacing:.07em;">Cluster metrics</span>
      </div>

      <div class="lg-item"><span class="lg-term">Common Shape</span><span class="lg-def">The longest token subsequence present in <em>every</em> member of the cluster — the structural skeleton they all share. A longer shape means the cluster has a richer shared convention. A very short shape (2–4 tokens) may indicate coincidental similarity rather than a real pattern.</span></div>
      <div class="lg-item"><span class="lg-term">Avg Pairwise Score</span><span class="lg-def">Mean ∛(seqSim × importJaccard × callJaccard) computed over every unique pair of functions in the cluster. The cube root is a geometric mean — all three dimensions must contribute; a zero on any one collapses the score to zero. Range 0–1. Higher = tighter, more coherent cluster. Clusters are sorted by this value descending.</span></div>
      <div class="lg-item"><span class="lg-term">Member Score</span><span class="lg-def">Each function's mean pairwise score against every other member. The cluster avg is the mean of these. Members are sorted high→low — the lowest-scoring members are outlier candidates that weakened the cluster. A large gap between the top and bottom member scores is a sign the cluster should be split.</span></div>
      <div class="lg-item"><span class="lg-term">Import Coh.</span><span class="lg-def">Mean pairwise Jaccard similarity of the direct import sets across all member pairs. High (≥ 0.60) means the cluster operates in a shared package domain. Low means the structural shape is shared across unrelated package domains.</span></div>
      <div class="lg-item"><span class="lg-term">Call Coh.</span><span class="lg-def">Mean pairwise Jaccard similarity of the call-target sets (external functions called). High (≥ 0.60) means the cluster uses the same external vocabulary — a strong signal of shared structural role, not coincidence.</span></div>

      <div class="lg-item" style="grid-column:1/-1;padding-bottom:6px;margin-bottom:2px;border-bottom:1px solid var(--border);margin-top:10px;">
        <span class="lg-term" style="color:var(--accent);font-size:11px;text-transform:uppercase;letter-spacing:.07em;">Token reference</span>
        <span class="lg-def" style="margin-left:8px;">Every function is reduced to an ordered sequence of these tokens — no names, no literals, only structure. The common shape is a subsequence of these tokens.</span>
      </div>

      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CALL</span><span class="lg-def">A plain local function call — either a bare identifier call like <code>doWork()</code>, a builtin like <code>make()</code> or <code>len()</code>, or a type conversion. The callee is defined in the same package or is a language builtin.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CALL_PKG</span><span class="lg-def">A package-qualified call — the receiver is an imported package alias, e.g. <code>fmt.Sprintf()</code>, <code>xorm.In()</code>, <code>errors.New()</code>. Distinguishes calls into external packages from calls on variables or methods.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CALL_METHOD</span><span class="lg-def">A method or chained call — the receiver is a variable, struct field, or chained expression, e.g. <code>w.Close()</code>, <code>db.Where(...).Find()</code>, <code>rref.LinkName()</code>. Indicates the function is orchestrating behaviour through an object or interface.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">ASSIGN</span><span class="lg-def">A variable assignment or short variable declaration (<code>:=</code> or <code>=</code>). Signals that the function is building up local state rather than just delegating.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">RETURN</span><span class="lg-def">A return statement. Each return value in the function signature appends one RETURN token — so a function returning <code>(*T, error)</code> emits two RETURN tokens at the end. This encodes the output arity into the shape.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">IF</span><span class="lg-def">An if statement (including <code>if err != nil</code> guards). High frequency of IF in a shape indicates defensive/error-handling code.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">FOR</span><span class="lg-def">A C-style for loop (<code>for i := 0; i &lt; n; i++</code>). Distinct from RANGE — this token signals index-based iteration.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">RANGE</span><span class="lg-def">A range-based for loop (<code>for k, v := range m</code>). The most common iteration token in idiomatic Go — frequent in shapes involving slices, maps, or channels.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">SWITCH</span><span class="lg-def">A switch statement (expression switch or type switch). Paired with CASE tokens for each branch.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CASE</span><span class="lg-def">A case clause inside a switch or select block. The number of CASE tokens encodes how many branches the switch has.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">FUNCLIT</span><span class="lg-def">A function literal (anonymous function / closure), e.g. <code>func() { ... }</code> passed as an argument or assigned to a variable. Common in goroutine launches, callbacks, and deferred cleanup patterns.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">DEFER</span><span class="lg-def">A defer statement. Signals resource cleanup or unlock patterns. Often paired with CALL_METHOD (e.g. <code>defer rows.Close()</code>) or FUNCLIT (e.g. <code>defer func() { ... }()</code>).</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">GO</span><span class="lg-def">A go statement — launching a goroutine (<code>go doWork()</code>). Presence in a shape indicates concurrency in the function's control flow.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">SEND</span><span class="lg-def">A channel send operation (<code>ch &lt;- value</code>). Signals producer-side channel communication.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">SELECT</span><span class="lg-def">A select statement — multiplexing over multiple channel operations. Paired with COMM tokens for each case.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">COMM</span><span class="lg-def">A communication clause inside a select block (each <code>case &lt;-ch:</code> or <code>case ch &lt;- v:</code> branch).</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">BREAK / CONTINUE</span><span class="lg-def">Loop control flow. BREAK exits the innermost loop or switch; CONTINUE skips to the next iteration.</span></div>

      <div class="quadrant-wrap" style="margin-top:14px;">
        <div class="quadrant-label">Coherence quadrant guide</div>
        <div class="quadrant-grid">
          <div></div><div class="qg-col-hdr">High Call Coh. (≥ 0.60)</div><div class="qg-col-hdr">Low Call Coh. (&lt; 0.60)</div>
          <div class="qg-row-hdr">High Import (≥ 0.60)</div>
          <div class="qg-cell qg-hh"><strong>HH — Tight domain-local</strong><span style="color:var(--muted)">Shares both package context and call vocabulary. The strongest signal — a real settled convention. Most actionable for onboarding and refactoring.</span></div>
          <div class="qg-cell qg-hl"><strong>HL — Domain-cohesive</strong><span style="color:var(--muted)">Same package domain, divergent call targets. The convention is drifting — functions do the same kind of thing but via different paths. May benefit from splitting or standardising the approach.</span></div>
          <div class="qg-row-hdr">Low Import (&lt; 0.60)</div>
          <div class="qg-cell qg-lh"><strong>LH — Cross-cutting</strong><span style="color:var(--muted)">Same structural role, different domains. Classic adapter or hook pattern — functions that all wrap or delegate in the same way regardless of what they wrap.</span></div>
          <div class="qg-cell qg-ll"><strong>LL — Noise</strong><span style="color:var(--muted)">Shape coincidence only. Neither domain nor call vocabulary is shared. Treat with scepticism — likely incidental structural similarity.</span></div>
        </div>
      </div>

      <div class="lg-item" style="grid-column:1/-1;margin-top:10px;padding-top:9px;border-top:1px solid var(--border);">
        <span class="lg-term" style="color:var(--muted);">Filtered noise</span>
        <span class="lg-def">Clusters whose trigram shape matches ≥ 5% of the entire corpus are dropped before clustering — structural stop-words (e.g. a function that is just one RETURN) with no discriminating signal.</span>
      </div>
    </div>
  </details>

  <div class="dist-section">
    <div class="dist-grid">
      <div>
        <div class="dist-title">Score distribution — avg pairwise ∛(seq × imp × call)</div>
{{range .ScoreDist}}
        <div class="dist-row">
          <span class="dist-label">{{.Label}}</span>
          <div class="dist-bar-bg"><div class="dist-bar-fill" style="width:{{.Width}}%"></div></div>
          <span class="dist-count">{{.Count}}</span>
        </div>
{{end}}
        <p class="dist-explain">{{.ScoreExplain}}</p>
      </div>
      <div>
        <div class="dist-title">Size distribution — functions per cluster</div>
{{range .SizeDist}}
        <div class="dist-row">
          <span class="dist-label">{{.Label}}</span>
          <div class="dist-bar-bg"><div class="dist-bar-fill" style="width:{{.Width}}%;background:var(--accent3)"></div></div>
          <span class="dist-count">{{.Count}}</span>
        </div>
{{end}}
        <p class="dist-explain">{{.SizeExplain}}</p>
      </div>
    </div>
  </div>

  <div class="quad-filter-bar">
    <span class="quad-filter-label">Show</span>
    <label class="quad-filter-item"><input type="checkbox" class="quad-cb" data-quad="hh" checked/><span class="quad-pill quad-hh">HH</span><span style="font-size:11px;color:var(--muted)">High Import · High Call</span></label>
    <div class="quad-filter-divider"></div>
    <label class="quad-filter-item"><input type="checkbox" class="quad-cb" data-quad="lh" checked/><span class="quad-pill quad-lh">LH</span><span style="font-size:11px;color:var(--muted)">Low Import · High Call</span></label>
    <div class="quad-filter-divider"></div>
    <label class="quad-filter-item"><input type="checkbox" class="quad-cb" data-quad="hl" checked/><span class="quad-pill quad-hl">HL</span><span style="font-size:11px;color:var(--muted)">High Import · Low Call</span></label>
    <div class="quad-filter-divider"></div>
    <label class="quad-filter-item"><input type="checkbox" class="quad-cb" data-quad="ll" checked/><span class="quad-pill quad-ll">LL</span><span style="font-size:11px;color:var(--muted)">Low Import · Low Call</span></label>
  </div>

  <div class="sec-bar">
    <div class="sec">Clusters — sorted by avg pairwise score ↓</div>
    <div class="search-wrap">
      <span class="search-icon">⌕</span>
      <input type="text" id="fn-search" class="search-input" placeholder="Function name…" autocomplete="off" spellcheck="false"/>
      <button id="fn-search-clear" class="search-clear" title="Clear">✕</button>
      <span class="search-divider">·</span>
      <input type="text" id="file-search" class="search-input" placeholder="File path…" autocomplete="off" spellcheck="false"/>
      <button id="file-search-clear" class="search-clear" title="Clear">✕</button>
      <span id="search-count" class="search-count"></span>
    </div>
  </div>

  <div class="tbl-scroll-hint">
    <svg width="12" height="12" viewBox="0 0 12 12" fill="none" xmlns="http://www.w3.org/2000/svg">
      <path d="M6 2v8M6 10l-2.5-2.5M6 10l2.5-2.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/>
    </svg>
    scroll to explore
  </div>
  <div class="tbl-wrap">
  <table class="clusters" id="cl-table">
    <thead>
      <tr>
        <th data-col="quadrant" style="width:52px">Type</th>
        <th data-col="shape">Common Shape</th>
        <th data-col="size">Size</th>
        <th data-col="avgscore" class="sorted-desc">Avg Score</th>
        <th data-col="coherence">Import Coh.</th>
        <th data-col="callcoherence">Call Coh.</th>
        <th data-col="packages">Packages</th>
        <th data-col="potential" title="Orphaned functions with strong structural affinity to this cluster">Potential</th>
      </tr>
    </thead>
    <tbody>
{{range $i, $cl := .Clusters}}
      <tr class="cl-row" data-idx="{{$i}}" data-quadrant="{{quadrantCode $cl.Coherence $cl.CallCoherence}}" onclick="toggleRow({{$i}})">
        <td style="text-align:center;vertical-align:middle;"><span class="quad-pill quad-{{quadrantCode $cl.Coherence $cl.CallCoherence}}">{{quadrantLabel $cl.Coherence $cl.CallCoherence}}</span></td>
        <td>
          <span class="caret">▶</span>{{tokenChips $cl.CommonShape}}
          <span class="cl-hash">{{$cl.ShapeHash}}{{if $cl.Label}} · {{$cl.Label}}{{end}}</span>
        </td>
        <td class="num" style="vertical-align:middle;">{{$cl.Size}}</td>
        <td style="vertical-align:middle;"><span class="badge {{scoreBadge $cl.AvgPairwiseScore}}">{{f3 $cl.AvgPairwiseScore}}</span></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.Coherence}}">{{f2 $cl.Coherence}}</span></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.CallCoherence}}">{{f2 $cl.CallCoherence}}</span></td>
        <td style="vertical-align:middle;">
          <div class="pkg-pills">{{range $cl.Packages}}<span class="pkg-pill">{{.}}</span>{{end}}</div>
        </td>
        <td style="vertical-align:top;padding:6px 12px;">
          {{if $cl.Potentials}}
            <div style="display:flex;flex-direction:column;gap:3px;">
            {{range $cl.Potentials}}
              <span style="font-size:10px;color:var(--accent2);white-space:nowrap;font-family:monospace;">{{.}}</span>
            {{end}}
            </div>
          {{end}}
        </td>
      </tr>
      <tr class="cl-detail" id="detail-{{$i}}">
        <td colspan="8">
          <div class="detail-panel">
            <div class="detail-meta">
              <span class="detail-meta-item">top imports: <strong>{{joinComma $cl.TopImports}}</strong></span>
              <span class="detail-meta-item">cyclo mean: <strong>{{f1 $cl.CycloMean}}</strong></span>
            </div>
            <table class="members">
              <thead><tr><th>Function</th><th>Package</th><th>File</th><th>Line</th><th>Score</th></tr></thead>
              <tbody>
{{range $cl.Members}}
                <tr>
                  <td><span class="fn-name">{{.Name}}</span></td>
                  <td><span class="fn-pkg">{{.Package}}</span></td>
                  <td><span class="fn-file" title="{{.FilePath}}">{{shortPath .FilePath}}</span></td>
                  <td><span class="fn-line">{{.Line}}</span></td>
                  <td>
                    <div class="score-wrap">
                      <span class="score-num {{scoreBadge .PairwiseScore}}">{{f3 .PairwiseScore}}</span>
                      <div class="score-bar-bg"><div class="score-bar-fill {{scoreBadge .PairwiseScore}}" style="width:{{scorePct .PairwiseScore}}%"></div></div>
                    </div>
                  </td>
                </tr>
{{end}}
              </tbody>
            </table>
{{if or $cl.SemanticIdiom $cl.Verdict $cl.SuggestedAction $cl.SearchQuestions}}
            <div class="enrich-panel">
{{if $cl.SemanticIdiom}}
              <div class="enrich-row"><span class="enrich-key">Idiom</span><span class="enrich-val">{{$cl.SemanticIdiom}}{{if $cl.Confidence}} &nbsp;<span class="conf-{{$cl.Confidence}}">{{$cl.Confidence}}</span>{{end}}</span></div>
{{end}}
{{if $cl.CanonicalMember}}
              <div class="enrich-row"><span class="enrich-key">Canonical</span><span class="enrich-val"><code style="font-size:11px;font-family:monospace;color:var(--accent2)">{{$cl.CanonicalMember}}</code></span></div>
{{end}}
{{if $cl.Verdict}}
              <div class="enrich-row"><span class="enrich-key">Verdict</span><span class="enrich-val">{{$cl.Verdict}}</span></div>
{{end}}
{{if $cl.SuggestedAction}}
              <div class="enrich-row"><span class="enrich-key">Action</span><span class="enrich-val {{if eq $cl.SuggestedAction "none"}}action-none{{else}}action-attention{{end}}">{{$cl.SuggestedAction}}</span></div>
{{end}}
{{if $cl.SearchQuestions}}
              <div class="enrich-row"><span class="enrich-key">Questions</span><span class="enrich-val"><div class="sq-list">{{range $cl.SearchQuestions}}<div class="sq-item">{{.}}</div>{{end}}</div></span></div>
{{end}}
            </div>
{{end}}
          </div>
        </td>
      </tr>
{{end}}
    </tbody>
  </table>
  </div>

</div>

{{if .Orphans}}
<div style="max-width:1400px;margin:32px auto;padding:0 24px;">
<h2 style="font-size:1rem;font-weight:700;color:var(--accent);margin-bottom:12px;">
  Function Deviations
  <span style="font-size:11px;font-weight:400;color:var(--muted);margin-left:8px;">{{len .Orphans}} function(s) that did not join any cluster — shown with closest structural cluster match</span>
</h2>
<table id="orphan-table" style="width:100%;border-collapse:collapse;font-size:12px;">
<thead>
  <tr style="background:var(--surface2);color:var(--muted);text-align:left;">
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);">Function</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);">Package</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);">File : Line</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);">Closest Cluster</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);" title="Token-sequence similarity">Seq</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);" title="Import Jaccard">Imp</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);" title="CallTarget Jaccard">Call</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);" title="Arithmetic mean of the three scores">(S+I+C)/3</th>
    <th style="padding:8px 12px;border-bottom:1px solid var(--border);" title="Z = (arith − cluster_mean) / max(cluster_std, 0.05). Higher = stronger fit. Z > 0 beats the cluster mean.">Z</th>
  </tr>
</thead>
<tbody>
{{range .Orphans}}
  {{$fn := .}}
  {{if .Candidates}}
    {{with index .Candidates 0}}
    <tr style="border-bottom:1px solid var(--border);">
      <td style="padding:8px 12px;font-weight:600;color:var(--text);">{{$fn.Name}}</td>
      <td style="padding:8px 12px;color:var(--muted);">{{$fn.Package}}</td>
      <td style="padding:8px 12px;color:var(--muted2);font-size:11px;">{{shortPath $fn.FilePath}}:{{$fn.Line}}</td>
      <td style="padding:8px 12px;">
        {{if .Idiom}}<span style="color:var(--accent2);">{{.Idiom}}</span>{{else}}<span style="color:var(--muted2);">cluster-{{.ClusterIdx}}</span>{{end}}
        <span style="font-size:10px;color:var(--muted2);margin-left:4px;">{{.ShapeHash}}</span>
      </td>
      <td style="padding:8px 12px;text-align:right;">{{f2 .SeqScore}}</td>
      <td style="padding:8px 12px;text-align:right;">{{f2 .ImpScore}}</td>
      <td style="padding:8px 12px;text-align:right;">{{f2 .CallScore}}</td>
      <td style="padding:8px 12px;text-align:right;font-weight:600;">{{f2 .ArithScore}}</td>
      <td style="padding:8px 12px;text-align:right;">
        {{if gt .ZScore 2.0}}<span style="color:var(--green);font-weight:600;">{{f2 .ZScore}}</span>
        {{else if gt .ZScore 0.0}}<span style="color:var(--yellow);">{{f2 .ZScore}}</span>
        {{else}}<span style="color:var(--muted);">{{f2 .ZScore}}</span>{{end}}
      </td>
    </tr>
    {{end}}
  {{end}}
{{end}}
</tbody>
</table>
</div>
{{end}}

<footer>beats · vocabulary-independent structural fingerprinting for Go · {{.GeneratedAt}}</footer>

<script>
function toggleRow(idx) {
  var row = document.querySelector('tr.cl-row[data-idx="'+idx+'"]');
  var det = document.getElementById('detail-'+idx);
  var open = det.classList.contains('open');
  det.classList.toggle('open',!open);
  row.classList.toggle('open',!open);
}

(function(){
  var state={col:'avgscore',asc:false};
  var colIndex={quadrant:0,shape:1,size:2,avgscore:3,coherence:4,callcoherence:5,packages:6,potential:7};

  document.querySelectorAll('#cl-table thead th').forEach(function(th){
    th.addEventListener('click',function(){
      var col=th.dataset.col;
      if(state.col===col){state.asc=!state.asc;}
      else{state.col=col;state.asc=(col==='shape'||col==='packages');}
      document.querySelectorAll('#cl-table thead th').forEach(function(t){t.classList.remove('sorted-asc','sorted-desc');});
      th.classList.add(state.asc?'sorted-asc':'sorted-desc');
      sortTable(col,state.asc);
    });
  });

  function sortTable(col,asc){
    var tbody=document.querySelector('#cl-table tbody');
    var pairs=[];
    document.querySelectorAll('#cl-table tbody tr.cl-row').forEach(function(row){
      pairs.push({row:row,det:document.getElementById('detail-'+row.dataset.idx)});
    });
    pairs.sort(function(a,b){
      var av=cellVal(a.row,col),bv=cellVal(b.row,col);
      if(typeof av==='number')return asc?av-bv:bv-av;
      return asc?av.localeCompare(bv):bv.localeCompare(av);
    });
    pairs.forEach(function(p){tbody.appendChild(p.row);tbody.appendChild(p.det);});
  }

  function cellVal(row,col){
    var ci=colIndex[col];
    if(ci===undefined)return '';
    var cell=row.querySelectorAll('td')[ci];
    var text=cell?cell.textContent.trim():'';
    var n=parseFloat(text);
    return isNaN(n)?text:n;
  }
})();

(function(){
  var fnInput=document.getElementById('fn-search');
  var fnClear=document.getElementById('fn-search-clear');
  var fileInput=document.getElementById('file-search');
  var fileClear=document.getElementById('file-search-clear');
  var countEl=document.getElementById('search-count');

  fnInput.addEventListener('input',runSearch);
  fileInput.addEventListener('input',runSearch);
  fnClear.addEventListener('click',function(){fnInput.value='';runSearch();fnInput.focus();});
  fileClear.addEventListener('click',function(){fileInput.value='';runSearch();fileInput.focus();});

  function runSearch(){
    var fnTerm=fnInput.value.trim().toLowerCase();
    var fileTerm=fileInput.value.trim().toLowerCase();
    fnClear.style.display=fnTerm?'block':'none';
    fileClear.style.display=fileTerm?'block':'none';
    var searching=fnTerm||fileTerm;
    if(!searching){
      document.querySelectorAll('tr.cl-row').forEach(function(r){r.classList.remove('search-hidden');});
      document.querySelectorAll('tr.cl-detail').forEach(function(d){
        d.classList.remove('search-hidden');
        d.querySelectorAll('table.members tr').forEach(function(mr){mr.classList.remove('member-hidden','member-match');});
        d.querySelectorAll('.fn-name mark,.fn-file mark').forEach(function(m){m.outerHTML=m.textContent;});
      });
      countEl.textContent='';return;
    }
    var mc=0,mf=0;
    document.querySelectorAll('tr.cl-row').forEach(function(row){
      var idx=row.dataset.idx;
      var det=document.getElementById('detail-'+idx);
      var memberRows=det.querySelectorAll('table.members tbody tr');
      var hit=false;
      memberRows.forEach(function(mr){
        mr.querySelectorAll('.fn-name mark,.fn-file mark').forEach(function(m){m.outerHTML=m.textContent;});
        var nameEl=mr.querySelector('.fn-name');
        var fileEl=mr.querySelector('.fn-file');
        if(!nameEl||!fileEl)return;
        var fnOk=!fnTerm||nameEl.textContent.toLowerCase().includes(fnTerm);
        var fileOk=!fileTerm||(fileEl.getAttribute('title')||fileEl.textContent).toLowerCase().includes(fileTerm);
        if(fnOk&&fileOk){
          mr.classList.remove('member-hidden');mr.classList.add('member-match');hit=true;mf++;
          if(fnTerm){var r=nameEl.textContent,lo=r.toLowerCase().indexOf(fnTerm);if(lo!==-1)nameEl.innerHTML=esc(r.slice(0,lo))+'<mark>'+esc(r.slice(lo,lo+fnTerm.length))+'</mark>'+esc(r.slice(lo+fnTerm.length));}
          if(fileTerm){var rf=fileEl.textContent,lf=rf.toLowerCase().indexOf(fileTerm);if(lf!==-1)fileEl.innerHTML=esc(rf.slice(0,lf))+'<mark style="background:rgba(96,165,250,.35);border-radius:2px;padding:0 1px">'+esc(rf.slice(lf,lf+fileTerm.length))+'</mark>'+esc(rf.slice(lf+fileTerm.length));}
        }else{mr.classList.add('member-hidden');mr.classList.remove('member-match');}
      });
      if(hit){row.classList.remove('search-hidden');det.classList.remove('search-hidden');det.classList.add('open');row.classList.add('open');mc++;}
      else{row.classList.add('search-hidden');det.classList.add('search-hidden');det.classList.remove('open');row.classList.remove('open');}
    });
    countEl.textContent=mf>0?(mf+' fn'+(mf!==1?'s':'')+' in '+mc+' cluster'+(mc!==1?'s':'')):'no matches';
  }
  function esc(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
})();

(function(){
  function applyQuadFilter(){
    var q={};
    document.querySelectorAll('.quad-cb').forEach(function(cb){if(cb.checked)q[cb.dataset.quad]=true;});
    document.querySelectorAll('tr.cl-row').forEach(function(row){
      var vis=!!q[row.dataset.quadrant];
      row.classList.toggle('quad-hidden',!vis);
      var det=document.getElementById('detail-'+row.dataset.idx);
      if(det){det.classList.toggle('quad-hidden',!vis);if(!vis){det.classList.remove('open');row.classList.remove('open');}}
    });
    document.querySelectorAll('.quad-filter-item').forEach(function(item){
      var cb=item.querySelector('.quad-cb');item.classList.toggle('inactive',cb&&!cb.checked);
    });
  }
  document.querySelectorAll('.quad-cb').forEach(function(cb){cb.addEventListener('change',applyQuadFilter);});
})();
</script>
</body>
</html>`
