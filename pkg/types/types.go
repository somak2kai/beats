package ds

import (
	"github.com/google/uuid"
)

type Language string

const (
	Language_GOLANG Language = "golang"
	Language_JAVA   Language = "java"
)

// FileMeta carrries name and path metadata about golang file.
type FileMeta struct {
	Name string
	Path string
	Lang Language
}

// PkgToFileMeta contains package information to file metadata mapping.
type PkgToFileMeta map[string][]FileMeta

// ParamInfo describes a single function parameter.
type ParamInfo struct {
	TypeName    string `json:"type_name"`
	IsFuncType  bool   `json:"is_func_type"`
	IsInterface bool   `json:"is_interface"`
}

// ReturnInfo describes a single return value.
type ReturnInfo struct {
	TypeName string `json:"type_name"`
	IsError  bool   `json:"is_error"`
}

// FunctionMeta carries metadata about individual functions found in go files.
type FunctionMeta struct {
	Name       string
	Package    string
	FileMeta   FileMeta
	Start_line int
	End_line   int
	LineCount  int
	IsMethod   bool
	IsExported bool
	// receiver type for methods, empty for functions
	Receiver     string
	Params       []ParamInfo
	Returns      []ReturnInfo
	Features     StructuralFeatures
	TokenSeq     []int
	TokenSeqHash []int64
	// outgoing calls of this function
	CallTargets []string
	// packages imported by the file this function lives in
	Imports []string
	// packages this function actually references (subset)
	DirectImports []string
	// auto code generated such as proto generated code
	GeneratedCode bool
	// true when the file this function lives in imports a test framework
	TestCode bool
	// true for New* functions whose entire body is a single composite-literal return
	IsConstructor bool
	// full source text of the function including signature, captured at parse time
	Body string
}

type StructuralFeatures struct {
	// complexity 1 + decision points
	CyclomaticComplexity int
	// max nesting of branching constructs
	BranchingDepth int
	// max nesting of any scope-opening construct
	NestingDepth int
	// returns before the final return statement
	EarlyReturns int

	// control flow counts
	ControlFlow ControlFlowCounts

	// call profile total call expression count
	OutboundCalls int
	// anonymous functions defined inline
	FuncLiteralCount int
	// alias for ControlFlow.Go, explicit for clarity
	GoroutineSpawns int

	// parameter shape
	ParamCount  int
	ReturnCount int
	// accepts a function parameter
	HasFuncParam bool
	// accepts context.Context
	HasContextParam bool
	// returns error as last return value
	HasErrorReturn bool
}

type ControlFlowCounts struct {
	// if and else-if branches
	If int
	// traditional for loops
	For int
	// range loops
	Range int
	// switch and type-switch statements
	Switch int
	// select statements (channel multiplexing)
	Select int
	// all return statements
	Return int
	// deferred function calls
	Defer int
	// goroutine spawns
	Go int
	// channel send operations
	Send int
	// continue statements
	Continue int
	// break statements
	Break int
	// goto statements (rare but notable)
	Goto int
}

type ClusterProfile struct {
	CycloMin, CycloMax  int
	CycloMean, CycloStd float64

	NestingMax int

	CallsMin, CallsMax int
	CallsMean          float64
	// fraction of members with at least one defer
	DeferRate        float64
	EarlyReturnRate  float64
	ContextParamRate float64
	ErrorReturnRate  float64
	GoroutineRate    float64

	// Percentile distributions — used for conformity scoring (Apps 2, 3, 9).
	// All values are linear-interpolated over the sorted member distribution.
	CycloP50, CycloP75, CycloP95                      float64
	NestingP50, NestingP75, NestingP95                float64
	CallsP50, CallsP75, CallsP95                      float64
	EarlyReturnsP50, EarlyReturnsP75, EarlyReturnsP95 float64
	DeferCountP50, DeferCountP75, DeferCountP95       float64
	// most frequent DirectImports across members
	TopImports []string
	// most frequent CallTargets across members
	TopCallTargets []string
}

