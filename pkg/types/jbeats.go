package ds

// JBeatsFunctionMeta mirrors the per-function JSON output from jbeats.
type JBeatsFunctionMeta struct {
	Name          string       `json:"name"`
	PackageName   string       `json:"package_name"`
	FileName      string       `json:"file_name"`
	FilePath      string       `json:"file_path"`
	StartLine     int          `json:"start_line"`
	EndLine       int          `json:"end_line"`
	LineCount     int          `json:"line_count"`
	IsMethod      bool         `json:"is_method"`
	IsExported    bool         `json:"is_exported"`
	Receiver      string       `json:"receiver"`
	Params        []ParamInfo  `json:"params"`
	Returns       []ReturnInfo `json:"returns"`
	TokenSeq      []int        `json:"token_seq"`
	TokenSeqHash  []int64      `json:"token_seq_hash"`
	CallTargets   []string     `json:"call_targets"`
	DirectImports []string     `json:"direct_imports"`
	Imports       []string     `json:"imports"`
	GeneratedCode bool         `json:"generated_code"`
	TestCode      bool         `json:"test_code"`
	IsConstructor bool         `json:"is_constructor"`
	Body          string       `json:"body"`
}

// JBeatsFileResult mirrors the top-level JSON output from jbeats for a single file.
type JBeatsFileResult struct {
	File        string               `json:"file"`
	PackageName string               `json:"package_name"`
	Imports     []string             `json:"imports"`
	Functions   []JBeatsFunctionMeta `json:"functions"`
}

// ToFunctionMeta converts a JBeatsFunctionMeta to the canonical FunctionMeta type.
func ToFunctionMeta(j JBeatsFunctionMeta) FunctionMeta {
	return FunctionMeta{
		Name:    j.Name,
		Package: j.PackageName,
		FileMeta: FileMeta{
			Name: j.FileName,
			Path: j.FilePath,
			Lang: Language_JAVA,
		},
		Start_line:    j.StartLine,
		End_line:      j.EndLine,
		LineCount:     j.LineCount,
		IsMethod:      j.IsMethod,
		IsExported:    j.IsExported,
		Receiver:      j.Receiver,
		Params:        j.Params,
		Returns:       j.Returns,
		TokenSeq:      j.TokenSeq,
		TokenSeqHash:  j.TokenSeqHash,
		CallTargets:   j.CallTargets,
		DirectImports: j.DirectImports,
		Imports:       j.Imports,
		GeneratedCode: j.GeneratedCode,
		TestCode:      j.TestCode,
		IsConstructor: j.IsConstructor,
		Body:          j.Body,
	}
}
