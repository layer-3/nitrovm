package nitrovm

import (
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/ethereum/go-ethereum/rlp"
	"golang.org/x/crypto/sha3"
)

// TxType discriminates transaction kinds.
type TxType uint8

const (
	TxStore       TxType = 1
	TxInstantiate TxType = 2
	TxExecute     TxType = 3
)

// Transaction is the RLP-encodable transaction envelope.
// Field order is the canonical signing order.
type Transaction struct {
	ChainID  string
	Nonce    uint64
	GasLimit uint64
	GasPrice uint64
	Type     TxType
	// Store fields
	Code []byte // WASM bytecode (TxStore only)
	// Instantiate fields
	CodeID []byte // code hash (TxInstantiate only)
	Label  string // (TxInstantiate only)
	// Execute fields
	Contract Address // (TxExecute only)
	// Shared fields
	Msg   []byte    // JSON message (TxInstantiate, TxExecute)
	Funds []RLPCoin // attached funds
}

// RLPCoin is an RLP-encodable coin for transaction funds.
type RLPCoin struct {
	Denom  string
	Amount string
}

// SignedTransaction wraps a Transaction with its ECDSA signature.
type SignedTransaction struct {
	Tx Transaction
	V  uint8    // recovery ID (0 or 1)
	R  [32]byte // signature R
	S  [32]byte // signature S
}

// Keccak256 computes the Ethereum-compatible keccak256 hash.
func Keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// PubKeyToAddress derives an Ethereum-style address from an uncompressed
// secp256k1 public key (65 bytes with 0x04 prefix).
func PubKeyToAddress(pubkey []byte) Address {
	// Strip the 0x04 prefix if present.
	if len(pubkey) == 65 && pubkey[0] == 0x04 {
		pubkey = pubkey[1:]
	}
	hash := Keccak256(pubkey)
	return BytesToAddress(hash[12:])
}

// DeriveAddress computes the address for a secp256k1 private key.
func DeriveAddress(privkey *secp256k1.PrivateKey) Address {
	pub := privkey.PubKey().SerializeUncompressed()
	return PubKeyToAddress(pub)
}

// EncodeTx RLP-encodes a transaction for signing.
func EncodeTx(tx *Transaction) ([]byte, error) {
	return rlp.EncodeToBytes(tx)
}

// DecodeTx RLP-decodes a transaction.
func DecodeTx(data []byte) (*Transaction, error) {
	var tx Transaction
	if err := rlp.DecodeBytes(data, &tx); err != nil {
		return nil, fmt.Errorf("decode tx: %w", err)
	}
	return &tx, nil
}

// HashTx returns keccak256(RLP(tx)).
func HashTx(tx *Transaction) ([]byte, error) {
	encoded, err := EncodeTx(tx)
	if err != nil {
		return nil, err
	}
	return Keccak256(encoded), nil
}

// SignTx signs a transaction with a secp256k1 private key.
// Returns a SignedTransaction with V (recovery ID 0 or 1), R, S.
func SignTx(tx *Transaction, privkey *secp256k1.PrivateKey) (*SignedTransaction, error) {
	hash, err := HashTx(tx)
	if err != nil {
		return nil, fmt.Errorf("hash tx: %w", err)
	}

	sig := ecdsa.SignCompact(privkey, hash, false)
	// SignCompact returns [V || R || S] where V is the recovery flag (27/28).
	// We store V as 0 or 1.
	v := sig[0] - 27

	stx := &SignedTransaction{
		Tx: *tx,
		V:  v,
	}
	copy(stx.R[:], sig[1:33])
	copy(stx.S[:], sig[33:65])
	return stx, nil
}

// RecoverSender recovers the sender address from a signed transaction.
func RecoverSender(stx *SignedTransaction) (Address, error) {
	hash, err := HashTx(&stx.Tx)
	if err != nil {
		return Address{}, fmt.Errorf("hash tx: %w", err)
	}

	// Reconstruct compact signature: [V || R || S]
	var sig [65]byte
	sig[0] = stx.V + 27
	copy(sig[1:33], stx.R[:])
	copy(sig[33:65], stx.S[:])

	pubkey, _, err := ecdsa.RecoverCompact(sig[:], hash)
	if err != nil {
		return Address{}, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}

	return PubKeyToAddress(pubkey.SerializeUncompressed()), nil
}

// EncodeSignedTx serializes a SignedTransaction to bytes: RLP(tx) || V || R || S.
func EncodeSignedTx(stx *SignedTransaction) ([]byte, error) {
	txBytes, err := EncodeTx(&stx.Tx)
	if err != nil {
		return nil, err
	}
	// Append V (1 byte) + R (32 bytes) + S (32 bytes) = 65 bytes
	out := make([]byte, len(txBytes)+65)
	copy(out, txBytes)
	out[len(txBytes)] = stx.V
	copy(out[len(txBytes)+1:], stx.R[:])
	copy(out[len(txBytes)+33:], stx.S[:])
	return out, nil
}

// DecodeSignedTx deserializes a SignedTransaction from bytes.
func DecodeSignedTx(data []byte) (*SignedTransaction, error) {
	if len(data) < 66 { // minimum: 1 byte RLP + 65 bytes sig
		return nil, fmt.Errorf("%w: data too short", ErrInvalidSignature)
	}

	// The signature is the last 65 bytes.
	sigStart := len(data) - 65
	txBytes := data[:sigStart]

	tx, err := DecodeTx(txBytes)
	if err != nil {
		return nil, err
	}

	stx := &SignedTransaction{
		Tx: *tx,
		V:  data[sigStart],
	}
	copy(stx.R[:], data[sigStart+1:sigStart+33])
	copy(stx.S[:], data[sigStart+33:sigStart+65])
	return stx, nil
}
