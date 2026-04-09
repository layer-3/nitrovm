package nitrovm

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Hardhat account 0 test vector.
const (
	testPrivKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	testAddrHex    = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
)

func testPrivKey(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	b, err := hex.DecodeString(testPrivKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	return secp256k1.PrivKeyFromBytes(b)
}

func TestPubKeyToAddress_HardhatVector(t *testing.T) {
	priv := testPrivKey(t)
	addr := DeriveAddress(priv)
	if !strings.EqualFold(addr.Hex(), testAddrHex) {
		t.Fatalf("got %s, want %s", addr.Hex(), testAddrHex)
	}
}

func TestKeccak256(t *testing.T) {
	// keccak256("") = c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470
	hash := Keccak256(nil)
	want := "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	got := hex.EncodeToString(hash)
	if got != want {
		t.Fatalf("keccak256('') = %s, want %s", got, want)
	}
}

func TestRLPRoundtrip(t *testing.T) {
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    42,
		GasLimit: 1_000_000,
		GasPrice: 1,
		Type:     TxExecute,
		Contract: Address{0x01, 0x02, 0x03},
		Msg:      []byte(`{"transfer":{"to":"0x00","amount":"100"}}`),
		Funds:    []RLPCoin{{Denom: "YELLOW", Amount: "500"}},
	}
	encoded, err := EncodeTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTx(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ChainID != tx.ChainID {
		t.Fatalf("ChainID: got %q, want %q", decoded.ChainID, tx.ChainID)
	}
	if decoded.Nonce != tx.Nonce {
		t.Fatalf("Nonce: got %d, want %d", decoded.Nonce, tx.Nonce)
	}
	if decoded.Type != tx.Type {
		t.Fatalf("Type: got %d, want %d", decoded.Type, tx.Type)
	}
	if decoded.Contract != tx.Contract {
		t.Fatalf("Contract: got %s, want %s", decoded.Contract.Hex(), tx.Contract.Hex())
	}
	if string(decoded.Msg) != string(tx.Msg) {
		t.Fatalf("Msg: got %s, want %s", decoded.Msg, tx.Msg)
	}
	if len(decoded.Funds) != 1 || decoded.Funds[0].Denom != "YELLOW" || decoded.Funds[0].Amount != "500" {
		t.Fatalf("Funds mismatch: %+v", decoded.Funds)
	}
}

func TestSignAndRecover_Execute(t *testing.T) {
	priv := testPrivKey(t)
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    0,
		GasLimit: 500_000,
		GasPrice: 1,
		Type:     TxExecute,
		Contract: Address{0xaa},
		Msg:      []byte(`{"do_something":{}}`),
	}
	stx, err := SignTx(tx, priv)
	if err != nil {
		t.Fatal(err)
	}

	sender, err := RecoverSender(stx)
	if err != nil {
		t.Fatal(err)
	}
	expected := DeriveAddress(priv)
	if sender != expected {
		t.Fatalf("recovered %s, want %s", sender.Hex(), expected.Hex())
	}
}

func TestSignAndRecover_Store(t *testing.T) {
	priv := testPrivKey(t)
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    1,
		GasLimit: 10_000_000,
		GasPrice: 1,
		Type:     TxStore,
		Code:     []byte{0x00, 0x61, 0x73, 0x6d}, // WASM magic
	}
	stx, err := SignTx(tx, priv)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := RecoverSender(stx)
	if err != nil {
		t.Fatal(err)
	}
	if sender != DeriveAddress(priv) {
		t.Fatalf("sender mismatch")
	}
}

func TestSignAndRecover_Instantiate(t *testing.T) {
	priv := testPrivKey(t)
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    2,
		GasLimit: 5_000_000,
		GasPrice: 1,
		Type:     TxInstantiate,
		CodeID:   []byte{0xde, 0xad, 0xbe, 0xef},
		Label:    "my-contract",
		Msg:      []byte(`{"init":{}}`),
		Funds:    []RLPCoin{{Denom: "YELLOW", Amount: "1000"}},
	}
	stx, err := SignTx(tx, priv)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := RecoverSender(stx)
	if err != nil {
		t.Fatal(err)
	}
	if sender != DeriveAddress(priv) {
		t.Fatalf("sender mismatch")
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	priv := testPrivKey(t)
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    0,
		GasLimit: 500_000,
		GasPrice: 1,
		Type:     TxExecute,
		Contract: Address{0xbb},
		Msg:      []byte(`{}`),
	}
	stx, err := SignTx(tx, priv)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with R.
	stx.R[0] ^= 0xff

	_, err = RecoverSender(stx)
	if err == nil {
		t.Fatal("expected error for tampered signature")
	}
}

func TestSignedTxEncodeDecode(t *testing.T) {
	priv := testPrivKey(t)
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    7,
		GasLimit: 1_000_000,
		GasPrice: 2,
		Type:     TxExecute,
		Contract: Address{0xcc},
		Msg:      []byte(`{"ping":{}}`),
	}
	stx, err := SignTx(tx, priv)
	if err != nil {
		t.Fatal(err)
	}

	// Encode -> decode roundtrip.
	encoded, err := EncodeSignedTx(stx)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSignedTx(encoded)
	if err != nil {
		t.Fatal(err)
	}

	// Verify recovered sender matches.
	sender, err := RecoverSender(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if sender != DeriveAddress(priv) {
		t.Fatalf("sender mismatch after encode/decode roundtrip")
	}
}

func TestWrongChainIDRecoversDifferentAddress(t *testing.T) {
	priv := testPrivKey(t)
	tx := &Transaction{
		ChainID:  "nitro-1",
		Nonce:    0,
		GasLimit: 100_000,
		GasPrice: 1,
		Type:     TxExecute,
		Contract: Address{0xdd},
		Msg:      []byte(`{}`),
	}
	stx, err := SignTx(tx, priv)
	if err != nil {
		t.Fatal(err)
	}

	// Change chain ID after signing — recovered sender will differ.
	stx.Tx.ChainID = "other-chain"
	sender, err := RecoverSender(stx)
	// Recovery may succeed but yield a different address.
	if err == nil && sender == DeriveAddress(priv) {
		t.Fatal("expected different sender for wrong chain ID")
	}
}

func TestAmountMul(t *testing.T) {
	a := NewAmount(300)
	b := NewAmount(5000)
	result := a.Mul(b)
	if result.String() != "1500000" {
		t.Fatalf("300 * 5000 = %s, want 1500000", result.String())
	}
}
