package ds

import (
	"github.com/google/uuid"
)

// FileMeta carrries name and path metadata about golang file.
type FileMeta struct {
	Name string
	Path string
}

// PkgToFileMeta contains package information to file metadata mapping.
type PkgToFileMeta map[string][]FileMeta

// ParamInfo describes a single function parameter.
type ParamInfo struct {
	TypeName    string
	IsFuncType  bool // parameter type is a function
	IsInterface bool // parameter type is an interface
}

// ReturnInfo describes a single return value.
type ReturnInfo struct {
	TypeName string
	IsError  bool
}

// FunctionMeta carries metadata about individual functions found in go files.
type FunctionMeta struct {
	Name          string
	Package       string
	FileMeta      FileMeta
	Start_line    int
	End_line      int
	LineCount     int
	IsMethod      bool
	IsExported    bool
	Receiver      string // receiver type for methods, empty for functions
	Params        []ParamInfo
	Returns       []ReturnInfo
	Features      StructuralFeatures
	TokenSeq      []int
	TokenSeqHash  []int64
	CallTargets   []string
	Imports       []string // packages imported by the file this function lives in
	DirectImports []string // packages this function actually references (subset)
	GeneratedCode bool
}

type StructuralFeatures struct {
	// complexity
	CyclomaticComplexity int // 1 + decision points
	BranchingDepth       int // max nesting of branching constructs
	NestingDepth         int // max nesting of any scope-opening construct
	EarlyReturns         int // returns before the final return statement

	// control flow counts
	ControlFlow ControlFlowCounts

	// call profile
	OutboundCalls    int // total call expression count
	FuncLiteralCount int // anonymous functions defined inline
	GoroutineSpawns  int // alias for ControlFlow.Go, explicit for clarity

	// parameter shape
	ParamCount      int
	ReturnCount     int
	HasFuncParam    bool // accepts a function parameter
	HasContextParam bool // accepts context.Context
	HasErrorReturn  bool // returns error as last return value
}

type ControlFlowCounts struct {
	If       int // if and else-if branches
	For      int // traditional for loops
	Range    int // range loops
	Switch   int // switch and type-switch statements
	Select   int // select statements (channel multiplexing)
	Return   int // all return statements
	Defer    int // deferred function calls
	Go       int // goroutine spawns
	Send     int // channel send operations
	Continue int // continue statements
	Break    int // break statements
	Goto     int // goto statements (rare but notable)
}

type ClusterProfile struct {
	CycloMin, CycloMax  int
	CycloMean, CycloStd float64

	NestingMax int

	CallsMin, CallsMax int
	CallsMean          float64

	DeferRate        float64 // fraction of members with at least one defer
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

	TopImports     []string // most frequent DirectImports across members
	TopCallTargets []string // most frequent CallTargets across members
}

type Cluster struct {
	SeqKey        string // canonical string key of the token sequence
	ShapeHash     string // SHA-256 prefix of SeqKey — stable identity across runs (16 hex chars)
	TokenSeq      []int
	ShapeVariants [][]int // token seqs of absorbed clusters (non-empty after CollapseToFamilies)
	Members       []FunctionMeta
	Size          int
	Profile       ClusterProfile
	Coherence     float64 // mean pairwise Jaccard of DirectImports
	CallCoherence float64 // mean pairwise Jaccard of CallTargets
	IsPrimitive   bool    // true if cluster is too common to be meaningful (IDF stop-word)
	Label         string  // filled later by labelling pass
}

type Index struct {
	FuncMeta  map[string]FunctionMeta
	Postings  map[int64][]string
	DocFreq   map[int64]int // how many functions contain this hash
	TotalDocs int
}

// MemberScore holds the cluster membership probability distribution for a
// single function. Computed after clustering by scoring the function against
// every non-primitive collapsed cluster using the three-term scoring function:
//
//	score(f, C) = shape_match + import_jaccard + call_target_jaccard
//
// Scores are normalised to probabilities via softmax. High Entropy means the
// function fits multiple clusters (boundary candidate). Low Entropy means it
// clearly belongs to one cluster.
type MemberScore struct {
	FunctionID string // stable 16-hex identity: sha256(pkg.name@path:line)
	Package    string
	Name       string
	FilePath   string
	Line       int
	Probs      map[string]float64 // clusterShapeHash → P(C_i | f)
	WinnerID   string             // shapeHash of the highest-probability cluster
	Entropy    float64            // H = -Σ p·log(p); high = boundary candidate
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
