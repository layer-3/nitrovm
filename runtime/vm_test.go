package runtime

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	cosmwasm "github.com/CosmWasm/wasmvm/v2"
	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage/memory"
)

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

	if err := gm.ConsumeGas(501); err != core.ErrOutOfGas {
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
	// Minimal VM with in-memory storage for unit testing dispatch logic.
	// We don't need wasmvm here — just the account/contract maps and storage
	// for savepoint support.
	vm := &NitroVM{
		accounts:  make(map[core.Address]*core.Account),
		contracts: make(map[core.Address]*contractMeta),
		codes:     make(map[string]cosmwasm.Checksum),
		codeBySeq: make(map[uint64]cosmwasm.Checksum),
		seqByCode: make(map[string]uint64),
		storage:   memory.New(),
	}
	// Default: reply returns an error (no wasm VM in unit tests).
	vm.replyFn = func(_ cosmwasm.Checksum, _ wasmvmtypes.Env, _ wasmvmtypes.Reply, _ wasmvmtypes.KVStore, _ uint64) (*wasmvmtypes.ContractResult, uint64, error) {
		return nil, 0, fmt.Errorf("reply not configured in test")
	}
	return vm
}

func TestDispatchBankSend(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")

	// Give the contract 1000 YELLOW.
	vm.SetBalance(contract, core.NewAmount(1000))

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

	dr, err := vm.dispatchMessages(contract, msgs, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}

	// Check balances.
	if bal := vm.GetBalance(contract); !bal.Equal(core.NewAmount(700)) {
		t.Errorf("contract balance = %s, want 700", bal)
	}
	if bal := vm.GetBalance(recipient); !bal.Equal(core.NewAmount(300)) {
		t.Errorf("recipient balance = %s, want 300", bal)
	}

	// Check events.
	if len(dr.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(dr.Events))
	}
	if dr.Events[0].Type != "transfer" {
		t.Errorf("event type = %q, want transfer", dr.Events[0].Type)
	}
}

func TestDispatchBankSendInsufficientFunds(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")

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
	if !errors.Is(err, core.ErrInsufficientFunds) {
		t.Errorf("error = %v, want ErrInsufficientFunds", err)
	}
}

func TestDispatchDepthLimit(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")

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
	if !errors.Is(err, core.ErrMaxDispatchDepth) {
		t.Errorf("error = %v, want ErrMaxDispatchDepth", err)
	}
}

