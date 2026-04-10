package storage

import "github.com/layer-3/nitrovm/core"

// Order specifies key iteration order.
type Order uint8

const (
	Ascending  Order = 1
	Descending Order = 2
)

// StorageIterator iterates over key-value pairs.
type StorageIterator interface {
	Valid() bool
	Next()
	Key() []byte
	Value() []byte
	Close() error
}

// StorageAdapter is the pluggable backend for per-contract key-value storage.
type StorageAdapter interface {
	Get(contractAddr core.Address, key []byte) ([]byte, error)
	Set(contractAddr core.Address, key []byte, value []byte) error
	Delete(contractAddr core.Address, key []byte) error
	Range(contractAddr core.Address, start, end []byte, order Order) (StorageIterator, error)
	Close() error
}

// Snapshotable is optionally implemented by StorageAdapter backends
// that support snapshot/restore (e.g., in-memory stores).
// The VM uses this to include storage in its own Snapshot/Restore cycle,
// eliminating the need for separate transactional savepoints.
type Snapshotable interface {
	Snapshot() any
	Restore(snap any)
}
