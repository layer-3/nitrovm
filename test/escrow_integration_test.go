package test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/runtime"
	"github.com/layer-3/nitrovm/storage/sqlite"
)

const (
	tokenWASMPath  = "../contracts/token/target/wasm32-unknown-unknown/release/token.wasm"
	escrowWASMPath = "../contracts/escrow/target/wasm32-unknown-unknown/release/escrow.wasm"
	testGasLimit   = uint64(500_000_000_000)
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

func loadWASM(t *testing.T, path string) []byte {
	t.Helper()
	code, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("WASM not found (%v); build contracts first", err)
	}
	return code
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustExec(t *testing.T, vm *runtime.NitroVM, contract, sender core.Address, msg []byte, funds []wasmvmtypes.Coin) {
	t.Helper()
	if _, _, err := vm.Execute(contract, sender, msg, funds, testGasLimit, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func storeCode(t *testing.T, vm *runtime.NitroVM, path string) []byte {
	t.Helper()
	code := loadWASM(t, path)
	codeID, _, err := vm.StoreCode(code, nil, nil)
	if err != nil {
		t.Fatalf("StoreCode(%s): %v", path, err)
	}
	return codeID
}

func instantiateToken(t *testing.T, vm *runtime.NitroVM, creator core.Address, balances map[string]string) ([]byte, core.Address) {
	t.Helper()
	initial := make([]map[string]any, 0, len(balances))
	for addr, amt := range balances {
		initial = append(initial, map[string]any{"address": addr, "amount": amt})
	}
	initMsg := mustJSON(t, map[string]any{
		"name": "Yellow Token", "symbol": "YLW", "decimals": 18,
		"initial_balances": initial,
	})
	codeID := storeCode(t, vm, tokenWASMPath)
	res, _, err := vm.Instantiate(codeID, creator, initMsg, "token", nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("Instantiate token: %v", err)
	}
	return codeID, res.ContractAddress
}

func instantiateEscrow(
	t *testing.T, vm *runtime.NitroVM, creator core.Address,
	tokenAddr core.Address, sellerAddr, arbiterAddr core.Address,
	amount string, expirySeconds *uint64, escrowCodeSeq *uint64,
) ([]byte, core.Address) {
	t.Helper()
	msg := map[string]any{
		"token_contract": tokenAddr.Hex(),
		"seller":         sellerAddr.Hex(),
		"arbiter":        arbiterAddr.Hex(),
		"amount":         amount,
	}
	if expirySeconds != nil {
		msg["expiry_seconds"] = *expirySeconds
	}
	if escrowCodeSeq != nil {
		msg["escrow_code_id"] = *escrowCodeSeq
	}
	codeID := storeCode(t, vm, escrowWASMPath)
	res, _, err := vm.Instantiate(codeID, creator, mustJSON(t, msg), "escrow", nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("Instantiate escrow: %v", err)
	}
	return codeID, res.ContractAddress
}

func queryTokenBalance(t *testing.T, vm *runtime.NitroVM, tokenAddr core.Address, who string) string {
	t.Helper()
	msg := mustJSON(t, map[string]any{"balance": map[string]any{"address": who}})
	result, _, err := vm.Query(tokenAddr, msg, testGasLimit)
	if err != nil {
		t.Fatalf("query token balance: %v", err)
	}
	var resp struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal balance: %v (raw: %s)", err, result)
	}
	return resp.Balance
}

func queryEscrowStatus(t *testing.T, vm *runtime.NitroVM, escrowAddr core.Address) string {
	t.Helper()
	msg := mustJSON(t, map[string]any{"status": map[string]any{}})
	result, _, err := vm.Query(escrowAddr, msg, testGasLimit)
	if err != nil {
		t.Fatalf("query escrow status: %v", err)
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	return resp.Status
}

func queryEscrowInfo(t *testing.T, vm *runtime.NitroVM, escrowAddr core.Address) map[string]any {
	t.Helper()
	msg := mustJSON(t, map[string]any{"escrow_info": map[string]any{}})
	result, _, err := vm.Query(escrowAddr, msg, testGasLimit)
	if err != nil {
		t.Fatalf("query escrow info: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal escrow info: %v", err)
	}
	return resp
}

func addr(b byte) core.Address {
	a, _ := core.HexToAddress("0x00000000000000000000000000000000000000" + string("0123456789abcdef"[b/16]) + string("0123456789abcdef"[b%16]))
	return a
}

func hasAttr(events []wasmvmtypes.Event, attrs []wasmvmtypes.EventAttribute, key, value string) bool {
	for _, a := range attrs {
		if a.Key == key && a.Value == value {
			return true
		}
	}
	for _, e := range events {
		for _, a := range e.Attributes {
			if a.Key == key && a.Value == value {
				return true
			}
		}
	}
	return false
}

func hasEventType(events []wasmvmtypes.Event, typ string) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// Standard addresses.
var (
	deployer = addr(0x01)
	buyer    = addr(0x02)
	seller   = addr(0x03)
	arbiter  = addr(0x04)
	outsider = addr(0x05)
)

// ---------------------------------------------------------------------------
// Tests: Cross-contract execute (WasmMsg::Execute)
// ---------------------------------------------------------------------------

// TestEscrow_DepositApproveRelease exercises the full happy-path:
// buyer deposits tokens, arbiter approves, escrow sends WasmMsg::Execute
// to the token contract to transfer tokens to the seller.
func TestEscrow_DepositApproveRelease(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "500", nil, nil)

	// Buyer transfers 500 tokens to escrow via token contract.
	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "500"},
	})
	if _, _, err := vm.Execute(tokenAddr, buyer, transferMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("buyer->escrow token transfer: %v", err)
	}

	// Buyer records deposit in escrow contract.
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "500"}})
	if _, _, err := vm.Execute(escrowAddr, buyer, depositMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// Arbiter approves -> escrow sends WasmMsg::Execute to token.transfer(seller, 500).
	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	res, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Verify cross-contract token transfer worked.
	if bal := queryTokenBalance(t, vm, tokenAddr, seller.Hex()); bal != "500" {
		t.Errorf("seller token balance = %s, want 500", bal)
	}
	if bal := queryTokenBalance(t, vm, tokenAddr, escrowAddr.Hex()); bal != "0" {
		t.Errorf("escrow token balance = %s, want 0", bal)
	}
	if bal := queryTokenBalance(t, vm, tokenAddr, buyer.Hex()); bal != "500" {
		t.Errorf("buyer token balance = %s, want 500", bal)
	}
	if st := queryEscrowStatus(t, vm, escrowAddr); st != "released" {
		t.Errorf("escrow status = %q, want released", st)
	}

	// Verify escrow's own attributes are present.
	if !hasAttr(res.Events, res.Attributes, "action", "approve") {
		t.Error("missing 'approve' action attribute")
	}
	// Sub-message response attributes are wrapped into a "wasm" event.
	if !hasAttr(res.Events, nil, "action", "transfer") {
		t.Error("missing 'transfer' action from cross-contract token call")
	}
}

