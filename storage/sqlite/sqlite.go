// Package sqlite implements storage.StorageAdapter backed by SQLite.
package sqlite

import (
	"database/sql"
	"fmt"
	"regexp"
	"sync"

	_ "github.com/mattn/go-sqlite3"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage"
)

// validSavepointName matches only safe alphanumeric/underscore savepoint names.
var validSavepointName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Store implements storage.StorageAdapter backed by SQLite.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// New opens (or creates) a SQLite database at path for contract storage.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS contract_storage (
		contract_addr BLOB NOT NULL,
		key BLOB NOT NULL,
		value BLOB NOT NULL,
		PRIMARY KEY (contract_addr, key)
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Get(contractAddr core.Address, key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var value []byte
	err := s.db.QueryRow(
		"SELECT value FROM contract_storage WHERE contract_addr = ? AND key = ?",
		contractAddr[:], key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return value, err
}

func (s *Store) Set(contractAddr core.Address, key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO contract_storage (contract_addr, key, value) VALUES (?, ?, ?)",
		contractAddr[:], key, value,
	)
	return err
}

func (s *Store) Delete(contractAddr core.Address, key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"DELETE FROM contract_storage WHERE contract_addr = ? AND key = ?",
		contractAddr[:], key,
	)
	return err
}

func (s *Store) Range(contractAddr core.Address, start, end []byte, order storage.Order) (storage.StorageIterator, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := "ASC"
	if order == storage.Descending {
		dir = "DESC"
	}

	var rows *sql.Rows
	var err error

	switch {
	case start == nil && end == nil:
		rows, err = s.db.Query(
			fmt.Sprintf("SELECT key, value FROM contract_storage WHERE contract_addr = ? ORDER BY key %s", dir),
			contractAddr[:],
		)
	case end == nil:
		rows, err = s.db.Query(
			fmt.Sprintf("SELECT key, value FROM contract_storage WHERE contract_addr = ? AND key >= ? ORDER BY key %s", dir),
			contractAddr[:], start,
		)
	case start == nil:
		rows, err = s.db.Query(
			fmt.Sprintf("SELECT key, value FROM contract_storage WHERE contract_addr = ? AND key < ? ORDER BY key %s", dir),
			contractAddr[:], end,
		)
	default:
		rows, err = s.db.Query(
			fmt.Sprintf("SELECT key, value FROM contract_storage WHERE contract_addr = ? AND key >= ? AND key < ? ORDER BY key %s", dir),
			contractAddr[:], start, end,
		)
	}
	if err != nil {
		return nil, err
	}

	return loadIterator(rows)
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Savepoint creates a named SQLite savepoint for transactional rollback.
func (s *Store) Savepoint(name string) error {
	if !validSavepointName.MatchString(name) {
		return fmt.Errorf("invalid savepoint name: %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("SAVEPOINT " + name)
	return err
}

// RollbackTo rolls back to a previously created savepoint.
func (s *Store) RollbackTo(name string) error {
	if !validSavepointName.MatchString(name) {
		return fmt.Errorf("invalid savepoint name: %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("ROLLBACK TO " + name)
	return err
}

// ReleaseSavepoint releases a savepoint without rolling back.
func (s *Store) ReleaseSavepoint(name string) error {
	if !validSavepointName.MatchString(name) {
		return fmt.Errorf("invalid savepoint name: %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("RELEASE SAVEPOINT " + name)
	return err
}

// sliceIterator pre-loads all results into memory to avoid holding open DB cursors.
type sliceIterator struct {
	pairs []kvPair
	pos   int
}

type kvPair struct {
	key   []byte
	value []byte
}

func loadIterator(rows *sql.Rows) (*sliceIterator, error) {
	defer rows.Close()
	var pairs []kvPair
	for rows.Next() {
		var k, v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		pairs = append(pairs, kvPair{key: k, value: v})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &sliceIterator{pairs: pairs}, nil
}

func (it *sliceIterator) Valid() bool { return it.pos < len(it.pairs) }
func (it *sliceIterator) Next()      { it.pos++ }

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
