package db

import (
	"errors"
	"os"

	"github.com/tecbot/gorocksdb"
)

// KVDB is the (thin) abstraction layer of an implementation-agnostic key-value store.
type KVDB interface {
	// Get returns the value for a key.
	Get(key []byte) ([]byte, error)

	// Put stores the value for a key.
	Put(key []byte, value []byte) error

	// Delete removes the value for a key.
	Delete(key []byte) error

	// Close gracefully shuts down the database.
	Close() error
}

// RocksDB implements the KVStore interface with a thinly wrapped RocksDB instance.
type RocksDB struct {
	// Pointer to the RocksDB object
	rdb *gorocksdb.DB

	// Read options for generic reads
	ro *gorocksdb.ReadOptions

	// Write options for generic writes
	wo *gorocksdb.WriteOptions
}

// NewRocksDB creates a new RocksDB instance with default read and write options.
func NewRocksDB(dbDir string) (*RocksDB, error) {
	os.MkdirAll(dbDir, os.ModePerm)
	options := gorocksdb.NewDefaultOptions()
	options.SetCreateIfMissing(true)
	db, err := gorocksdb.OpenDb(options, dbDir)
	if err != nil {
		return nil, err
	}

	return &RocksDB{
		rdb: db,
		ro:  gorocksdb.NewDefaultReadOptions(),
		wo:  gorocksdb.NewDefaultWriteOptions(),
	}, nil
}

func (db *RocksDB) Get(key []byte) ([]byte, error) {
	// Return copy of bytes instead of a slice to make it simpler for the user. If this proves slow for large reads
	// we might want to add a separate method for getting the slice (or an abstraction of it) directly.
	if db == nil {
		return nil, errors.New("RocksDB struct is nil!")
	}
	if db.rdb == nil {
		return nil, errors.New("rdb is nil!")
	}
	return db.rdb.GetBytes(db.ro, key)
}

func (db *RocksDB) Put(key []byte, value []byte) error {
	return db.rdb.Put(db.wo, key, value)
}

func (db *RocksDB) Delete(key []byte) error {
	return db.rdb.Delete(db.wo, key)
}

func (db *RocksDB) Close() error {
	db.rdb.Close()
	return nil
}