// ---------------------------------------------------------------------------
// Tests: Cross-contract query (WasmQuery::Smart)
// ---------------------------------------------------------------------------

// TestEscrow_CrossContractQuery exercises WasmQuery::Smart from escrow to token.
func TestEscrow_CrossContractQuery(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "300", nil, nil)

	// Transfer tokens to escrow.
	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "300"},
	})
	if _, _, err := vm.Execute(tokenAddr, buyer, transferMsg, nil, testGasLimit, nil); err != nil {
		t.Fatal(err)
	}

	// CheckBalance calls WasmQuery::Smart on the token contract.
	checkMsg := mustJSON(t, map[string]any{"check_balance": map[string]any{}})
	res, _, err := vm.Execute(escrowAddr, buyer, checkMsg, nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("check_balance: %v", err)
	}

	if !hasAttr(res.Events, res.Attributes, "token_balance", "300") {
		t.Errorf("expected token_balance=300 attribute, got attrs=%v events=%v", res.Attributes, res.Events)
	}
}

// ---------------------------------------------------------------------------
// Tests: Sub-message instantiate (WasmMsg::Instantiate)
// ---------------------------------------------------------------------------

// TestEscrow_CloneViaInstantiate exercises WasmMsg::Instantiate sub-message dispatch.
func TestEscrow_CloneViaInstantiate(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})

	escrowCode := loadWASM(t, escrowWASMPath)
	escrowCodeID, _, err := vm.StoreCode(escrowCode, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seqID, ok := vm.GetCodeSeq(hex.EncodeToString(escrowCodeID))
	if !ok {
		t.Fatal("escrow code seq not found")
	}

	initMsg := mustJSON(t, map[string]any{
		"token_contract": tokenAddr.Hex(),
		"seller":         seller.Hex(),
		"arbiter":        arbiter.Hex(),
		"amount":         "100",
		"escrow_code_id": seqID,
	})
	parentRes, _, err := vm.Instantiate(escrowCodeID, deployer, initMsg, "escrow-parent", nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("Instantiate parent: %v", err)
	}

	// Clone via sub-message instantiate.
	cloneMsg := mustJSON(t, map[string]any{
		"clone_escrow": map[string]any{
			"seller":  outsider.Hex(),
			"arbiter": arbiter.Hex(),
			"amount":  "200",
		},
	})
	res, _, err := vm.Execute(parentRes.ContractAddress, deployer, cloneMsg, nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("clone_escrow: %v", err)
	}

	if !hasEventType(res.Events, "instantiate") {
		t.Error("missing 'instantiate' event from clone sub-message")
	}

	// Find cloned contract address.
	var clonedAddr string
	for _, e := range res.Events {
		if e.Type == "instantiate" {
			for _, a := range e.Attributes {
				if a.Key == "_contract_address" {
					clonedAddr = a.Value
				}
			}
		}
	}
	if clonedAddr == "" {
		t.Fatal("could not find cloned contract address in events")
	}

	// Query cloned escrow.
	clonedAddrParsed, _ := core.HexToAddress(clonedAddr)
	info := queryEscrowInfo(t, vm, clonedAddrParsed)
	if info["seller"] != outsider.Hex() {
		t.Errorf("cloned seller = %v, want %s", info["seller"], outsider.Hex())
	}
	if info["amount"] != "200" {
		t.Errorf("cloned amount = %v, want 200", info["amount"])
	}
	if info["status"] != "open" {
		t.Errorf("cloned status = %v, want open", info["status"])
	}
}

