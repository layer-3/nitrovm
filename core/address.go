package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Address is a 20-byte EVM-style account/contract identifier.
type Address [20]byte

// ZeroAddress is the zero-value address.
var ZeroAddress Address

// TrimHexPrefix removes an optional "0x" or "0X" prefix from a hex string.
func TrimHexPrefix(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

// HexToAddress converts a hex string (with or without 0x prefix) to an Address.
func HexToAddress(s string) (Address, error) {
	s = TrimHexPrefix(s)
	if len(s) != 40 {
		return Address{}, fmt.Errorf("invalid address length: got %d hex chars, want 40", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return Address{}, fmt.Errorf("invalid hex: %w", err)
	}
	var addr Address
	copy(addr[:], b)
	return addr, nil
}

// BytesToAddress converts a byte slice to an Address, taking the rightmost 20 bytes.
func BytesToAddress(b []byte) Address {
	var addr Address
	if len(b) > 20 {
		b = b[len(b)-20:]
	}
	copy(addr[20-len(b):], b)
	return addr
}

// Hex returns the EVM-style hex representation with 0x prefix.
func (a Address) Hex() string {
	return "0x" + hex.EncodeToString(a[:])
}

// String implements fmt.Stringer.
func (a Address) String() string {
	return a.Hex()
}

// IsZero returns true if the address is all zeros.
func (a Address) IsZero() bool {
	return a == ZeroAddress
}

// Bytes returns the address as a byte slice.
func (a Address) Bytes() []byte {
	return a[:]
}

// CreateContractAddress generates a deterministic contract address from
// the creator address, code ID, and instance counter.
func CreateContractAddress(creator Address, codeID []byte, instanceID uint64) Address {
	h := sha256.New()
	h.Write(creator[:])
	h.Write(codeID)
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, instanceID)
	h.Write(b)
	return BytesToAddress(h.Sum(nil))
}
