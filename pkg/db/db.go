package db

import (
	"encoding/binary"
	"fmt"
	"log"

	badger "github.com/dgraph-io/badger/v4"
	badgerx "github.com/somak2kai/badgerx"
	ds "github.com/somak2kai/beats/pkg/types"
)

const (
	TierRaw        = "raw"
	TierCollapsed  = "collapsed"
	TierLabel      = "label"
	TierIdentified = "identified"
)

type BadgerDb struct {
	db   *badger.DB
	path string
}
type BadgerXDb struct {
	db *badgerx.BadgerXDb
}

func NewDb(path string) *BadgerDb {
	opts := badger.
		DefaultOptions(path).
		WithValueLogFileSize(128 << 20). // 128MB instead of default 2GB pre-allocation
		WithLoggingLevel(badger.ERROR)
	db, err := badger.Open(opts)
	if err != nil {
		log.Fatal(err)
	}
	return &BadgerDb{db: db, path: path}
}

func NewBadgerXDb(path string) *BadgerXDb {
	opts := badger.
		DefaultOptions(path).
		WithValueLogFileSize(128 << 20). // 128MB instead of default 2GB pre-allocation
		WithLoggingLevel(badger.ERROR)
	db, err := badger.Open(opts)
	if err != nil {
		log.Fatal(err)
	}
	return &BadgerXDb{badgerx.NewBadgerXDb(db, badgerx.WithCompressor(&badgerx.DefaultNoOpCompressor{}))}
}

func (d *BadgerDb) Close() error {
	return d.db.Close()
}

func (d *BadgerXDb) Close() error {
	return d.db.Close()
}

func (d *BadgerXDb) StoreFunctionMeta(id string, fn ds.FunctionMeta) error {
	key := append([]byte("fncId:"), []byte(id)...)
	return d.db.Update(key, fn)
}

func (d *BadgerXDb) StorePostings(hash int64, fnId []string) error {
	key := append([]byte("post:"), int64ToBytes(hash)...)
	return d.db.Update(key, fnId)
}

func (d *BadgerXDb) StoreDocFreq(hash int64, count int) error {
	key := append([]byte("freq:"), int64ToBytes(hash)...)
	return d.db.Update(key, count)
}

func (d *BadgerXDb) StoreCluster(tier, shapeHash string, c ds.Cluster) error {
	key := fmt.Sprintf("cluster:%s:%s", tier, shapeHash)
	return d.db.Update([]byte(key), c)
}

// StoreClusterByIndex stores a cluster under a zero-padded numeric key so that
// clusters can be retrieved by sequential index without knowing the shape hash.
// Key format: cluster:<tier>:<10-digit-index>  e.g. cluster:identified:0000000042
func (d *BadgerXDb) StoreClusterByIndex(tier string, idx int, c ds.Cluster) error {
	key := fmt.Sprintf("cluster:%s:%05d", tier, idx)
	return d.db.Update([]byte(key), c)
}

// LoadClusterByIndex retrieves a single cluster by tier and numeric index.
func (d *BadgerXDb) LoadClusterByIndex(tier string, idx int) (ds.Cluster, error) {
	key := fmt.Sprintf("cluster:%s:%05d", tier, idx)
	var c ds.Cluster
	err := d.db.View([]byte(key), &c)
	return c, err
}

// StoreClusterCount persists the total number of clusters for a tier so callers
// don't have to scan the full prefix to discover how many there are.
// Key format: meta:<tier>:count
func (d *BadgerXDb) StoreClusterCount(tier string, count int) error {
	key := fmt.Sprintf("meta:%s:count", tier)
	return d.db.Update([]byte(key), count)
}

// LoadClusterCount returns the number of clusters stored for a tier.
func (d *BadgerXDb) LoadClusterCount(tier string) (int, error) {
	key := fmt.Sprintf("meta:%s:count", tier)
	var count int
	err := d.db.View([]byte(key), &count)
	return count, err
}

// LoadCluster retrieves a single cluster by tier and shapeHash.
func (d *BadgerXDb) LoadCluster(tier, shapeHash string) (ds.Cluster, error) {
	key := fmt.Sprintf("cluster:%s:%s", tier, shapeHash)
	var c ds.Cluster
	err := d.db.View([]byte(key), &c)
	return c, err
}

func int64ToBytes(hash int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(hash))
	return b
}

// StoreOrphanedFunctions persists the full slice of OrphanedFunction values.
// Key: meta:orphans
func (d *BadgerXDb) StoreOrphanedFunctions(orphans []ds.OrphanedFunction) error {
	return d.db.Update([]byte("meta:orphans"), orphans)
}

// LoadOrphanedFunctions retrieves the orphaned functions slice, or returns an
// empty slice when none have been stored yet.
func (d *BadgerXDb) LoadOrphanedFunctions() ([]ds.OrphanedFunction, error) {
	var orphans []ds.OrphanedFunction
	err := d.db.View([]byte("meta:orphans"), &orphans)
	if err != nil {
		// Key not found is not an error — orphan analysis may not have run yet
		return nil, nil
	}
	return orphans, nil
}

// ScanClusters returns all clusters stored under the given tier prefix.
func (d *BadgerXDb) ScanClusters(tier string) ([]ds.Cluster, error) {
	prefix := []byte(fmt.Sprintf("cluster:%s:", tier))
	var clusters []ds.Cluster
	err := d.db.IterateView(prefix, badger.DefaultIteratorOptions, func(decode badgerx.DecodeFunc) error {
		var c ds.Cluster
		if err := decode(&c); err != nil {
			return err
		}
		clusters = append(clusters, c)
		return nil
	})
	return clusters, err
}
