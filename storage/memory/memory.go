// Package memory implements storage.StorageAdapter backed by in-memory maps.
package memory

import (
	"bytes"
	"fmt"
	"sort"
	"sync"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage"
)

// Store implements storage.StorageAdapter using nested maps.
type Store struct {
	mu        sync.RWMutex
	data      map[core.Address]map[string][]byte
	snapshots map[string]map[core.Address]map[string][]byte
}

// New creates a new in-memory storage adapter.
func New() *Store {
	return &Store{
		data:      make(map[core.Address]map[string][]byte),
		snapshots: make(map[string]map[core.Address]map[string][]byte),
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

// Savepoint creates a named snapshot of the current state.
func (s *Store) Savepoint(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[name] = s.deepCopy()
	return nil
}

// RollbackTo restores state to a previously created savepoint.
func (s *Store) RollbackTo(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[name]
	if !ok {
		return fmt.Errorf("savepoint %q not found", name)
	}
	s.data = s.deepCopyFrom(snap)
	delete(s.snapshots, name)
	return nil
}

// ReleaseSavepoint discards a savepoint without rolling back.
func (s *Store) ReleaseSavepoint(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, name)
	return nil
}

func (s *Store) deepCopy() map[core.Address]map[string][]byte {
	return s.deepCopyFrom(s.data)
}

func (s *Store) deepCopyFrom(src map[core.Address]map[string][]byte) map[core.Address]map[string][]byte {
	cp := make(map[core.Address]map[string][]byte, len(src))
	for addr, m := range src {
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
