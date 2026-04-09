package nitrovm

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
	Get(contractAddr Address, key []byte) ([]byte, error)
	Set(contractAddr Address, key []byte, value []byte) error
	Delete(contractAddr Address, key []byte) error
	Range(contractAddr Address, start, end []byte, order Order) (StorageIterator, error)
	Close() error
}
