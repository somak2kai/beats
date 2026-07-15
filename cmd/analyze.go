package main

import (
	"fmt"
	"html/template"
	"log/slog"
	"math"
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
	StableID         string  // first 6 hex chars of ShapeHash — stable across runs
	Ordinal          int     // 1-based rank within tier, sorted by ConfidenceScore desc
	Tier             string  // "high" | "medium" | "low"
	ConfidenceScore  float64 // ln(size) × ln(numPackages+1) × confidence(tier) × meanScore² × (impCoh+callCoh)/2
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

// OutlierDiff describes how one orphaned function diverges from its closest cluster.
type OutlierDiff struct {
	Name     string
	Package  string
	FilePath string
	Line     int
	// orphan cyclomatic complexity minus cluster mean; positive = more complex, negative = simpler
	CycloDelta     float64
	TokenShape     string   // orphan's full token sequence — compare visually to cluster LCS in header
	TokensAdded    []string // token types in orphan but not in cluster LCS (set diff)
	TokensRemoved  []string // token types in cluster LCS but not in orphan (set diff)
	ImportsAdded   []string // imports in orphan not in cluster TopImports (short names)
	ImportsRemoved []string // cluster TopImports not in orphan (short names)
	CallsAdded     []string // call targets in orphan not in cluster TopCallTargets
	CallsRemoved   []string // cluster TopCallTargets not in orphan
}

// ClusterOutlierGroup groups all outlier functions that point to the same cluster.
type ClusterOutlierGroup struct {
	ClusterID    string // first 6 hex chars of ShapeHash
	ClusterHash  string
	ClusterLabel string // SemanticIdiom or Label if enriched
	CommonShape  string // human-readable cluster LCS
	Tier         string // "high" | "medium" | "low"
	Outliers     []OutlierDiff
}

// DistBucket is one bar in a histogram — a label, a count, and a bar width
// (0–100) pre-scaled to the bucket with the highest count.
type DistBucket struct {
	Label string
	Count int
	Width int // 0–100, for CSS bar width %
}

// PackageCoverageRow is one package in the clustered-vs-outlier coverage chart.
type PackageCoverageRow struct {
	Package      string
	Clustered    int // functions belonging to a cluster
	Outliers     int // persisted structural outliers
	Total        int
	ClusteredPct int // 0–100, for CSS bar width
	OutliersPct  int // 0–100, for CSS bar width
}

// DeltaBucket is one bar in the outlier delta-direction chart.
type DeltaBucket struct {
	Label string
	Count int
	Width int    // 0–100
	Color string // CSS variable
}

// TokenFreqBucket is one token type in the negative-delta token frequency chart.
type TokenFreqBucket struct {
	Token string
	Count int
	Width int // 0–100
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
	ScoreDist           []DistBucket // score histogram: 0.55–0.65, 0.65–0.75, 0.75–0.85, 0.85–0.95, 0.95–1.00
	ScoreExplain        string       // data-driven interpretation of the score distribution shape
	SizeDist            []DistBucket // size histogram: 3–4, 5–9, 10+
	SizeExplain         string       // data-driven interpretation of the size distribution shape
	// Package coverage — always populated; top 20 packages by total functions.
	PackageCoverage []PackageCoverageRow
	// Outlier signal charts — nil when TotalOutliers == 0.
	DeltaDirectionDist []DeltaBucket     // negative-only / positive-only / mixed / no-delta
	TokenFreqDist      []TokenFreqBucket // top token types missing from outliers vs peers
	// Tier-split cluster lists — each sorted by ConfidenceScore descending.
	HighClusters   []ClusterRow
	MediumClusters []ClusterRow
	LowClusters    []ClusterRow
	OutlierGroups  []ClusterOutlierGroup // potential deviations grouped by target cluster
	TotalOutliers  int                   // sum of len(g.Outliers) across all groups
}

// ── entry point ───────────────────────────────────────────────────────────────

func runAnalyze(repo string) error {
	dbPath := beatsDBPath(repo)
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

	// common token subsequence → human-readable shape (pre-computed and stored in cluster)
	commonShape := ast.SeqString(c.CommonSeq)

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

	// stable ID: first 6 hex chars of ShapeHash
	stableID := c.ShapeHash
	if len(stableID) > 6 {
		stableID = stableID[:6]
	}

	// tier: trust stored value (set by clusterClassifier); recompute for old indexes.
	t := c.Tier
	if t == "" {
		t = clusterTier(c.Stats.StdScore)
	}

	// confidence score: trust stored value; recompute for old indexes.
	conf := c.CompositeScore

	return ClusterRow{
		ShapeHash:        c.ShapeHash,
		StableID:         stableID,
		Tier:             t,
		ConfidenceScore:  conf,
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

	body := b[0] + b[1] + b[2] // 0.55–0.85
	clones := b[4]             // 0.95–1.00
	trough := b[3]             // 0.85–0.95
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
				"near-identical function pairs.",
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

// buildSizeExplain reads the three size buckets and returns a short
// data-driven interpretation of the shape.
//
// buckets: [0]=3–4  [1]=5–9  [2]=10+
func buildSizeExplain(b [3]int) string {
	total := 0
	for _, c := range b {
		total += c
	}
	if total == 0 {
		return "No clusters to analyse."
	}

	small := b[0]        // 3–4
	large := b[1] + b[2] // 5+
	largePct := large * 100 / total

	switch {
	case b[2] >= 5:
		return fmt.Sprintf(
			"%d clusters recur 10 or more times — genuine codebase-wide idioms the team has "+
				"organically converged on. These are the highest-signal patterns worth documenting "+
				"or enforcing as canonical conventions.",
			b[2])
	case b[2] >= 1 && b[1] >= 3:
		return fmt.Sprintf(
			"%d%% of clusters have 5 or more members. A healthy spread of recurring conventions — "+
				"the codebase has settled on structural patterns that appear broadly, not just in isolated pockets.",
			largePct)
	case b[2] == 0 && b[1] <= 2:
		return fmt.Sprintf(
			"Most clusters are small (3–4 members, %d total). No single structural convention appears "+
				"broadly across the corpus — patterns are localised. Focus on the high-attention clusters "+
				"as early candidates for abstraction.",
			small)
	default:
		return fmt.Sprintf(
			"%d%% of clusters have 5 or more members, with %d clusters of size 10+. "+
				"A reasonable spread — the codebase has both local structural echoes and some "+
				"broader recurring conventions.",
			largePct, b[2])
	}
}

// tokenSetDiff returns token type names that differ between orphan and lcs by count (multiset
// semantics). A token that appears N times in orphan and M times in lcs contributes |N-M| entries
// to added (if N>M) or removed (if M>N). This catches structural differences such as a missing
// CATCH block that would be invisible to a pure type-presence (set) diff.
func tokenSetDiff(orphanSeq, lcsSeq []int) (added, removed []string) {
	orphanCounts := make(map[int]int, len(orphanSeq))
	for _, t := range orphanSeq {
		orphanCounts[t]++
	}
	lcsCounts := make(map[int]int, len(lcsSeq))
	for _, t := range lcsSeq {
		lcsCounts[t]++
	}
	// Collect all token types seen in either sequence.
	allTypes := make(map[int]bool, len(orphanCounts)+len(lcsCounts))
	for t := range orphanCounts {
		allTypes[t] = true
	}
	for t := range lcsCounts {
		allTypes[t] = true
	}
	for t := range allTypes {
		o, l := orphanCounts[t], lcsCounts[t]
		name := ast.TokenName(t)
		for i := 0; i < o-l; i++ {
			added = append(added, name)
		}
		for i := 0; i < l-o; i++ {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}

// stringSetDiff returns elements only in a (added) and only in b (removed).
func stringSetDiff(a, b []string) (onlyA, onlyB []string) {
	setB := make(map[string]bool, len(b))
	for _, s := range b {
		setB[s] = true
	}
	setA := make(map[string]bool, len(a))
	for _, s := range a {
		setA[s] = true
	}
	for _, s := range a {
		if !setB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !setA[s] {
			onlyB = append(onlyB, s)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return
}

// shortImport returns the last path segment of an import path for compact display.
func shortImport(imp string) string {
	parts := strings.Split(imp, "/")
	if len(parts) == 0 {
		return imp
	}
	return parts[len(parts)-1]
}

func buildOutlierGroups(clusters []ds.Cluster, orphans []ds.OrphanedFunction) []ClusterOutlierGroup {
	// Index clusters by ShapeHash — ClusterIdx is an analysis-time array index
	// that does not survive a DB round-trip reliably.
	clusterByHash := make(map[string]ds.Cluster, len(clusters))
	for _, cl := range clusters {
		if cl.ShapeHash != "" {
			clusterByHash[cl.ShapeHash] = cl
		}
	}

	type entry struct{ orphan ds.OrphanedFunction }
	groupMap := make(map[string][]entry)
	for _, o := range orphans {
		if len(o.Candidates) == 0 {
			continue
		}
		hash := o.Candidates[0].ShapeHash
		if hash == "" {
			continue
		}
		cl, ok := clusterByHash[hash]
		if !ok || cl.IsPrimitive {
			continue
		}
		groupMap[hash] = append(groupMap[hash], entry{o})
	}

	var groups []ClusterOutlierGroup
	for hash, entries := range groupMap {
		cl := clusterByHash[hash]

		stableID := cl.ShapeHash
		if len(stableID) > 6 {
			stableID = stableID[:6]
		}
		label := cl.SemanticIdiom
		if label == "" {
			label = cl.Label
		}
		tier := cl.Tier
		if tier == "" {
			tier = "low"
		}

		// Use cluster profile data for import/call comparison — always populated
		// from all cluster members, unlike medoid DirectImports which may be empty.
		topImports := make([]string, len(cl.Profile.TopImports))
		for i, imp := range cl.Profile.TopImports {
			topImports[i] = shortImport(imp)
		}
		topCalls := cl.Profile.TopCallTargets

		var outliers []OutlierDiff
		for _, e := range entries {
			o := e.orphan

			// shorten orphan import paths for comparison
			orphanImports := make([]string, len(o.Meta.DirectImports))
			for i, imp := range o.Meta.DirectImports {
				orphanImports[i] = shortImport(imp)
			}

			impAdded, impRemoved := stringSetDiff(orphanImports, topImports)
			callAdded, callRemoved := stringSetDiff(o.Meta.CallTargets, topCalls)
			tokAdded, tokRemoved := tokenSetDiff(o.Meta.TokenSeq, cl.CommonSeq)

			outliers = append(outliers, OutlierDiff{
				Name:           o.Meta.Name,
				Package:        o.Meta.Package,
				FilePath:       o.Meta.FileMeta.Path,
				Line:           o.Meta.Start_line,
				CycloDelta:     o.Candidates[0].CycloDelta,
				TokenShape:     ast.SeqString(o.Meta.TokenSeq),
				TokensAdded:    tokAdded,
				TokensRemoved:  tokRemoved,
				ImportsAdded:   impAdded,
				ImportsRemoved: impRemoved,
				CallsAdded:     callAdded,
				CallsRemoved:   callRemoved,
			})
		}

		groups = append(groups, ClusterOutlierGroup{
			ClusterID:    stableID,
			ClusterHash:  cl.ShapeHash,
			ClusterLabel: label,
			CommonShape:  ast.SeqString(cl.CommonSeq),
			Tier:         tier,
			Outliers:     outliers,
		})
	}

	// drop groups with no outliers
	filtered := groups[:0]
	for _, g := range groups {
		if len(g.Outliers) > 0 {
			filtered = append(filtered, g)
		}
	}
	groups = filtered

	tierOrder := map[string]int{"high": 0, "medium": 1, "low": 2}
	sort.Slice(groups, func(i, j int) bool {
		ti := tierOrder[groups[i].Tier]
		tj := tierOrder[groups[j].Tier]
		if ti != tj {
			return ti < tj
		}
		return groups[i].ClusterID < groups[j].ClusterID
	})
	return groups
}

func buildReport(repo string, clusters []ds.Cluster, orphanedFns []ds.OrphanedFunction) RepoReport {
	rows := make([]ClusterRow, 0, len(clusters))
	var totalCoherence, totalCallCoherence, totalAvgScore float64
	var functionsInClusters int

	for _, c := range clusters {
		if c.IsPrimitive {
			continue // never show primitive clusters — structural stop-words
		}
		row := buildClusterRow(c)
		rows = append(rows, row)
		totalCoherence += c.Coherence
		totalCallCoherence += c.CallCoherence
		totalAvgScore += row.AvgPairwiseScore
		functionsInClusters += c.Size
	}

	// group by tier
	var highRows, medRows, lowRows []ClusterRow
	for _, r := range rows {
		switch r.Tier {
		case "high":
			highRows = append(highRows, r)
		case "medium":
			medRows = append(medRows, r)
		default: // "low" or unset
			lowRows = append(lowRows, r)
		}
	}

	// sort each tier by ConfidenceScore descending, break ties by size
	sortByConfidence := func(rs []ClusterRow) {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].ConfidenceScore != rs[j].ConfidenceScore {
				return rs[i].ConfidenceScore > rs[j].ConfidenceScore
			}
			return rs[i].Size > rs[j].Size
		})
	}
	sortByConfidence(highRows)
	sortByConfidence(medRows)
	sortByConfidence(lowRows)

	// assign 1-based ordinals within each tier
	for i := range highRows {
		highRows[i].Ordinal = i + 1
	}
	for i := range medRows {
		medRows[i].Ordinal = i + 1
	}
	for i := range lowRows {
		lowRows[i].Ordinal = i + 1
	}

	n := len(rows)
	meanCoherence, meanCallCoherence, meanAvgScore := 0.0, 0.0, 0.0
	if n > 0 {
		meanCoherence = totalCoherence / float64(n)
		meanCallCoherence = totalCallCoherence / float64(n)
		meanAvgScore = totalAvgScore / float64(n)
	}

	// ── score distribution (all rows for histogram) ───────────────────────────
	scoreCounts := [5]int{}
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
	// Minimum cluster size is 3 (size-2 clusters are not formed), so buckets
	// start at 3–4.
	sizeCounts := [3]int{}
	for _, r := range rows {
		switch {
		case r.Size >= 10:
			sizeCounts[2]++
		case r.Size >= 5:
			sizeCounts[1]++
		default: // 3–4
			sizeCounts[0]++
		}
	}
	sizeLabels := [3]string{"3 – 4", "5 – 9", "10+"}
	sizeMax := 1
	for _, c := range sizeCounts {
		if c > sizeMax {
			sizeMax = c
		}
	}
	sizeDist := make([]DistBucket, 3)
	for i, c := range sizeCounts {
		sizeDist[i] = DistBucket{Label: sizeLabels[i], Count: c, Width: c * 100 / sizeMax}
	}

	// ── orphan potentials reverse index: ShapeHash → candidate entries ──────────
	clusterPotentials := make(map[string][]string)
	for _, o := range orphanedFns {
		for _, c := range o.Candidates {
			if c.ShapeHash == "" {
				continue
			}
			entry := o.Meta.Name + "#" + shortPath(o.Meta.FileMeta.Path) + "#" + fmt.Sprintf("%d", o.Meta.Start_line)
			clusterPotentials[c.ShapeHash] = append(clusterPotentials[c.ShapeHash], entry)
		}
	}

	// attach potentials to each tier's rows by matching ShapeHash
	attachPotentials := func(rs []ClusterRow) {
		for i := range rs {
			if pots := clusterPotentials[rs[i].ShapeHash]; len(pots) > 0 {
				rs[i].Potentials = pots
			}
		}
	}
	attachPotentials(highRows)
	attachPotentials(medRows)
	attachPotentials(lowRows)

	// ── package coverage ─────────────────────────────────────────────────────
	pkgClustered := make(map[string]int)
	for _, c := range clusters {
		if c.IsPrimitive {
			continue
		}
		for _, m := range c.Members {
			pkgClustered[m.Package]++
		}
	}
	pkgOutliers := make(map[string]int)
	for _, o := range orphanedFns {
		pkgOutliers[o.Meta.Package]++
	}
	allPkgs := make(map[string]struct{})
	for p := range pkgClustered {
		allPkgs[p] = struct{}{}
	}
	for p := range pkgOutliers {
		allPkgs[p] = struct{}{}
	}
	pkgCovRows := make([]PackageCoverageRow, 0, len(allPkgs))
	for p := range allPkgs {
		cl := pkgClustered[p]
		ol := pkgOutliers[p]
		total := cl + ol
		pkgCovRows = append(pkgCovRows, PackageCoverageRow{
			Package:   p,
			Clustered: cl,
			Outliers:  ol,
			Total:     total,
		})
	}
	sort.Slice(pkgCovRows, func(i, j int) bool {
		return pkgCovRows[i].Total > pkgCovRows[j].Total
	})
	if len(pkgCovRows) > 20 {
		pkgCovRows = pkgCovRows[:20]
	}
	if len(pkgCovRows) > 0 {
		maxTotal := pkgCovRows[0].Total
		for i := range pkgCovRows {
			r := &pkgCovRows[i]
			r.ClusteredPct = r.Clustered * 100 / maxTotal
			r.OutliersPct = r.Outliers * 100 / maxTotal
		}
	}

	// ── outlier signal charts (only when outliers exist) ─────────────────────
	outlierGroups := buildOutlierGroups(clusters, orphanedFns)
	totalOutliers := 0
	for _, g := range outlierGroups {
		totalOutliers += len(g.Outliers)
	}

	var deltaDirectionDist []DeltaBucket
	var tokenFreqDist []TokenFreqBucket

	if totalOutliers > 0 {
		negOnly, posOnly, mixed := 0, 0, 0
		tokenCounts := make(map[string]int)

		for _, g := range outlierGroups {
			for _, o := range g.Outliers {
				hasNeg := len(o.TokensRemoved)+len(o.ImportsRemoved)+len(o.CallsRemoved) > 0
				hasPos := len(o.TokensAdded)+len(o.ImportsAdded)+len(o.CallsAdded) > 0
				switch {
				case hasNeg && hasPos:
					mixed++
				case hasNeg:
					negOnly++
				case hasPos:
					posOnly++
				}
				for _, t := range o.TokensRemoved {
					tokenCounts[t]++
				}
			}
		}

		maxDir := negOnly
		for _, v := range []int{posOnly, mixed} {
			if v > maxDir {
				maxDir = v
			}
		}
		if maxDir == 0 {
			maxDir = 1
		}
		deltaDirectionDist = []DeltaBucket{
			{Label: "Missing from peers (−)", Count: negOnly, Width: negOnly * 100 / maxDir, Color: "var(--red)"},
			{Label: "Extends peers (+)", Count: posOnly, Width: posOnly * 100 / maxDir, Color: "var(--accent3)"},
			{Label: "Mixed (+ and −)", Count: mixed, Width: mixed * 100 / maxDir, Color: "var(--yellow)"},
		}

		type kv struct {
			token string
			count int
		}
		var tokenSlice []kv
		for t, c := range tokenCounts {
			tokenSlice = append(tokenSlice, kv{t, c})
		}
		sort.Slice(tokenSlice, func(i, j int) bool {
			return tokenSlice[i].count > tokenSlice[j].count
		})
		if len(tokenSlice) > 10 {
			tokenSlice = tokenSlice[:10]
		}
		if len(tokenSlice) > 0 {
			maxTok := tokenSlice[0].count
			for _, kv := range tokenSlice {
				tokenFreqDist = append(tokenFreqDist, TokenFreqBucket{
					Token: kv.token,
					Count: kv.count,
					Width: kv.count * 100 / maxTok,
				})
			}
		}
	}

	return RepoReport{
		Repo:                filepath.Base(repo),
		GeneratedAt:         time.Now().Format("2006-01-02 15:04:05"),
		TotalClusters:       n,
		FunctionsInClusters: functionsInClusters,
		MeanCoherence:       meanCoherence,
		MeanCallCoherence:   meanCallCoherence,
		MeanAvgScore:        meanAvgScore,
		ScoreDist:           scoreDist,
		ScoreExplain:        buildScoreExplain(scoreCounts),
		SizeDist:            sizeDist,
		SizeExplain:         buildSizeExplain(sizeCounts),
		PackageCoverage:     pkgCovRows,
		DeltaDirectionDist:  deltaDirectionDist,
		TokenFreqDist:       tokenFreqDist,
		HighClusters:        highRows,
		MediumClusters:      medRows,
		LowClusters:         lowRows,
		OutlierGroups:       outlierGroups,
		TotalOutliers:       totalOutliers,
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

// signedDelta formats a float with an explicit sign prefix, e.g. "+3.0", "-2.0", "0.0".
func signedDelta(v float64) string {
	if v > 0 {
		return fmt.Sprintf("+%.1f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func joinComma(ss []string) string { return strings.Join(ss, ", ") }

func labelOrHash(cr ClusterRow) string {
	if cr.Label != "" {
		return cr.Label
	}
	return cr.ShapeHash
}

// shortPath returns the last 3 path segments of a file path for display.
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

// tokenChips renders a space-separated token string as styled chips.
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

// diffChips renders added (green +) and removed (red −) chips for token/import/call diffs.
func diffChips(added, removed []string) template.HTML {
	if len(added) == 0 && len(removed) == 0 {
		return template.HTML(`<span style="color:var(--muted2);font-size:10px;">—</span>`)
	}
	var sb strings.Builder
	sb.WriteString(`<span class="diff-chips">`)
	for _, a := range added {
		sb.WriteString(`<span class="diff-add">+`)
		sb.WriteString(template.HTMLEscapeString(a))
		sb.WriteString(`</span>`)
	}
	for _, r := range removed {
		sb.WriteString(`<span class="diff-rem">−`)
		sb.WriteString(template.HTMLEscapeString(r))
		sb.WriteString(`</span>`)
	}
	sb.WriteString(`</span>`)
	return template.HTML(sb.String())
}

// scorePct converts a 0–1 score to an integer percentage for bar width.
func scorePct(s float64) int { return int(s * 100) }

// confidenceBar converts a confidence score to a bar width capped at 100.
// Scores above ~8.0 (ln(100) × ln(10+1) × 1.0 × 1.0²) are extremely rare.
func confidenceBar(a float64) int {
	pct := int(a / 8.0 * 100)
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

// suppress math import warning — math.Log is used in clusterAttentionScore (cmd.go)
var _ = math.Log

func renderHTML(w *os.File, report RepoReport) error {
	funcMap := template.FuncMap{
		"badgeClass":    coherenceBadgeClass,
		"scoreBadge":    scoreBadgeClass,
		"tokenChips":    tokenChips,
		"scorePct":      scorePct,
		"confidenceBar": confidenceBar,
		"pct":           pct,
		"f2":            f2,
		"f1":            f1,
		"f3":            func(v float64) string { return fmt.Sprintf("%.3f", v) },
		"joinComma":     joinComma,
		"labelOrHash":   labelOrHash,
		"shortPath":     shortPath,
		"diffChips":     diffChips,
		"signedDelta":   signedDelta,
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

  .container{max-width:1400px;margin:0 auto;padding:24px 32px;}

  /* summary cards */
  .cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin-bottom:24px;}
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
  .lg-term{min-width:130px;font-weight:600;color:var(--accent2);font-size:12px;flex-shrink:0;}
  .lg-def{color:var(--muted);font-size:12px;}
  .lg-def code{background:rgba(129,140,248,.12);color:var(--accent);border-radius:3px;padding:1px 5px;font-size:11px;font-family:'JetBrains Mono','Fira Code',monospace;}


  .sec{font-size:.75rem;font-weight:600;color:var(--accent);text-transform:uppercase;letter-spacing:.08em;margin:0 0 10px;padding-bottom:5px;border-bottom:1px solid var(--border);}
  .sec-bar{display:flex;align-items:center;justify-content:space-between;margin-bottom:10px;}
  .sec-bar .sec{margin:0;border:none;padding:0;}

  /* search */
  .search-wrap{display:flex;align-items:center;gap:6px;flex-wrap:wrap;}
  .search-icon{color:var(--muted);font-size:16px;line-height:1;}
  .search-input{background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);padding:5px 10px;font-size:13px;width:150px;outline:none;transition:border-color .15s;}
  .search-input:focus{border-color:var(--accent);}
  #id-search{width:110px;font-family:'JetBrains Mono','Fira Code',monospace;font-size:12px;}
  #id-search:focus{border-color:var(--accent2);}
  #file-search:focus{border-color:var(--accent2);}
  .search-clear{background:none;border:none;color:var(--muted);cursor:pointer;font-size:13px;padding:3px 5px;border-radius:4px;display:none;}
  .search-clear:hover{color:var(--text);background:var(--surface2);}
  .search-divider{color:var(--muted2);font-size:11px;padding:0 2px;}
  .search-count{font-size:11px;color:var(--muted);white-space:nowrap;min-width:80px;}


  /* quadrant pills */
  .quad-pill{display:inline-block;padding:2px 7px;border-radius:4px;font-size:10px;font-weight:700;letter-spacing:.05em;white-space:nowrap;}
  .quad-hh{background:rgba(52,211,153,.15);color:var(--green);border:1px solid rgba(52,211,153,.30);}
  .quad-lh{background:rgba(96,165,250,.15);color:var(--accent2);border:1px solid rgba(96,165,250,.30);}
  .quad-hl{background:rgba(251,191,36,.15);color:var(--yellow);border:1px solid rgba(251,191,36,.25);}
  .quad-ll{background:rgba(248,113,113,.10);color:var(--red);border:1px solid rgba(248,113,113,.20);}

  /* tier tabs */
  .tier-tabs{display:flex;border-bottom:2px solid var(--border);margin-bottom:0;gap:0;}
  .tier-tab{background:none;border:none;border-bottom:2px solid transparent;margin-bottom:-2px;padding:10px 24px;font-size:13px;font-weight:600;cursor:pointer;color:var(--muted);transition:color .15s,border-color .15s;letter-spacing:.01em;}
  .tier-tab:hover{color:var(--text);}
  .tier-tab.active{color:var(--text);border-bottom-color:var(--accent);}
  .tab-count{font-size:11px;font-weight:400;background:var(--surface2);color:var(--muted);border-radius:10px;padding:1px 8px;margin-left:6px;}
  .tier-panel{display:block;padding-top:12px;}
  .tier-panel.hidden{display:none;}

  /* cluster table */
  .tbl-wrap{overflow-x:auto;overflow-y:auto;max-height:calc(100vh - 260px);border:1px solid var(--border);border-radius:8px;}
  table.clusters{width:100%;border-collapse:collapse;}
  table.clusters thead th{text-align:left;padding:8px 10px;font-size:10px;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);background:var(--surface);border-bottom:2px solid var(--border);white-space:nowrap;cursor:pointer;user-select:none;position:sticky;top:0;z-index:10;box-shadow:0 1px 0 var(--border);}
  table.clusters thead th:hover{color:var(--text);}
  table.clusters thead th.sorted-asc::after{content:' ▲';color:var(--accent);}
  table.clusters thead th.sorted-desc::after{content:' ▼';color:var(--accent);}

  tr.cl-row{background:var(--surface);border-bottom:1px solid var(--border);cursor:pointer;transition:background .1s;}
  tr.cl-row:hover{background:var(--surface2);}
  tr.cl-row td{padding:9px 10px;vertical-align:top;}

  tr.cl-detail{display:none;background:var(--surface3);}
  tr.cl-detail.open{display:table-row;}
  tr.cl-detail td{padding:0;border-bottom:2px solid var(--border);}

  .caret{display:inline-block;font-size:9px;color:var(--muted);margin-right:5px;transition:transform .18s;vertical-align:middle;}
  tr.cl-row.open .caret{transform:rotate(90deg);}

  /* ordinal + stable ID */
  .cl-ordinal{font-size:11px;color:var(--muted2);font-variant-numeric:tabular-nums;text-align:right;width:28px;}
  .cl-stableid{font-family:'JetBrains Mono','Fira Code',monospace;font-size:11px;color:var(--accent);letter-spacing:.04em;}

  /* confidence score */
  .attn-wrap{display:flex;align-items:center;gap:6px;min-width:70px;}
  .attn-num{font-size:12px;font-weight:600;color:var(--accent2);font-variant-numeric:tabular-nums;min-width:32px;text-align:right;}
  .attn-bar-bg{flex:1;height:4px;background:var(--border);border-radius:2px;overflow:hidden;}
  .attn-bar-fill{height:100%;border-radius:2px;background:var(--accent2);opacity:.7;}

  /* token sequence */
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
  .members-wrap{max-height:360px;overflow-y:auto;border:1px solid var(--border);border-radius:6px;}
  table.members th{text-align:left;padding:5px 10px;color:var(--muted);font-size:10px;text-transform:uppercase;letter-spacing:.06em;border-bottom:1px solid var(--border);position:sticky;top:0;background:var(--surface3);z-index:5;box-shadow:0 1px 0 var(--border);}
  table.members td{padding:6px 10px;border-bottom:1px solid var(--border);vertical-align:middle;}
  table.members tr:last-child td{border-bottom:none;}
  .fn-name{font-weight:600;color:var(--accent2);font-family:monospace;font-size:12px;}
  .fn-pkg{color:var(--accent3);font-family:monospace;font-size:11px;}
  .fn-file{color:var(--muted);font-size:11px;font-family:monospace;}
  .fn-line{color:var(--muted2);font-size:11px;font-variant-numeric:tabular-nums;}

  .score-wrap{display:flex;align-items:center;gap:7px;min-width:110px;}
  .score-num{font-size:11px;font-variant-numeric:tabular-nums;font-weight:600;min-width:34px;text-align:right;}
  .score-num.score-high,.score-bar-fill.score-high{color:var(--green);}
  .score-num.score-mid,.score-bar-fill.score-mid{color:var(--yellow);}
  .score-num.score-low,.score-bar-fill.score-low{color:var(--red);}
  .score-bar-bg{flex:1;height:5px;background:var(--border);border-radius:3px;overflow:hidden;}
  .score-bar-fill{height:100%;border-radius:3px;transition:width .2s;}
  .score-bar-fill.score-high{background:var(--green);}
  .score-bar-fill.score-mid{background:var(--yellow);}
  .score-bar-fill.score-low{background:var(--red);}

  .tbl-scroll-hint{display:flex;align-items:center;justify-content:flex-end;gap:5px;font-size:10px;color:var(--muted2);margin-bottom:4px;user-select:none;}

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
  .sq-list{display:flex;flex-direction:column;gap:4px;margin-top:1px;}
  .sq-item{display:flex;align-items:baseline;gap:6px;font-size:11px;color:var(--muted);line-height:1.5;}
  .sq-item::before{content:'?';flex-shrink:0;font-size:10px;font-weight:700;color:var(--accent);opacity:.6;width:10px;text-align:center;}

  /* search state */
  tr.cl-row.search-hidden,tr.cl-detail.search-hidden{display:none!important;}
table.members tr.member-hidden{display:none;}
  table.members tr.member-match td{background:rgba(129,140,248,.07);}
  .fn-name mark{background:rgba(129,140,248,.35);color:var(--text);border-radius:2px;padding:0 1px;}

  /* no-clusters message */
  .no-clusters{padding:32px;text-align:center;color:var(--muted);font-size:13px;}

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


  /* potential deviations accordion */
  .outlier-body{max-height:65vh;overflow-y:auto;padding:12px 16px 16px;display:block;}
  details.outlier-group{border:1px solid var(--border);border-radius:6px;overflow:hidden;margin-bottom:12px;}
  details.outlier-group:last-child{margin-bottom:0;}
  details.outlier-group summary{list-style:none;cursor:pointer;}
  details.outlier-group summary::-webkit-details-marker{display:none;}
  .outlier-group-hdr{display:flex;align-items:center;gap:8px;padding:9px 14px;background:var(--surface2);flex-wrap:wrap;min-height:40px;border-bottom:1px solid var(--border);}
  details.outlier-group:not([open]) .outlier-group-hdr{border-bottom:none;}
  .outlier-group-hdr::before{content:'▶';font-size:9px;color:var(--muted);flex-shrink:0;transition:transform .2s;}
  details.outlier-group[open] .outlier-group-hdr::before{transform:rotate(90deg);}
  .outlier-cluster-id{font-family:'JetBrains Mono','Fira Code',monospace;font-size:11px;font-weight:600;color:var(--accent);background:rgba(129,140,248,.08);border:1px solid rgba(129,140,248,.25);border-radius:4px;padding:3px 9px;user-select:all;cursor:text;white-space:nowrap;flex-shrink:0;}
  .outlier-cluster-id:hover{background:rgba(129,140,248,.18);}
  .outlier-cluster-link{background:none;border:1px solid rgba(129,140,248,.35);border-radius:4px;color:var(--accent);font-size:11px;font-weight:600;padding:3px 9px;cursor:pointer;display:inline-flex;align-items:center;gap:4px;white-space:nowrap;flex-shrink:0;}
  .outlier-cluster-link:hover{background:rgba(129,140,248,.20);}
  .outlier-cluster-lbl{font-size:11px;color:var(--muted);font-style:italic;flex-shrink:0;}
  table.outlier-tbl{width:100%;border-collapse:collapse;font-size:12px;background:var(--surface);}
  table.outlier-tbl thead th{text-align:left;padding:6px 12px;color:var(--muted);font-size:10px;text-transform:uppercase;letter-spacing:.06em;border-bottom:1px solid var(--border);background:var(--surface3);}
  table.outlier-tbl td{padding:7px 12px;border-bottom:1px solid var(--border);vertical-align:top;}
  table.outlier-tbl tr:last-child td{border-bottom:none;}
  table.outlier-tbl tr:hover td{background:var(--surface2);}
  table.outlier-tbl .token-seq{flex-wrap:wrap;max-width:340px;row-gap:3px;}
  .diff-chips{display:flex;flex-wrap:wrap;gap:3px;}
  .diff-add{background:rgba(52,211,153,.12);color:var(--green);border:1px solid rgba(52,211,153,.25);border-radius:3px;padding:1px 5px;font-size:10px;font-family:'JetBrains Mono','Fira Code',monospace;white-space:nowrap;}
  .diff-rem{background:rgba(248,113,113,.10);color:var(--red);border:1px solid rgba(248,113,113,.20);border-radius:3px;padding:1px 5px;font-size:10px;font-family:'JetBrains Mono','Fira Code',monospace;white-space:nowrap;}

  /* potential outlier badge + tooltip */
  .pot-badge{display:inline-flex;align-items:center;gap:4px;background:rgba(96,165,250,.12);color:var(--accent2);border:1px solid rgba(96,165,250,.28);border-radius:12px;padding:2px 8px;font-size:11px;font-weight:600;cursor:pointer;user-select:none;white-space:nowrap;}
  .pot-badge:hover{background:rgba(96,165,250,.22);}
  .pot-badge .pot-icon{font-size:10px;opacity:.7;}
  #pot-tooltip{position:fixed;z-index:9999;background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:10px 14px;box-shadow:0 8px 24px rgba(0,0,0,.5);display:none;max-width:480px;min-width:220px;pointer-events:none;}
  #pot-tooltip.visible{display:block;}
  #pot-tooltip .pot-tt-title{font-size:10px;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);font-weight:600;margin-bottom:7px;}
  #pot-tooltip .pot-tt-item{font-size:11px;color:var(--accent2);font-family:'JetBrains Mono','Fira Code',monospace;padding:2px 0;word-break:break-all;}
  /* column header info icon */
  th[data-tip]{position:relative;cursor:help;}
  th[data-tip]::after{content:'ⓘ';margin-left:4px;font-size:10px;color:var(--muted);opacity:.6;vertical-align:middle;font-style:normal;}
  th[data-tip]:hover::after{color:var(--accent);opacity:1;}
  #col-tip{position:fixed;background:var(--surface2);border:1px solid var(--border);border-radius:6px;padding:8px 12px;font-size:12px;color:var(--fg);max-width:320px;line-height:1.5;pointer-events:none;z-index:10000;box-shadow:0 4px 16px rgba(0,0,0,.35);display:none;white-space:pre-wrap;}
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
    <div class="card" style="border-color:rgba(52,211,153,.35);">
      <div class="lbl" style="color:var(--green);">High Structural Confidence Score</div>
      <div class="val" style="color:var(--green);">{{len .HighClusters}}</div>
      <div class="sub">std &lt; 0.05 — tight</div>
    </div>
    <div class="card" style="border-color:rgba(251,191,36,.30);">
      <div class="lbl" style="color:var(--yellow);">Medium Structural Confidence Score</div>
      <div class="val" style="color:var(--yellow);">{{len .MediumClusters}}</div>
      <div class="sub">0.05 ≤ std &lt; 0.12</div>
    </div>
    <div class="card" style="border-color:rgba(248,113,113,.25);">
      <div class="lbl" style="color:var(--red);">Low Structural Confidence Score</div>
      <div class="val" style="color:var(--red);">{{len .LowClusters}}</div>
      <div class="sub">std ≥ 0.12 — broad</div>
    </div>
  </div>

  <details class="legend">
    <summary>Metric Glossary</summary>
    <div class="legend-body">

      <!-- ── FIRST: Tier + Confidence Score ────────────────────────────── -->
      <div class="lg-item" style="grid-column:1/-1;padding-bottom:8px;margin-bottom:4px;border-bottom:1px solid var(--border);">
        <span class="lg-term" style="color:var(--accent);font-size:11px;text-transform:uppercase;letter-spacing:.07em;">Structural Conformity &amp; Confidence</span>
      </div>

      <div class="lg-item" style="grid-column:1/-1;">
        <span class="lg-term" style="color:var(--green);">Structural Conformity Tiers</span>
        <span class="lg-def">
          Derived from the <strong style="color:var(--text);">standard deviation of arithmetic pairwise scores</strong>
          — (AST token-sequence similarity + import Jaccard + call-target Jaccard) / 3 — computed across all member pairs.
          Thresholds are calibrated to the natural spread of structural clusters (std devs typically 0.00–0.15).<br/>
          <strong style="color:var(--green);">High Structural Conformity Score (High StrcScore)</strong> (std &lt; 0.05): members are near-identical — the cluster encodes a very tight, settled convention.<br/>
          <strong style="color:var(--yellow);">Medium Structural Conformity Score (Medium StrcScore)</strong> (0.05 ≤ std &lt; 0.12): consistent pattern with variation — same structural family, some drift.<br/>
          <strong style="color:var(--red);">Low Structural Conformity Score (Low StrcScore)</strong> (std ≥ 0.12): broad structural family — shape is shared but with significant internal variation.
        </span>
      </div>

      <div class="lg-item" style="grid-column:1/-1;">
        <span class="lg-term" style="color:var(--accent2);">Confidence Score</span>
        <span class="lg-def">
          <strong style="color:var(--text);">ln(size) × ln(numPackages+1) × confidence(tier) × meanScore² × (importCoh + callCoh) / 2</strong> — a composite ranking signal.<br/>
          <strong style="color:var(--text);">ln(size)</strong>: logarithmic prevalence weight — size matters, but diminishingly.<br/>
          <strong style="color:var(--text);">ln(numPackages+1)</strong>: package-spread weight — patterns recurring across more packages are stronger signals of an established codebase-wide convention.<br/>
          <strong style="color:var(--text);">confidence</strong>: tier weight — High StrcScore = 1.0 / Medium StrcScore = 0.6 / Low StrcScore = 0.3.<br/>
          <strong style="color:var(--text);">meanScore²</strong>: structural tightness, squared — penalises loose clusters disproportionately; a score of 0.90 outweighs 0.75 by more than the raw difference suggests.<br/>
          <strong style="color:var(--text);">(importCoh + callCoh) / 2</strong>: coherence factor — rewards clusters where members share both import domain and call vocabulary, the strongest signal of a real settled convention.<br/>
          Within each tier tab, clusters are sorted descending by this score.
        </span>
      </div>

      <!-- ── Cluster metrics ─────────────────────────────────────────────── -->
      <div class="lg-item" style="grid-column:1/-1;padding-bottom:6px;margin-bottom:2px;border-bottom:1px solid var(--border);margin-top:10px;">
        <span class="lg-term" style="color:var(--accent);font-size:11px;text-transform:uppercase;letter-spacing:.07em;">Cluster metrics</span>
      </div>

      <div class="lg-item"><span class="lg-term">ID</span><span class="lg-def">First 6 hex characters of the ShapeHash — stable across <code>beats init</code> runs. Use this to reference a cluster in conversations or with teammates.</span></div>
      <div class="lg-item"><span class="lg-term">Common Shape</span><span class="lg-def">The longest token subsequence present in <em>every</em> member of the cluster — the structural skeleton they all share. A longer shape means the cluster has a richer shared convention.</span></div>
      <div class="lg-item"><span class="lg-term">Avg Pairwise Score</span><span class="lg-def">Mean ∛(seqSim × importJaccard × callJaccard) over every unique member pair. The cube root is a geometric mean — all three dimensions must contribute; a zero on any one collapses the score to zero.</span></div>
      <div class="lg-item"><span class="lg-term">Member Score</span><span class="lg-def">Each function's mean pairwise score against every other member. The lowest-scoring members are outlier candidates. A large gap between top and bottom member scores is a sign the cluster should be split.</span></div>
      <div class="lg-item"><span class="lg-term">Import Coh.</span><span class="lg-def">Mean pairwise Jaccard of direct import sets. High (≥ 0.60) means the cluster operates in a shared package domain.</span></div>
      <div class="lg-item"><span class="lg-term">Call Coh.</span><span class="lg-def">Mean pairwise Jaccard of call-target sets. High (≥ 0.60) means the cluster uses the same external vocabulary — a strong signal of shared structural role.</span></div>

      <!-- ── Token reference ─────────────────────────────────────────────── -->
      <div class="lg-item" style="grid-column:1/-1;padding-bottom:6px;margin-bottom:2px;border-bottom:1px solid var(--border);margin-top:10px;">
        <span class="lg-term" style="color:var(--accent);font-size:11px;text-transform:uppercase;letter-spacing:.07em;">Token reference</span>
        <span class="lg-def" style="margin-left:8px;">Every function is reduced to an ordered sequence of these tokens — no names, no literals, only structure.</span>
      </div>

      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CALL</span><span class="lg-def">A plain local function call or builtin (<code>make()</code>, <code>len()</code>) or type conversion.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CALL_PKG</span><span class="lg-def">A package-qualified call — the receiver is an imported package alias, e.g. <code>fmt.Sprintf()</code>.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">CALL_METHOD</span><span class="lg-def">A method or chained call — the receiver is a variable or struct field, e.g. <code>w.Close()</code>.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">ASSIGN</span><span class="lg-def">A variable assignment or short variable declaration (<code>:=</code> or <code>=</code>).</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">RETURN</span><span class="lg-def">A return statement. Each return value appends one RETURN token, encoding output arity into the shape.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">IF</span><span class="lg-def">An if statement (including <code>if err != nil</code> guards). High frequency indicates defensive/error-handling code.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">FOR / RANGE</span><span class="lg-def">C-style for loop vs range-based iteration. RANGE is the most common iteration token in idiomatic Go.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">SWITCH / CASE</span><span class="lg-def">Switch statement with CASE tokens for each branch, encoding branching arity.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">DEFER</span><span class="lg-def">A defer statement. Signals resource cleanup or unlock patterns.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">GO / SEND / SELECT</span><span class="lg-def">Goroutine spawn, channel send, and channel multiplexing. Presence indicates concurrency in the control flow.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">FUNCLIT</span><span class="lg-def">An anonymous function / closure, e.g. passed as a callback or used in a goroutine launch.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">COMPOSITE_LIT</span><span class="lg-def">A composite literal — struct, slice, map, or array initialisation, e.g. <code>&amp;Foo{}</code>, <code>[]string{...}</code>. High frequency indicates constructor-style or data-builder functions.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">BINARY_OP</span><span class="lg-def">A binary expression — arithmetic, comparison, or logical operator (<code>+</code>, <code>==</code>, <code>&amp;&amp;</code>, etc.). Distinguishes computation-heavy functions from pure dispatch/delegation shapes.</span></div>
      <div class="lg-item"><span class="lg-term" style="color:var(--accent2);font-family:monospace;">TYPE_ASSERT</span><span class="lg-def">A type assertion or type switch arm — <code>x.(T)</code>. Signals interface-unwrapping patterns common in middleware, codec, or handler code.</span></div>

      <div class="lg-item" style="grid-column:1/-1;margin-top:10px;padding-top:9px;border-top:1px solid var(--border);">
        <span class="lg-term" style="color:var(--muted);">Filtered noise</span>
        <span class="lg-def">Clusters whose trigram shape matches ≥ 5% of the entire corpus are suppressed — structural stop-words (e.g. a bare RETURN function) with no discriminating signal. They are excluded from all three tier tabs.</span>
      </div>
    </div>
  </details>

  <div class="dist-section">
    <div class="dist-grid" style="grid-template-columns:1fr 1fr;">
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

  <details class="legend" style="margin-bottom:20px;">
    <summary>Potential Outlier Charts</summary>
    <div style="padding:4px 16px 12px;">

  <div class="dist-section">
    <div class="dist-title">Package coverage — clustered vs outliers (top 20 by volume)</div>
    <div style="margin-top:8px;">
{{range .PackageCoverage}}
      <div class="dist-row" style="margin-bottom:4px;">
        <span class="dist-label" style="min-width:180px;font-size:11px;font-family:monospace;">{{.Package}}</span>
        <div class="dist-bar-bg" style="position:relative;flex:1;">
          <div class="dist-bar-fill" style="width:{{.ClusteredPct}}%;background:var(--accent3);position:absolute;top:0;left:0;height:100%;"></div>
          <div class="dist-bar-fill" style="width:{{.OutliersPct}}%;background:var(--red);opacity:0.7;position:absolute;top:0;left:0;height:100%;"></div>
        </div>
        <span class="dist-count" style="min-width:80px;text-align:right;font-size:11px;">{{.Clustered}} / {{.Outliers}}</span>
      </div>
{{end}}
      <p class="dist-explain" style="margin-top:8px;">
        <span style="color:var(--accent3);">■</span> clustered &nbsp;
        <span style="color:var(--red);">■</span> structural outliers &nbsp;·&nbsp;
        counts shown as clustered / outliers per package.
      </p>
    </div>
  </div>

{{if .DeltaDirectionDist}}
  <div class="dist-section" style="display:grid;grid-template-columns:1fr 1fr;gap:20px;">
    <div>
      <div class="dist-title">Outlier delta direction</div>
{{range .DeltaDirectionDist}}
      <div class="dist-row">
        <span class="dist-label">{{.Label}}</span>
        <div class="dist-bar-bg"><div class="dist-bar-fill" style="width:{{.Width}}%;background:{{.Color}}"></div></div>
        <span class="dist-count">{{.Count}}</span>
      </div>
{{end}}
      <p class="dist-explain">Functions missing something peers have (−) are the strongest bug signal. Functions that extend peers (+) are usually intentional.</p>
    </div>
    <div>
      <div class="dist-title">Token types missing from outliers vs peers</div>
{{range .TokenFreqDist}}
      <div class="dist-row">
        <span class="dist-label" style="font-family:monospace;font-size:11px;">{{.Token}}</span>
        <div class="dist-bar-bg"><div class="dist-bar-fill" style="width:{{.Width}}%;background:var(--red)"></div></div>
        <span class="dist-count">{{.Count}}</span>
      </div>
{{end}}
      <p class="dist-explain">Token types most frequently absent in outliers compared to their cluster peers — dominant types suggest a systemic missing pattern.</p>
    </div>
  </div>
{{end}}

    </div>
  </details>

{{if .OutlierGroups}}
  <details class="legend" style="margin-bottom:20px;">
    <summary>Potential Deviations <span style="font-size:10px;font-weight:400;color:var(--muted);margin-left:6px;">{{.TotalOutliers}} function(s) across {{len .OutlierGroups}} cluster(s) — functions that did not join but score close</span></summary>
    <div class="outlier-body">
{{range .OutlierGroups}}
      <details class="outlier-group" open>
        <summary class="outlier-group-hdr">
          <code class="outlier-cluster-id" title="Click to select · Ctrl+C to copy">{{.ClusterID}}</code>
          <button class="outlier-cluster-link" onclick="event.stopPropagation();jumpToCluster('{{.ClusterID}}')">↗ jump</button>
          {{if .ClusterLabel}}<span class="outlier-cluster-lbl">{{.ClusterLabel}}</span>{{end}}
          <span style="font-size:10px;color:var(--muted);font-weight:600;letter-spacing:.04em;white-space:nowrap;">Longest Common Subsequence:</span>{{tokenChips .CommonShape}}
        </summary>
        <table class="outlier-tbl">
          <thead><tr>
            <th>Function</th>
            <th>Package</th>
            <th>File : Line</th>
            <th title="This function's full token sequence — compare to cluster LCS above to see where the structure diverges">Token Shape</th>
            <th title="Token types present in this function but absent from the cluster LCS (+), or in the LCS but not here (−)">Token Δ</th>
            <th title="Imports present in this function but not in cluster's common imports (+), or vice versa (−)">Import Δ</th>
            <th title="Call targets present in this function but not in cluster's common calls (+), or vice versa (−)">Call Δ</th>
            <th title="Cyclomatic complexity of this function minus the cluster mean. Positive = more complex than the cluster average; negative = simpler.">Cyclo Δ</th>
          </tr></thead>
          <tbody>
{{range .Outliers}}
            <tr>
              <td><span class="fn-name">{{.Name}}</span></td>
              <td><span class="fn-pkg">{{.Package}}</span></td>
              <td><span class="fn-file" title="{{shortPath .FilePath}}">{{shortPath .FilePath}}:{{.Line}}</span></td>
              <td>{{tokenChips .TokenShape}}</td>
              <td>{{diffChips .TokensAdded .TokensRemoved}}</td>
              <td>{{diffChips .ImportsAdded .ImportsRemoved}}</td>
              <td>{{diffChips .CallsAdded .CallsRemoved}}</td>
              <td style="font-variant-numeric:tabular-nums;font-size:11px;white-space:nowrap;">{{signedDelta .CycloDelta}}</td>
            </tr>
{{end}}
          </tbody>
        </table>
      </details>
{{end}}
    </div>
  </details>
{{end}}

  <div class="sec-bar">
    <div class="sec">Clusters — sorted by confidence score ↓</div>
    <div class="search-wrap">
      <span class="search-icon" title="Filter by cluster ID">⌗</span>
      <input type="text" id="id-search" class="search-input" placeholder="Cluster ID…" autocomplete="off" spellcheck="false"/>
      <button id="id-search-clear" class="search-clear" title="Clear">✕</button>
      <span class="search-divider">·</span>
      <span class="search-icon">⌕</span>
      <input type="text" id="fn-search" class="search-input" placeholder="Function name…" autocomplete="off" spellcheck="false"/>
      <button id="fn-search-clear" class="search-clear" title="Clear">✕</button>
      <span class="search-divider">·</span>
      <input type="text" id="file-search" class="search-input" placeholder="File path…" autocomplete="off" spellcheck="false"/>
      <button id="file-search-clear" class="search-clear" title="Clear">✕</button>
      <span id="search-count" class="search-count"></span>
    </div>
  </div>

  <div class="tier-tabs" id="tier-tabs">
    <button class="tier-tab active" data-tier="high" onclick="switchTab('high')">High StrcScore<span class="tab-count">{{len .HighClusters}}</span></button>
    <button class="tier-tab" data-tier="medium" onclick="switchTab('medium')">Medium StrcScore<span class="tab-count">{{len .MediumClusters}}</span></button>
    <button class="tier-tab" data-tier="low" onclick="switchTab('low')">Low StrcScore<span class="tab-count">{{len .LowClusters}}</span></button>
  </div>

  <!-- ── HIGH tab ─────────────────────────────────────────────────────────── -->
  <div class="tier-panel" id="panel-high">
    <div class="tbl-scroll-hint"><svg width="12" height="12" viewBox="0 0 12 12" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M6 2v8M6 10l-2.5-2.5M6 10l2.5-2.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg> scroll to explore</div>
    <div class="tbl-wrap">
    <table class="clusters" id="tbl-high">
      <thead><tr>
        <th data-col="ordinal" style="width:32px;" title="Ordinal rank within this tier, sorted by confidence score">#</th>
        <th data-col="stableid" style="width:70px;" title="First 6 hex chars of ShapeHash — stable across runs. Use to reference a cluster in conversations or issues.">ID</th>
        <th data-col="shape" title="Longest token subsequence shared by every member — the structural skeleton they all share. Longer = richer shared convention.">Common Shape</th>
        <th data-col="size" title="Number of functions in this cluster">Size</th>
        <th data-col="avgscore" title="Mean ∛(seqSim × importJaccard × callJaccard) over all member pairs. Geometric mean — a zero on any dimension collapses to zero.">Avg Score</th>
        <th data-col="confidence" class="sorted-desc" title="ln(size) × ln(packages+1) × tier_weight × meanScore² × (importCoh+callCoh)/2. Composite ranking signal — size, spread, tightness, and coherence combined.">CScore</th>
        <th data-col="coherence" title="Mean pairwise Jaccard of direct import sets. ≥0.60 = members share a package domain.">Import Coh.</th>
        <th data-col="callcoherence" title="Mean pairwise Jaccard of call-target sets. ≥0.60 = members share a function vocabulary — strongest signal of a settled convention.">Call Coh.</th>
        <th data-col="packages" title="Unique packages containing cluster members. More packages = pattern is codebase-wide, not localised.">Packages</th>
        <th data-col="potential" title="Orphaned functions with structural affinity to this cluster — did not join but scored close. Click badge to see names.">Potential</th>
      </tr></thead>
      <tbody>
{{if not .HighClusters}}<tr><td colspan="10" class="no-clusters">No high-conformity clusters in this index.</td></tr>{{end}}
{{range $i, $cl := .HighClusters}}
      <tr class="cl-row" data-idx="{{$i}}" data-stableid="{{$cl.StableID}}" onclick="toggleRow('high',{{$i}})">
        <td class="cl-ordinal">{{$cl.Ordinal}}</td>
        <td><span class="cl-stableid">{{$cl.StableID}}</span></td>
        <td><span class="caret">▶</span>{{tokenChips $cl.CommonShape}}<span class="cl-hash">{{$cl.ShapeHash}}{{if $cl.Label}} · {{$cl.Label}}{{end}}</span></td>
        <td class="num" style="vertical-align:middle;">{{$cl.Size}}</td>
        <td style="vertical-align:middle;"><span class="badge {{scoreBadge $cl.AvgPairwiseScore}}">{{f3 $cl.AvgPairwiseScore}}</span></td>
        <td style="vertical-align:middle;"><div class="attn-wrap"><span class="attn-num">{{f2 $cl.ConfidenceScore}}</span><div class="attn-bar-bg"><div class="attn-bar-fill" style="width:{{confidenceBar $cl.ConfidenceScore}}%"></div></div></div></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.Coherence}}">{{f2 $cl.Coherence}}</span></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.CallCoherence}}">{{f2 $cl.CallCoherence}}</span></td>
        <td style="vertical-align:middle;"><div class="pkg-pills">{{range $cl.Packages}}<span class="pkg-pill">{{.}}</span>{{end}}</div></td>
        <td style="vertical-align:middle;padding:6px 10px;">{{if $cl.Potentials}}<span class="pot-badge" onclick="showPotTooltip(event,this)" data-names="{{range $cl.Potentials}}{{.}}|{{end}}"><span class="pot-icon">⚑</span>{{len $cl.Potentials}}</span>{{end}}</td>
      </tr>
      <tr class="cl-detail" id="detail-high-{{$i}}">
        <td colspan="10"><div class="detail-panel">
          <div class="detail-meta">
            <span class="detail-meta-item">top imports: <strong>{{joinComma $cl.TopImports}}</strong></span>
            <span class="detail-meta-item">cyclo mean: <strong>{{f1 $cl.CycloMean}}</strong></span>
          </div>
          <div class="members-wrap"><table class="members"><thead><tr><th>Function</th><th>Package</th><th>File</th><th>Line</th><th>Score</th></tr></thead>
          <tbody>
{{range $cl.Members}}
            <tr><td><span class="fn-name">{{.Name}}</span></td><td><span class="fn-pkg">{{.Package}}</span></td>
              <td><span class="fn-file" title="{{shortPath .FilePath}}">{{shortPath .FilePath}}</span></td>
              <td><span class="fn-line">{{.Line}}</span></td>
              <td><div class="score-wrap"><span class="score-num {{scoreBadge .PairwiseScore}}">{{f3 .PairwiseScore}}</span><div class="score-bar-bg"><div class="score-bar-fill {{scoreBadge .PairwiseScore}}" style="width:{{scorePct .PairwiseScore}}%"></div></div></div></td>
            </tr>
{{end}}
          </tbody></table></div>
{{if or $cl.SemanticIdiom $cl.Verdict $cl.SuggestedAction $cl.SearchQuestions}}
          <div class="enrich-panel">
{{if $cl.SemanticIdiom}}<div class="enrich-row"><span class="enrich-key">Idiom</span><span class="enrich-val">{{$cl.SemanticIdiom}}{{if $cl.Confidence}} &nbsp;<span class="conf-{{$cl.Confidence}}">{{$cl.Confidence}}</span>{{end}}</span></div>{{end}}
{{if $cl.CanonicalMember}}<div class="enrich-row"><span class="enrich-key">Canonical</span><span class="enrich-val"><code style="font-size:11px;font-family:monospace;color:var(--accent2)">{{$cl.CanonicalMember}}</code></span></div>{{end}}
{{if $cl.Verdict}}<div class="enrich-row"><span class="enrich-key">Verdict</span><span class="enrich-val">{{$cl.Verdict}}</span></div>{{end}}
{{if $cl.SuggestedAction}}<div class="enrich-row"><span class="enrich-key">Action</span><span class="enrich-val {{if eq $cl.SuggestedAction "none"}}action-none{{else}}action-attention{{end}}">{{$cl.SuggestedAction}}</span></div>{{end}}
{{if $cl.SearchQuestions}}<div class="enrich-row"><span class="enrich-key">Questions</span><span class="enrich-val"><div class="sq-list">{{range $cl.SearchQuestions}}<div class="sq-item">{{.}}</div>{{end}}</div></span></div>{{end}}
          </div>
{{end}}
        </div></td>
      </tr>
{{end}}
      </tbody>
    </table>
    </div>
  </div>

  <!-- ── MEDIUM tab ───────────────────────────────────────────────────────── -->
  <div class="tier-panel hidden" id="panel-medium">
    <div class="tbl-scroll-hint"><svg width="12" height="12" viewBox="0 0 12 12" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M6 2v8M6 10l-2.5-2.5M6 10l2.5-2.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg> scroll to explore</div>
    <div class="tbl-wrap">
    <table class="clusters" id="tbl-medium">
      <thead><tr>
        <th data-col="ordinal" style="width:32px;" title="Ordinal rank within this tier, sorted by confidence score">#</th>
        <th data-col="stableid" style="width:70px;" title="First 6 hex chars of ShapeHash — stable across runs. Use to reference a cluster in conversations or issues.">ID</th>
        <th data-col="shape" title="Longest token subsequence shared by every member — the structural skeleton they all share. Longer = richer shared convention.">Common Shape</th>
        <th data-col="size" title="Number of functions in this cluster">Size</th>
        <th data-col="avgscore" title="Mean ∛(seqSim × importJaccard × callJaccard) over all member pairs. Geometric mean — a zero on any dimension collapses to zero.">Avg Score</th>
        <th data-col="confidence" class="sorted-desc" title="ln(size) × ln(packages+1) × tier_weight × meanScore² × (importCoh+callCoh)/2. Composite ranking signal — size, spread, tightness, and coherence combined.">CScore</th>
        <th data-col="coherence" title="Mean pairwise Jaccard of direct import sets. ≥0.60 = members share a package domain.">Import Coh.</th>
        <th data-col="callcoherence" title="Mean pairwise Jaccard of call-target sets. ≥0.60 = members share a function vocabulary — strongest signal of a settled convention.">Call Coh.</th>
        <th data-col="packages" title="Unique packages containing cluster members. More packages = pattern is codebase-wide, not localised.">Packages</th>
        <th data-col="potential" title="Orphaned functions with structural affinity to this cluster — did not join but scored close. Click badge to see names.">Potential</th>
      </tr></thead>
      <tbody>
{{if not .MediumClusters}}<tr><td colspan="10" class="no-clusters">No medium-conformity clusters in this index.</td></tr>{{end}}
{{range $i, $cl := .MediumClusters}}
      <tr class="cl-row" data-idx="{{$i}}" data-stableid="{{$cl.StableID}}" onclick="toggleRow('medium',{{$i}})">
        <td class="cl-ordinal">{{$cl.Ordinal}}</td>
        <td><span class="cl-stableid">{{$cl.StableID}}</span></td>
        <td><span class="caret">▶</span>{{tokenChips $cl.CommonShape}}<span class="cl-hash">{{$cl.ShapeHash}}{{if $cl.Label}} · {{$cl.Label}}{{end}}</span></td>
        <td class="num" style="vertical-align:middle;">{{$cl.Size}}</td>
        <td style="vertical-align:middle;"><span class="badge {{scoreBadge $cl.AvgPairwiseScore}}">{{f3 $cl.AvgPairwiseScore}}</span></td>
        <td style="vertical-align:middle;"><div class="attn-wrap"><span class="attn-num">{{f2 $cl.ConfidenceScore}}</span><div class="attn-bar-bg"><div class="attn-bar-fill" style="width:{{confidenceBar $cl.ConfidenceScore}}%"></div></div></div></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.Coherence}}">{{f2 $cl.Coherence}}</span></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.CallCoherence}}">{{f2 $cl.CallCoherence}}</span></td>
        <td style="vertical-align:middle;"><div class="pkg-pills">{{range $cl.Packages}}<span class="pkg-pill">{{.}}</span>{{end}}</div></td>
        <td style="vertical-align:middle;padding:6px 10px;">{{if $cl.Potentials}}<span class="pot-badge" onclick="showPotTooltip(event,this)" data-names="{{range $cl.Potentials}}{{.}}|{{end}}"><span class="pot-icon">⚑</span>{{len $cl.Potentials}}</span>{{end}}</td>
      </tr>
      <tr class="cl-detail" id="detail-medium-{{$i}}">
        <td colspan="10"><div class="detail-panel">
          <div class="detail-meta">
            <span class="detail-meta-item">top imports: <strong>{{joinComma $cl.TopImports}}</strong></span>
            <span class="detail-meta-item">cyclo mean: <strong>{{f1 $cl.CycloMean}}</strong></span>
          </div>
          <div class="members-wrap"><table class="members"><thead><tr><th>Function</th><th>Package</th><th>File</th><th>Line</th><th>Score</th></tr></thead>
          <tbody>
{{range $cl.Members}}
            <tr><td><span class="fn-name">{{.Name}}</span></td><td><span class="fn-pkg">{{.Package}}</span></td>
              <td><span class="fn-file" title="{{shortPath .FilePath}}">{{shortPath .FilePath}}</span></td>
              <td><span class="fn-line">{{.Line}}</span></td>
              <td><div class="score-wrap"><span class="score-num {{scoreBadge .PairwiseScore}}">{{f3 .PairwiseScore}}</span><div class="score-bar-bg"><div class="score-bar-fill {{scoreBadge .PairwiseScore}}" style="width:{{scorePct .PairwiseScore}}%"></div></div></div></td>
            </tr>
{{end}}
          </tbody></table></div>
{{if or $cl.SemanticIdiom $cl.Verdict $cl.SuggestedAction $cl.SearchQuestions}}
          <div class="enrich-panel">
{{if $cl.SemanticIdiom}}<div class="enrich-row"><span class="enrich-key">Idiom</span><span class="enrich-val">{{$cl.SemanticIdiom}}{{if $cl.Confidence}} &nbsp;<span class="conf-{{$cl.Confidence}}">{{$cl.Confidence}}</span>{{end}}</span></div>{{end}}
{{if $cl.CanonicalMember}}<div class="enrich-row"><span class="enrich-key">Canonical</span><span class="enrich-val"><code style="font-size:11px;font-family:monospace;color:var(--accent2)">{{$cl.CanonicalMember}}</code></span></div>{{end}}
{{if $cl.Verdict}}<div class="enrich-row"><span class="enrich-key">Verdict</span><span class="enrich-val">{{$cl.Verdict}}</span></div>{{end}}
{{if $cl.SuggestedAction}}<div class="enrich-row"><span class="enrich-key">Action</span><span class="enrich-val {{if eq $cl.SuggestedAction "none"}}action-none{{else}}action-attention{{end}}">{{$cl.SuggestedAction}}</span></div>{{end}}
{{if $cl.SearchQuestions}}<div class="enrich-row"><span class="enrich-key">Questions</span><span class="enrich-val"><div class="sq-list">{{range $cl.SearchQuestions}}<div class="sq-item">{{.}}</div>{{end}}</div></span></div>{{end}}
          </div>
{{end}}
        </div></td>
      </tr>
{{end}}
      </tbody>
    </table>
    </div>
  </div>

  <!-- ── LOW tab ──────────────────────────────────────────────────────────── -->
  <div class="tier-panel hidden" id="panel-low">
    <div class="tbl-scroll-hint"><svg width="12" height="12" viewBox="0 0 12 12" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M6 2v8M6 10l-2.5-2.5M6 10l2.5-2.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg> scroll to explore</div>
    <div class="tbl-wrap">
    <table class="clusters" id="tbl-low">
      <thead><tr>
        <th data-col="ordinal" style="width:32px;" title="Ordinal rank within this tier, sorted by confidence score">#</th>
        <th data-col="stableid" style="width:70px;" title="First 6 hex chars of ShapeHash — stable across runs. Use to reference a cluster in conversations or issues.">ID</th>
        <th data-col="shape" title="Longest token subsequence shared by every member — the structural skeleton they all share. Longer = richer shared convention.">Common Shape</th>
        <th data-col="size" title="Number of functions in this cluster">Size</th>
        <th data-col="avgscore" title="Mean ∛(seqSim × importJaccard × callJaccard) over all member pairs. Geometric mean — a zero on any dimension collapses to zero.">Avg Score</th>
        <th data-col="confidence" class="sorted-desc" title="ln(size) × ln(packages+1) × tier_weight × meanScore² × (importCoh+callCoh)/2. Composite ranking signal — size, spread, tightness, and coherence combined.">CScore</th>
        <th data-col="coherence" title="Mean pairwise Jaccard of direct import sets. ≥0.60 = members share a package domain.">Import Coh.</th>
        <th data-col="callcoherence" title="Mean pairwise Jaccard of call-target sets. ≥0.60 = members share a function vocabulary — strongest signal of a settled convention.">Call Coh.</th>
        <th data-col="packages" title="Unique packages containing cluster members. More packages = pattern is codebase-wide, not localised.">Packages</th>
        <th data-col="potential" title="Orphaned functions with structural affinity to this cluster — did not join but scored close. Click badge to see names.">Potential</th>
      </tr></thead>
      <tbody>
{{if not .LowClusters}}<tr><td colspan="10" class="no-clusters">No low-conformity clusters in this index.</td></tr>{{end}}
{{range $i, $cl := .LowClusters}}
      <tr class="cl-row" data-idx="{{$i}}" data-stableid="{{$cl.StableID}}" onclick="toggleRow('low',{{$i}})">
        <td class="cl-ordinal">{{$cl.Ordinal}}</td>
        <td><span class="cl-stableid">{{$cl.StableID}}</span></td>
        <td><span class="caret">▶</span>{{tokenChips $cl.CommonShape}}<span class="cl-hash">{{$cl.ShapeHash}}{{if $cl.Label}} · {{$cl.Label}}{{end}}</span></td>
        <td class="num" style="vertical-align:middle;">{{$cl.Size}}</td>
        <td style="vertical-align:middle;"><span class="badge {{scoreBadge $cl.AvgPairwiseScore}}">{{f3 $cl.AvgPairwiseScore}}</span></td>
        <td style="vertical-align:middle;"><div class="attn-wrap"><span class="attn-num">{{f2 $cl.ConfidenceScore}}</span><div class="attn-bar-bg"><div class="attn-bar-fill" style="width:{{confidenceBar $cl.ConfidenceScore}}%"></div></div></div></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.Coherence}}">{{f2 $cl.Coherence}}</span></td>
        <td style="vertical-align:middle;"><span class="badge {{badgeClass $cl.CallCoherence}}">{{f2 $cl.CallCoherence}}</span></td>
        <td style="vertical-align:middle;"><div class="pkg-pills">{{range $cl.Packages}}<span class="pkg-pill">{{.}}</span>{{end}}</div></td>
        <td style="vertical-align:middle;padding:6px 10px;">{{if $cl.Potentials}}<span class="pot-badge" onclick="showPotTooltip(event,this)" data-names="{{range $cl.Potentials}}{{.}}|{{end}}"><span class="pot-icon">⚑</span>{{len $cl.Potentials}}</span>{{end}}</td>
      </tr>
      <tr class="cl-detail" id="detail-low-{{$i}}">
        <td colspan="10"><div class="detail-panel">
          <div class="detail-meta">
            <span class="detail-meta-item">top imports: <strong>{{joinComma $cl.TopImports}}</strong></span>
            <span class="detail-meta-item">cyclo mean: <strong>{{f1 $cl.CycloMean}}</strong></span>
          </div>
          <div class="members-wrap"><table class="members"><thead><tr><th>Function</th><th>Package</th><th>File</th><th>Line</th><th>Score</th></tr></thead>
          <tbody>
{{range $cl.Members}}
            <tr><td><span class="fn-name">{{.Name}}</span></td><td><span class="fn-pkg">{{.Package}}</span></td>
              <td><span class="fn-file" title="{{shortPath .FilePath}}">{{shortPath .FilePath}}</span></td>
              <td><span class="fn-line">{{.Line}}</span></td>
              <td><div class="score-wrap"><span class="score-num {{scoreBadge .PairwiseScore}}">{{f3 .PairwiseScore}}</span><div class="score-bar-bg"><div class="score-bar-fill {{scoreBadge .PairwiseScore}}" style="width:{{scorePct .PairwiseScore}}%"></div></div></div></td>
            </tr>
{{end}}
          </tbody></table></div>
{{if or $cl.SemanticIdiom $cl.Verdict $cl.SuggestedAction $cl.SearchQuestions}}
          <div class="enrich-panel">
{{if $cl.SemanticIdiom}}<div class="enrich-row"><span class="enrich-key">Idiom</span><span class="enrich-val">{{$cl.SemanticIdiom}}{{if $cl.Confidence}} &nbsp;<span class="conf-{{$cl.Confidence}}">{{$cl.Confidence}}</span>{{end}}</span></div>{{end}}
{{if $cl.CanonicalMember}}<div class="enrich-row"><span class="enrich-key">Canonical</span><span class="enrich-val"><code style="font-size:11px;font-family:monospace;color:var(--accent2)">{{$cl.CanonicalMember}}</code></span></div>{{end}}
{{if $cl.Verdict}}<div class="enrich-row"><span class="enrich-key">Verdict</span><span class="enrich-val">{{$cl.Verdict}}</span></div>{{end}}
{{if $cl.SuggestedAction}}<div class="enrich-row"><span class="enrich-key">Action</span><span class="enrich-val {{if eq $cl.SuggestedAction "none"}}action-none{{else}}action-attention{{end}}">{{$cl.SuggestedAction}}</span></div>{{end}}
{{if $cl.SearchQuestions}}<div class="enrich-row"><span class="enrich-key">Questions</span><span class="enrich-val"><div class="sq-list">{{range $cl.SearchQuestions}}<div class="sq-item">{{.}}</div>{{end}}</div></span></div>{{end}}
          </div>
{{end}}
        </div></td>
      </tr>
{{end}}
      </tbody>
    </table>
    </div>
  </div>

</div>

<footer>beats · vocabulary-independent structural fingerprinting for Go · {{.GeneratedAt}}</footer>

<div id="pot-tooltip"><div class="pot-tt-title">Potential outliers</div><div id="pot-tt-body"></div></div>

<script>
// ── tab switching ──────────────────────────────────────────────────────────────
function switchTab(tier) {
  document.querySelectorAll('.tier-tab').forEach(function(t){t.classList.remove('active');});
  var btn = document.querySelector('.tier-tab[data-tier="'+tier+'"]');
  if (btn) btn.classList.add('active');
  document.querySelectorAll('.tier-panel').forEach(function(p){p.classList.add('hidden');});
  var panel = document.getElementById('panel-'+tier);
  if (panel) panel.classList.remove('hidden');
  if (window.runSearch) window.runSearch();
}

// ── row expand/collapse (tier-scoped IDs) ─────────────────────────────────────
function toggleRow(tier, idx) {
  var row = document.querySelector('#tbl-'+tier+' tr.cl-row[data-idx="'+idx+'"]');
  var det = document.getElementById('detail-'+tier+'-'+idx);
  if (!det || !row) return;
  var open = det.classList.contains('open');
  det.classList.toggle('open', !open);
  row.classList.toggle('open', !open);
}

// ── sortable column headers (per table) ───────────────────────────────────────
(function(){
  var colIndex = {ordinal:0,stableid:1,shape:2,size:3,avgscore:4,confidence:5,coherence:6,callcoherence:7,packages:8,potential:9};

  ['high','medium','low'].forEach(function(tier){
    var tbl = document.getElementById('tbl-'+tier);
    if (!tbl) return;
    var state = {col:'confidence', asc:false};
    tbl.querySelectorAll('thead th').forEach(function(th){
      th.addEventListener('click', function(){
        var col = th.dataset.col;
        if (state.col===col){state.asc=!state.asc;}
        else{state.col=col;state.asc=(col==='shape'||col==='packages'||col==='stableid');}
        tbl.querySelectorAll('thead th').forEach(function(t){t.classList.remove('sorted-asc','sorted-desc');});
        th.classList.add(state.asc?'sorted-asc':'sorted-desc');
        sortTierTable(tier, col, state.asc);
      });
    });
  });

  function sortTierTable(tier, col, asc){
    var tbody = document.querySelector('#tbl-'+tier+' tbody');
    if (!tbody) return;
    var pairs = [];
    tbody.querySelectorAll('tr.cl-row').forEach(function(row){
      pairs.push({row:row, det:document.getElementById('detail-'+tier+'-'+row.dataset.idx)});
    });
    pairs.sort(function(a,b){
      var av=cellVal(a.row,col), bv=cellVal(b.row,col);
      if (typeof av==='number') return asc?av-bv:bv-av;
      return asc?av.localeCompare(bv):bv.localeCompare(av);
    });
    pairs.forEach(function(p){tbody.appendChild(p.row);if(p.det)tbody.appendChild(p.det);});
  }

  function cellVal(row,col){
    var ci=colIndex[col];
    if (ci===undefined) return '';
    var cell=row.querySelectorAll('td')[ci];
    var text=cell?cell.textContent.trim():'';
    var n=parseFloat(text);
    return isNaN(n)?text:n;
  }
})();

// ── search (ID, function name, file path) — applied to active tab ─────────────
(function(){
  var fnInput=document.getElementById('fn-search');
  var fnClear=document.getElementById('fn-search-clear');
  var fileInput=document.getElementById('file-search');
  var fileClear=document.getElementById('file-search-clear');
  var idInput=document.getElementById('id-search');
  var idClear=document.getElementById('id-search-clear');
  var countEl=document.getElementById('search-count');

  function getActiveTier(){var b=document.querySelector('.tier-tab.active');return b?b.dataset.tier:'high';}
  function getActivePanel(){return document.querySelector('.tier-panel:not(.hidden)');}

  [fnInput,fileInput,idInput].forEach(function(el){el.addEventListener('input',runSearch);});
  fnClear.addEventListener('click',function(){fnInput.value='';runSearch();fnInput.focus();});
  fileClear.addEventListener('click',function(){fileInput.value='';runSearch();fileInput.focus();});
  idClear.addEventListener('click',function(){idInput.value='';runSearch();idInput.focus();});

  function runSearch(){
    var fnTerm=fnInput.value.trim().toLowerCase();
    var fileTerm=fileInput.value.trim().toLowerCase();
    var idTerm=idInput.value.trim().toLowerCase();
    fnClear.style.display=fnTerm?'block':'none';
    fileClear.style.display=fileTerm?'block':'none';
    idClear.style.display=idTerm?'block':'none';
    var tier=getActiveTier();
    var panel=getActivePanel();
    if(!panel) return;
    var searching=fnTerm||fileTerm||idTerm;
    if(!searching){
      panel.querySelectorAll('tr.cl-row').forEach(function(r){r.classList.remove('search-hidden');});
      panel.querySelectorAll('tr.cl-detail').forEach(function(d){
        d.classList.remove('search-hidden');
        d.querySelectorAll('table.members tr').forEach(function(mr){mr.classList.remove('member-hidden','member-match');});
        d.querySelectorAll('.fn-name mark,.fn-file mark').forEach(function(m){m.outerHTML=m.textContent;});
      });
      countEl.textContent='';return;
    }
    var mc=0,mf=0;
    panel.querySelectorAll('tr.cl-row').forEach(function(row){
      // ID filter: if an ID term is given and this row doesn't match, hide immediately
      if(idTerm && !row.dataset.stableid.toLowerCase().includes(idTerm)){
        row.classList.add('search-hidden');
        var det=document.getElementById('detail-'+tier+'-'+row.dataset.idx);
        if(det){det.classList.add('search-hidden');det.classList.remove('open');}
        row.classList.remove('open');
        return;
      }
      var idx=row.dataset.idx;
      var det=document.getElementById('detail-'+tier+'-'+idx);
      // If only ID term matched (no fn/file term), show the row without expanding
      if(idTerm && !fnTerm && !fileTerm){
        row.classList.remove('search-hidden');
        if(det) det.classList.remove('search-hidden');
        mc++;return;
      }
      var memberRows=det?det.querySelectorAll('table.members tbody tr'):[];
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
      if(hit){row.classList.remove('search-hidden');if(det){det.classList.remove('search-hidden');det.classList.add('open');}row.classList.add('open');mc++;}
      else{row.classList.add('search-hidden');if(det){det.classList.add('search-hidden');det.classList.remove('open');}row.classList.remove('open');}
    });
    countEl.textContent=mf>0?(mf+' fn'+(mf!==1?'s':'')+' in '+mc+' cluster'+(mc!==1?'s':'')):(mc>0?mc+' cluster'+(mc!==1?'s':''):'no matches');
  }

  window.runSearch = runSearch; // expose so switchTab can re-apply after tab change
  function esc(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
})();


// ── jump to cluster from deviation table ──────────────────────────────────────
function jumpToCluster(stableId) {
  var tiers = ['high', 'medium', 'low'];
  for (var i = 0; i < tiers.length; i++) {
    var tier = tiers[i];
    var row = document.querySelector('#tbl-' + tier + ' tr.cl-row[data-stableid="' + stableId + '"]');
    if (!row) continue;
    switchTab(tier);
    var idx = row.dataset.idx;
    var det = document.getElementById('detail-' + tier + '-' + idx);
    if (det && !det.classList.contains('open')) {
      det.classList.add('open');
      row.classList.add('open');
    }
    (function(r){ setTimeout(function(){ r.scrollIntoView({behavior:'smooth', block:'center'}); }, 150); })(row);
    return;
  }
}

// ── potential outlier tooltip ──────────────────────────────────────────────────
(function(){
  var tt=document.getElementById('pot-tooltip');
  var tb=document.getElementById('pot-tt-body');
  var current=null;

  window.showPotTooltip=function(e,el){
    e.stopPropagation();
    if(current===el){hideTt();return;}
    current=el;
    var names=(el.dataset.names||'').split('|').filter(function(s){return s.trim();});
    tb.innerHTML='';
    names.forEach(function(n){
      var d=document.createElement('div');
      d.className='pot-tt-item';
      // format: "FuncName#short/path#line" → show as "FuncName · path:line"
      var parts=n.split('#');
      d.textContent=parts[0]+(parts[1]?' · '+parts[1]+(parts[2]?':'+parts[2]:''):'');
      tb.appendChild(d);
    });
    tt.classList.add('visible');
    positionTt(e);
  };

  function positionTt(e){
    var x=e.clientX+12, y=e.clientY+12;
    var w=tt.offsetWidth||240, h=tt.offsetHeight||100;
    if(x+w>window.innerWidth-8) x=e.clientX-w-8;
    if(y+h>window.innerHeight-8) y=e.clientY-h-8;
    tt.style.left=x+'px';
    tt.style.top=y+'px';
  }

  function hideTt(){tt.classList.remove('visible');current=null;}
  document.addEventListener('click',function(e){if(!tt.contains(e.target)&&e.target!==current)hideTt();});
  document.addEventListener('keydown',function(e){if(e.key==='Escape')hideTt();});
})();

// ── column header instant tooltip ─────────────────────────────────────────────
(function(){
  var tip = document.createElement('div');
  tip.id = 'col-tip';
  document.body.appendChild(tip);

  function position(e){
    var x = e.clientX + 14, y = e.clientY + 14;
    if(x + tip.offsetWidth + 20 > window.innerWidth) x = e.clientX - tip.offsetWidth - 14;
    if(y + tip.offsetHeight + 20 > window.innerHeight) y = e.clientY - tip.offsetHeight - 14;
    tip.style.left = x + 'px';
    tip.style.top  = y + 'px';
  }

  document.querySelectorAll('th[title]').forEach(function(th){
    var text = th.getAttribute('title');
    th.removeAttribute('title');       // suppress native 1s-delay browser tooltip
    th.setAttribute('data-tip', text); // drives the CSS ::after ⓘ icon

    th.addEventListener('mouseenter', function(e){
      tip.textContent = text;
      tip.style.display = 'block';
      position(e);
    });
    th.addEventListener('mousemove', position);
    th.addEventListener('mouseleave', function(){
      tip.style.display = 'none';
    });
  });
})();

</script>
</body>
</html>`