// RankedMember is a cluster member with its arithmetic-mean pairwise score.
// Used to track the top-3 most representative members (medoids).
type RankedMember struct {
	Meta FunctionMeta
	// arithmetic mean of (seqS + impS + callS) / 3 against all other members
	Score float64
}

// ClusterStats holds summary statistics computed during agglomeration, stored
// alongside the cluster so orphan analysis can use them without recomputing.
type ClusterStats struct {
	// arithmetic mean of all pairwise arithmetic scores
	MeanScore float64
	// sample std deviation; floored at 0.05 in Z-score calc
	StdScore float64
	// up to 3 members with highest mean pairwise score (medoids)
	Top3 []RankedMember
}

// ClusterCandidate is a cluster that an orphaned function has some affinity
// toward, scored via arithmetic mean rather than geometric mean so that
// partial matches across dimensions are visible.
type ClusterCandidate struct {
	ClusterIdx int
	ShapeHash  string
	// token-sequence similarity
	SeqScore float64
	// DirectImports Jaccard
	ImpScore float64
	// CallTargets Jaccard
	CallScore  float64
	ArithScore float64
	// CycloDelta is orphan cyclomatic complexity minus cluster mean cyclomatic complexity.
	// Positive = orphan is more complex than the cluster average; negative = simpler.
	CycloDelta float64
	// SemanticIdiom of the candidate cluster (if enriched)
	Idiom string
}

// OrphanedFunction is a function that did not join any cluster during
// agglomeration. It carries its own metadata and a ranked list of the
// clusters it came closest to.
type OrphanedFunction struct {
	Meta       FunctionMeta
	Candidates []ClusterCandidate
}

type Cluster struct {
	// canonical string key of the token sequence
	SeqKey string
	// SHA-256 prefix of SeqKey — stable identity across runs (16 hex chars)
	ShapeHash string
	TokenSeq  []int
	// LCS of all member token sequences — structural skeleton shared by every member
	CommonSeq []int
	Members   []FunctionMeta
	Size      int
	Profile   ClusterProfile
	// summary statistics for the cluster.
	Stats ClusterStats
	// mean pairwise Jaccard of DirectImports
	Coherence float64
	// mean pairwise Jaccard of CallTargets
	CallCoherence float64
	// true if cluster is too common to be meaningful (IDF stop-word)
	IsPrimitive bool
	// filled later by labelling pass
	Label string

	// Conformity tier — stamped by clusterClassifier during beats init.
	// Derived from the standard deviation of internal pairwise arithmetic scores.
	Tier string

	// CompositeScore is the ranking signal of the cluster, based on members within this cluster.
	CompositeScore float64
	Confidence     string // "high" | "medium" | "low"
	// TODO remove
	// LLM enrichment — populated by beats update cluster, empty until then
	SemanticIdiom   string   // 3–6 word name for the structural idiom, e.g. "webhook config deserializer"
	Verdict         string   // one-sentence description of what the cluster represents
	CanonicalMember string   // pkg/FuncName of the most representative member
	SuggestedAction string   // suggested action: "none", or a short attention-candidate note
	SearchQuestions []string // 5–8 natural-language questions a developer might ask whose answer is this cluster
}

type Index struct {
	FuncMeta  map[string]FunctionMeta
	Postings  map[int64][]string
	DocFreq   map[int64]int // how many functions contain this hash
	TotalDocs int
}

func PopulateIndex(fMeta []FunctionMeta) Index {

	funcMeta := make(map[string]FunctionMeta, len(fMeta))
	post := make(map[int64][]string, len(fMeta))
	index := Index{Postings: post, TotalDocs: len(fMeta), FuncMeta: funcMeta}

	for _, i := range fMeta {
		id := uuid.NewString()
		funcMeta[id] = i
		tokenHash := i.TokenSeqHash
		for _, j := range tokenHash {
			post[j] = append(post[j], id)
		}
	}
	freq := make(map[int64]int, len(post))
	index.DocFreq = freq
	for k, v := range index.Postings {
		freq[k] = len(v)
	}
	return index
}