// ---------------------------------------------------------------------------
// Tests: Multi-hop (clone child, then child -> token)
// ---------------------------------------------------------------------------

// TestEscrow_MultiHop_CloneThenDeposit exercises dispatch depth > 0.
// Parent clones a child escrow, then the child performs a cross-contract
// execute to the token contract on approve.
func TestEscrow_MultiHop_CloneThenDeposit(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "5000",
	})

	escrowCode := loadWASM(t, escrowWASMPath)
	escrowCodeID, _, err := vm.StoreCode(escrowCode, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seqID, _ := vm.GetCodeSeq(hex.EncodeToString(escrowCodeID))

	initMsg := mustJSON(t, map[string]any{
		"token_contract": tokenAddr.Hex(),
		"seller":         seller.Hex(),
		"arbiter":        arbiter.Hex(),
		"amount":         "1000",
		"escrow_code_id": seqID,
	})
	parentRes, _, _ := vm.Instantiate(escrowCodeID, deployer, initMsg, "parent", nil, testGasLimit, nil)

	// Clone child.
	cloneMsg := mustJSON(t, map[string]any{
		"clone_escrow": map[string]any{
			"seller":  outsider.Hex(),
			"arbiter": arbiter.Hex(),
			"amount":  "300",
		},
	})
	cloneRes, _, err := vm.Execute(parentRes.ContractAddress, deployer, cloneMsg, nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	var childAddr core.Address
	for _, e := range cloneRes.Events {
		if e.Type == "instantiate" {
			for _, a := range e.Attributes {
				if a.Key == "_contract_address" {
					childAddr, _ = core.HexToAddress(a.Value)
				}
			}
		}
	}
	if childAddr == (core.Address{}) {
		t.Fatal("child address not found")
	}

	// Fund child and deposit.
	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": childAddr.Hex(), "amount": "300"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "300"}})
	mustExec(t, vm, childAddr, buyer, depositMsg, nil)

	// Approve child -> cross-contract execute to token.transfer(outsider, 300).
	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	if _, _, err := vm.Execute(childAddr, arbiter, approveMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("approve child: %v", err)
	}

	if bal := queryTokenBalance(t, vm, tokenAddr, outsider.Hex()); bal != "300" {
		t.Errorf("outsider balance = %s, want 300", bal)
	}
	if bal := queryTokenBalance(t, vm, tokenAddr, buyer.Hex()); bal != "4700" {
		t.Errorf("buyer balance = %s, want 4700", bal)
	}
}

// ---------------------------------------------------------------------------
// Tests: Cross-contract error propagation
// ---------------------------------------------------------------------------

// TestEscrow_CrossContractErrorPropagation verifies that token contract errors
// during cross-contract execute propagate back to the caller.
func TestEscrow_CrossContractErrorPropagation(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "500", nil, nil)

	// Record deposit WITHOUT actually transferring tokens to escrow.
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "500"}})
	if _, _, err := vm.Execute(escrowAddr, buyer, depositMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// Approve triggers token.transfer(seller, 500) but escrow has 0 token balance.
	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	_, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected error from cross-contract insufficient funds")
	}
	if !strings.Contains(err.Error(), "insufficient funds") {
		t.Errorf("error = %v, want 'insufficient funds'", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Authorization edge cases
// ---------------------------------------------------------------------------

func TestEscrow_DoubleApprove(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	if _, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("first approve: %v", err)
	}

	_, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected error on double approve")
	}
	if !strings.Contains(err.Error(), "already finalized") {
		t.Errorf("error = %v, want 'already finalized'", err)
	}
}

