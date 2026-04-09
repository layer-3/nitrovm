package nitrovm

import (
	"errors"
	"strings"
	"testing"

	cosmwasm "github.com/CosmWasm/wasmvm/v2"
	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"
)

func TestHexToAddress(t *testing.T) {
	tests := []struct {
		input string
		want  string
		err   bool
	}{
		{"0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000001", false},
		{"0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000001", false},
		{"0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false},
		{"0xinvalid", "", true},
		{"0x00", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		addr, err := HexToAddress(tt.input)
		if tt.err {
			if err == nil {
				t.Errorf("HexToAddress(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("HexToAddress(%q): %v", tt.input, err)
			continue
		}
		if got := addr.Hex(); got != tt.want {
			t.Errorf("HexToAddress(%q).Hex() = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBytesToAddress(t *testing.T) {
	b := make([]byte, 32)
	b[31] = 0x01
	addr := BytesToAddress(b)
	if addr[19] != 0x01 {
		t.Errorf("BytesToAddress: last byte = %x, want 01", addr[19])
	}

	short := []byte{0xab}
	addr2 := BytesToAddress(short)
	if addr2[19] != 0xab {
		t.Errorf("BytesToAddress short: last byte = %x, want ab", addr2[19])
	}
}

func TestAddressIsZero(t *testing.T) {
	var addr Address
	if !addr.IsZero() {
		t.Error("zero address should be zero")
	}
	addr[0] = 1
	if addr.IsZero() {
		t.Error("non-zero address should not be zero")
	}
}

func TestCreateContractAddress(t *testing.T) {
	creator, _ := HexToAddress("0x0000000000000000000000000000000000000001")
	codeID := make([]byte, 32)

	addr1 := CreateContractAddress(creator, codeID, 1)
	addr2 := CreateContractAddress(creator, codeID, 2)

	if addr1 == addr2 {
		t.Error("different instance IDs should produce different addresses")
	}
	if addr1.IsZero() {
		t.Error("contract address should not be zero")
	}

	addr1b := CreateContractAddress(creator, codeID, 1)
	if addr1 != addr1b {
		t.Error("same inputs should produce same address")
	}
}

func TestGasMeter(t *testing.T) {
	gm := NewGasMeter(1000)

	if gm.GasConsumed() != 0 {
		t.Errorf("initial consumed = %d, want 0", gm.GasConsumed())
	}
	if gm.GasRemaining() != 1000 {
		t.Errorf("initial remaining = %d, want 1000", gm.GasRemaining())
	}

	if err := gm.ConsumeGas(500); err != nil {
		t.Errorf("ConsumeGas(500): %v", err)
	}
	if gm.GasConsumed() != 500 {
		t.Errorf("consumed = %d, want 500", gm.GasConsumed())
	}

	if err := gm.ConsumeGas(501); err != ErrOutOfGas {
		t.Errorf("ConsumeGas(501) = %v, want ErrOutOfGas", err)
	}
}

func TestHostAddressAPI(t *testing.T) {
	canonical := make([]byte, 20)
	canonical[19] = 1

	human, _, err := humanizeAddress(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if human != "0x0000000000000000000000000000000000000001" {
		t.Errorf("humanize = %q", human)
	}

	back, _, err := canonicalizeAddress(human)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 20 || back[19] != 1 {
		t.Errorf("canonicalize roundtrip failed: %x", back)
	}

	if _, err := validateAddress(human); err != nil {
		t.Errorf("validate valid: %v", err)
	}
	if _, err := validateAddress("0xinvalid"); err == nil {
		t.Error("validate should reject invalid")
	}
	if _, _, err := humanizeAddress([]byte{1, 2, 3}); err == nil {
		t.Error("humanize should reject wrong length")
	}
}

// --- Dispatch unit tests ---

func testVMInternal(t *testing.T) *NitroVM {
	t.Helper()
	// Minimal VM with in-memory-like setup for unit testing dispatch logic.
	// We don't need wasmvm here — just the account/contract maps.
	vm := &NitroVM{
		accounts:  make(map[Address]*Account),
		contracts: make(map[Address]*contractMeta),
		codes:     make(map[string]cosmwasm.Checksum),
		codeBySeq: make(map[uint64]cosmwasm.Checksum),
		seqByCode: make(map[string]uint64),
	}
	return vm
}

func TestDispatchBankSend(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := HexToAddress("0x0000000000000000000000000000000000000bbb")

	// Give the contract 1000 YELLOW.
	vm.SetBalance(contract, NewAmount(1000))

	msgs := []wasmvmtypes.SubMsg{
		{
			ID: 1,
			Msg: wasmvmtypes.CosmosMsg{
				Bank: &wasmvmtypes.BankMsg{
					Send: &wasmvmtypes.SendMsg{
						ToAddress: recipient.Hex(),
						Amount:    wasmvmtypes.Array[wasmvmtypes.Coin]{{Denom: "YELLOW", Amount: "300"}},
					},
				},
			},
			ReplyOn: wasmvmtypes.ReplyNever,
		},
	}

	events, err := vm.dispatchMessages(contract, msgs, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}

	// Check balances.
	if bal := vm.GetBalance(contract); !bal.Equal(NewAmount(700)) {
		t.Errorf("contract balance = %s, want 700", bal)
	}
	if bal := vm.GetBalance(recipient); !bal.Equal(NewAmount(300)) {
		t.Errorf("recipient balance = %s, want 300", bal)
	}

	// Check events.
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != "transfer" {
		t.Errorf("event type = %q, want transfer", events[0].Type)
	}
}

func TestDispatchBankSendInsufficientFunds(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := HexToAddress("0x0000000000000000000000000000000000000bbb")

	// Contract has 0 balance.
	msgs := []wasmvmtypes.SubMsg{
		{
			ID: 1,
			Msg: wasmvmtypes.CosmosMsg{
				Bank: &wasmvmtypes.BankMsg{
					Send: &wasmvmtypes.SendMsg{
						ToAddress: recipient.Hex(),
						Amount:    wasmvmtypes.Array[wasmvmtypes.Coin]{{Denom: "YELLOW", Amount: "100"}},
					},
				},
			},
			ReplyOn: wasmvmtypes.ReplyNever,
		},
	}

	_, err := vm.dispatchMessages(contract, msgs, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error for insufficient funds")
	}
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("error = %v, want ErrInsufficientFunds", err)
	}
}

func TestDispatchDepthLimit(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := HexToAddress("0x0000000000000000000000000000000000000aaa")

	// Any message — depth check happens before dispatch.
	msgs := []wasmvmtypes.SubMsg{
		{
			ID: 1,
			Msg: wasmvmtypes.CosmosMsg{
				Bank: &wasmvmtypes.BankMsg{
					Send: &wasmvmtypes.SendMsg{
						ToAddress: contract.Hex(),
						Amount:    wasmvmtypes.Array[wasmvmtypes.Coin]{{Denom: "YELLOW", Amount: "1"}},
					},
				},
			},
			ReplyOn: wasmvmtypes.ReplyNever,
		},
	}

	_, err := vm.dispatchMessages(contract, msgs, 1_000_000, maxDispatchDepth)
	if err == nil {
		t.Fatal("expected ErrMaxDispatchDepth")
	}
	if !errors.Is(err, ErrMaxDispatchDepth) {
		t.Errorf("error = %v, want ErrMaxDispatchDepth", err)
	}
}

func TestDispatchUnsupportedMsg(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := HexToAddress("0x0000000000000000000000000000000000000aaa")

	msgs := []wasmvmtypes.SubMsg{
		{
			ID: 1,
			Msg: wasmvmtypes.CosmosMsg{
				Staking: &wasmvmtypes.StakingMsg{
					Delegate: &wasmvmtypes.DelegateMsg{
						Validator: "val1",
						Amount:    wasmvmtypes.Coin{Denom: "YELLOW", Amount: "100"},
					},
				},
			},
			ReplyOn: wasmvmtypes.ReplyNever,
		},
	}

	_, err := vm.dispatchMessages(contract, msgs, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected ErrUnsupportedMsg")
	}
	if !errors.Is(err, ErrUnsupportedMsg) {
		t.Errorf("error = %v, want ErrUnsupportedMsg", err)
	}
}

func TestCodeSequentialIDs(t *testing.T) {
	vm := testVMInternal(t)

	// Simulate two different codes being stored.
	checksum1 := cosmwasm.Checksum([]byte("abcdef1234567890abcdef1234567890"))
	checksum2 := cosmwasm.Checksum([]byte("ffffffffffffffffffffffffffffffff"))
	hex1 := "6162636465663132333435363738393061626364656631323334353637383930"
	hex2 := "6666666666666666666666666666666666666666666666666666666666666666"

	vm.mu.Lock()
	vm.codes[hex1] = checksum1
	vm.codeSeq++
	vm.codeBySeq[vm.codeSeq] = checksum1
	vm.seqByCode[hex1] = vm.codeSeq
	vm.codes[hex2] = checksum2
	vm.codeSeq++
	vm.codeBySeq[vm.codeSeq] = checksum2
	vm.seqByCode[hex2] = vm.codeSeq
	vm.mu.Unlock()

	// Verify sequential IDs.
	seq1, ok := vm.GetCodeSeq(hex1)
	if !ok || seq1 != 1 {
		t.Errorf("code1 seq = %d, ok = %v; want 1, true", seq1, ok)
	}
	seq2, ok := vm.GetCodeSeq(hex2)
	if !ok || seq2 != 2 {
		t.Errorf("code2 seq = %d, ok = %v; want 2, true", seq2, ok)
	}

	// Non-existent code.
	_, ok = vm.GetCodeSeq("0000000000000000000000000000000000000000000000000000000000000000")
	if ok {
		t.Error("expected ok=false for non-existent code")
	}
}

func TestDispatchWasmInstantiateCodeNotFound(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := HexToAddress("0x0000000000000000000000000000000000000aaa")

	msgs := []wasmvmtypes.SubMsg{
		{
			ID: 1,
			Msg: wasmvmtypes.CosmosMsg{
				Wasm: &wasmvmtypes.WasmMsg{
					Instantiate: &wasmvmtypes.InstantiateMsg{
						CodeID: 999,
						Msg:    []byte(`{}`),
						Label:  "test",
					},
				},
			},
			ReplyOn: wasmvmtypes.ReplyNever,
		},
	}

	_, err := vm.dispatchMessages(contract, msgs, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error for unknown code_id")
	}
	if !strings.Contains(err.Error(), "code_id 999 not found") {
		t.Errorf("error = %v, want 'code_id 999 not found'", err)
	}
}

func TestNonceValidation(t *testing.T) {
	vm := testVMInternal(t)
	sender, _ := HexToAddress("0x0000000000000000000000000000000000000001")
	contract, _ := HexToAddress("0x0000000000000000000000000000000000000002")

	// Set sender nonce to 5.
	vm.SetNonce(sender, 5)

	// Execute with wrong nonce should fail before touching wasmvm.
	wrongNonce := uint64(3)
	_, err := vm.Execute(contract, sender, []byte(`{}`), nil, 1_000_000, &wrongNonce)
	if err == nil {
		t.Fatal("expected ErrInvalidNonce")
	}
	if !errors.Is(err, ErrInvalidNonce) {
		t.Errorf("error = %v, want ErrInvalidNonce", err)
	}

	// Execute with nil nonce should pass nonce check (and fail later on missing contract).
	_, err = vm.Execute(contract, sender, []byte(`{}`), nil, 1_000_000, nil)
	if !errors.Is(err, ErrContractNotFound) {
		t.Errorf("nil nonce should skip validation; error = %v, want ErrContractNotFound", err)
	}

	// Instantiate with wrong nonce.
	wrongNonce = uint64(0)
	_, err = vm.Instantiate([]byte("nonexistent"), sender, []byte(`{}`), "test", nil, 1_000_000, &wrongNonce)
	if !errors.Is(err, ErrInvalidNonce) {
		t.Errorf("error = %v, want ErrInvalidNonce", err)
	}
}
