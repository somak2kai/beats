package util

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"strings"

	ds "github.com/somak2kai/beats/pkg/types"
)

// GetFileMetadata returns the file metadata per package name found in provided path.
// returns empty if no golang source code found.
func GetFileMetadata(root string) (ds.PkgToFileMeta, error) {
	root = filepath.Clean(root)
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("invalid path provided path: %s, err: %w", root, err)
	}
	resp := make(ds.PkgToFileMeta)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {

			// quick/dirty way to remove all vendored code.
			if strings.EqualFold(entry.Name(), "vendor") {
				return filepath.SkipDir
			}
			// continue
			return nil
		}
		if !IsAcceptedLanguageFile(entry.Name()) {
			// continue
			return nil
		}

		if isSkippableGoFile(entry.Name()) {
			// continue
			return nil
		}

		if strings.HasSuffix(entry.Name(), ".java") && isSkippableJavaFile(root, path) {
			// continue
			return nil
		}
		pkg, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		lang := ds.Language_GOLANG
		if strings.HasSuffix(path, ".java") {
			lang = ds.Language_JAVA
		}
		if _, ok := resp[pkg]; !ok {
			resp[pkg] = []ds.FileMeta{{Name: entry.Name(), Path: path, Lang: lang}}
			return nil
		}

		resp[pkg] = append(resp[pkg], ds.FileMeta{Name: entry.Name(), Path: path, Lang: lang})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("unable to collect file metadata err: %w", err)
	}
	return resp, nil
}

func IsAcceptedLanguageFile(file string) bool {
	return strings.HasSuffix(file, ".go") || strings.HasSuffix(file, ".java")
}

// skip proto generated files/ test and mock files.
// TODO auto gen code are not skipped well eg https://github.com/linkerd/linkerd2/blob/main/controller/gen/client/informers/externalversions/serviceprofile/v1alpha2/interface.go.
func isSkippableGoFile(file string) bool {
	return strings.HasSuffix(file, ".pb.go") ||
		strings.HasSuffix(file, ".pb.gw.go") ||
		strings.HasSuffix(file, "_test.go") ||
		strings.HasSuffix(file, ".ignore.go") ||
		strings.HasSuffix(file, "invalid.go") ||
		strings.HasPrefix(file, "mock_") ||
		strings.HasPrefix(file, "zz_generated")
}

// skip java files that live outside production source:
//   - under "_" prefixed dirs or testdata: fixtures and deliberately odd code that shows up
//     as fake outliers.
//   - under src/test, src/it, src/integrationTest, src/testFixtures, src/jmh: Maven/Gradle test
//     and benchmark source roots. Only matched directly under src/, since production packages
//     can be named "test" (eg org.apache.hadoop.test).
//   - under src/main/resources, src/test/resources: resources, not code.
func isSkippableJavaFile(root, path string) bool {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return false
	}
	// wrap in slashes so "/src/test/" never matches "src/testing" or "mysrc/test".
	dir := "/" + filepath.ToSlash(rel) + "/"
	for _, skip := range []string{
		"/_", "/testdata/",
		"/src/test/", "/src/it/", "/src/integrationTest/", "/src/testFixtures/", "/src/jmh/",
		"/src/main/resources/",
	} {
		if strings.Contains(dir, skip) {
			return true
		}
	}
	return false
}