func TestEscrow_UnauthorizedApprove(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	_, _, err := vm.Execute(escrowAddr, outsider, approveMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if !strings.Contains(err.Error(), "unauthorized") && !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error = %v, want 'unauthorized'", err)
	}
}

func TestEscrow_BuyerCanApprove(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	if _, _, err := vm.Execute(escrowAddr, buyer, approveMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("buyer approve: %v", err)
	}

	if bal := queryTokenBalance(t, vm, tokenAddr, seller.Hex()); bal != "100" {
		t.Errorf("seller balance = %s, want 100", bal)
	}
}

func TestEscrow_ApproveInsufficientDeposit(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "500", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "200"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "200"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	_, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected not funded error")
	}
	if !strings.Contains(err.Error(), "not funded") {
		t.Errorf("error = %v, want 'not funded'", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Refund scenarios
// ---------------------------------------------------------------------------

func TestEscrow_Refund_ByArbiter(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "400", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "400"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "400"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	refundMsg := mustJSON(t, map[string]any{"refund": map[string]any{}})
	if _, _, err := vm.Execute(escrowAddr, arbiter, refundMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("refund: %v", err)
	}

	if bal := queryTokenBalance(t, vm, tokenAddr, buyer.Hex()); bal != "1000" {
		t.Errorf("buyer balance after refund = %s, want 1000", bal)
	}
	if st := queryEscrowStatus(t, vm, escrowAddr); st != "refunded" {
		t.Errorf("status = %q, want refunded", st)
	}
}

func TestEscrow_RefundWithoutDeposit(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	refundMsg := mustJSON(t, map[string]any{"refund": map[string]any{}})
	if _, _, err := vm.Execute(escrowAddr, arbiter, refundMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("refund without deposit: %v", err)
	}

	if st := queryEscrowStatus(t, vm, escrowAddr); st != "refunded" {
		t.Errorf("status = %q, want refunded", st)
	}
}

// ---------------------------------------------------------------------------
// Tests: Time-based expiry
// ---------------------------------------------------------------------------

func TestEscrow_RefundAfterExpiry(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	expiry := uint64(10)
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "300", &expiry, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "300"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "300"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	// Outsider refund before expiry should fail.
	refundMsg := mustJSON(t, map[string]any{"refund": map[string]any{}})
	_, _, err := vm.Execute(escrowAddr, outsider, refundMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected error before expiry")
	}

	// Advance block time past expiry.
	vm.SetBlockInfo(100, uint64(time.Now().UnixNano())+11_000_000_000)

	// Now outsider can refund.
	if _, _, err := vm.Execute(escrowAddr, outsider, refundMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("refund after expiry: %v", err)
	}

	if bal := queryTokenBalance(t, vm, tokenAddr, buyer.Hex()); bal != "1000" {
		t.Errorf("buyer balance = %s, want 1000", bal)
	}
	if st := queryEscrowStatus(t, vm, escrowAddr); st != "refunded" {
		t.Errorf("status = %q, want refunded", st)
	}
}

func TestEscrow_RefundBeforeExpiry_ArbiterAllowed(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	expiry := uint64(3600)
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "200", &expiry, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "200"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "200"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	refundMsg := mustJSON(t, map[string]any{"refund": map[string]any{}})
	if _, _, err := vm.Execute(escrowAddr, arbiter, refundMsg, nil, testGasLimit, nil); err != nil {
		t.Fatalf("arbiter refund before expiry: %v", err)
	}

	if bal := queryTokenBalance(t, vm, tokenAddr, buyer.Hex()); bal != "1000" {
		t.Errorf("buyer balance = %s, want 1000", bal)
	}
}

// ---------------------------------------------------------------------------
// Tests: Deposit edge cases
// ---------------------------------------------------------------------------

func TestEscrow_DepositAfterFinalized(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)
	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	mustExec(t, vm, escrowAddr, arbiter, approveMsg, nil)

	_, _, err := vm.Execute(escrowAddr, buyer, depositMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected error depositing after finalization")
	}
	if !strings.Contains(err.Error(), "already finalized") {
		t.Errorf("error = %v, want 'already finalized'", err)
	}
}

func TestEscrow_DepositExceedsRequired(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "200"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)

	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "200"}})
	_, _, err := vm.Execute(escrowAddr, buyer, depositMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected error for over-deposit")
	}
	if !strings.Contains(err.Error(), "exceeds") && !strings.Contains(err.Error(), "Deposit exceeds") {
		t.Errorf("error = %v, want deposit exceeds message", err)
	}
}