func TestDispatchUnsupportedMsg(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")

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
	if !errors.Is(err, core.ErrUnsupportedMsg) {
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
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")

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
	sender, _ := core.HexToAddress("0x0000000000000000000000000000000000000001")
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000002")

	// Set sender nonce to 5.
	vm.SetNonce(sender, 5)

	// Execute with wrong nonce should fail before touching wasmvm.
	wrongNonce := uint64(3)
	_, err := vm.Execute(contract, sender, []byte(`{}`), nil, 1_000_000, &wrongNonce)
	if err == nil {
		t.Fatal("expected ErrInvalidNonce")
	}
	if !errors.Is(err, core.ErrInvalidNonce) {
		t.Errorf("error = %v, want ErrInvalidNonce", err)
	}

	// Execute with nil nonce should pass nonce check (and fail later on missing contract).
	_, err = vm.Execute(contract, sender, []byte(`{}`), nil, 1_000_000, nil)
	if !errors.Is(err, core.ErrContractNotFound) {
		t.Errorf("nil nonce should skip validation; error = %v, want ErrContractNotFound", err)
	}

	// Instantiate with wrong nonce.
	wrongNonce = uint64(0)
	_, err = vm.Instantiate([]byte("nonexistent"), sender, []byte(`{}`), "test", nil, 1_000_000, &wrongNonce)
	if !errors.Is(err, core.ErrInvalidNonce) {
		t.Errorf("error = %v, want ErrInvalidNonce", err)
	}
}

// --- ReplyOn unit tests ---

// bankSendSubMsg creates a SubMsg with a BankMsg::Send.
func bankSendSubMsg(id uint64, to core.Address, amount string) wasmvmtypes.SubMsg {
	return wasmvmtypes.SubMsg{
		ID: id,
		Msg: wasmvmtypes.CosmosMsg{
			Bank: &wasmvmtypes.BankMsg{
				Send: &wasmvmtypes.SendMsg{
					ToAddress: to.Hex(),
					Amount:    wasmvmtypes.Array[wasmvmtypes.Coin]{{Denom: "YELLOW", Amount: amount}},
				},
			},
		},
		ReplyOn: wasmvmtypes.ReplyNever,
	}
}

// mockReplySuccess returns a replyFn that records the Reply and returns a success.
func mockReplySuccess(captured *wasmvmtypes.Reply, resp *wasmvmtypes.Response) func(cosmwasm.Checksum, wasmvmtypes.Env, wasmvmtypes.Reply, wasmvmtypes.KVStore, uint64) (*wasmvmtypes.ContractResult, uint64, error) {
	if resp == nil {
		resp = &wasmvmtypes.Response{}
	}
	return func(_ cosmwasm.Checksum, _ wasmvmtypes.Env, reply wasmvmtypes.Reply, _ wasmvmtypes.KVStore, _ uint64) (*wasmvmtypes.ContractResult, uint64, error) {
		if captured != nil {
			*captured = reply
		}
		return &wasmvmtypes.ContractResult{Ok: resp}, 0, nil
	}
}

func TestReplySuccess_Invoked(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	var captured wasmvmtypes.Reply
	vm.replyFn = mockReplySuccess(&captured, nil)

	sub := bankSendSubMsg(42, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplySuccess
	sub.Payload = []byte("ctx")
	msgs := []wasmvmtypes.SubMsg{sub}

	dr, err := vm.dispatchMessages(contract, msgs, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}

	if captured.ID != 42 {
		t.Errorf("reply ID = %d, want 42", captured.ID)
	}
	if string(captured.Payload) != "ctx" {
		t.Errorf("reply Payload = %q, want %q", captured.Payload, "ctx")
	}
	if captured.Result.Ok == nil {
		t.Fatal("reply Result.Ok is nil, want success")
	}
	if len(dr.Events) == 0 {
		t.Error("expected events from sub-message dispatch")
	}
}

func TestReplySuccess_ErrorAborts(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	// Contract has 0 balance — bank send will fail.

	called := false
	vm.replyFn = func(_ cosmwasm.Checksum, _ wasmvmtypes.Env, _ wasmvmtypes.Reply, _ wasmvmtypes.KVStore, _ uint64) (*wasmvmtypes.ContractResult, uint64, error) {
		called = true
		return &wasmvmtypes.ContractResult{Ok: &wasmvmtypes.Response{}}, 0, nil
	}

	sub := bankSendSubMsg(1, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplySuccess

	_, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error for insufficient funds")
	}
	if called {
		t.Error("reply should not be called on error with ReplySuccess")
	}
}

func TestReplyError_CatchesError(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}
	// Contract has 0 balance — bank send will fail.

	var captured wasmvmtypes.Reply
	vm.replyFn = mockReplySuccess(&captured, nil)

	sub := bankSendSubMsg(7, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplyError

	dr, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("expected error to be caught, got: %v", err)
	}

	if captured.Result.Err == "" {
		t.Error("reply should receive error string")
	}
	if captured.ID != 7 {
		t.Errorf("reply ID = %d, want 7", captured.ID)
	}
	_ = dr
}

func TestReplyError_SuccessSkipsReply(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))

	called := false
	vm.replyFn = func(_ cosmwasm.Checksum, _ wasmvmtypes.Env, _ wasmvmtypes.Reply, _ wasmvmtypes.KVStore, _ uint64) (*wasmvmtypes.ContractResult, uint64, error) {
		called = true
		return &wasmvmtypes.ContractResult{Ok: &wasmvmtypes.Response{}}, 0, nil
	}

	sub := bankSendSubMsg(1, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplyError

	dr, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}
	if called {
		t.Error("reply should not be called on success with ReplyError")
	}
	if len(dr.Events) != 1 || dr.Events[0].Type != "transfer" {
		t.Errorf("expected transfer event, got %v", dr.Events)
	}
}

func TestReplyAlways_OnSuccess(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	var captured wasmvmtypes.Reply
	vm.replyFn = mockReplySuccess(&captured, nil)

	sub := bankSendSubMsg(10, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplyAlways

	_, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}
	if captured.Result.Ok == nil {
		t.Error("reply should receive success result with ReplyAlways")
	}
}

func TestReplyAlways_OnError(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}
	// 0 balance → bank send fails.

	var captured wasmvmtypes.Reply
	vm.replyFn = mockReplySuccess(&captured, nil)

	sub := bankSendSubMsg(11, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplyAlways

	_, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("expected error to be caught with ReplyAlways, got: %v", err)
	}
	if captured.Result.Err == "" {
		t.Error("reply should receive error string with ReplyAlways")
	}
}

func TestReply_DataOverride(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	replyData := []byte("from-reply")
	vm.replyFn = mockReplySuccess(nil, &wasmvmtypes.Response{Data: replyData})

	sub := bankSendSubMsg(1, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplySuccess

	dr, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}
	if dr.DataOverride == nil {
		t.Fatal("expected DataOverride to be set")
	}
	if string(*dr.DataOverride) != "from-reply" {
		t.Errorf("DataOverride = %q, want %q", *dr.DataOverride, "from-reply")
	}
}

