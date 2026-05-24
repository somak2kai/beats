package ast

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/somak2kai/beats/pkg/hash"
	ds "github.com/somak2kai/beats/pkg/types"
)

const (
	TK_IF = iota
	TK_FOR
	TK_RANGE
	TK_SWITCH
	TK_CASE
	TK_SELECT
	TK_COMM
	TK_RETURN
	TK_GO
	TK_SEND
	TK_DEFER
	TK_CONTINUE
	TK_BREAK
	TK_GOTO
	TK_CALL // plain local call: drawBlock(...), delete(...), make(...)
	TK_FUNCLIT
	TK_ASSIGN
	TK_CALL_PKG    // package-qualified call: fmt.Sprintf(...), xorm.In(...)
	TK_CALL_METHOD // method or chained call: rref.LinkName(), w.Close(), a.b.Method()
)

func ParseFile(f ds.FileMeta) ([]ds.FunctionMeta, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.Path, nil, parser.AllErrors|parser.ParseComments)
	if err != nil {
		return nil, err
	}

	imports := extractImports(file)
	aliasMap := buildImportAliasMap(file)
	isGeneratedCode := isGeneratedCode(file)

	funcs := make([]ds.FunctionMeta, 0)

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}

		start := fset.Position(fn.Pos())
		end := fset.Position(fn.End())

		features, tokens := extractStructuralFeatures(fn, aliasMap)
		funcs = append(funcs, ds.FunctionMeta{
			Package:       file.Name.Name,
			FileMeta:      f,
			Start_line:    start.Line,
			End_line:      end.Line,
			LineCount:     end.Line - start.Line + 1,
			Name:          fn.Name.Name,
			IsMethod:      fn.Recv != nil,
			IsExported:    fn.Name.IsExported(),
			Receiver:      extractReceiver(fn),
			Params:        extractParams(fn.Type.Params),
			Returns:       extractReturns(fn.Type.Results),
			Features:      features,
			TokenSeq:      tokens,
			TokenSeqHash:  hash.ComputeWindowHash(tokens),
			Imports:       imports,
			CallTargets:   extractCallTargets(fn, aliasMap),
			DirectImports: extractDirectImports(fn, aliasMap),
			GeneratedCode: isGeneratedCode,
		})
		return true
	})

	return funcs, nil
}

func isGeneratedCode(file *ast.File) bool {
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "// Code generated") {
				return true
			}
		}
	}
	return false
}

func extractImports(file *ast.File) []string {
	out := make([]string, 0, len(file.Imports))
	for _, imp := range file.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out
}

func extractCallTargets(fn *ast.FuncDecl, aliasMap map[string]string) []string {
	seen := make(map[string]bool)
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true // not a selector call (plain func call, method on local var)
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true // chained call like a.b.Method() — X is not a bare ident
		}
		if path, exists := aliasMap[ident.Name]; exists {
			seen[path+"."+sel.Sel.Name] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out
}

// buildImportAliasMap maps local identifier → full import path.
func buildImportAliasMap(file *ast.File) map[string]string {
	m := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			m[imp.Name.Name] = path
		} else {
			parts := strings.Split(path, "/")
			m[parts[len(parts)-1]] = path
		}
	}
	return m
}

// extractDirectImports finds import paths directly referenced by fn via SelectorExpr.
func extractDirectImports(fn *ast.FuncDecl, aliasMap map[string]string) []string {
	seen := make(map[string]bool)
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path, exists := aliasMap[ident.Name]; exists {
			seen[path] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	return out
}

func extractReceiver(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return "*" + ident.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func extractParams(fields *ast.FieldList) []ds.ParamInfo {
	if fields == nil {
		return nil
	}
	var out []ds.ParamInfo
	for _, field := range fields.List {
		typeName := typeString(field.Type)
		_, isFuncType := field.Type.(*ast.FuncType)
		_, isIface := field.Type.(*ast.InterfaceType)
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for range make([]struct{}, n) {
			out = append(out, ds.ParamInfo{
				TypeName:    typeName,
				IsFuncType:  isFuncType,
				IsInterface: isIface,
			})
		}
	}
	return out
}

func extractReturns(fields *ast.FieldList) []ds.ReturnInfo {
	if fields == nil {
		return nil
	}
	var out []ds.ReturnInfo
	for _, field := range fields.List {
		typeName := typeString(field.Type)
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for range make([]struct{}, n) {
			out = append(out, ds.ReturnInfo{
				TypeName: typeName,
				IsError:  typeName == "error",
			})
		}
	}
	return out
}

func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	case *ast.MapType:
		return "map[" + typeString(t.Key) + "]" + typeString(t.Value)
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.ChanType:
		return "chan " + typeString(t.Value)
	case *ast.Ellipsis:
		return "..." + typeString(t.Elt)
	}
	return ""
}