func TestEscrow_ZeroDeposit(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "0"}})
	_, _, err := vm.Execute(escrowAddr, buyer, depositMsg, nil, testGasLimit, nil)
	if err == nil {
		t.Fatal("expected error for zero deposit")
	}
	if !strings.Contains(err.Error(), "zero amount") {
		t.Errorf("error = %v, want 'zero amount'", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Event aggregation across dispatch depth
// ---------------------------------------------------------------------------

func TestEscrow_EventAggregation(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	res, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Escrow attributes contain "action"="approve".
	if !hasAttr(nil, res.Attributes, "action", "approve") {
		t.Error("missing escrow 'approve' in top-level attributes")
	}
	// Token sub-message attributes are now wrapped into a "wasm" event.
	if !hasAttr(res.Events, nil, "action", "transfer") {
		t.Error("missing token 'transfer' in sub-message wasm event")
	}
}

// ---------------------------------------------------------------------------
// Tests: Gas across cross-contract calls
// ---------------------------------------------------------------------------

func TestEscrow_GasAcrossCrossContract(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)

	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrowAddr, buyer, depositMsg, nil)

	approveMsg := mustJSON(t, map[string]any{"approve": map[string]any{}})
	res, _, err := vm.Execute(escrowAddr, arbiter, approveMsg, nil, testGasLimit, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if res.GasUsed == 0 {
		t.Error("expected GasUsed > 0 for cross-contract approve")
	}

	// With gas limit 1, it should fail.
	_, escrowAddr2 := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "50", nil, nil)
	transferMsg2 := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrowAddr2.Hex(), "amount": "50"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg2, nil)
	depositMsg2 := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "50"}})
	mustExec(t, vm, escrowAddr2, buyer, depositMsg2, nil)

	_, _, err = vm.Execute(escrowAddr2, arbiter, approveMsg, nil, 1, nil)
	if err == nil {
		t.Error("expected out of gas error with gas limit 1")
	}
}

