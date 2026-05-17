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
		if !isGoFile(entry.Name()) {
			// continue
			return nil
		}

		if isSkippableGoFile(entry.Name()) {
			// continue
			return nil
		}
		pkg, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		if _, ok := resp[pkg]; !ok {
			resp[pkg] = []ds.FileMeta{{Name: entry.Name(), Path: path}}
			return nil
		}
		resp[pkg] = append(resp[pkg], ds.FileMeta{Name: entry.Name(), Path: path})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("unable to collect file metadata err: %w", err)
	}
	return resp, nil
}

func isGoFile(file string) bool {
	return strings.HasSuffix(file, ".go")
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
