package node

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/crypto"
	"github.com/layer-3/nitrovm/storage/memory"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Mock VM
// ---------------------------------------------------------------------------

// mockVM implements nitrovm.VM with function-field stubs.
// Nil stubs fall through to simple default behavior.
type mockVM struct {
	storeCodeFn        func([]byte, *core.Address, *uint64) ([]byte, uint64, error)
	instantiateFn      func([]byte, core.Address, []byte, string, []wasmvmtypes.Coin, uint64, *uint64) (*core.InstantiateResult, uint64, error)
	executeFn          func(core.Address, core.Address, []byte, []wasmvmtypes.Coin, uint64, *uint64) (*core.ExecuteResult, uint64, error)
	queryFn            func(core.Address, []byte, uint64) ([]byte, uint64, error)
	getBalanceFn       func(core.Address) core.Amount
	setBalanceFn       func(core.Address, core.Amount)
	deductGasFeeFn     func(core.Address, uint64, uint64) error
	getContractInfoFn  func(core.Address) *core.ContractInfo
	registerContractFn func(core.Address, core.Address, []byte, string)
	setInstanceCountFn func(uint64)
	setCodeSeqFn       func(uint64)
	setBlockInfoFn     func(uint64, uint64)
	setNonceFn         func(core.Address, uint64)

	// Simple state for defaults.
	balances     map[core.Address]core.Amount
	nonces       map[core.Address]uint64
	codes        []string
	contracts    []core.ContractInfo
	contractInfo map[core.Address]*core.ContractInfo
	chainID      string
	opSeq        uint64

	// Dirty-address tracking.
	touched map[core.Address]struct{}

	// Counters for assertions.
	snapshotCount int
	restoreCount  int
	tickOpCount   int
}

func (m *mockVM) StoreCode(code []byte, sender *core.Address, nonce *uint64) ([]byte, uint64, error) {
	if m.storeCodeFn != nil {
		return m.storeCodeFn(code, sender, nonce)
	}
	return []byte{0xab, 0xcd}, 42000, nil
}

func (m *mockVM) Instantiate(codeID []byte, sender core.Address, msg []byte, label string, funds []wasmvmtypes.Coin, gasLimit uint64, nonce *uint64) (*core.InstantiateResult, uint64, error) {
	if m.instantiateFn != nil {
		return m.instantiateFn(codeID, sender, msg, label, funds, gasLimit, nonce)
	}
	addr, _ := core.HexToAddress("0x00000000000000000000000000000000deadbeef")
	return &core.InstantiateResult{ContractAddress: addr, GasUsed: 5000}, 5000, nil
}

func (m *mockVM) Execute(contract, sender core.Address, msg []byte, funds []wasmvmtypes.Coin, gasLimit uint64, nonce *uint64) (*core.ExecuteResult, uint64, error) {
	if m.executeFn != nil {
		return m.executeFn(contract, sender, msg, funds, gasLimit, nonce)
	}
	return &core.ExecuteResult{GasUsed: 1000}, 1000, nil
}

func (m *mockVM) Query(contract core.Address, msg []byte, gasLimit uint64) ([]byte, uint64, error) {
	if m.queryFn != nil {
		return m.queryFn(contract, msg, gasLimit)
	}
	return []byte(`{}`), 100, nil
}

func (m *mockVM) GetBalance(addr core.Address) core.Amount {
	if m.getBalanceFn != nil {
		return m.getBalanceFn(addr)
	}
	if m.balances != nil {
		if b, ok := m.balances[addr]; ok {
			return b
		}
	}
	return core.Amount{}
}

func (m *mockVM) markTouched(addr core.Address) {
	if m.touched == nil {
		m.touched = make(map[core.Address]struct{})
	}
	m.touched[addr] = struct{}{}
}

func (m *mockVM) TouchedAddresses() []core.Address {
	addrs := make([]core.Address, 0, len(m.touched))
	for addr := range m.touched {
		addrs = append(addrs, addr)
	}
	m.touched = nil
	return addrs
}

func (m *mockVM) SetBalance(addr core.Address, balance core.Amount) {
	if m.setBalanceFn != nil {
		m.setBalanceFn(addr, balance)
		return
	}
	if m.balances == nil {
		m.balances = make(map[core.Address]core.Amount)
	}
	m.balances[addr] = balance
	m.markTouched(addr)
}