func TestReply_SubMessages(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	final, _ := core.HexToAddress("0x0000000000000000000000000000000000000ccc")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	// Reply handler returns a BankMsg::Send sub-message.
	vm.replyFn = mockReplySuccess(nil, &wasmvmtypes.Response{
		Messages: wasmvmtypes.Array[wasmvmtypes.SubMsg]{
			bankSendSubMsg(0, final, "50"),
		},
	})

	sub := bankSendSubMsg(1, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplySuccess

	_, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("dispatchMessages: %v", err)
	}

	// Original send: contract -100 → recipient.
	// Reply sub-message: contract -50 → final.
	if bal := vm.GetBalance(contract); !bal.Equal(core.NewAmount(850)) {
		t.Errorf("contract balance = %s, want 850", bal)
	}
	if bal := vm.GetBalance(recipient); !bal.Equal(core.NewAmount(100)) {
		t.Errorf("recipient balance = %s, want 100", bal)
	}
	if bal := vm.GetBalance(final); !bal.Equal(core.NewAmount(50)) {
		t.Errorf("final balance = %s, want 50", bal)
	}
}

func TestReply_DepthLimit(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	// Reply handler that itself returns sub-messages,
	// causing recursive dispatch from reply.
	vm.replyFn = mockReplySuccess(nil, &wasmvmtypes.Response{
		Messages: wasmvmtypes.Array[wasmvmtypes.SubMsg]{
			bankSendSubMsg(0, recipient, "1"),
		},
	})

	sub := bankSendSubMsg(1, recipient, "1")
	sub.ReplyOn = wasmvmtypes.ReplySuccess

	// Start near the depth limit so the reply sub-message dispatch exceeds it.
	_, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, maxDispatchDepth-1)
	if err == nil {
		t.Fatal("expected ErrMaxDispatchDepth")
	}
	if !errors.Is(err, core.ErrMaxDispatchDepth) {
		t.Errorf("error = %v, want ErrMaxDispatchDepth", err)
	}
}

func TestReplyError_Rollback(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	// Two sub-messages: first succeeds (sends 500), second fails.
	// With ReplyError on the second, the second's state changes should be
	// rolled back, but the first should persist.
	sub1 := bankSendSubMsg(1, recipient, "500")
	sub2 := bankSendSubMsg(2, recipient, "600")
	sub2.ReplyOn = wasmvmtypes.ReplyError // This will fail: contract only has 500 left.

	vm.replyFn = mockReplySuccess(nil, nil)

	dr, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub1, sub2}, 1_000_000, 0)
	if err != nil {
		t.Fatalf("expected error to be caught, got: %v", err)
	}

	// First send: contract had 1000, sent 500 → 500 left.
	// Second send fails and is rolled back → balances stay at 500/500.
	if bal := vm.GetBalance(contract); !bal.Equal(core.NewAmount(500)) {
		t.Errorf("contract balance = %s, want 500", bal)
	}
	if bal := vm.GetBalance(recipient); !bal.Equal(core.NewAmount(500)) {
		t.Errorf("recipient balance = %s, want 500", bal)
	}
	_ = dr
}

func TestReply_HandlerError_Aborts(t *testing.T) {
	vm := testVMInternal(t)
	contract, _ := core.HexToAddress("0x0000000000000000000000000000000000000aaa")
	recipient, _ := core.HexToAddress("0x0000000000000000000000000000000000000bbb")
	vm.SetBalance(contract, core.NewAmount(1000))
	vm.contracts[contract] = &contractMeta{Checksum: []byte("test")}

	// Reply handler returns a contract error.
	vm.replyFn = func(_ cosmwasm.Checksum, _ wasmvmtypes.Env, _ wasmvmtypes.Reply, _ wasmvmtypes.KVStore, _ uint64) (*wasmvmtypes.ContractResult, uint64, error) {
		return &wasmvmtypes.ContractResult{Err: "reply failed"}, 0, nil
	}

	sub := bankSendSubMsg(1, recipient, "100")
	sub.ReplyOn = wasmvmtypes.ReplySuccess

	_, err := vm.dispatchMessages(contract, []wasmvmtypes.SubMsg{sub}, 1_000_000, 0)
	if err == nil {
		t.Fatal("expected error from failed reply handler")
	}
	if !errors.Is(err, core.ErrReplyFailed) {
		t.Errorf("error = %v, want ErrReplyFailed", err)
	}
}
