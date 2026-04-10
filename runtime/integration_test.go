package runtime_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/crypto"
	"github.com/layer-3/nitrovm/runtime"
	"github.com/layer-3/nitrovm/storage/sqlite"
)

const tokenWASMPath = "../contracts/token/target/wasm32-unknown-unknown/release/token.wasm"

// testingGasLimit matches wasmvm's own test gas limit.
const testingGasLimit = uint64(500_000_000_000)

// testVM creates a NitroVM backed by a temp-dir SQLite store.
func testVM(t *testing.T) *runtime.NitroVM {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.New(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.DefaultConfig()
	cfg.DataDir = dir
	vm, err := runtime.New(cfg, store)
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
func instantiateToken(t *testing.T, vm *runtime.NitroVM, creator core.Address, initMsg []byte) ([]byte, core.Address) {
	t.Helper()
	code := loadTokenWASM(t)
	codeID, _, err := vm.StoreCode(code, nil, nil)
	if err != nil {
		t.Fatalf("StoreCode: %v", err)
	}
	res, _, err := vm.Instantiate(codeID, creator, initMsg, "token", nil, testingGasLimit, nil)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	return codeID, res.ContractAddress
}

// queryBalance returns the token balance for addr on the given contract.
func queryBalance(t *testing.T, vm *runtime.NitroVM, contract core.Address, addr string) string {
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
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

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
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")

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
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

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
	_, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit, nil)
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
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

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
	_, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit, nil)
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
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

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
	_, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit, nil)
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
	if !strings.Contains(err.Error(), "zero amount") {
		t.Errorf("error = %v, want 'zero amount'", err)
	}
}

func TestTokenQueryUnknownAddress(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	unknown, _ := core.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

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
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")
	charlie, _ := core.HexToAddress("0x0000000000000000000000000000000000000003")

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
	if _, _, err := vm.Execute(contract, alice, msg1, nil, testingGasLimit, nil); err != nil {
		t.Fatal(err)
	}

	// Alice -> Charlie 200
	msg2, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": charlie.Hex(), "amount": "200"},
	})
	if _, _, err := vm.Execute(contract, alice, msg2, nil, testingGasLimit, nil); err != nil {
		t.Fatal(err)
	}

	// Bob -> Charlie 100
	msg3, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": charlie.Hex(), "amount": "100"},
	})
	if _, _, err := vm.Execute(contract, bob, msg3, nil, testingGasLimit, nil); err != nil {
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

// --- Feature 1: Gas limit tests ---

func TestExecuteOutOfGas(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "100"},
	})
	// Gas limit of 1 should be far too low.
	_, _, err := vm.Execute(contract, alice, execMsg, nil, 1, nil)
	if err == nil {
		t.Fatal("expected out of gas error")
	}
}

func TestQueryWithGasLimit(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	msg, _ := json.Marshal(map[string]any{"balance": map[string]any{"address": alice.Hex()}})
	result, gasUsed, err := vm.Query(contract, msg, testingGasLimit)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if gasUsed == 0 {
		t.Error("expected gas_used > 0")
	}
	if len(result) == 0 {
		t.Error("expected non-empty result")
	}
}

// --- Feature 2: Funds tests ---

func TestInstantiateWithFunds(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")

	// Give Alice native YELLOW balance.
	vm.SetBalance(alice, core.NewAmount(10000))

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "100"},
		},
	})

	code := loadTokenWASM(t)
	codeID, _, err := vm.StoreCode(code, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	funds := []wasmvmtypes.Coin{{Denom: "YELLOW", Amount: "500"}}
	res, _, err := vm.Instantiate(codeID, alice, initMsg, "token", funds, testingGasLimit, nil)
	if err != nil {
		t.Fatalf("Instantiate with funds: %v", err)
	}

	// Alice should have 10000 - 500 = 9500.
	if bal := vm.GetBalance(alice); !bal.Equal(core.NewAmount(9500)) {
		t.Errorf("alice native balance = %s, want 9500", bal)
	}
	// Contract should have 500.
	if bal := vm.GetBalance(res.ContractAddress); !bal.Equal(core.NewAmount(500)) {
		t.Errorf("contract native balance = %s, want 500", bal)
	}
}

func TestExecuteWithFunds(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

	vm.SetBalance(alice, core.NewAmount(5000))

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "100"},
	})
	funds := []wasmvmtypes.Coin{{Denom: "YELLOW", Amount: "200"}}
	_, _, err := vm.Execute(contract, alice, execMsg, funds, testingGasLimit, nil)
	if err != nil {
		t.Fatalf("execute with funds: %v", err)
	}

	// Alice native: 5000 - 200 = 4800.
	if bal := vm.GetBalance(alice); !bal.Equal(core.NewAmount(4800)) {
		t.Errorf("alice native balance = %s, want 4800", bal)
	}
	// Contract native: 0 + 200 = 200.
	if bal := vm.GetBalance(contract); !bal.Equal(core.NewAmount(200)) {
		t.Errorf("contract native balance = %s, want 200", bal)
	}
}

