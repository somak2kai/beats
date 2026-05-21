package main

import (
	"fmt"
	"html/template"
	"log/slog"
	log "log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
)

// ── report data structures ────────────────────────────────────────────────────

type MemberRow struct {
	Package  string
	Name     string
	FilePath string // full path
	Line     int
}

type ClusterRow struct {
	ShapeHash     string
	Label         string
	Size          int
	Coherence     float64 // mean pairwise Jaccard of DirectImports
	CallCoherence float64 // mean pairwise Jaccard of CallTargets
	CycloP95      float64
	CycloMean     float64
	TopImports    []string
	Packages      []string    // unique package names, sorted
	Members       []MemberRow // sorted by package then name
}

type RepoReport struct {
	Repo                string
	GeneratedAt         string
	TotalClusters       int
	FunctionsInClusters int
	CorpusSize          int          // total functions analysed (including those not in any cluster)
	MeanCoherence       float64      // mean import Jaccard across clusters
	MeanCallCoherence   float64      // mean call target Jaccard across clusters
	QuadHH              int          // import >= 0.60 AND call >= 0.60
	QuadHL              int          // import >= 0.60 AND call <  0.60
	QuadLH              int          // import <  0.60 AND call >= 0.60
	QuadLL              int          // import <  0.60 AND call <  0.60
	Clusters            []ClusterRow // sorted by combined coherence desc
}

// ── entry point ───────────────────────────────────────────────────────────────

func RunAnalyze(repo string) error {
	dbPath := filepath.Join(os.TempDir(), "badger", repo)
	bDb := db.NewDb(dbPath)
	defer bDb.Close()

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

	log.Info("loaded clusters", slog.Int("count", len(clusters)), slog.String("tier", tier))

	var corpusSize int
	if err := bDb.Load("meta:corpus_size", &corpusSize); err != nil {
		// non-fatal: index predates corpus_size storage; fall back to 0
		log.Warn("corpus size not found in index (re-run beats init to populate)", slog.Any("error", err))
	}

	report := buildReport(repo, clusters, corpusSize)

	beatsDir := filepath.Join(repo, ".beats")
	if err := os.MkdirAll(beatsDir, 0755); err != nil {
		return fmt.Errorf("create .beats dir: %w", err)
	}

	outPath := filepath.Join(beatsDir, "report.html")
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	defer f.Close()

	if err := renderHTML(f, report); err != nil {
		return fmt.Errorf("render html: %w", err)
	}

	log.Info("report written", slog.String("path", outPath))
	return nil
}

// ── report builder ────────────────────────────────────────────────────────────

func buildClusterRow(c ds.Cluster) ClusterRow {
	pkgSet := make(map[string]bool)
	members := make([]MemberRow, 0, len(c.Members))
	for _, m := range c.Members {
		pkgSet[m.Package] = true
		members = append(members, MemberRow{
			Package:  m.Package,
			Name:     m.Name,
			FilePath: m.FileMeta.Path,
			Line:     m.Start_line,
		})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Package != members[j].Package {
			return members[i].Package < members[j].Package
		}
		return members[i].Name < members[j].Name
	})

	pkgs := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	return ClusterRow{
		ShapeHash:     c.ShapeHash,
		Label:         c.Label,
		Size:          c.Size,
		Coherence:     c.Coherence,
		CallCoherence: c.CallCoherence,
		CycloP95:      c.Profile.CycloP95,
		CycloMean:     c.Profile.CycloMean,
		TopImports:    c.Profile.TopImports,
		Packages:      pkgs,
		Members:       members,
	}
}

