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
