package util

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeTree creates small files at the given slash-separated paths under root.
func writeTree(t *testing.T, root string, files []string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func collectPaths(t *testing.T, root string) []string {
	t.Helper()
	meta, err := GetFileMetadata(root)
	if err != nil {
		t.Fatalf("GetFileMetadata: %v", err)
	}
	var got []string
	for _, files := range meta {
		for _, f := range files {
			rel, err := filepath.Rel(root, f.Path)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, filepath.ToSlash(rel))
		}
	}
	sort.Strings(got)
	return got
}

func TestGetFileMetadata_SkipsNonProductionJavaFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{
		// java: kept
		"module/src/main/java/org/x/App.java",
		"module/src/main/java/org/x/test/Helper.java", // production package named "test"
		"tools/test/Tool.java",                        // "test" dir not under src/
		// java: skipped
		"_out/Gen.java",
		"module/testdata/Fixture.java",
		"module/src/test/java/org/x/AppTest.java",
		"module/src/it/java/org/x/AppIT.java",
		"module/src/integrationTest/java/org/x/Flow.java",
		"module/src/testFixtures/java/org/x/Fixture.java",
		"module/src/jmh/java/org/x/Bench.java",
		"module/src/main/resources/Template.java",
		"module/src/test/resources/Sample.java",
		// go: the java rules must not apply
		"_out/gen.go",
		"pkg/testdata/fixture.go",
		"svc/src/test/t.go",
	})

	got := collectPaths(t, root)
	want := []string{
		"_out/gen.go",
		"module/src/main/java/org/x/App.java",
		"module/src/main/java/org/x/test/Helper.java",
		"pkg/testdata/fixture.go",
		"svc/src/test/t.go",
		"tools/test/Tool.java",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestIsSkippableJavaFile_RootNameIgnored(t *testing.T) {
	// only directories below the repo root count; the root's own name never does
	root := filepath.FromSlash("/tmp/_repo")
	if isSkippableJavaFile(root, filepath.Join(root, "src", "main", "java", "App.java")) {
		t.Fatal("file under a root named _repo must not be skipped")
	}
}