func buildReport(repo string, clusters []ds.Cluster, corpusSize int) RepoReport {
	rows := make([]ClusterRow, 0, len(clusters))
	var totalCoherence, totalCallCoherence float64
	var functionsInClusters int
	var quadHH, quadHL, quadLH, quadLL int

	for _, c := range clusters {
		row := buildClusterRow(c)
		rows = append(rows, row)
		totalCoherence += c.Coherence
		totalCallCoherence += c.CallCoherence
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

	// default sort: combined coherence (import + call) descending, size as tiebreak.
	// combined = (ImportJaccard + CallJaccard) / 2 — see glossary for quadrant meanings.
	sort.Slice(rows, func(i, j int) bool {
		ci := (rows[i].Coherence + rows[i].CallCoherence) / 2
		cj := (rows[j].Coherence + rows[j].CallCoherence) / 2
		if ci != cj {
			return ci > cj
		}
		return rows[i].Size > rows[j].Size
	})

	n := len(clusters)
	meanCoherence, meanCallCoherence := 0.0, 0.0
	if n > 0 {
		meanCoherence = totalCoherence / float64(n)
		meanCallCoherence = totalCallCoherence / float64(n)
	}

	return RepoReport{
		Repo:                filepath.Base(repo),
		GeneratedAt:         time.Now().Format("2006-01-02 15:04:05"),
		TotalClusters:       n,
		FunctionsInClusters: functionsInClusters,
		CorpusSize:          corpusSize,
		MeanCoherence:       meanCoherence,
		MeanCallCoherence:   meanCallCoherence,
		QuadHH:              quadHH,
		QuadHL:              quadHL,
		QuadLH:              quadLH,
		QuadLL:              quadLL,
		Clusters:            rows,
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

func renderHTML(w *os.File, report RepoReport) error {
	funcMap := template.FuncMap{
		"badgeClass":     coherenceBadgeClass,
		"pct":            pct,
		"f2":             f2,
		"f1":             f1,
		"joinComma":      joinComma,
		"labelOrHash":    labelOrHash,
		"shortPath":      shortPath,
		"quadrantCode":   quadrantCode,
		"quadrantLabel":  quadrantLabel,
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
<title>beats analyze — {{.Repo}}</title>
<style>
  :root {
    --bg: #0f1117; --surface: #1a1d27; --surface2: #22263a; --surface3: #191c2a;
    --border: #2e3350; --text: #e0e4f0; --muted: #7a82a6; --muted2: #4a5070;
    --green: #34d399; --yellow: #fbbf24; --red: #f87171;
    --accent: #818cf8; --accent2: #60a5fa; --accent3: #c084fc;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg); color: var(--text); font-family: 'Inter', 'Segoe UI', system-ui, sans-serif; font-size: 14px; line-height: 1.6; }

  /* ── header ── */
  header { background: var(--surface); border-bottom: 1px solid var(--border); padding: 20px 32px; display: flex; align-items: center; justify-content: space-between; }
  .hdr-left h1 { font-size: 1.25rem; font-weight: 700; color: var(--accent); letter-spacing: -.01em; }
  .hdr-left .repo { font-size: 13px; color: var(--muted); margin-top: 2px; }
  .hdr-right { font-size: 11px; color: var(--muted2); text-align: right; }

  /* ── layout ── */
  .container { max-width: 1280px; margin: 0 auto; padding: 28px 32px; }

  /* ── summary cards ── */
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: 14px; margin-bottom: 28px; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 14px 18px; }
  .card .lbl { font-size: 10px; text-transform: uppercase; letter-spacing: .08em; color: var(--muted); margin-bottom: 4px; }
  .card .val { font-size: 1.75rem; font-weight: 700; color: var(--text); line-height: 1.1; }
  .card .sub { font-size: 11px; color: var(--muted); margin-top: 3px; }

  /* ── legend ── */
  details.legend { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; margin-bottom: 24px; }
  details.legend summary { padding: 12px 18px; cursor: pointer; font-size: 11px; text-transform: uppercase; letter-spacing: .08em; color: var(--muted); font-weight: 600; list-style: none; display: flex; align-items: center; gap: 8px; }
  details.legend summary::before { content: '▶'; font-size: 9px; transition: transform .2s; }
  details.legend[open] summary::before { transform: rotate(90deg); }
  .legend-body { padding: 4px 18px 16px; display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 10px; }
  .lg-item { display: flex; gap: 10px; }
  .lg-term { min-width: 120px; font-weight: 600; color: var(--accent2); font-size: 12px; flex-shrink: 0; }
  .lg-def { color: var(--muted); font-size: 12px; }

  /* ── coherence quadrant matrix ── */
  .quadrant-wrap { grid-column: 1 / -1; margin: 4px 0 2px; }
  .quadrant-label { font-size: 10px; text-transform: uppercase; letter-spacing: .07em; color: var(--muted); margin-bottom: 6px; font-weight: 600; }
  .quadrant-grid { display: grid; grid-template-columns: 120px 1fr 1fr; grid-template-rows: auto auto auto; gap: 2px; font-size: 11px; }
  .qg-corner { }
  .qg-col-hdr { padding: 5px 10px; text-align: center; font-weight: 700; color: var(--muted); background: var(--surface2); border-radius: 4px 4px 0 0; }
  .qg-row-hdr { padding: 5px 10px; display: flex; align-items: center; font-weight: 700; color: var(--muted); background: var(--surface2); border-radius: 4px 0 0 4px; }
  .qg-cell { padding: 8px 12px; border-radius: 4px; }
  .qg-hh { background: rgba(52,211,153,.10); border: 1px solid rgba(52,211,153,.25); }
  .qg-lh { background: rgba(96,165,250,.10); border: 1px solid rgba(96,165,250,.25); }
  .qg-hl { background: rgba(251,191,36,.10); border: 1px solid rgba(251,191,36,.20); }
  .qg-ll { background: rgba(248,113,113,.08); border: 1px solid rgba(248,113,113,.20); }
  .qg-cell strong { display: block; margin-bottom: 2px; font-size: 11px; }
  .qg-hh strong { color: var(--green); }
  .qg-lh strong { color: var(--accent2); }
  .qg-hl strong { color: var(--yellow); }
  .qg-ll strong { color: var(--red); }

  /* ── section heading ── */
  .sec { font-size: .8rem; font-weight: 600; color: var(--accent); text-transform: uppercase; letter-spacing: .08em; margin: 0 0 10px; padding-bottom: 6px; border-bottom: 1px solid var(--border); }

  /* ── cluster table ── */
  .tbl-wrap { overflow-x: auto; overflow-y: auto; max-height: calc(100vh - 260px); border: 1px solid var(--border); border-radius: 8px; }
  table.clusters { width: 100%; border-collapse: collapse; }
  table.clusters thead th {
    text-align: left; padding: 9px 12px; font-size: 11px; text-transform: uppercase;
    letter-spacing: .07em; color: var(--muted); background: var(--surface);
    border-bottom: 2px solid var(--border); white-space: nowrap;
    cursor: pointer; user-select: none; position: sticky; top: 0; z-index: 10;
    box-shadow: 0 1px 0 var(--border);
  }
  table.clusters thead th:hover { color: var(--text); }
  table.clusters thead th.sorted-asc::after  { content: ' ▲'; color: var(--accent); }
  table.clusters thead th.sorted-desc::after { content: ' ▼'; color: var(--accent); }

  tr.cl-row { background: var(--surface); border-bottom: 1px solid var(--border); cursor: pointer; transition: background .1s; }
  tr.cl-row:hover { background: var(--surface2); }
  tr.cl-row td { padding: 10px 12px; vertical-align: middle; }

  tr.cl-detail { display: none; background: var(--surface3); }
  tr.cl-detail.open { display: table-row; }
  tr.cl-detail td { padding: 0; border-bottom: 2px solid var(--border); }

  /* caret */
  .caret { display: inline-block; font-size: 9px; color: var(--muted); margin-right: 6px; transition: transform .18s; }
  tr.cl-row.open .caret { transform: rotate(90deg); }

  /* label / hash */
  .cl-label { font-weight: 600; color: var(--text); font-size: 13px; }
  .cl-hash { font-family: 'JetBrains Mono', 'Fira Code', monospace; font-size: 11px; color: var(--accent); }
  .pkg-pills { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 4px; }
  .pkg-pill { background: rgba(129,140,248,.12); color: var(--accent); border-radius: 3px; padding: 1px 6px; font-size: 11px; font-family: monospace; }

  /* badge */
  .badge { display: inline-block; padding: 2px 8px; border-radius: 4px; font-size: 11px; font-weight: 600; }
  .badge-green  { background: rgba(52,211,153,.15); color: var(--green); }
  .badge-yellow { background: rgba(251,191,36,.15); color: var(--yellow); }
  .badge-red    { background: rgba(248,113,113,.15); color: var(--red); }

  .num   { font-variant-numeric: tabular-nums; }
  .muted { color: var(--muted); }
  .imports-cell { color: var(--muted); font-size: 11px; max-width: 280px; }

  /* ── member detail panel ── */
  .detail-panel { padding: 14px 18px 18px; }
  .detail-panel h4 { font-size: 10px; text-transform: uppercase; letter-spacing: .08em; color: var(--muted); margin-bottom: 10px; }
  table.members { width: 100%; border-collapse: collapse; font-size: 12px; }
  table.members th { text-align: left; padding: 5px 10px; color: var(--muted); font-size: 10px; text-transform: uppercase; letter-spacing: .06em; border-bottom: 1px solid var(--border); }
  table.members td { padding: 6px 10px; border-bottom: 1px solid var(--border); vertical-align: top; }
  table.members tr:last-child td { border-bottom: none; }
  .fn-name { font-weight: 600; color: var(--accent2); font-family: monospace; font-size: 12px; }
  .fn-pkg  { color: var(--accent3); font-family: monospace; font-size: 11px; }
  .fn-file { color: var(--muted); font-size: 11px; font-family: monospace; }
  .fn-line { color: var(--muted2); font-size: 11px; }

  footer { text-align: center; padding: 28px; color: var(--muted2); font-size: 11px; border-top: 1px solid var(--border); margin-top: 40px; }

  /* ── search bar ── */
  .sec-bar { display: flex; align-items: center; justify-content: space-between; margin-bottom: 10px; }
  .sec-bar .sec { margin: 0; border: none; padding: 0; }
  .search-wrap { display: flex; align-items: center; gap: 6px; }
  .search-icon { color: var(--muted); font-size: 16px; line-height: 1; }
  .search-input {
    background: var(--surface); border: 1px solid var(--border); border-radius: 6px;
    color: var(--text); padding: 6px 10px; font-size: 13px; width: 210px;
    outline: none; transition: border-color .15s;
  }
  .search-input:focus { border-color: var(--accent); }
  #file-search:focus { border-color: var(--accent2); }
  .search-clear {
    background: none; border: none; color: var(--muted); cursor: pointer;
    font-size: 13px; padding: 4px 6px; border-radius: 4px; line-height: 1;
    display: none;
  }
  .search-clear:hover { color: var(--text); background: var(--surface2); }
  .search-divider { color: var(--muted2); font-size: 11px; padding: 0 2px; }
  .search-count { font-size: 11px; color: var(--muted); white-space: nowrap; min-width: 80px; }

  /* search state */
  tr.cl-row.search-hidden { display: none; }
  tr.cl-detail.search-hidden { display: none; }
  tr.cl-row.quad-hidden { display: none; }
  tr.cl-detail.quad-hidden { display: none; }
  table.members tr.member-hidden { display: none; }
  table.members tr.member-match td { background: rgba(129,140,248,.07); }
  .fn-name mark { background: rgba(129,140,248,.35); color: var(--text); border-radius: 2px; padding: 0 1px; }

  /* ── quadrant pills (table column + filter bar) ── */
  .quad-pill { display: inline-block; padding: 2px 8px; border-radius: 4px; font-size: 10px; font-weight: 700; letter-spacing: .05em; white-space: nowrap; }
  .quad-hh { background: rgba(52,211,153,.15); color: var(--green);   border: 1px solid rgba(52,211,153,.30); }
  .quad-lh { background: rgba(96,165,250,.15); color: var(--accent2); border: 1px solid rgba(96,165,250,.30); }
  .quad-hl { background: rgba(251,191,36,.15); color: var(--yellow);  border: 1px solid rgba(251,191,36,.25); }
  .quad-ll { background: rgba(248,113,113,.10); color: var(--red);    border: 1px solid rgba(248,113,113,.20); }

  /* ── quadrant filter bar ── */
  .quad-filter-bar { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-bottom: 10px; padding: 10px 14px; background: var(--surface); border: 1px solid var(--border); border-radius: 8px; }
  .quad-filter-label { font-size: 10px; text-transform: uppercase; letter-spacing: .08em; color: var(--muted); font-weight: 600; margin-right: 4px; }
  .quad-filter-item { display: flex; align-items: center; gap: 5px; cursor: pointer; padding: 4px 8px; border-radius: 6px; border: 1px solid transparent; transition: background .1s; user-select: none; }
  .quad-filter-item:hover { background: var(--surface2); }
  .quad-filter-item input[type=checkbox] { accent-color: var(--accent); width: 13px; height: 13px; cursor: pointer; }
  .quad-filter-item.inactive { opacity: 0.45; }
  .quad-filter-divider { width: 1px; height: 18px; background: var(--border); margin: 0 4px; }
</style>
</head>
<body>

<header>
  <div class="hdr-left">
    <h1>beats analyze</h1>
    <div class="repo">{{.Repo}}</div>
  </div>
  <div class="hdr-right">
    collapsed clusters<br/>
    vocabulary-independent structural fingerprinting<br/>
    {{.GeneratedAt}}
  </div>
</header>

<div class="container">

  <!-- summary cards — row 1: totals + coherence means -->
  <div class="cards">
    <div class="card">
      <div class="lbl">Total Clusters</div>
      <div class="val">{{.TotalClusters}}</div>
      <div class="sub">collapsed families</div>
    </div>
    <div class="card">
      <div class="lbl">Functions in Clusters</div>
      <div class="val">{{.FunctionsInClusters}}</div>
      <div class="sub">across all clusters</div>
    </div>
    <div class="card">
      <div class="lbl">Mean Import Coh.</div>
      <div class="val">{{f2 .MeanCoherence}}</div>
      <div class="sub">mean pairwise Jaccard similarity of function level import</div>
    </div>
    <div class="card">
      <div class="lbl">Mean Call Coh.</div>
      <div class="val">{{f2 .MeanCallCoherence}}</div>
      <div class="sub">mean pairwise Jaccard similarity of function level call targets(fan outs)</div>
    </div>
  </div>
  <!-- summary cards — row 2: coherence quadrant counts -->
  <div class="cards" style="margin-top: -4px;">
    <div class="card" style="border-color: rgba(52,211,153,.35);">
      <div class="lbl" style="color: var(--green);">High Import · High Call</div>
      <div class="val" style="color: var(--green);">{{.QuadHH}}</div>
      <div class="sub">{{pct .QuadHH .TotalClusters}} — tight domain-local</div>
    </div>
    <div class="card" style="border-color: rgba(96,165,250,.35);">
      <div class="lbl" style="color: var(--accent2);">Low Import · High Call</div>
      <div class="val" style="color: var(--accent2);">{{.QuadLH}}</div>
      <div class="sub">{{pct .QuadLH .TotalClusters}} — cross-cutting structural</div>
    </div>
    <div class="card" style="border-color: rgba(251,191,36,.30);">
      <div class="lbl" style="color: var(--yellow);">High Import · Low Call</div>
      <div class="val" style="color: var(--yellow);">{{.QuadHL}}</div>
      <div class="sub">{{pct .QuadHL .TotalClusters}} — domain-cohesive, diverse</div>
    </div>
    <div class="card" style="border-color: rgba(248,113,113,.25);">
      <div class="lbl" style="color: var(--red);">Low Import · Low Call</div>
      <div class="val" style="color: var(--red);">{{.QuadLL}}</div>
      <div class="sub">{{pct .QuadLL .TotalClusters}} — probably noise</div>
    </div>
  </div>

  <!-- collapsible legend -->
  <details class="legend">
    <summary>Metric Glossary</summary>
    <div class="legend-body">
      <div class="lg-item"><span class="lg-term">Import Coherence (Coh.)</span><span class="lg-def">Mean pairwise Jaccard of DirectImports across members. High = every member touches the same package domain (e.g. all use "database/sql"). Measures <em>domain locality</em>.</span></div>
      <div class="lg-item"><span class="lg-term">Call Coherence(Coh.)</span><span class="lg-def">Mean pairwise Jaccard of CallTargets across members. High = every member invokes the same external functions (e.g. all call RegisterTaskFatal). Measures <em>structural role</em>.</span></div>
      <div class="quadrant-wrap">
        <div class="quadrant-label">Coherence quadrant guide</div>
        <div class="quadrant-grid">
          <div class="qg-corner"></div>
          <div class="qg-col-hdr">High Call Coh.</div>
          <div class="qg-col-hdr">Low Call Coh.</div>
          <div class="qg-row-hdr">High Import Coh.</div>
          <div class="qg-cell qg-hh"><strong>Tight domain-local pattern</strong><span style="color:var(--muted)">Shares both package context and exact call vocabulary. Most actionable.</span></div>
          <div class="qg-cell qg-hl"><strong>Domain-cohesive, structurally diverse</strong><span style="color:var(--muted)">Shared package domain, divergent calls. May need splitting.</span></div>
          <div class="qg-row-hdr">Low Import Coh.</div>
          <div class="qg-cell qg-lh"><strong>Cross-cutting structural pattern</strong><span style="color:var(--muted)">Different domains, same structural role (e.g. cron registration, adapters).</span></div>
          <div class="qg-cell qg-ll"><strong>Probably noise</strong><span style="color:var(--muted)">Shape coincidence, not convention. Treat with scepticism.</span></div>
        </div>
      </div>
      <div class="lg-item"><span class="lg-term">Quadrant cards</span><span class="lg-def">Each cluster is placed in one of four quadrants based on whether Import Coh. and Call Coh. are above or below 0.60. The four summary cards at the top show counts per quadrant. Table is sorted by combined score (import + call) / 2 descending by default.</span></div>
      <div class="lg-item"><span class="lg-term">Cyclo P95</span><span class="lg-def">95th-percentile cyclomatic complexity among cluster members. Cyclo = 1 + decision points (if/for/case). P95 ≥ 10 means at least 5% of functions are complex.</span></div>
<div class="lg-item"><span class="lg-term">Top Imports</span><span class="lg-def">Most frequent DirectImports across cluster members — the shared abstractions that define what this cluster "does".</span></div>
      <div class="lg-item"><span class="lg-term">Packages</span><span class="lg-def">Go packages that contribute members to this cluster. Multiple packages = cross-cutting structural pattern. Single package = localised idiom.</span></div>
      <div class="lg-item"><span class="lg-term">Shape Hash</span><span class="lg-def">Stable 16-hex identity of the cluster's token sequence. Same hash in two different repos = structurally identical pattern across codebases.</span></div>
      <div class="lg-item"><span class="lg-term">Coherence badge</span><span class="lg-def"><span class="badge badge-green">≥ 0.60 tight</span> &nbsp; <span class="badge badge-yellow">0.40–0.59 moderate</span> &nbsp; <span class="badge badge-red">&lt; 0.40 loose</span></span></div>
      <div class="lg-item" style="grid-column: 1 / -1; margin-top: 6px; padding-top: 10px; border-top: 1px solid var(--border);">
        <span class="lg-term" style="color: var(--muted);">Filtered noise</span>
        <span class="lg-def">Clusters whose shape appears in <strong style="color:var(--text)">≥ 5% of all analysed functions</strong> are dropped before this report is generated. These are structural stop-words — patterns so universal (e.g. every model has a fetch-by-ID or a simple getter) that they are technically correct groupings but carry no useful signal. Showing them would dilute the findings that actually reveal conventions and idioms. The function count in the summary cards reflects only the clusters shown here.</span>
      </div>
    </div>
  </details>

  <!-- quadrant filter bar -->
  <div class="quad-filter-bar">
    <span class="quad-filter-label">Show</span>
    <label class="quad-filter-item" id="fi-hh">
      <input type="checkbox" class="quad-cb" data-quad="hh" checked/>
      <span class="quad-pill quad-hh">HH</span>
      <span style="font-size:11px;color:var(--muted)">High Import · High Call</span>
    </label>
    <div class="quad-filter-divider"></div>
    <label class="quad-filter-item" id="fi-lh">
      <input type="checkbox" class="quad-cb" data-quad="lh" checked/>
      <span class="quad-pill quad-lh">LH</span>
      <span style="font-size:11px;color:var(--muted)">Low Import · High Call</span>
    </label>
    <div class="quad-filter-divider"></div>
    <label class="quad-filter-item" id="fi-hl">
      <input type="checkbox" class="quad-cb" data-quad="hl" checked/>
      <span class="quad-pill quad-hl">HL</span>
      <span style="font-size:11px;color:var(--muted)">High Import · Low Call</span>
    </label>
    <div class="quad-filter-divider"></div>
    <label class="quad-filter-item" id="fi-ll">
      <input type="checkbox" class="quad-cb" data-quad="ll" checked/>
      <span class="quad-pill quad-ll">LL</span>
      <span style="font-size:11px;color:var(--muted)">Low Import · Low Call</span>
    </label>
  </div>

  <!-- cluster table -->
  <div class="sec-bar">
    <div class="sec">Clusters — sorted by combined coherence ↓</div>
    <div class="search-wrap">
      <span class="search-icon">⌕</span>
      <input type="text" id="fn-search"   class="search-input" placeholder="Function name…"  autocomplete="off" spellcheck="false"/>
      <button id="fn-search-clear"   class="search-clear" title="Clear function search">✕</button>
      <span class="search-divider">·</span>
      <input type="text" id="file-search" class="search-input" placeholder="File path…" autocomplete="off" spellcheck="false"/>
      <button id="file-search-clear" class="search-clear" title="Clear file search">✕</button>
      <span id="search-count" class="search-count"></span>
    </div>
  </div>
  <div class="tbl-wrap">
  <table class="clusters" id="cl-table">
    <thead>
      <tr>
        <th data-col="quadrant">Type</th>
        <th data-col="label">Label / Shape</th>
        <th data-col="size">Size</th>
        <th data-col="coherence">Import Coh.</th>
        <th data-col="callcoherence" class="sorted-desc">Call Coh.</th>
        <th data-col="cyclo95">Cyclo P95</th>
        <th data-col="packages">Packages</th>
        <th data-col="imports">Top Imports</th>
      </tr>
    </thead>
    <tbody>
{{range $i, $cl := .Clusters}}
      <tr class="cl-row" data-idx="{{$i}}" data-quadrant="{{quadrantCode $cl.Coherence $cl.CallCoherence}}" onclick="toggleRow({{$i}})">
        <td style="width:52px;text-align:center"><span class="quad-pill quad-{{quadrantCode $cl.Coherence $cl.CallCoherence}}">{{quadrantLabel $cl.Coherence $cl.CallCoherence}}</span></td>
        <td>
          <span class="caret">▶</span>
          {{if $cl.Label}}<span class="cl-label">{{$cl.Label}}</span><br/><span class="cl-hash">{{$cl.ShapeHash}}</span>
          {{else}}<span class="cl-hash">{{$cl.ShapeHash}}</span>{{end}}
        </td>
        <td class="num">{{$cl.Size}}</td>
        <td><span class="badge {{badgeClass $cl.Coherence}}">{{f2 $cl.Coherence}}</span></td>
        <td><span class="badge {{badgeClass $cl.CallCoherence}}">{{f2 $cl.CallCoherence}}</span></td>
        <td class="num">{{f1 $cl.CycloP95}}</td>
        <td>
          <div class="pkg-pills">
            {{range $cl.Packages}}<span class="pkg-pill">{{.}}</span>{{end}}
          </div>
        </td>
        <td class="imports-cell">{{joinComma $cl.TopImports}}</td>
      </tr>
      <tr class="cl-detail" id="detail-{{$i}}">
        <td colspan="8">
          <div class="detail-panel">
            <h4>{{$cl.Size}} member functions</h4>
            <table class="members">
              <thead><tr><th>Function</th><th>Package</th><th>File</th><th>Line</th></tr></thead>
              <tbody>
{{range $cl.Members}}
                <tr>
                  <td><span class="fn-name">{{.Name}}</span></td>
                  <td><span class="fn-pkg">{{.Package}}</span></td>
                  <td><span class="fn-file" title="{{.FilePath}}">{{shortPath .FilePath}}</span></td>
                  <td><span class="fn-line">{{.Line}}</span></td>
                </tr>
{{end}}
              </tbody>
            </table>
          </div>
        </td>
      </tr>
{{end}}
    </tbody>
  </table>
  </div>

</div>

<footer>
  generated by beats &nbsp;·&nbsp; vocabulary-independent structural fingerprinting for Go &nbsp;·&nbsp; {{.GeneratedAt}}
</footer>

<script>
function toggleRow(idx) {
  var row  = document.querySelector('tr.cl-row[data-idx="' + idx + '"]');
  var det  = document.getElementById('detail-' + idx);
  var open = det.classList.contains('open');
  det.classList.toggle('open', !open);
  row.classList.toggle('open', !open);
}

// ── column sort ────────────────────────────────────────────────────────────
(function () {
  var state = { col: 'callcoherence', asc: false };

  var colIndex = { quadrant: 0, label: 1, size: 2, coherence: 3, callcoherence: 4, cyclo95: 5, packages: 6, imports: 7 };

  document.querySelectorAll('#cl-table thead th').forEach(function (th) {
    th.addEventListener('click', function () {
      var col = th.dataset.col;
      if (state.col === col) { state.asc = !state.asc; }
      else { state.col = col; state.asc = col === 'label' || col === 'packages'; }

      document.querySelectorAll('#cl-table thead th').forEach(function (t) {
        t.classList.remove('sorted-asc', 'sorted-desc');
      });
      th.classList.add(state.asc ? 'sorted-asc' : 'sorted-desc');
      sortTable(col, state.asc);
    });
  });

  function sortTable(col, asc) {
    var tbody = document.querySelector('#cl-table tbody');
    var pairs = [];
    document.querySelectorAll('#cl-table tbody tr.cl-row').forEach(function (row) {
      var idx = row.dataset.idx;
      pairs.push({ row: row, det: document.getElementById('detail-' + idx) });
    });

    pairs.sort(function (a, b) {
      var av = cellVal(a.row, col);
      var bv = cellVal(b.row, col);
      if (typeof av === 'number') return asc ? av - bv : bv - av;
      return asc ? av.localeCompare(bv) : bv.localeCompare(av);
    });

    pairs.forEach(function (p) {
      tbody.appendChild(p.row);
      tbody.appendChild(p.det);
    });
  }

  function cellVal(row, col) {
    var ci = colIndex[col];
    if (ci === undefined) return '';
    var cell = row.querySelectorAll('td')[ci];
    var text = cell ? cell.textContent.trim() : '';
    var n = parseFloat(text);
    return isNaN(n) ? text : n;
  }
})();

// ── search (function name + file path) ────────────────────────────────────
(function () {
  var fnInput    = document.getElementById('fn-search');
  var fnClear    = document.getElementById('fn-search-clear');
  var fileInput  = document.getElementById('file-search');
  var fileClear  = document.getElementById('file-search-clear');
  var countEl    = document.getElementById('search-count');

  fnInput.addEventListener('input',   runSearch);
  fileInput.addEventListener('input', runSearch);

  fnClear.addEventListener('click', function () {
    fnInput.value = ''; runSearch(); fnInput.focus();
  });
  fileClear.addEventListener('click', function () {
    fileInput.value = ''; runSearch(); fileInput.focus();
  });

  function runSearch() {
    var fnTerm   = fnInput.value.trim().toLowerCase();
    var fileTerm = fileInput.value.trim().toLowerCase();

    fnClear.style.display   = fnTerm   ? 'block' : 'none';
    fileClear.style.display = fileTerm ? 'block' : 'none';

    var searching = fnTerm || fileTerm;

    if (!searching) {
      document.querySelectorAll('tr.cl-row').forEach(function (row) {
        row.classList.remove('search-hidden');
      });
      document.querySelectorAll('tr.cl-detail').forEach(function (det) {
        det.classList.remove('search-hidden');
        det.querySelectorAll('table.members tr').forEach(function (mr) {
          mr.classList.remove('member-hidden', 'member-match');
        });
        det.querySelectorAll('.fn-name mark, .fn-file mark').forEach(function (m) {
          m.outerHTML = m.textContent;
        });
      });
      countEl.textContent = '';
      return;
    }

    var matchedClusters  = 0;
    var matchedFunctions = 0;

    document.querySelectorAll('tr.cl-row').forEach(function (row) {
      var idx = row.dataset.idx;
      var det = document.getElementById('detail-' + idx);
      var memberRows = det.querySelectorAll('table.members tbody tr');
      var clusterHasMatch = false;

      memberRows.forEach(function (mr) {
        // strip previous highlights
        mr.querySelectorAll('.fn-name mark, .fn-file mark').forEach(function (m) {
          m.outerHTML = m.textContent;
        });

        var nameEl = mr.querySelector('.fn-name');
        var fileEl = mr.querySelector('.fn-file');
        if (!nameEl || !fileEl) return;

        var nameText = nameEl.textContent.toLowerCase();
        // match against both the display text and the full path in title=""
        var fileText = (fileEl.getAttribute('title') || fileEl.textContent).toLowerCase();

        var fnMatch   = !fnTerm   || nameText.includes(fnTerm);
        var fileMatch = !fileTerm || fileText.includes(fileTerm);

        if (fnMatch && fileMatch) {
          mr.classList.remove('member-hidden');
          mr.classList.add('member-match');
          clusterHasMatch = true;
          matchedFunctions++;

          // highlight function name match
          if (fnTerm) {
            var raw = nameEl.textContent;
            var lo  = raw.toLowerCase().indexOf(fnTerm);
            if (lo !== -1) {
              nameEl.innerHTML =
                escHtml(raw.slice(0, lo)) +
                '<mark>' + escHtml(raw.slice(lo, lo + fnTerm.length)) + '</mark>' +
                escHtml(raw.slice(lo + fnTerm.length));
            }
          }

          // highlight file path match (display text only)
          if (fileTerm) {
            var rawF = fileEl.textContent;
            var loF  = rawF.toLowerCase().indexOf(fileTerm);
            if (loF !== -1) {
              fileEl.innerHTML =
                escHtml(rawF.slice(0, loF)) +
                '<mark style="background:rgba(96,165,250,.35);border-radius:2px;padding:0 1px">' +
                escHtml(rawF.slice(loF, loF + fileTerm.length)) + '</mark>' +
                escHtml(rawF.slice(loF + fileTerm.length));
            }
          }
        } else {
          mr.classList.add('member-hidden');
          mr.classList.remove('member-match');
        }
      });

      if (clusterHasMatch) {
        row.classList.remove('search-hidden');
        det.classList.remove('search-hidden');
        det.classList.add('open');
        row.classList.add('open');
        matchedClusters++;
      } else {
        row.classList.add('search-hidden');
        det.classList.add('search-hidden');
        det.classList.remove('open');
        row.classList.remove('open');
      }
    });

    var parts = [];
    if (matchedFunctions > 0) {
      parts.push(matchedFunctions + ' fn' + (matchedFunctions !== 1 ? 's' : ''));
      parts.push(matchedClusters + ' cluster' + (matchedClusters !== 1 ? 's' : ''));
    } else {
      parts.push('no matches');
    }
    countEl.textContent = parts.join(' in ');
  }

  function escHtml(s) {
    return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
  }
})();

// ── quadrant filter (checkboxes) ──────────────────────────────────────────
(function () {
  function activeQuads() {
    var q = {};
    document.querySelectorAll('.quad-cb').forEach(function (cb) {
      if (cb.checked) q[cb.dataset.quad] = true;
    });
    return q;
  }

  function applyQuadFilter() {
    var q = activeQuads();
    document.querySelectorAll('tr.cl-row').forEach(function (row) {
      var quad = row.dataset.quadrant;
      var visible = !!q[quad];
      row.classList.toggle('quad-hidden', !visible);
      var det = document.getElementById('detail-' + row.dataset.idx);
      if (det) {
        det.classList.toggle('quad-hidden', !visible);
        if (!visible) {
          det.classList.remove('open');
          row.classList.remove('open');
        }
      }
    });
    // update filter item opacity so unchecked ones look dimmed
    document.querySelectorAll('.quad-filter-item').forEach(function (item) {
      var cb = item.querySelector('.quad-cb');
      item.classList.toggle('inactive', cb && !cb.checked);
    });
  }

  document.querySelectorAll('.quad-cb').forEach(function (cb) {
    cb.addEventListener('change', applyQuadFilter);
  });
})();
</script>
</body>
</html>`