func TestExecuteWithFundsInsufficientBalance(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

	vm.SetBalance(alice, core.NewAmount(10)) // only 10

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "100"},
	})
	funds := []wasmvmtypes.Coin{{Denom: "YELLOW", Amount: "100"}}
	_, _, err := vm.Execute(contract, alice, execMsg, funds, testingGasLimit, nil)
	if err == nil {
		t.Fatal("expected insufficient funds error")
	}
	if !errors.Is(err, core.ErrInsufficientFunds) {
		t.Errorf("error = %v, want ErrInsufficientFunds", err)
	}
}

// --- Feature 3: Events tests ---

func TestExecuteReturnsEvents(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "100"},
	})
	res, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Token contract returns attributes: action, from, to, amount.
	if len(res.Attributes) == 0 {
		t.Fatal("expected attributes from transfer")
	}

	attrMap := make(map[string]string)
	for _, a := range res.Attributes {
		attrMap[a.Key] = a.Value
	}

	if attrMap["action"] != "transfer" {
		t.Errorf("action = %q, want transfer", attrMap["action"])
	}
	if attrMap["from"] != alice.Hex() {
		t.Errorf("from = %q, want %s", attrMap["from"], alice.Hex())
	}
	if attrMap["to"] != bob.Hex() {
		t.Errorf("to = %q, want %s", attrMap["to"], bob.Hex())
	}
	if attrMap["amount"] != "100" {
		t.Errorf("amount = %q, want 100", attrMap["amount"])
	}
}

func TestInstantiateReturnsEvents(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})

	code := loadTokenWASM(t)
	codeID, _, err := vm.StoreCode(code, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	res, _, err := vm.Instantiate(codeID, alice, initMsg, "token", nil, testingGasLimit, nil)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	// Token contract returns attributes on instantiate including total_supply.
	if len(res.Attributes) == 0 {
		t.Fatal("expected attributes from instantiate")
	}

	attrMap := make(map[string]string)
	for _, a := range res.Attributes {
		attrMap[a.Key] = a.Value
	}
	if attrMap["total_supply"] != "1000" {
		t.Errorf("total_supply = %q, want 1000", attrMap["total_supply"])
	}
}

func TestExecuteResultGasUsed(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	_, contract := instantiateToken(t, vm, alice, initMsg)

	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "100"},
	})
	res, _, err := vm.Execute(contract, alice, execMsg, nil, testingGasLimit, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.GasUsed == 0 {
		t.Error("expected GasUsed > 0")
	}
}

// --- Feature 4: Cross-contract query test ---

func TestCrossContractQuery(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")

	// Deploy two separate token contracts.
	initMsg1, _ := json.Marshal(map[string]any{
		"name": "Token A", "symbol": "TKA", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "1000"},
		},
	})
	initMsg2, _ := json.Marshal(map[string]any{
		"name": "Token B", "symbol": "TKB", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": alice.Hex(), "amount": "2000"},
		},
	})
	_, contract1 := instantiateToken(t, vm, alice, initMsg1)
	_, contract2 := instantiateToken(t, vm, alice, initMsg2)

	// Query each independently — proves both contracts are functional and isolated.
	if bal := queryBalance(t, vm, contract1, alice.Hex()); bal != "1000" {
		t.Errorf("contract1 alice = %s, want 1000", bal)
	}
	if bal := queryBalance(t, vm, contract2, alice.Hex()); bal != "2000" {
		t.Errorf("contract2 alice = %s, want 2000", bal)
	}
}

// --- Transaction Signing Tests ---