// ---------------------------------------------------------------------------
// Tests: Storage isolation
// ---------------------------------------------------------------------------

func TestEscrow_StorageIsolation(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "2000",
	})

	_, escrow1 := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "100", nil, nil)
	_, escrow2 := instantiateEscrow(t, vm, deployer, tokenAddr, outsider, arbiter, "200", nil, nil)

	// Deposit into escrow1 only.
	transferMsg := mustJSON(t, map[string]any{
		"transfer": map[string]any{"recipient": escrow1.Hex(), "amount": "100"},
	})
	mustExec(t, vm, tokenAddr, buyer, transferMsg, nil)
	depositMsg := mustJSON(t, map[string]any{"deposit": map[string]any{"amount": "100"}})
	mustExec(t, vm, escrow1, buyer, depositMsg, nil)

	info1 := queryEscrowInfo(t, vm, escrow1)
	info2 := queryEscrowInfo(t, vm, escrow2)

	if info1["deposited"] != "100" {
		t.Errorf("escrow1 deposited = %v, want 100", info1["deposited"])
	}
	if info2["deposited"] != "0" {
		t.Errorf("escrow2 deposited = %v, want 0", info2["deposited"])
	}
	if info1["seller"] != seller.Hex() {
		t.Errorf("escrow1 seller = %v, want %s", info1["seller"], seller.Hex())
	}
	if info2["seller"] != outsider.Hex() {
		t.Errorf("escrow2 seller = %v, want %s", info2["seller"], outsider.Hex())
	}
}

// ---------------------------------------------------------------------------
// Tests: Query endpoints
// ---------------------------------------------------------------------------

func TestEscrow_QueryEscrowInfo(t *testing.T) {
	vm := testVM(t)

	_, tokenAddr := instantiateToken(t, vm, deployer, map[string]string{
		buyer.Hex(): "1000",
	})
	expiry := uint64(60)
	_, escrowAddr := instantiateEscrow(t, vm, deployer, tokenAddr, seller, arbiter, "500", &expiry, nil)

	info := queryEscrowInfo(t, vm, escrowAddr)
	if info["token_contract"] != tokenAddr.Hex() {
		t.Errorf("token_contract = %v, want %s", info["token_contract"], tokenAddr.Hex())
	}
	if info["seller"] != seller.Hex() {
		t.Errorf("seller = %v, want %s", info["seller"], seller.Hex())
	}
	if info["arbiter"] != arbiter.Hex() {
		t.Errorf("arbiter = %v, want %s", info["arbiter"], arbiter.Hex())
	}
	if info["amount"] != "500" {
		t.Errorf("amount = %v, want 500", info["amount"])
	}
	if info["status"] != "open" {
		t.Errorf("status = %v, want open", info["status"])
	}
	if info["buyer"] != nil {
		t.Errorf("buyer should be nil before deposit, got %v", info["buyer"])
	}
}
