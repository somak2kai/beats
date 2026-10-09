package ds

// TBeatsFunctionMeta mirrors the per-function JSON output from tbeats.
type TBeatsFunctionMeta struct {
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

// TBeatsFileResult mirrors the top-level JSON output from tbeats for a single file.
type TBeatsFileResult struct {
	Functions []TBeatsFunctionMeta `json:"functions"`
	TestCode  bool                 `json:"test_code"`
}

// TsToFunctionMeta converts a TBeatsFunctionMeta to the canonical FunctionMeta type.
func TsToFunctionMeta(t TBeatsFunctionMeta) FunctionMeta {
	return FunctionMeta{
		Name:    t.Name,
		Package: t.PackageName,
		FileMeta: FileMeta{
			Name: t.FileName,
			Path: t.FilePath,
			Lang: Language_TYPESCRIPT,
		},
		Start_line:    t.StartLine,
		End_line:      t.EndLine,
		LineCount:     t.LineCount,
		IsMethod:      t.IsMethod,
		IsExported:    t.IsExported,
		Receiver:      t.Receiver,
		Params:        t.Params,
		Returns:       t.Returns,
		TokenSeq:      t.TokenSeq,
		TokenSeqHash:  t.TokenSeqHash,
		CallTargets:   t.CallTargets,
		DirectImports: t.DirectImports,
		Imports:       t.Imports,
		GeneratedCode: t.GeneratedCode,
		TestCode:      t.TestCode,
		IsConstructor: t.IsConstructor,
		Body:          t.Body,
	}
}
