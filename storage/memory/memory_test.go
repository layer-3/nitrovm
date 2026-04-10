package memory

import (
	"testing"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage"
)

func TestStoreCRUD(t *testing.T) {
	store := New()
	defer store.Close()

	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	key := []byte("hello")
	val := []byte("world")

	// Get non-existent key.
	got, err := store.Get(addr, key)
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing key, got %q", got)
	}

	// Set + Get.
	if err := store.Set(addr, key, val); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _ = store.Get(addr, key)
	if string(got) != "world" {
		t.Fatalf("Get = %q, want %q", got, "world")
	}

	// Overwrite.
	store.Set(addr, key, []byte("updated"))
	got, _ = store.Get(addr, key)
	if string(got) != "updated" {
		t.Fatalf("overwrite = %q, want updated", got)
	}

	// Delete.
	store.Delete(addr, key)
	got, _ = store.Get(addr, key)
	if got != nil {
		t.Fatalf("expected nil after delete, got %q", got)
	}
}

func TestStoreIsolation(t *testing.T) {
	store := New()
	defer store.Close()

	addr1, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	addr2, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")
	key := []byte("key")

	store.Set(addr1, key, []byte("val1"))
	store.Set(addr2, key, []byte("val2"))

	got1, _ := store.Get(addr1, key)
	got2, _ := store.Get(addr2, key)

	if string(got1) != "val1" {
		t.Errorf("addr1 = %q, want val1", got1)
	}
	if string(got2) != "val2" {
		t.Errorf("addr2 = %q, want val2", got2)
	}
}

func TestStoreRange(t *testing.T) {
	store := New()
	defer store.Close()

	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	store.Set(addr, []byte("a"), []byte("1"))
	store.Set(addr, []byte("b"), []byte("2"))
	store.Set(addr, []byte("c"), []byte("3"))

	// Full ascending.
	iter, _ := store.Range(addr, nil, nil, storage.Ascending)
	var keys []string
	for iter.Valid() {
		keys = append(keys, string(iter.Key()))
		iter.Next()
	}
	iter.Close()
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Errorf("ascending = %v, want [a b c]", keys)
	}

	// Full descending.
	iter, _ = store.Range(addr, nil, nil, storage.Descending)
	keys = nil
	for iter.Valid() {
		keys = append(keys, string(iter.Key()))
		iter.Next()
	}
	iter.Close()
	if len(keys) != 3 || keys[0] != "c" || keys[1] != "b" || keys[2] != "a" {
		t.Errorf("descending = %v, want [c b a]", keys)
	}

	// Bounded [b, c).
	iter, _ = store.Range(addr, []byte("b"), []byte("c"), storage.Ascending)
	keys = nil
	for iter.Valid() {
		keys = append(keys, string(iter.Key()))
		iter.Next()
	}
	iter.Close()
	if len(keys) != 1 || keys[0] != "b" {
		t.Errorf("bounded = %v, want [b]", keys)
	}
}

func TestSavepointRollback(t *testing.T) {
	store := New()
	defer store.Close()

	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	store.Set(addr, []byte("key"), []byte("original"))

	store.Savepoint("sp1")
	store.Set(addr, []byte("key"), []byte("modified"))
	store.Set(addr, []byte("new"), []byte("value"))

	// Verify modified state.
	got, _ := store.Get(addr, []byte("key"))
	if string(got) != "modified" {
		t.Fatalf("after modify = %q, want modified", got)
	}

	// Rollback.
	store.RollbackTo("sp1")
	got, _ = store.Get(addr, []byte("key"))
	if string(got) != "original" {
		t.Fatalf("after rollback = %q, want original", got)
	}
	got, _ = store.Get(addr, []byte("new"))
	if got != nil {
		t.Fatalf("after rollback new key should be nil, got %q", got)
	}
}
