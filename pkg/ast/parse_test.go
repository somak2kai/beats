package ast

import (
	"os"
	"path/filepath"
	"testing"

	ds "github.com/somak2kai/beats/pkg/types"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// writeTempGo writes src as a Go source file under a fresh temp directory and
// returns the corresponding FileMeta. The file is cleaned up automatically by t.
func writeTempGo(t *testing.T, src string) ds.FileMeta {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatalf("write temp Go file: %v", err)
	}
	return ds.FileMeta{Name: "test.go", Path: path}
}

// findFn returns the FunctionMeta with the given name, failing the test if not found.
func findFn(t *testing.T, fns []ds.FunctionMeta, name string) ds.FunctionMeta {
	t.Helper()
	for _, fn := range fns {
		if fn.Name == name {
			return fn
		}
	}
	names := make([]string, len(fns))
	for i, fn := range fns {
		names[i] = fn.Name
	}
	t.Fatalf("function %q not found; parsed functions: %v", name, names)
	return ds.FunctionMeta{}
}

// hasToken reports whether tok appears at least once in seq.
func hasToken(seq []int, tok int) bool {
	for _, t := range seq {
		if t == tok {
			return true
		}
	}
	return false
}

// countToken returns how many times tok appears in seq.
func countToken(seq []int, tok int) int {
	n := 0
	for _, t := range seq {
		if t == tok {
			n++
		}
	}
	return n
}

// ── basic metadata ────────────────────────────────────────────────────────────

func TestParseFile_ExportedFunction(t *testing.T) {
	src := `package testpkg
func Exported() {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Exported")
	if !fn.IsExported {
		t.Error("expected IsExported=true")
	}
	if fn.IsMethod {
		t.Error("expected IsMethod=false")
	}
	if fn.Receiver != "" {
		t.Errorf("expected empty Receiver, got %q", fn.Receiver)
	}
}

func TestParseFile_UnexportedFunction(t *testing.T) {
	src := `package testpkg
func helper() {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "helper")
	if fn.IsExported {
		t.Error("expected IsExported=false for unexported function")
	}
}

func TestParseFile_PointerReceiverMethod(t *testing.T) {
	src := `package testpkg
type S struct{}
func (s *S) Do() {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Do")
	if !fn.IsMethod {
		t.Error("expected IsMethod=true")
	}
	if fn.Receiver != "*S" {
		t.Errorf("expected Receiver=*S, got %q", fn.Receiver)
	}
}

func TestParseFile_ValueReceiverMethod(t *testing.T) {
	src := `package testpkg
type T struct{}
func (t T) Name() string { return "" }
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Name")
	if fn.Receiver != "T" {
		t.Errorf("expected Receiver=T, got %q", fn.Receiver)
	}
}