func testKey(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	// Hardhat account 0.
	b, _ := hex.DecodeString("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	return secp256k1.PrivKeyFromBytes(b)
}

func TestSignedStoreInstantiateExecute(t *testing.T) {
	vm := testVM(t)
	key := testKey(t)
	sender := crypto.DeriveAddress(key)

	// Fund the sender for gas fees.
	vm.SetBalance(sender, core.NewAmount(1_000_000_000))

	// Signed store.
	code := loadTokenWASM(t)
	nonce := uint64(0)
	codeID, gasUsed, err := vm.StoreCode(code, &sender, &nonce)
	if err != nil {
		t.Fatalf("signed store: %v", err)
	}
	if gasUsed == 0 {
		t.Error("expected gas used > 0 for store")
	}
	// Nonce should have incremented.
	if got := vm.GetNonce(sender); got != 1 {
		t.Fatalf("nonce after store = %d, want 1", got)
	}

	// Signed instantiate.
	initMsg, _ := json.Marshal(map[string]any{
		"name": "Signed Token", "symbol": "SIG", "decimals": 18,
		"initial_balances": []map[string]any{
			{"address": sender.Hex(), "amount": "5000"},
		},
	})
	nonce = 1
	res, _, err := vm.Instantiate(codeID, sender, initMsg, "signed-token", nil, testingGasLimit, &nonce)
	if err != nil {
		t.Fatalf("signed instantiate: %v", err)
	}
	// Nonce should have incremented to 2.
	if got := vm.GetNonce(sender); got != 2 {
		t.Fatalf("nonce after instantiate = %d, want 2", got)
	}

	// Signed execute.
	bob, _ := core.HexToAddress("0x0000000000000000000000000000000000000099")
	execMsg, _ := json.Marshal(map[string]any{
		"transfer": map[string]any{"recipient": bob.Hex(), "amount": "200"},
	})
	nonce = 2
	_, _, err = vm.Execute(res.ContractAddress, sender, execMsg, nil, testingGasLimit, &nonce)
	if err != nil {
		t.Fatalf("signed execute: %v", err)
	}
	// Nonce should have incremented to 3.
	if got := vm.GetNonce(sender); got != 3 {
		t.Fatalf("nonce after execute = %d, want 3", got)
	}

	// Verify transfer happened.
	if bal := queryBalance(t, vm, res.ContractAddress, bob.Hex()); bal != "200" {
		t.Errorf("bob balance = %s, want 200", bal)
	}
}

func TestSignedNonceRejection(t *testing.T) {
	vm := testVM(t)
	key := testKey(t)
	sender := crypto.DeriveAddress(key)

	code := loadTokenWASM(t)
	// Store with nonce 0 succeeds.
	nonce := uint64(0)
	_, _, err := vm.StoreCode(code, &sender, &nonce)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// Try store with nonce 0 again (replay) — should fail.
	nonce = 0
	_, _, err = vm.StoreCode(code, &sender, &nonce)
	if !errors.Is(err, core.ErrInvalidNonce) {
		t.Fatalf("replay store: got %v, want ErrInvalidNonce", err)
	}
}

func TestDeductGasFee(t *testing.T) {
	vm := testVM(t)
	key := testKey(t)
	sender := crypto.DeriveAddress(key)

	vm.SetBalance(sender, core.NewAmount(1_000_000))

	// Deduct fee: 100 gas * 5 gas_price = 500
	err := vm.DeductGasFee(sender, 100, 5)
	if err != nil {
		t.Fatalf("deduct gas fee: %v", err)
	}
	if bal := vm.GetBalance(sender); !bal.Equal(core.NewAmount(999_500)) {
		t.Errorf("balance after fee = %s, want 999500", bal)
	}

	// Zero gas price should be a no-op.
	err = vm.DeductGasFee(sender, 1000, 0)
	if err != nil {
		t.Fatalf("zero gas price: %v", err)
	}
	if bal := vm.GetBalance(sender); !bal.Equal(core.NewAmount(999_500)) {
		t.Errorf("balance after zero-price fee = %s, want 999500", bal)
	}
}

func TestDeductGasFeeInsufficientFunds(t *testing.T) {
	vm := testVM(t)
	key := testKey(t)
	sender := crypto.DeriveAddress(key)

	vm.SetBalance(sender, core.NewAmount(100))

	// Deduct fee: 1000 gas * 1 gas_price = 1000 > 100 balance
	err := vm.DeductGasFee(sender, 1000, 1)
	if !errors.Is(err, core.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
	// Balance unchanged.
	if bal := vm.GetBalance(sender); !bal.Equal(core.NewAmount(100)) {
		t.Errorf("balance should be unchanged, got %s", bal)
	}
}

func TestSignAndRecoverRoundtrip(t *testing.T) {
	key := testKey(t)
	sender := crypto.DeriveAddress(key)

	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    42,
		GasLimit: 1_000_000,
		GasPrice: 1,
		Type:     crypto.TxExecute,
		Contract: core.Address{0xaa, 0xbb},
		Msg:      []byte(`{"test":true}`),
	}

	stx, err := crypto.SignTx(tx, key)
	if err != nil {
		t.Fatal(err)
	}

	// Encode -> decode -> recover.
	encoded, err := crypto.EncodeSignedTx(stx)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := crypto.DecodeSignedTx(encoded)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := crypto.RecoverSender(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != sender {
		t.Fatalf("recovered %s, want %s", recovered.Hex(), sender.Hex())
	}
}

func TestInstantiateIncrementsNonce(t *testing.T) {
	vm := testVM(t)
	alice, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")

	if got := vm.GetNonce(alice); got != 0 {
		t.Fatalf("initial nonce = %d, want 0", got)
	}

	code := loadTokenWASM(t)
	codeID, _, err := vm.StoreCode(code, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	initMsg, _ := json.Marshal(map[string]any{
		"name": "Test", "symbol": "T", "decimals": 18,
		"initial_balances": []map[string]any{},
	})
	nonce := uint64(0)
	_, _, err = vm.Instantiate(codeID, alice, initMsg, "test", nil, testingGasLimit, &nonce)
	if err != nil {
		t.Fatal(err)
	}

	// Instantiate with explicit nonce should increment nonce.
	if got := vm.GetNonce(alice); got != 1 {
		t.Fatalf("nonce after instantiate = %d, want 1", got)
	}
}