func (m *mockVM) GetNonce(addr core.Address) uint64 {
	if m.nonces != nil {
		if n, ok := m.nonces[addr]; ok {
			return n
		}
	}
	return 0
}

func (m *mockVM) SetNonce(addr core.Address, nonce uint64) {
	if m.setNonceFn != nil {
		m.setNonceFn(addr, nonce)
		return
	}
	if m.nonces == nil {
		m.nonces = make(map[core.Address]uint64)
	}
	m.nonces[addr] = nonce
}

func (m *mockVM) SetBlockInfo(height, timeNanos uint64) {
	if m.setBlockInfoFn != nil {
		m.setBlockInfoFn(height, timeNanos)
		return
	}
	m.opSeq = height
}

func (m *mockVM) TickOp() { m.tickOpCount++ }

func (m *mockVM) GetOpSeq() uint64 { return m.opSeq }

func (m *mockVM) RegisterContract(addr, creator core.Address, checksum []byte, label string) {
	if m.registerContractFn != nil {
		m.registerContractFn(addr, creator, checksum, label)
	}
}

func (m *mockVM) SetInstanceCount(count uint64) {
	if m.setInstanceCountFn != nil {
		m.setInstanceCountFn(count)
	}
}

func (m *mockVM) GetInstanceCount() uint64 { return 0 }

func (m *mockVM) ListCodes() []string {
	if m.codes != nil {
		return m.codes
	}
	return []string{}
}

func (m *mockVM) ListContracts() []core.ContractInfo {
	if m.contracts != nil {
		return m.contracts
	}
	return []core.ContractInfo{}
}

func (m *mockVM) GetContractInfo(addr core.Address) *core.ContractInfo {
	if m.getContractInfoFn != nil {
		return m.getContractInfoFn(addr)
	}
	if m.contractInfo != nil {
		return m.contractInfo[addr]
	}
	return nil
}

func (m *mockVM) GetCodeSeq(hexCodeID string) (uint64, bool) { return 1, true }

func (m *mockVM) SetCodeSeq(seq uint64) {
	if m.setCodeSeqFn != nil {
		m.setCodeSeqFn(seq)
	}
}

func (m *mockVM) DeductGasFee(sender core.Address, gasUsed, gasPrice uint64) error {
	if m.deductGasFeeFn != nil {
		return m.deductGasFeeFn(sender, gasUsed, gasPrice)
	}
	return nil
}

func (m *mockVM) ChainID() string {
	if m.chainID != "" {
		return m.chainID
	}
	return "nitro-test"
}

func (m *mockVM) Snapshot() any { m.snapshotCount++; return m.snapshotCount }
func (m *mockVM) Restore(any)   { m.restoreCount++ }
func (m *mockVM) Close()        {}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestServer creates a Server with in-memory SQLite and memory storage.
// testDB opens a file-backed SQLite in a temp dir with WAL mode.
// WAL allows concurrent reads while a transaction holds a write lock,
// matching production behavior.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateMetaTables(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T, vm *mockVM) *Server {
	t.Helper()
	db := testDB(t)
	store := memory.New()
	return New(Config{Network: Devnet}, db, store, vm)
}

// newTestServerWithCfg allows custom server config.
func newTestServerWithCfg(t *testing.T, vm *mockVM, cfg Config) *Server {
	t.Helper()
	db := testDB(t)
	store := memory.New()
	return New(cfg, db, store, vm)
}

// testKey returns the Hardhat account 0 private key.
func testKey(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	b, _ := hex.DecodeString("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	return secp256k1.PrivKeyFromBytes(b)
}

// signAndPost signs a transaction and POSTs {"tx":"<hex>"} to the given path.
func signAndPost(t *testing.T, handler http.Handler, path string, tx *crypto.Transaction, key *secp256k1.PrivateKey) *httptest.ResponseRecorder {
	t.Helper()
	stx, err := crypto.SignTx(tx, key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	encoded, err := crypto.EncodeSignedTx(stx)
	if err != nil {
		t.Fatalf("EncodeSignedTx: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"tx": hex.EncodeToString(encoded)})
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// parseJSON decodes the response body into a map.
func parseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("parse response JSON: %v (body: %s)", err, w.Body.String())
	}
	return m
}
