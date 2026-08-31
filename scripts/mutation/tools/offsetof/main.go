// Command offsetof prints a 0-based byte offset into a file, for gopls's
// CLI location syntax "file:#byteOffset". A real Go program instead of
// shell/awk byte-counting on purpose — offsets need to be exact, and
// shell portability has already bitten this script twice (mapfile,
// sort -V) from assuming BSD (macOS) and GNU (Linux) tools agree.
//
// Two modes:
//
//	offsetof <file> <substring>       offset of the first match of <substring>
//	offsetof -func <file> <FuncName>  offset of the identifier in a
//	                                   top-level "func FuncName(...)" declaration
//
// The -func mode exists so callers renaming a function never need to know
// or hardcode how long the "func " keyword is — it parses the file and
// points straight at the identifier gopls actually wants.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
)

func main() {
	args := os.Args[1:]

	switch {
	case len(args) == 3 && args[0] == "-func":
		funcOffset(args[1], args[2])
	case len(args) == 2:
		substringOffset(args[0], args[1])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  offsetof <file> <substring>")
	fmt.Fprintln(os.Stderr, "  offsetof -func <file> <FuncName>")
	os.Exit(2)
}

func substringOffset(file, needle string) {
	data, err := os.ReadFile(file)
	if err != nil {
		fail("offsetof: %v", err)
	}
	idx := bytes.Index(data, []byte(needle))
	if idx < 0 {
		fail("offsetof: %q not found in %s", needle, file)
	}
	fmt.Println(idx)
}

func funcOffset(file, name string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		fail("offsetof: %v", err)
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != name {
			continue
		}
		fmt.Println(fset.Position(fd.Name.Pos()).Offset)
		return
	}
	fail("offsetof: no top-level function %q in %s", name, file)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
