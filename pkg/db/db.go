package db

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"log"

	badger "github.com/dgraph-io/badger/v4"
	ds "github.com/somak2kai/beats/pkg/types"
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
