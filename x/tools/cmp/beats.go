package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
)

// loadClusters reads a beats cluster JSONL file (one ds.Cluster per line)
// as produced by dumpClusters in cmd/main.go.
func loadClusters(path string) ([]ds.Cluster, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening cluster file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	var clusters []ds.Cluster
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 8*1024*1024), 8*1024*1024) // 8 MB per line — large clusters can be big
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c ds.Cluster
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("unmarshalling cluster: %w", err)
		}
		clusters = append(clusters, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning cluster file: %w", err)
	}
	return clusters, nil
}

// badgerPathForRepo returns the BadgerDB path beats writes for a given repo,
// mirroring the logic in cmd/cmd.go: filepath.Join(os.TempDir(), "badger", repoPath).
// Using os.TempDir() here ensures the path is correct regardless of platform —
// on macOS $TMPDIR is /var/folders/…/T/, not /tmp, and it differs per user/machine.
func badgerPathForRepo(repoPath string) string {
	return filepath.Join(os.TempDir(), "badger", repoPath)
}

// loadClustersFromBadger takes a repo path (e.g. /Users/alice/src/myrepo),
// resolves the beats BadgerDB location via os.TempDir(), and returns all clusters
// stored under the TierIdentified prefix.
func loadClustersFromBadger(repoPath string) ([]ds.Cluster, error) {
	dbPath := badgerPathForRepo(repoPath)
	bdb := db.NewBadgerXDb(dbPath)
	defer bdb.Close() //nolint:errcheck
	clusters, err := bdb.ScanClusters(db.TierIdentified)
	if err != nil {
		return nil, fmt.Errorf("scanning %s tier from %s: %w", db.TierIdentified, dbPath, err)
	}
	return clusters, nil
}

// qualifiedName returns a human-readable identifier for a function member.
// Used for reporting divergent members.
func qualifiedName(m ds.FunctionMeta) string {
	if m.Receiver != "" {
		return fmt.Sprintf("(%s).%s", m.Receiver, m.Name)
	}
	return fmt.Sprintf("%s.%s", m.Package, m.Name)
}