func extractStructuralFeatures(fn *ast.FuncDecl, aliasMap map[string]string) (ds.StructuralFeatures, []int) {
	var f ds.StructuralFeatures
	f.CyclomaticComplexity = 1

	tokens := make([]int, 0)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			f.ControlFlow.If++
			f.CyclomaticComplexity++
			tokens = append(tokens, TK_IF)
		case *ast.ForStmt:
			f.ControlFlow.For++
			f.CyclomaticComplexity++
			tokens = append(tokens, TK_FOR)
		case *ast.RangeStmt:
			f.ControlFlow.Range++
			f.CyclomaticComplexity++
			tokens = append(tokens, TK_RANGE)
		case *ast.SwitchStmt, *ast.TypeSwitchStmt:
			f.ControlFlow.Switch++
			tokens = append(tokens, TK_SWITCH)
		case *ast.CaseClause:
			if node.List != nil {
				f.CyclomaticComplexity++
			}
			tokens = append(tokens, TK_CASE)
		case *ast.SelectStmt:
			f.ControlFlow.Select++
			tokens = append(tokens, TK_SELECT)
		case *ast.CommClause:
			f.CyclomaticComplexity++
			tokens = append(tokens, TK_COMM)
		case *ast.ReturnStmt:
			f.ControlFlow.Return++
			tokens = append(tokens, TK_RETURN)
		case *ast.GoStmt:
			f.ControlFlow.Go++
			f.GoroutineSpawns++
			tokens = append(tokens, TK_GO)
		case *ast.SendStmt:
			f.ControlFlow.Send++
			tokens = append(tokens, TK_SEND)
		case *ast.DeferStmt:
			f.ControlFlow.Defer++
			tokens = append(tokens, TK_DEFER)
		case *ast.BranchStmt:
			switch node.Tok {
			case token.CONTINUE:
				f.ControlFlow.Continue++
				tokens = append(tokens, TK_CONTINUE)
			case token.BREAK:
				f.ControlFlow.Break++
				tokens = append(tokens, TK_BREAK)
			case token.GOTO:
				f.ControlFlow.Goto++
				f.CyclomaticComplexity++
				tokens = append(tokens, TK_GOTO)
			}
		case *ast.CallExpr:
			f.OutboundCalls++
			tokens = append(tokens, classifyCall(node, aliasMap))
		case *ast.FuncLit:
			f.FuncLiteralCount++
			tokens = append(tokens, TK_FUNCLIT)
		case *ast.AssignStmt:
			tokens = append(tokens, TK_ASSIGN)
		}
		return true
	})

	f.NestingDepth = computeNestingDepth(fn.Body)
	f.BranchingDepth = computeBranchingDepth(fn.Body)
	f.EarlyReturns = countEarlyReturns(fn)
	f.ParamCount = fieldCount(fn.Type.Params)
	f.ReturnCount = fieldCount(fn.Type.Results)
	f.HasFuncParam = hasFuncTypeParam(fn.Type.Params)
	f.HasContextParam = hasContextParam(fn.Type.Params)
	f.HasErrorReturn = hasErrorReturn(fn.Type.Results)

	// Append one TK_RETURN token per return value so that functions sharing
	// an identical control-flow body but different return contracts produce
	// different token sequences and land in different clusters.
	//
	// func() error             → [...body..., RETURN]
	// func() (*T, error)       → [...body..., RETURN, RETURN]
	// func() (*T, *R, error)   → [...body..., RETURN, RETURN, RETURN]
	for range f.ReturnCount {
		tokens = append(tokens, TK_RETURN)
	}

	return f, tokens
}

// classifyCall returns the token type for a call expression:
//   - TK_CALL_PKG    — package-qualified call where the selector's left-hand side
//     is a known import alias: fmt.Sprintf(...), xorm.In(...).
//   - TK_CALL_METHOD — method or chained call where the left-hand side is a
//     variable or another expression: rref.LinkName(), w.Close(), a.b.Method().
//   - TK_CALL        — plain identifier call (local function, builtin, type
//     conversion): drawBlock(...), make(...), int64(x).
//
// This three-way split is what separates b19 ([ASSIGN CALL CALL CALL CALL])
// from RecipeUploadURLs ([ASSIGN CALL CALL_PKG CALL CALL_METHOD]) — functions
// that are semantically unrelated but would otherwise share an identical
// token sequence.
func classifyCall(call *ast.CallExpr, aliasMap map[string]string) int {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return TK_CALL // plain ident call or type conversion
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return TK_CALL_METHOD // chained: a.b.Method() — X is not a bare ident
	}
	if _, isPkg := aliasMap[ident.Name]; isPkg {
		return TK_CALL_PKG // known import alias: fmt.Sprintf, xorm.In
	}
	return TK_CALL_METHOD // variable receiver: rref.LinkName(), w.Close()
}

