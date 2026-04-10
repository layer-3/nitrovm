// Package memory implements storage.StorageAdapter backed by in-memory maps.
package memory

import (
	"bytes"
	"sort"
	"sync"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage"
)

// Store implements storage.StorageAdapter using nested maps.
type Store struct {
	mu   sync.RWMutex
	data map[core.Address]map[string][]byte
}

// New creates a new in-memory storage adapter.
func New() *Store {
	return &Store{
		data: make(map[core.Address]map[string][]byte),
	}
}

func (s *Store) Get(contractAddr core.Address, key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.data[contractAddr]; ok {
		if v, ok := m[string(key)]; ok {
			return append([]byte(nil), v...), nil
		}
	}
	return nil, nil
}

func (s *Store) Set(contractAddr core.Address, key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data[contractAddr]
	if !ok {
		m = make(map[string][]byte)
		s.data[contractAddr] = m
	}
	m[string(key)] = append([]byte(nil), value...)
	return nil
}

func (s *Store) Delete(contractAddr core.Address, key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.data[contractAddr]; ok {
		delete(m, string(key))
	}
	return nil
}

func (s *Store) Range(contractAddr core.Address, start, end []byte, order storage.Order) (storage.StorageIterator, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	m := s.data[contractAddr]
	var keys []string
	for k := range m {
		kb := []byte(k)
		if start != nil && bytes.Compare(kb, start) < 0 {
			continue
		}
		if end != nil && bytes.Compare(kb, end) >= 0 {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if order == storage.Descending {
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
	}

	pairs := make([]kvPair, len(keys))
	for i, k := range keys {
		pairs[i] = kvPair{
			key:   []byte(k),
			value: append([]byte(nil), m[k]...),
		}
	}
	return &sliceIterator{pairs: pairs}, nil
}

func (s *Store) Close() error { return nil }

// Snapshot returns a deep copy of the store's data for rollback.
func (s *Store) Snapshot() any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deepCopy()
}

// Restore replaces the store's data from a snapshot previously returned by Snapshot.
func (s *Store) Restore(snap any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = snap.(map[core.Address]map[string][]byte)
}

// ForEach iterates over all stored key-value pairs.
// Used to flush in-memory state to persistent storage.
func (s *Store) ForEach(fn func(addr core.Address, key, value []byte)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for addr, m := range s.data {
		for k, v := range m {
			fn(addr, []byte(k), v)
		}
	}
}

func (s *Store) deepCopy() map[core.Address]map[string][]byte {
	cp := make(map[core.Address]map[string][]byte, len(s.data))
	for addr, m := range s.data {
		cm := make(map[string][]byte, len(m))
		for k, v := range m {
			cm[k] = append([]byte(nil), v...)
		}
		cp[addr] = cm
	}
	return cp
}

// sliceIterator pre-loads all results into memory.
type sliceIterator struct {
	pairs []kvPair
	pos   int
}

type kvPair struct {
	key   []byte
	value []byte
}

func (it *sliceIterator) Valid() bool { return it.pos < len(it.pairs) }
func (it *sliceIterator) Next()       { it.pos++ }

func (it *sliceIterator) Key() []byte {
	if !it.Valid() {
		return nil
	}
	return it.pairs[it.pos].key
}

func (it *sliceIterator) Value() []byte {
	if !it.Valid() {
		return nil
	}
	return it.pairs[it.pos].value
}

func (it *sliceIterator) Close() error { return nil }
func (it *sliceIterator) Error() error { return nil }
