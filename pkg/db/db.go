package db

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"log"

	badger "github.com/dgraph-io/badger/v4"
	ds "github.com/somak2kai/beats/pkg/types"
)

const (
	TierRaw       = "raw"
	TierCollapsed = "collapsed"
	TierLabel     = "label"
)

type BadgerDb struct {
	db   *badger.DB
	path string
}

func NewDb(path string) *BadgerDb {
	opts := badger.
		DefaultOptions(path).
		WithValueLogFileSize(128 << 20) // 128MB instead of default 2GB pre-allocation
	db, err := badger.Open(opts)
	if err != nil {
		log.Fatal(err)
	}
	return &BadgerDb{db: db, path: path}
}

func (d *BadgerDb) Close() error {
	return d.db.Close()
}

func (d *BadgerDb) Save(key string, v any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), buf.Bytes())
	})
}

func (d *BadgerDb) Load(key string, dst any) error {
	return d.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return gobDecode(val, dst)
		})
	})
}
func (d *BadgerDb) StoreFunctionMeta(id string, fn ds.FunctionMeta) error {
	key := append([]byte("fncId:"), []byte(id)...)
	val, err := gobEncode(fn)
	if err != nil {
		log.Fatal("unable to save function meta", err)
		return err
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, val)
	})
}

func (d *BadgerDb) StorePostings(hash int64, fnId []string) error {
	key := append([]byte("post:"), int64ToBytes(hash)...)
	val, err := gobEncode(fnId)
	if err != nil {
		log.Fatal("unable to save document inverted index", err)
		return err
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, val)
	})
}

func (d *BadgerDb) StoreDocFreq(hash int64, count int) error {
	key := append([]byte("freq:"), int64ToBytes(hash)...)
	val, err := gobEncode(count)
	if err != nil {
		log.Fatal("unable to save document frequency", err)
		return err
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, val)
	})
}

func int64ToBytes(hash int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(hash))
	return b
}

func gobEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gobDecode(data []byte, v any) error {
	buf := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buf)
	return dec.Decode(v)
}

// StoreCluster persists a cluster under the given tier prefix.
// Key format: cluster:<tier>:<shapeHash>
func (d *BadgerDb) StoreCluster(tier, shapeHash string, c ds.Cluster) error {
	key := fmt.Sprintf("cluster:%s:%s", tier, shapeHash)
	val, err := gobEncode(c)
	if err != nil {
		return fmt.Errorf("encode cluster %s/%s: %w", tier, shapeHash, err)
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), val)
	})
}

// LoadCluster retrieves a single cluster by tier and shapeHash.
func (d *BadgerDb) LoadCluster(tier, shapeHash string) (ds.Cluster, error) {
	key := fmt.Sprintf("cluster:%s:%s", tier, shapeHash)
	var c ds.Cluster
	err := d.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return gobDecode(val, &c)
		})
	})
	return c, err
}

// StoreMemberScore persists a MemberScore under key score:<functionID>.
func (d *BadgerDb) StoreMemberScore(id string, ms ds.MemberScore) error {
	key := fmt.Sprintf("score:%s", id)
	val, err := gobEncode(ms)
	if err != nil {
		return fmt.Errorf("encode member score %s: %w", id, err)
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), val)
	})
}

// LoadMemberScore retrieves a MemberScore by functionID.
func (d *BadgerDb) LoadMemberScore(id string) (ds.MemberScore, error) {
	key := fmt.Sprintf("score:%s", id)
	var ms ds.MemberScore
	err := d.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return gobDecode(val, &ms)
		})
	})
	return ms, err
}

// ScanClusters returns all clusters stored under the given tier prefix.
func (d *BadgerDb) ScanClusters(tier string) ([]ds.Cluster, error) {
	prefix := []byte(fmt.Sprintf("cluster:%s:", tier))
	var clusters []ds.Cluster
	err := d.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			var c ds.Cluster
			if err := it.Item().Value(func(val []byte) error {
				return gobDecode(val, &c)
			}); err != nil {
				return err
			}
			clusters = append(clusters, c)
		}
		return nil
	})
	return clusters, err
}