// computeNestingDepth returns max nesting depth of scope-opening constructs.
func computeNestingDepth(body *ast.BlockStmt) int {
	if body == nil {
		return 0
	}
	max := 0
	nestingWalk(body, 0, &max)
	return max
}

func nestingWalk(node ast.Node, depth int, max *int) {
	if depth > *max {
		*max = depth
	}
	switch n := node.(type) {
	case *ast.BlockStmt:
		for _, stmt := range n.List {
			nestingWalk(stmt, depth, max)
		}
	case *ast.IfStmt:
		nestingWalk(n.Body, depth+1, max)
		if n.Else != nil {
			nestingWalk(n.Else, depth+1, max)
		}
	case *ast.ForStmt:
		nestingWalk(n.Body, depth+1, max)
	case *ast.RangeStmt:
		nestingWalk(n.Body, depth+1, max)
	case *ast.SwitchStmt:
		nestingWalk(n.Body, depth+1, max)
	case *ast.TypeSwitchStmt:
		nestingWalk(n.Body, depth+1, max)
	case *ast.SelectStmt:
		nestingWalk(n.Body, depth+1, max)
	case *ast.CaseClause:
		for _, stmt := range n.Body {
			nestingWalk(stmt, depth, max)
		}
	case *ast.CommClause:
		for _, stmt := range n.Body {
			nestingWalk(stmt, depth, max)
		}
	case *ast.FuncLit:
		nestingWalk(n.Body, depth+1, max)
	}
}

// computeBranchingDepth returns max nesting depth of branching constructs only (if/switch/select).
func computeBranchingDepth(body *ast.BlockStmt) int {
	if body == nil {
		return 0
	}
	max := 0
	branchingWalk(body, 0, &max)
	return max
}

func branchingWalk(node ast.Node, depth int, max *int) {
	switch n := node.(type) {
	case *ast.BlockStmt:
		for _, stmt := range n.List {
			branchingWalk(stmt, depth, max)
		}
	case *ast.IfStmt:
		if depth+1 > *max {
			*max = depth + 1
		}
		branchingWalk(n.Body, depth+1, max)
		if n.Else != nil {
			branchingWalk(n.Else, depth+1, max)
		}
	case *ast.ForStmt:
		branchingWalk(n.Body, depth, max)
	case *ast.RangeStmt:
		branchingWalk(n.Body, depth, max)
	case *ast.SwitchStmt:
		if depth+1 > *max {
			*max = depth + 1
		}
		branchingWalk(n.Body, depth+1, max)
	case *ast.TypeSwitchStmt:
		if depth+1 > *max {
			*max = depth + 1
		}
		branchingWalk(n.Body, depth+1, max)
	case *ast.SelectStmt:
		if depth+1 > *max {
			*max = depth + 1
		}
		branchingWalk(n.Body, depth+1, max)
	case *ast.CaseClause:
		for _, stmt := range n.Body {
			branchingWalk(stmt, depth, max)
		}
	case *ast.CommClause:
		for _, stmt := range n.Body {
			branchingWalk(stmt, depth, max)
		}
	case *ast.FuncLit:
		branchingWalk(n.Body, depth, max)
	}
}

// countEarlyReturns counts return statements before the last top-level statement.
func countEarlyReturns(fn *ast.FuncDecl) int {
	stmts := fn.Body.List
	count := 0
	for i, stmt := range stmts {
		if _, ok := stmt.(*ast.ReturnStmt); ok && i < len(stmts)-1 {
			count++
		}
	}
	return count
}

func fieldCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	count := 0
	for _, f := range fields.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		count += n
	}
	return count
}

func hasFuncTypeParam(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, f := range fields.List {
		if _, ok := f.Type.(*ast.FuncType); ok {
			return true
		}
	}
	return false
}

func hasContextParam(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, f := range fields.List {
		if sel, ok := f.Type.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok {
				if ident.Name == "context" && sel.Sel.Name == "Context" {
					return true
				}
			}
		}
	}
	return false
}

func hasErrorReturn(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, f := range fields.List {
		if ident, ok := f.Type.(*ast.Ident); ok && ident.Name == "error" {
			return true
		}
	}
	return false
}
