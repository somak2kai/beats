package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	ds "github.com/somak2kai/beats/pkg/types"
	scippb "github.com/sourcegraph/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// SCIPIndex is a queryable view of a SCIP binary index.
// It maps relative file path → occurrences sorted by start line,
// enabling efficient range queries for outbound reference extraction.
type SCIPIndex struct {
	docs        map[string][]*scippb.Occurrence // relPath → sorted occurrences
	DocCount    int
	SymbolCount int
}

// loadSCIPIndex reads a SCIP binary protobuf file and builds a queryable index.
// Generate the input file with: scip-go --output index.scip
func loadSCIPIndex(path string) (*SCIPIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading SCIP file: %w", err)
	}

	var raw scippb.Index
	if err := proto.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshalling SCIP index: %w", err)
	}

	idx := &SCIPIndex{
		docs:     make(map[string][]*scippb.Occurrence, len(raw.Documents)),
		DocCount: len(raw.Documents),
	}

	for _, doc := range raw.Documents {
		occs := doc.Occurrences
		// Sort by start line so OutboundRefs can binary-search and early-exit.
		sort.Slice(occs, func(i, j int) bool {
			return occStartLine(occs[i]) < occStartLine(occs[j])
		})
		idx.docs[doc.RelativePath] = occs
		idx.SymbolCount += len(doc.Symbols)
	}

	return idx, nil
}

// OutboundRefs returns the distinct set of SCIP symbols referenced within a
// function's source range. beats lines are 1-indexed; SCIP ranges are 0-indexed.
//
// It excludes:
//   - The function's own definition occurrence
//   - Local variable symbols (prefixed "local ") — they add noise without signal
//   - Empty symbol strings
func (idx *SCIPIndex) OutboundRefs(relPath string, startLine1, endLine1 int) []string {
	occs, ok := idx.docs[relPath]
	if !ok {
		return nil
	}

	// Convert beats 1-indexed lines to SCIP 0-indexed
	start0 := startLine1 - 1
	end0 := endLine1 - 1

	// Binary search to the first occurrence at or after start0
	lo := sort.Search(len(occs), func(i int) bool {
		return occStartLine(occs[i]) >= start0
	})

	seen := make(map[string]struct{})
	for i := lo; i < len(occs); i++ {
		occ := occs[i]
		sl := occStartLine(occ)
		if sl > end0 {
			break
		}
		// Skip definitions — they are declarations, not outbound references.
		if occ.SymbolRoles&int32(scippb.SymbolRole_Definition) != 0 {
			continue
		}
		sym := occ.Symbol
		if sym == "" || strings.HasPrefix(sym, "local ") {
			continue
		}
		seen[sym] = struct{}{}
	}

	refs := make([]string, 0, len(seen))
	for s := range seen {
		refs = append(refs, s)
	}
	sort.Strings(refs)
	return refs
}

// HasDocument reports whether the index contains the given relative path.
func (idx *SCIPIndex) HasDocument(relPath string) bool {
	_, ok := idx.docs[relPath]
	return ok
}

// SamplePaths returns up to n relative paths from the index (arbitrary order).
// Useful for diagnosing path-alignment between beats and SCIP.
func (idx *SCIPIndex) SamplePaths(n int) []string {
	out := make([]string, 0, n)
	for p := range idx.docs {
		out = append(out, p)
		if len(out) >= n {
			break
		}
	}
	sort.Strings(out)
	return out
}

// FunctionKey uniquely identifies a function in the corpus by its location.
type FunctionKey struct {
	RelPath   string
	StartLine int // 1-indexed, same as beats
}

// FunctionEntry pairs a key with a human-readable name for reporting.
type FunctionEntry struct {
	Key  FunctionKey
	Name string // qualifiedName from beats data
}

// BuildFunctionRefMap computes SCIP outbound refs for every member across
// all provided clusters. The result is the global corpus map used for
// discovery queries: given a set of symbols, find all functions that reference them.
//
// Duplicate members (same file+line across multiple clusters) are deduplicated —
// the last write wins, but refs are identical so it doesn't matter.
func (idx *SCIPIndex) BuildFunctionRefMap(
	clusters []ds.Cluster,
	repoRoot string,
) (map[FunctionKey][]string, []FunctionEntry) {

	root := strings.TrimRight(repoRoot, "/") + "/"
	refMap := make(map[FunctionKey][]string)
	var entries []FunctionEntry

	seen := make(map[FunctionKey]struct{})
	for _, c := range clusters {
		for _, m := range c.Members {
			relPath := strings.TrimPrefix(m.FileMeta.Path, root)
			key := FunctionKey{RelPath: relPath, StartLine: m.Start_line}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			refs := idx.OutboundRefs(relPath, m.Start_line, m.End_line)
			refMap[key] = refs
			entries = append(entries, FunctionEntry{Key: key, Name: qualifiedName(m)})
		}
	}
	return refMap, entries
}

// occStartLine extracts the 0-indexed start line from a SCIP occurrence range.
// SCIP encodes ranges as either [startLine, startChar, endChar] (3 elements, single line)
// or [startLine, startChar, endLine, endChar] (4 elements, multi-line).
func occStartLine(occ *scippb.Occurrence) int {
	if len(occ.Range) >= 1 {
		return int(occ.Range[0])
	}
	return 0
}
