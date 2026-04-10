package node

import (
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/crypto"
)

// =========================================================================
// Group A: Read-only endpoints
// =========================================================================

func TestBalance(t *testing.T) {
	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	vm := &mockVM{balances: map[core.Address]core.Amount{addr: core.NewAmount(5000)}}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/balance/"+addr.Hex(), nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := parseJSON(t, w)
	// Amount serialises as a JSON string.
	if resp["balance"] != "5000" {
		t.Errorf("balance = %v, want 5000", resp["balance"])
	}
}

func TestBalance_BadAddress(t *testing.T) {
	srv := newTestServer(t, &mockVM{})
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/balance/not-hex", nil))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestStatus(t *testing.T) {
	vm := &mockVM{
		chainID:   "nitro-test",
		opSeq:     42,
		codes:     []string{"aabb"},
		contracts: []core.ContractInfo{{Address: "0x01"}},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := parseJSON(t, w)
	if resp["chain_id"] != "nitro-test" {
		t.Errorf("chain_id = %v", resp["chain_id"])
	}
	if resp["block_height"] != float64(42) {
		t.Errorf("block_height = %v", resp["block_height"])
	}
	if resp["code_count"] != float64(1) {
		t.Errorf("code_count = %v", resp["code_count"])
	}
	if resp["contract_count"] != float64(1) {
		t.Errorf("contract_count = %v", resp["contract_count"])
	}
}

func TestAccount_EOA(t *testing.T) {
	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	vm := &mockVM{
		balances: map[core.Address]core.Amount{addr: core.NewAmount(100)},
		nonces:   map[core.Address]uint64{addr: 3},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/account/"+addr.Hex(), nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := parseJSON(t, w)
	if resp["address"] != addr.Hex() {
		t.Errorf("address = %v", resp["address"])
	}
	if resp["nonce"] != float64(3) {
		t.Errorf("nonce = %v", resp["nonce"])
	}
	// EOA should not have code_id.
	if _, ok := resp["code_id"]; ok {
		t.Error("EOA should not have code_id")
	}
}

func TestAccount_Contract(t *testing.T) {
	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	vm := &mockVM{
		contractInfo: map[core.Address]*core.ContractInfo{
			addr: {Address: addr.Hex(), CodeID: "abcd", Label: "token", Creator: "0x02"},
		},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/account/"+addr.Hex(), nil))

	resp := parseJSON(t, w)
	if resp["code_id"] != "abcd" {
		t.Errorf("code_id = %v", resp["code_id"])
	}
	if resp["label"] != "token" {
		t.Errorf("label = %v", resp["label"])
	}
	if resp["creator"] != "0x02" {
		t.Errorf("creator = %v", resp["creator"])
	}
}

func TestListCodes(t *testing.T) {
	vm := &mockVM{codes: []string{"aabb", "ccdd"}}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/codes", nil))

	resp := parseJSON(t, w)
	codes, ok := resp["codes"].([]any)
	if !ok || len(codes) != 2 {
		t.Fatalf("codes = %v", resp["codes"])
	}
}

func TestListContracts(t *testing.T) {
	vm := &mockVM{contracts: []core.ContractInfo{
		{Address: "0x01", CodeID: "ab", Label: "t1", Creator: "0x99"},
	}}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/contracts", nil))

	resp := parseJSON(t, w)
	list, ok := resp["contracts"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("contracts = %v", resp["contracts"])
	}
}

func TestContractInfo(t *testing.T) {
	addr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	vm := &mockVM{
		contractInfo: map[core.Address]*core.ContractInfo{
			addr: {Address: addr.Hex(), CodeID: "abcd", Label: "token", Creator: "0x99"},
		},
		balances: map[core.Address]core.Amount{addr: core.NewAmount(500)},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/contract/"+addr.Hex(), nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := parseJSON(t, w)
	if resp["label"] != "token" {
		t.Errorf("label = %v", resp["label"])
	}
}

func TestContractInfo_NotFound(t *testing.T) {
	srv := newTestServer(t, &mockVM{})
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/contract/0x0000000000000000000000000000000000000001", nil))

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestQuery(t *testing.T) {
	vm := &mockVM{
		queryFn: func(_ core.Address, _ []byte, _ uint64) ([]byte, uint64, error) {
			return []byte(`{"balance":"1000"}`), 500, nil
		},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	body := `{"contract":"0x0000000000000000000000000000000000000001","msg":{"balance":{}}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/query", strings.NewReader(body)))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := parseJSON(t, w)
	if resp["gas_used"] != float64(500) {
		t.Errorf("gas_used = %v", resp["gas_used"])
	}
}

func TestQuery_BadContract(t *testing.T) {
	vm := &mockVM{
		queryFn: func(_ core.Address, _ []byte, _ uint64) ([]byte, uint64, error) {
			return nil, 0, core.ErrContractNotFound
		},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	body := `{"contract":"0x0000000000000000000000000000000000000001","msg":{}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/query", strings.NewReader(body)))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// =========================================================================
// Group B: Signed transaction validation
// =========================================================================

func TestStore_ChainIDMismatch(t *testing.T) {
	vm := &mockVM{chainID: "nitro-test"}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{ChainID: "wrong-chain", Type: crypto.TxStore, Code: []byte("x")}
	w := signAndPost(t, srv.Handler(), "/store", tx, testKey(t))

	if w.Code != 401 {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid chain ID") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestStore_GasPriceBelowMin(t *testing.T) {
	vm := &mockVM{chainID: "nitro-test"}
	srv := newTestServerWithCfg(t, vm, Config{Network: Devnet, MinGasPrice: 10})

	tx := &crypto.Transaction{ChainID: "nitro-test", GasPrice: 5, Type: crypto.TxStore, Code: []byte("x")}
	w := signAndPost(t, srv.Handler(), "/store", tx, testKey(t))

	if w.Code != 401 {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "gas price below minimum") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestStore_WrongTxType(t *testing.T) {
	vm := &mockVM{chainID: "nitro-test"}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{ChainID: "nitro-test", Type: crypto.TxExecute, Contract: core.Address{0x01}}
	w := signAndPost(t, srv.Handler(), "/store", tx, testKey(t))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tx type must be store") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestInstantiate_WrongTxType(t *testing.T) {
	vm := &mockVM{chainID: "nitro-test"}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{ChainID: "nitro-test", Type: crypto.TxStore, Code: []byte("x")}
	w := signAndPost(t, srv.Handler(), "/instantiate", tx, testKey(t))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tx type must be instantiate") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestExecute_WrongTxType(t *testing.T) {
	vm := &mockVM{chainID: "nitro-test"}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{ChainID: "nitro-test", Type: crypto.TxStore, Code: []byte("x")}
	w := signAndPost(t, srv.Handler(), "/execute", tx, testKey(t))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tx type must be execute") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestStore_InvalidBody(t *testing.T) {
	vm := &mockVM{chainID: "nitro-test"}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	tests := []struct {
		name string
		body string
	}{
		{"not json", "this is not json"},
		{"empty tx", `{"tx":""}`},
		{"bad hex", `{"tx":"not-hex-data"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/store", strings.NewReader(tt.body)))
			if w.Code != 401 {
				t.Errorf("status = %d, want 401 (body: %s)", w.Code, w.Body.String())
			}
		})
	}
}

// =========================================================================
// Group C: Signed happy paths
// =========================================================================

func TestStore_Signed(t *testing.T) {
	key := testKey(t)
	sender := crypto.DeriveAddress(key)

	var capturedCode []byte
	var capturedSender core.Address
	var capturedNonce uint64

	vm := &mockVM{
		chainID: "nitro-test",
		storeCodeFn: func(code []byte, s *core.Address, n *uint64) ([]byte, uint64, error) {
			capturedCode = code
			if s != nil {
				capturedSender = *s
			}
			if n != nil {
				capturedNonce = *n
			}
			return []byte{0x01, 0x02}, 100, nil
		},
	}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    7,
		GasLimit: 1000000,
		Type:     crypto.TxStore,
		Code:     []byte("fake-wasm"),
	}
	w := signAndPost(t, srv.Handler(), "/store", tx, key)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if string(capturedCode) != "fake-wasm" {
		t.Errorf("code = %q", capturedCode)
	}
	if capturedSender != sender {
		t.Errorf("sender = %s, want %s", capturedSender.Hex(), sender.Hex())
	}
	if capturedNonce != 7 {
		t.Errorf("nonce = %d, want 7", capturedNonce)
	}

	resp := parseJSON(t, w)
	if resp["code_id"] != hex.EncodeToString([]byte{0x01, 0x02}) {
		t.Errorf("code_id = %v", resp["code_id"])
	}
	if resp["sender"] != sender.Hex() {
		t.Errorf("sender = %v", resp["sender"])
	}
}

func TestInstantiate_Signed(t *testing.T) {
	key := testKey(t)
	sender := crypto.DeriveAddress(key)
	contractAddr, _ := core.HexToAddress("0x00000000000000000000000000000000deadbeef")

	var capturedCodeID []byte
	var capturedLabel string

	vm := &mockVM{
		chainID: "nitro-test",
		instantiateFn: func(codeID []byte, s core.Address, msg []byte, label string, funds []wasmvmtypes.Coin, gasLimit uint64, nonce *uint64) (*core.InstantiateResult, uint64, error) {
			capturedCodeID = codeID
			capturedLabel = label
			return &core.InstantiateResult{ContractAddress: contractAddr, GasUsed: 3000}, 3000, nil
		},
	}
	srv := newTestServer(t, vm)

	codeID := []byte{0xaa, 0xbb}
	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    0,
		GasLimit: 1000000,
		Type:     crypto.TxInstantiate,
		CodeID:   codeID,
		Label:    "my-token",
		Msg:      []byte(`{"name":"Token"}`),
	}
	w := signAndPost(t, srv.Handler(), "/instantiate", tx, key)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if string(capturedCodeID) != string(codeID) {
		t.Errorf("codeID = %x, want %x", capturedCodeID, codeID)
	}
	if capturedLabel != "my-token" {
		t.Errorf("label = %q", capturedLabel)
	}

	resp := parseJSON(t, w)
	if resp["contract"] != contractAddr.Hex() {
		t.Errorf("contract = %v", resp["contract"])
	}
	if resp["sender"] != sender.Hex() {
		t.Errorf("sender = %v", resp["sender"])
	}
}

func TestExecute_Signed(t *testing.T) {
	key := testKey(t)
	sender := crypto.DeriveAddress(key)
	contractAddr, _ := core.HexToAddress("0x0000000000000000000000000000000000000099")

	var capturedContract core.Address
	var capturedMsg []byte

	vm := &mockVM{
		chainID: "nitro-test",
		executeFn: func(contract, s core.Address, msg []byte, funds []wasmvmtypes.Coin, gasLimit uint64, nonce *uint64) (*core.ExecuteResult, uint64, error) {
			capturedContract = contract
			capturedMsg = msg
			return &core.ExecuteResult{
				GasUsed:    2000,
				Data:       []byte("ok"),
				Attributes: []wasmvmtypes.EventAttribute{{Key: "action", Value: "transfer"}},
			}, 2000, nil
		},
	}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    0,
		GasLimit: 1000000,
		Type:     crypto.TxExecute,
		Contract: contractAddr,
		Msg:      []byte(`{"transfer":{}}`),
	}
	w := signAndPost(t, srv.Handler(), "/execute", tx, key)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if capturedContract != contractAddr {
		t.Errorf("contract = %s", capturedContract.Hex())
	}
	if string(capturedMsg) != `{"transfer":{}}` {
		t.Errorf("msg = %s", capturedMsg)
	}

	resp := parseJSON(t, w)
	if resp["sender"] != sender.Hex() {
		t.Errorf("sender = %v", resp["sender"])
	}
	if resp["gas_used"] != float64(2000) {
		t.Errorf("gas_used = %v", resp["gas_used"])
	}
	attrs, ok := resp["attributes"].([]any)
	if !ok || len(attrs) == 0 {
		t.Error("expected attributes in response")
	}
}

func TestExecute_WithGasFee(t *testing.T) {
	key := testKey(t)

	var deductCalled bool
	vm := &mockVM{
		chainID: "nitro-test",
		executeFn: func(_ core.Address, _ core.Address, _ []byte, _ []wasmvmtypes.Coin, _ uint64, _ *uint64) (*core.ExecuteResult, uint64, error) {
			return &core.ExecuteResult{GasUsed: 1000}, 1000, nil
		},
		deductGasFeeFn: func(_ core.Address, gasUsed, gasPrice uint64) error {
			deductCalled = true
			if gasUsed != 1000 {
				t.Errorf("gasUsed = %d, want 1000", gasUsed)
			}
			if gasPrice != 5 {
				t.Errorf("gasPrice = %d, want 5", gasPrice)
			}
			return nil
		},
	}
	srv := newTestServer(t, vm)

	contractAddr, _ := core.HexToAddress("0x0000000000000000000000000000000000000099")
	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    0,
		GasLimit: 1000000,
		GasPrice: 5,
		Type:     crypto.TxExecute,
		Contract: contractAddr,
		Msg:      []byte(`{}`),
	}
	w := signAndPost(t, srv.Handler(), "/execute", tx, key)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !deductCalled {
		t.Error("DeductGasFee was not called")
	}

	resp := parseJSON(t, w)
	if resp["gas_fee"] == nil {
		t.Error("expected gas_fee in response")
	}
}

// =========================================================================
// Group D: Gas fee rollback
// =========================================================================

func TestStore_GasFeeRollback(t *testing.T) {
	vm := &mockVM{
		chainID: "nitro-test",
		storeCodeFn: func(_ []byte, _ *core.Address, _ *uint64) ([]byte, uint64, error) {
			return []byte{0x01}, 100, nil
		},
		deductGasFeeFn: func(_ core.Address, _, _ uint64) error {
			return core.ErrInsufficientFunds
		},
	}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{ChainID: "nitro-test", GasPrice: 10, Type: crypto.TxStore, Code: []byte("x")}
	w := signAndPost(t, srv.Handler(), "/store", tx, testKey(t))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "gas fee") {
		t.Errorf("body = %s", w.Body.String())
	}
	if vm.snapshotCount == 0 {
		t.Error("expected Snapshot to be called")
	}
	if vm.restoreCount == 0 {
		t.Error("expected Restore to be called on gas fee failure")
	}
}

func TestExecute_GasFeeRollback(t *testing.T) {
	contractAddr, _ := core.HexToAddress("0x0000000000000000000000000000000000000099")
	vm := &mockVM{
		chainID: "nitro-test",
		executeFn: func(_ core.Address, _ core.Address, _ []byte, _ []wasmvmtypes.Coin, _ uint64, _ *uint64) (*core.ExecuteResult, uint64, error) {
			return &core.ExecuteResult{GasUsed: 1000}, 1000, nil
		},
		deductGasFeeFn: func(_ core.Address, _, _ uint64) error {
			return core.ErrInsufficientFunds
		},
	}
	srv := newTestServer(t, vm)

	tx := &crypto.Transaction{
		ChainID: "nitro-test", GasPrice: 10, Type: crypto.TxExecute,
		Contract: contractAddr, Msg: []byte(`{}`),
	}
	w := signAndPost(t, srv.Handler(), "/execute", tx, testKey(t))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if vm.restoreCount == 0 {
		t.Error("expected Restore to be called on gas fee failure")
	}
}

// =========================================================================
// Group E: Simulate
// =========================================================================

func TestSimulate_Execute(t *testing.T) {
	var executeCalled bool
	vm := &mockVM{
		executeFn: func(_ core.Address, _ core.Address, _ []byte, _ []wasmvmtypes.Coin, _ uint64, _ *uint64) (*core.ExecuteResult, uint64, error) {
			executeCalled = true
			return &core.ExecuteResult{GasUsed: 3000, Data: []byte("sim")}, 3000, nil
		},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	body := `{
		"type":"execute",
		"contract":"0x0000000000000000000000000000000000000001",
		"sender":"0x0000000000000000000000000000000000000002",
		"msg":{}
	}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/simulate", strings.NewReader(body)))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !executeCalled {
		t.Error("Execute was not called")
	}
	if vm.snapshotCount == 0 {
		t.Error("expected Snapshot")
	}
	if vm.restoreCount == 0 {
		t.Error("expected Restore after simulate")
	}

	resp := parseJSON(t, w)
	if resp["gas_used"] != float64(3000) {
		t.Errorf("gas_used = %v", resp["gas_used"])
	}
}

func TestSimulate_Instantiate(t *testing.T) {
	contractAddr, _ := core.HexToAddress("0x00000000000000000000000000000000deadbeef")
	vm := &mockVM{
		instantiateFn: func(_ []byte, _ core.Address, _ []byte, _ string, _ []wasmvmtypes.Coin, _ uint64, _ *uint64) (*core.InstantiateResult, uint64, error) {
			return &core.InstantiateResult{ContractAddress: contractAddr, GasUsed: 4000}, 4000, nil
		},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	body := `{
		"type":"instantiate",
		"code_id":"aabb",
		"sender":"0x0000000000000000000000000000000000000002",
		"msg":{},
		"label":"test"
	}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/simulate", strings.NewReader(body)))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := parseJSON(t, w)
	if resp["contract"] != contractAddr.Hex() {
		t.Errorf("contract = %v", resp["contract"])
	}
	if vm.restoreCount == 0 {
		t.Error("expected Restore after simulate")
	}
}

func TestSimulate_InvalidType(t *testing.T) {
	srv := newTestServer(t, &mockVM{})
	h := srv.Handler()

	body := `{"type":"query","contract":"0x01","sender":"0x02","msg":{}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/simulate", strings.NewReader(body)))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// =========================================================================
// Group F: Faucet + network gating
// =========================================================================

func TestFaucet(t *testing.T) {
	var setAddr core.Address
	var setAmt core.Amount
	vm := &mockVM{
		setBalanceFn: func(addr core.Address, amt core.Amount) {
			setAddr = addr
			setAmt = amt
		},
	}
	srv := newTestServer(t, vm)
	h := srv.Handler()

	body := `{"address":"0x0000000000000000000000000000000000000001","amount":"5000"}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/faucet", strings.NewReader(body)))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	expected, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	if setAddr != expected {
		t.Errorf("addr = %s, want %s", setAddr.Hex(), expected.Hex())
	}
	if !setAmt.Equal(core.NewAmount(5000)) {
		t.Errorf("amount = %s, want 5000", setAmt)
	}
}

func TestFaucet_DisabledOnTestnet(t *testing.T) {
	vm := &mockVM{}
	srv := newTestServerWithCfg(t, vm, Config{Network: Testnet})
	h := srv.Handler()

	body := `{"address":"0x0000000000000000000000000000000000000001","amount":"5000"}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/faucet", strings.NewReader(body)))

	// Route not registered — Go 1.22+ returns 405 for method-specific routes.
	if w.Code == 200 {
		t.Fatalf("expected faucet to be disabled on testnet, got 200")
	}
}

func TestFaucet_BadAddress(t *testing.T) {
	srv := newTestServer(t, &mockVM{})
	h := srv.Handler()

	body := `{"address":"not-valid","amount":"100"}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/faucet", strings.NewReader(body)))

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// =========================================================================
// Group G: Events
// =========================================================================

func TestEvents_Empty(t *testing.T) {
	srv := newTestServer(t, &mockVM{})
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/events", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	resp := parseJSON(t, w)
	events, ok := resp["events"].([]any)
	if !ok {
		t.Fatalf("events is not array: %v", resp["events"])
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestEvents_FilterByContract(t *testing.T) {
	srv := newTestServer(t, &mockVM{opSeq: 1})

	// Insert events for two contracts directly into DB.
	srv.db.Exec("INSERT INTO events (op_seq,tx_type,contract,sender,event_type,attributes,created_at) VALUES (1,'execute','0xAAAA','0x01','wasm','[]',1000)")
	srv.db.Exec("INSERT INTO events (op_seq,tx_type,contract,sender,event_type,attributes,created_at) VALUES (1,'execute','0xBBBB','0x01','wasm','[]',1001)")
	srv.db.Exec("INSERT INTO events (op_seq,tx_type,contract,sender,event_type,attributes,created_at) VALUES (1,'execute','0xAAAA','0x01','transfer','[]',1002)")

	h := srv.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/events?contract=0xAAAA", nil))

	resp := parseJSON(t, w)
	events := resp["events"].([]any)
	if len(events) != 2 {
		t.Errorf("expected 2 events for 0xAAAA, got %d", len(events))
	}
}

func TestEvents_FilterByType(t *testing.T) {
	srv := newTestServer(t, &mockVM{opSeq: 1})

	srv.db.Exec("INSERT INTO events (op_seq,tx_type,contract,sender,event_type,attributes,created_at) VALUES (1,'execute','0x01','0x01','wasm','[]',1000)")
	srv.db.Exec("INSERT INTO events (op_seq,tx_type,contract,sender,event_type,attributes,created_at) VALUES (1,'execute','0x01','0x01','transfer','[]',1001)")

	h := srv.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/events?type=transfer", nil))

	resp := parseJSON(t, w)
	events := resp["events"].([]any)
	if len(events) != 1 {
		t.Errorf("expected 1 transfer event, got %d", len(events))
	}
}

func TestEvents_Pagination(t *testing.T) {
	srv := newTestServer(t, &mockVM{opSeq: 1})

	for i := 0; i < 5; i++ {
		srv.db.Exec("INSERT INTO events (op_seq,tx_type,contract,sender,event_type,attributes,created_at) VALUES (1,'execute','0x01','0x01','wasm','[]',?)", 1000+i)
	}

	h := srv.Handler()

	// Page 1.
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, httptest.NewRequest("GET", "/events?limit=2&offset=0", nil))
	resp1 := parseJSON(t, w1)
	events1 := resp1["events"].([]any)
	if len(events1) != 2 {
		t.Errorf("page 1: expected 2 events, got %d", len(events1))
	}

	// Page 2.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest("GET", "/events?limit=2&offset=2", nil))
	resp2 := parseJSON(t, w2)
	events2 := resp2["events"].([]any)
	if len(events2) != 2 {
		t.Errorf("page 2: expected 2 events, got %d", len(events2))
	}

	// Page 3 (partial).
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, httptest.NewRequest("GET", "/events?limit=2&offset=4", nil))
	resp3 := parseJSON(t, w3)
	events3 := resp3["events"].([]any)
	if len(events3) != 1 {
		t.Errorf("page 3: expected 1 event, got %d", len(events3))
	}
}

// =========================================================================
// Group H: Persistence restore
// =========================================================================

func TestRestore_Accounts(t *testing.T) {
	var balAddr core.Address
	var balAmt core.Amount
	var nonceAddr core.Address
	var nonceVal uint64

	vm := &mockVM{
		setBalanceFn: func(addr core.Address, amt core.Amount) {
			balAddr = addr
			balAmt = amt
		},
		setNonceFn: func(addr core.Address, n uint64) {
			nonceAddr = addr
			nonceVal = n
		},
	}
	srv := newTestServer(t, vm)

	addr := "0x0000000000000000000000000000000000000001"
	srv.db.Exec("INSERT INTO accounts (address, balance, nonce) VALUES (?, ?, ?)", addr, "9999", 5)

	if err := srv.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	expected, _ := core.HexToAddress(addr)
	if balAddr != expected {
		t.Errorf("SetBalance addr = %s", balAddr.Hex())
	}
	if !balAmt.Equal(core.MustNewAmount("9999")) {
		t.Errorf("SetBalance amount = %s, want 9999", balAmt)
	}
	if nonceAddr != expected {
		t.Errorf("SetNonce addr = %s", nonceAddr.Hex())
	}
	if nonceVal != 5 {
		t.Errorf("SetNonce nonce = %d, want 5", nonceVal)
	}
}

func TestRestore_Codes(t *testing.T) {
	var capturedWasm []byte
	vm := &mockVM{
		storeCodeFn: func(code []byte, _ *core.Address, _ *uint64) ([]byte, uint64, error) {
			capturedWasm = code
			return []byte{0x01}, 0, nil
		},
	}
	srv := newTestServer(t, vm)

	wasm := []byte("fake-wasm-bytes")
	srv.db.Exec("INSERT INTO codes (code_id, wasm) VALUES (?, ?)", "aabbccdd", wasm)

	if err := srv.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(capturedWasm) != string(wasm) {
		t.Errorf("StoreCode wasm = %q, want %q", capturedWasm, wasm)
	}
}

func TestRestore_Contracts(t *testing.T) {
	var regAddr, regCreator core.Address
	var regChecksum []byte
	var regLabel string

	vm := &mockVM{
		registerContractFn: func(addr, creator core.Address, checksum []byte, label string) {
			regAddr = addr
			regCreator = creator
			regChecksum = checksum
			regLabel = label
		},
	}
	srv := newTestServer(t, vm)

	srv.db.Exec("INSERT INTO contracts (address, code_id, label, creator) VALUES (?, ?, ?, ?)",
		"0x0000000000000000000000000000000000000001", "aabb", "my-token", "0x0000000000000000000000000000000000000099")

	if err := srv.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	expectedAddr, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	expectedCreator, _ := core.HexToAddress("0x0000000000000000000000000000000000000099")
	if regAddr != expectedAddr {
		t.Errorf("RegisterContract addr = %s", regAddr.Hex())
	}
	if regCreator != expectedCreator {
		t.Errorf("RegisterContract creator = %s", regCreator.Hex())
	}
	if hex.EncodeToString(regChecksum) != "aabb" {
		t.Errorf("RegisterContract checksum = %x", regChecksum)
	}
	if regLabel != "my-token" {
		t.Errorf("RegisterContract label = %q", regLabel)
	}
}

func TestRestore_InstanceCount(t *testing.T) {
	var capturedCount uint64
	vm := &mockVM{
		setInstanceCountFn: func(n uint64) { capturedCount = n },
	}
	srv := newTestServer(t, vm)

	srv.db.Exec("INSERT INTO meta (key, value) VALUES ('instance_count', '5')")

	if err := srv.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if capturedCount != 5 {
		t.Errorf("SetInstanceCount = %d, want 5", capturedCount)
	}
}

func TestRestore_CodeSeq(t *testing.T) {
	var capturedSeq uint64
	vm := &mockVM{
		setCodeSeqFn: func(seq uint64) { capturedSeq = seq },
	}
	srv := newTestServer(t, vm)

	srv.db.Exec("INSERT INTO code_seqs (seq_id, code_id) VALUES (1, 'aa')")
	srv.db.Exec("INSERT INTO code_seqs (seq_id, code_id) VALUES (3, 'bb')")

	if err := srv.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if capturedSeq != 3 {
		t.Errorf("SetCodeSeq = %d, want 3", capturedSeq)
	}
}

// TestExecute_SubMessageTransferBalanceNotPersisted demonstrates that when a
// contract execution triggers a BankMsg::Send to a third-party address (not
// the sender or the target contract), the recipient's balance is updated
// in-memory but never persisted to SQLite. On server restart the funds vanish.
func TestExecute_SubMessageTransferBalanceNotPersisted(t *testing.T) {
	key := testKey(t)
	sender := crypto.DeriveAddress(key)
	contract, _ := core.HexToAddress("0x00000000000000000000000000000000deadbeef")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000099")

	vm := &mockVM{
		balances: map[core.Address]core.Amount{
			sender:   core.NewAmount(1_000_000),
			contract: core.NewAmount(500_000),
		},
		nonces:  map[core.Address]uint64{sender: 0},
		chainID: "nitro-test",
	}

	// Simulate a contract whose execute dispatches BankMsg::Send,
	// transferring 1000 from the contract to a third-party recipient.
	vm.executeFn = func(c, s core.Address, msg []byte, funds []wasmvmtypes.Coin, gl uint64, nonce *uint64) (*core.ExecuteResult, uint64, error) {
		vm.SetBalance(contract, core.NewAmount(499_000))
		vm.SetBalance(recipient, core.NewAmount(1_000))
		return &core.ExecuteResult{GasUsed: 5000}, 5000, nil
	}

	srv := newTestServer(t, vm)
	h := srv.Handler()

	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    0,
		GasLimit: 100_000,
		Type:     crypto.TxExecute,
		Contract: contract,
		Msg:      []byte(`{"transfer":{}}`),
	}

	w := signAndPost(t, h, "/execute", tx, key)
	if w.Code != 200 {
		t.Fatalf("execute failed: status %d, body %s", w.Code, w.Body.String())
	}

	// In-memory: the VM knows recipient has 1000.
	if bal := vm.GetBalance(recipient); !bal.Equal(core.NewAmount(1_000)) {
		t.Fatalf("in-memory recipient balance = %s, want 1000", bal.String())
	}

	// Persistence: recipient balance must be in SQLite so it survives restart.
	var balStr string
	err := srv.db.QueryRow(
		"SELECT balance FROM accounts WHERE address = ?", recipient.Hex(),
	).Scan(&balStr)
	if err != nil {
		t.Fatalf("recipient balance not persisted to SQLite: %v (funds lost on restart)", err)
	}
	if balStr != "1000" {
		t.Errorf("persisted recipient balance = %s, want 1000", balStr)
	}
}

// TestInstantiate_SubMessageTransferBalancePersisted verifies that the
// instantiate handler persists balances for third-party addresses modified
// by sub-message dispatch during contract instantiation.
func TestInstantiate_SubMessageTransferBalancePersisted(t *testing.T) {
	key := testKey(t)
	sender := crypto.DeriveAddress(key)
	contractAddr, _ := core.HexToAddress("0x00000000000000000000000000000000deadbeef")
	recipient, _ := core.HexToAddress("0x00000000000000000000000000000000000000aa")

	vm := &mockVM{
		balances: map[core.Address]core.Amount{
			sender: core.NewAmount(1_000_000),
		},
		nonces:  map[core.Address]uint64{sender: 0},
		chainID: "nitro-test",
	}

	codeID := []byte{0xab, 0xcd}

	// Simulate an instantiation whose init handler dispatches BankMsg::Send
	// to a third-party recipient.
	vm.instantiateFn = func(cID []byte, s core.Address, msg []byte, label string, funds []wasmvmtypes.Coin, gl uint64, nonce *uint64) (*core.InstantiateResult, uint64, error) {
		vm.SetBalance(contractAddr, core.NewAmount(2_000))
		vm.SetBalance(recipient, core.NewAmount(500))
		return &core.InstantiateResult{ContractAddress: contractAddr, GasUsed: 3000}, 3000, nil
	}

	srv := newTestServer(t, vm)
	h := srv.Handler()

	tx := &crypto.Transaction{
		ChainID:  "nitro-test",
		Nonce:    0,
		GasLimit: 100_000,
		Type:     crypto.TxInstantiate,
		CodeID:   codeID,
		Msg:      []byte(`{}`),
		Label:    "test",
	}

	w := signAndPost(t, h, "/instantiate", tx, key)
	if w.Code != 200 {
		t.Fatalf("instantiate failed: status %d, body %s", w.Code, w.Body.String())
	}

	// In-memory: the VM knows recipient has 500.
	if bal := vm.GetBalance(recipient); !bal.Equal(core.NewAmount(500)) {
		t.Fatalf("in-memory recipient balance = %s, want 500", bal.String())
	}

	// Persistence: recipient balance must be in SQLite so it survives restart.
	var balStr string
	err := srv.db.QueryRow(
		"SELECT balance FROM accounts WHERE address = ?", recipient.Hex(),
	).Scan(&balStr)
	if err != nil {
		t.Fatalf("recipient balance not persisted to SQLite: %v (funds lost on restart)", err)
	}
	if balStr != "500" {
		t.Errorf("persisted recipient balance = %s, want 500", balStr)
	}
}

func TestRestore_OpSeq(t *testing.T) {
	var capturedHeight uint64
	vm := &mockVM{
		setBlockInfoFn: func(height, _ uint64) { capturedHeight = height },
	}
	srv := newTestServer(t, vm)

	srv.db.Exec("INSERT INTO meta (key, value) VALUES ('op_seq', '42')")

	if err := srv.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if capturedHeight != 42 {
		t.Errorf("SetBlockInfo height = %d, want 42", capturedHeight)
	}
}
