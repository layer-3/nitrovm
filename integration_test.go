package nitrovm_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layer-3/nitrovm"
	"github.com/layer-3/nitrovm/storage/sqlite"
)

const tokenWASMPath = "contracts/token/target/wasm32-unknown-unknown/release/token.wasm"

// testingGasLimit matches wasmvm's own test gas limit.
const testingGasLimit = uint64(500_000_000_000)

// testVM creates a NitroVM backed by a temp-dir SQLite store.
func testVM(t *testing.T) *nitrovm.NitroVM {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.New(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := nitrovm.DefaultConfig()
	cfg.DataDir = dir
	vm, err := nitrovm.New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vm.Close() })
	return vm
}

// loadTokenWASM reads the compiled token contract WASM.
func loadTokenWASM(t *testing.T) []byte {
	t.Helper()
	code, err := os.ReadFile(tokenWASMPath)
	if err != nil {
		t.Skipf("token WASM not found (%v); build with: cd contracts/token && cargo build --release --target wasm32-unknown-unknown", err)
	}
	return code
}

// instantiateToken stores code and creates a token instance.
// Returns (codeID, contractAddr).
func instantiateToken(t *testing.T, vm *nitrovm.NitroVM, creator nitrovm.Address, initMsg []byte) ([]byte, nitrovm.Address) {
	t.Helper()
	code := loadTokenWASM(t)
	codeID, err := vm.StoreCode(code)
	if err != nil {
		t.Fatalf("StoreCode: %v", err)
	}
	addr, _, err := vm.Instantiate(codeID, creator, initMsg, "token", nil, testingGasLimit)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return codeID, addr
}

// queryBalance returns the token balance for addr on the given contract.
func queryBalance(t *testing.T, vm *nitrovm.NitroVM, contract nitrovm.Address, addr string) string {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"balance": map[string]any{"address": addr}})
	result, _, err := vm.Query(contract, msg, testingGasLimit)
	if err != nil {
		t.Fatalf("query balance: %v", err)
	}
	var resp struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal balance: %v (raw: %s)", err, result)
	}
	return resp.Balance
}

// --- Tests ---

func TestTokenInstantiateAndQueryBalance(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000000"},
			{"address": bob.Hex(), "amount": "500000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	if bal := queryBalance(t, vm, contract, alice.Hex()); bal != "1000000" {
		t.Errorf("alice balance = %s, want 1000000", bal)
	}
	if bal := queryBalance(t, vm, contract, bob.Hex()); bal != "500000" {
		t.Errorf("bob balance = %s, want 500000", bal)
	}
}

func TestTokenQueryTokenInfo(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	msg, _ := json.Marshal(map[string]any{"token_info": map[string]any{}})
	result, _, err := vm.Query(contract, msg, testingGasLimit)
	if err != nil {
		t.Fatalf("query token_info: %v", err)
	}

	var info struct {
		Name        string `json:"name"`
		Symbol      string `json:"symbol"`
		Decimals    int    `json:"decimals"`
		TotalSupply string `json:"total_supply"`
	}
	json.Unmarshal(result, &info)

	if info.Name != "Yellow Token" {
		t.Errorf("name = %q", info.Name)
	}
	if info.Symbol != "YLW" {
		t.Errorf("symbol = %q", info.Symbol)
	}
	if info.Decimals != 18 {
		t.Errorf("decimals = %d", info.Decimals)
	}
	if info.TotalSupply != "1000000" {
		t.Errorf("total_supply = %s", info.TotalSupply)
	}
}

func TestTokenTransfer(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000000"},
			{"address": bob.Hex(), "amount": "500000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	// Alice transfers 250000 to Bob
	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{
			"recipient": bob.Hex(),
			"amount":    "250000",
		},
	})
	_, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if bal := queryBalance(t, vm, contract, alice.Hex()); bal != "750000" {
		t.Errorf("alice post-transfer = %s, want 750000", bal)
	}
	if bal := queryBalance(t, vm, contract, bob.Hex()); bal != "750000" {
		t.Errorf("bob post-transfer = %s, want 750000", bal)
	}
}

func TestTokenTransferInsufficientFunds(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "100"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{
			"recipient": bob.Hex(),
			"amount":    "200",
		},
	})
	_, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit)
	if err == nil {
		t.Fatal("expected error for insufficient funds")
	}
	if !strings.Contains(err.Error(), "insufficient funds") {
		t.Errorf("error = %v, want 'insufficient funds'", err)
	}

	// Balance unchanged
	if bal := queryBalance(t, vm, contract, alice.Hex()); bal != "100" {
		t.Errorf("alice balance should be unchanged, got %s", bal)
	}
}

func TestTokenTransferZeroAmount(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "100"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{
			"recipient": bob.Hex(),
			"amount":    "0",
		},
	})
	_, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit)
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
	if !strings.Contains(err.Error(), "zero amount") {
		t.Errorf("error = %v, want 'zero amount'", err)
	}
}

func TestTokenQueryUnknownAddress(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")
	unknown, _ := nitrovm.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "100"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	if bal := queryBalance(t, vm, contract, unknown.Hex()); bal != "0" {
		t.Errorf("unknown address balance = %s, want 0", bal)
	}
}

func TestTokenMultipleTransfers(t *testing.T) {
	vm := testVM(t)
	alice, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000002")
	charlie, _ := nitrovm.HexToAddress("0x0000000000000000000000000000000000000003")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	// Alice -> Bob 300
	msg1, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "300"},
	})
	if _, _, err := vm.Execute(contract, alice, msg1, nil, testingGasLimit); err != nil {
		t.Fatal(err)
	}

	// Alice -> Charlie 200
	msg2, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": charlie.Hex(), "amount": "200"},
	})
	if _, _, err := vm.Execute(contract, alice, msg2, nil, testingGasLimit); err != nil {
		t.Fatal(err)
	}

	// Bob -> Charlie 100
	msg3, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": charlie.Hex(), "amount": "100"},
	})
	if _, _, err := vm.Execute(contract, bob, msg3, nil, testingGasLimit); err != nil {
		t.Fatal(err)
	}

	// Final: Alice=500, Bob=200, Charlie=300
	if bal := queryBalance(t, vm, contract, alice.Hex()); bal != "500" {
		t.Errorf("alice = %s, want 500", bal)
	}
	if bal := queryBalance(t, vm, contract, bob.Hex()); bal != "200" {
		t.Errorf("bob = %s, want 200", bal)
	}
	if bal := queryBalance(t, vm, contract, charlie.Hex()); bal != "300" {
		t.Errorf("charlie = %s, want 300", bal)
	}
}