func TestParseFile_LineCounts(t *testing.T) {
	src := `package testpkg

func Multi() {
	_ = 1
	_ = 2
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Multi")
	if fn.LineCount < 3 {
		t.Errorf("expected LineCount ≥ 3 for a 4-line function body, got %d", fn.LineCount)
	}
	if fn.Start_line >= fn.End_line {
		t.Errorf("Start_line (%d) must be < End_line (%d)", fn.Start_line, fn.End_line)
	}
}

func TestParseFile_SkipsBodylessFuncDecl(t *testing.T) {
	// Interface method stubs have no body — ParseFile must skip them.
	src := `package testpkg
type Doer interface {
	Do() error
}
func Real() {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(fns) != 1 || fns[0].Name != "Real" {
		t.Fatalf("expected only Real(), got %d functions: %v", len(fns), fns)
	}
}

// ── params & returns ──────────────────────────────────────────────────────────

func TestParseFile_ParamCount(t *testing.T) {
	// a, b int counts as 2 params; c string counts as 1.
	src := `package testpkg
func Multi(a, b int, c string) {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Multi")
	if fn.Features.ParamCount != 3 {
		t.Errorf("expected ParamCount=3, got %d", fn.Features.ParamCount)
	}
}

func TestParseFile_HasErrorReturn(t *testing.T) {
	src := `package testpkg
func Fail() error { return nil }
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Fail")
	if !fn.Features.HasErrorReturn {
		t.Error("expected HasErrorReturn=true")
	}
	if fn.Features.ReturnCount != 1 {
		t.Errorf("expected ReturnCount=1, got %d", fn.Features.ReturnCount)
	}
}

func TestParseFile_HasContextParam(t *testing.T) {
	src := `package testpkg
import "context"
func WithCtx(ctx context.Context) {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "WithCtx")
	if !fn.Features.HasContextParam {
		t.Error("expected HasContextParam=true")
	}
}

func TestParseFile_HasFuncParam(t *testing.T) {
	src := `package testpkg
func WithCallback(fn func()) {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "WithCallback")
	if !fn.Features.HasFuncParam {
		t.Error("expected HasFuncParam=true")
	}
}

func TestParseFile_MultipleReturnValues(t *testing.T) {
	src := `package testpkg
func Two() (int, error) { return 0, nil }
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Two")
	if fn.Features.ReturnCount != 2 {
		t.Errorf("expected ReturnCount=2, got %d", fn.Features.ReturnCount)
	}
	if !fn.Features.HasErrorReturn {
		t.Error("expected HasErrorReturn=true")
	}
}

// ── control flow tokens ───────────────────────────────────────────────────────

func TestParseFile_IfToken(t *testing.T) {
	src := `package testpkg
func HasIf(x int) {
	if x > 0 {
	}
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "HasIf")
	if fn.Features.ControlFlow.If != 1 {
		t.Errorf("expected 1 if statement, got %d", fn.Features.ControlFlow.If)
	}
	if !hasToken(fn.TokenSeq, TK_IF) {
		t.Error("expected TK_IF in token sequence")
	}
}

func TestParseFile_ForToken(t *testing.T) {
	src := `package testpkg
func HasFor() {
	for i := 0; i < 3; i++ {
	}
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "HasFor")
	if fn.Features.ControlFlow.For != 1 {
		t.Errorf("expected 1 for loop, got %d", fn.Features.ControlFlow.For)
	}
	if !hasToken(fn.TokenSeq, TK_FOR) {
		t.Error("expected TK_FOR in token sequence")
	}
}

func TestParseFile_RangeToken(t *testing.T) {
	src := `package testpkg
func HasRange(s []int) {
	for range s {
	}
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "HasRange")
	if fn.Features.ControlFlow.Range != 1 {
		t.Errorf("expected 1 range loop, got %d", fn.Features.ControlFlow.Range)
	}
	if !hasToken(fn.TokenSeq, TK_RANGE) {
		t.Error("expected TK_RANGE in token sequence")
	}
}

func TestParseFile_DeferToken(t *testing.T) {
	src := `package testpkg
func HasDefer(f func()) {
	defer f()
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "HasDefer")
	if fn.Features.ControlFlow.Defer != 1 {
		t.Errorf("expected 1 defer, got %d", fn.Features.ControlFlow.Defer)
	}
	if !hasToken(fn.TokenSeq, TK_DEFER) {
		t.Error("expected TK_DEFER in token sequence")
	}
}

func TestParseFile_GoToken(t *testing.T) {
	src := `package testpkg
func Spawns(f func()) {
	go f()
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Spawns")
	if fn.Features.GoroutineSpawns != 1 {
		t.Errorf("expected GoroutineSpawns=1, got %d", fn.Features.GoroutineSpawns)
	}
	if !hasToken(fn.TokenSeq, TK_GO) {
		t.Error("expected TK_GO in token sequence")
	}
}

func TestParseFile_ReturnTokensAppendedPerReturnValue(t *testing.T) {
	// func with 2 return values → 2 trailing TK_RETURN tokens appended.
	// The body also has a return stmt → TK_RETURN from ast.Inspect.
	src := `package testpkg
func TwoRets() (int, error) { return 0, nil }
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "TwoRets")
	// 1 return stmt in body + 2 trailing = 3 TK_RETURN tokens total.
	if n := countToken(fn.TokenSeq, TK_RETURN); n != 3 {
		t.Errorf("expected 3 TK_RETURN tokens (1 body + 2 trailing), got %d", n)
	}
}

// ── call classification ───────────────────────────────────────────────────────

func TestParseFile_PackageCallIsCallPkg(t *testing.T) {
	src := `package testpkg
import "fmt"
func UsesFmt() {
	fmt.Println("hi")
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "UsesFmt")
	if !hasToken(fn.TokenSeq, TK_CALL_PKG) {
		t.Error("expected TK_CALL_PKG in token sequence for fmt.Println")
	}
}

func TestParseFile_MethodCallIsCallMethod(t *testing.T) {
	src := `package testpkg
type W struct{}
func (w *W) Write(b []byte) {}
func UsesMethod(w *W) {
	w.Write(nil)
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "UsesMethod")
	if !hasToken(fn.TokenSeq, TK_CALL_METHOD) {
		t.Error("expected TK_CALL_METHOD in token sequence for w.Write()")
	}
}

// ── imports & call targets ────────────────────────────────────────────────────

func TestParseFile_DirectImportsOnlyUsedPackages(t *testing.T) {
	// UsesFmtOnly references fmt but not os; os must not appear in DirectImports.
	src := `package testpkg
import (
	"fmt"
	"os"
)
func UsesFmtOnly() {
	fmt.Println("hi")
}
func UsesOs() {
	_ = os.Stderr
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "UsesFmtOnly")

	hasFmt, hasOs := false, false
	for _, imp := range fn.DirectImports {
		if imp == "fmt" {
			hasFmt = true
		}
		if imp == "os" {
			hasOs = true
		}
	}
	if !hasFmt {
		t.Error("expected fmt in DirectImports")
	}
	if hasOs {
		t.Error("os should not be in DirectImports — it is not referenced in UsesFmtOnly")
	}
}

func TestParseFile_CallTargetsPopulated(t *testing.T) {
	src := `package testpkg
import "fmt"
func CallsFmt() {
	fmt.Println("hi")
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "CallsFmt")
	found := false
	for _, ct := range fn.CallTargets {
		if ct == "fmt.Println" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fmt.Println in CallTargets, got %v", fn.CallTargets)
	}
}

func TestParseFile_ImportsFileLevelPopulated(t *testing.T) {
	// Imports (file-level) should contain all imported packages, even unused ones.
	src := `package testpkg
import (
	"fmt"
	"os"
)
func Fn() {
	fmt.Println("hi")
}
func UsesOs() {
	_ = os.Stderr
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Fn")
	hasFmt, hasOs := false, false
	for _, imp := range fn.Imports {
		if imp == "fmt" {
			hasFmt = true
		}
		if imp == "os" {
			hasOs = true
		}
	}
	if !hasFmt || !hasOs {
		t.Errorf("file-level Imports should contain all packages; hasFmt=%v hasOs=%v", hasFmt, hasOs)
	}
}

// ── complexity ────────────────────────────────────────────────────────────────

func TestParseFile_CyclomaticComplexity_Flat(t *testing.T) {
	// No branches → CyclomaticComplexity = 1 (base).
	src := `package testpkg
func Flat() { _ = 1 }
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Flat")
	if fn.Features.CyclomaticComplexity != 1 {
		t.Errorf("expected CyclomaticComplexity=1, got %d", fn.Features.CyclomaticComplexity)
	}
}

func TestParseFile_CyclomaticComplexity_NestedIf(t *testing.T) {
	// base=1, outer if +1, inner if +1 → 3.
	src := `package testpkg
func Nested(x, y int) int {
	if x > 0 {
		if y > 0 {
			return 1
		}
	}
	return 0
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Nested")
	if fn.Features.CyclomaticComplexity != 3 {
		t.Errorf("expected CyclomaticComplexity=3, got %d", fn.Features.CyclomaticComplexity)
	}
}

func TestParseFile_TokenSeqHashPopulated(t *testing.T) {
	src := `package testpkg
import "fmt"
func Hashed() {
	fmt.Println("hi")
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "Hashed")
	if len(fn.TokenSeqHash) == 0 {
		t.Error("expected non-empty TokenSeqHash after parsing")
	}
}

func TestParseFile_PackageName(t *testing.T) {
	src := `package mypkg
func F() {}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "F")
	if fn.Package != "mypkg" {
		t.Errorf("expected Package=mypkg, got %q", fn.Package)
	}
}

func TestParseFile_SwitchToken(t *testing.T) {
	src := `package testpkg
func HasSwitch(x int) {
	switch x {
	case 1:
	case 2:
	}
}
`
	fns, err := ParseFile(writeTempGo(t, src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	fn := findFn(t, fns, "HasSwitch")
	if fn.Features.ControlFlow.Switch != 1 {
		t.Errorf("expected 1 switch, got %d", fn.Features.ControlFlow.Switch)
	}
	if !hasToken(fn.TokenSeq, TK_SWITCH) {
		t.Error("expected TK_SWITCH in token sequence")
	}
}
