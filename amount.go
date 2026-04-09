package nitrovm

import (
	"encoding/json"
	"fmt"
	"math/big"
)

// Amount represents a non-negative token quantity backed by *big.Int,
// compatible with CosmWasm's string-encoded Coin.Amount (uint256 range).
// The zero value is a valid zero amount.
type Amount struct {
	v *big.Int
}

// val returns the underlying big.Int, treating nil as zero.
func (a Amount) val() *big.Int {
	if a.v != nil {
		return a.v
	}
	return new(big.Int)
}

// NewAmount creates an Amount from a uint64.
func NewAmount(n uint64) Amount {
	return Amount{v: new(big.Int).SetUint64(n)}
}

// NewAmountFromString parses a base-10 string into an Amount.
// Returns an error if the string is not a valid non-negative integer.
func NewAmountFromString(s string) (Amount, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return Amount{}, fmt.Errorf("invalid amount: %q", s)
	}
	if v.Sign() < 0 {
		return Amount{}, fmt.Errorf("negative amount: %q", s)
	}
	return Amount{v: v}, nil
}

// MustNewAmount parses a base-10 string into an Amount, panicking on error.
func MustNewAmount(s string) Amount {
	a, err := NewAmountFromString(s)
	if err != nil {
		panic(err)
	}
	return a
}

// Zero returns a zero Amount.
func Zero() Amount {
	return Amount{}
}

// Add returns a + b.
func (a Amount) Add(b Amount) Amount {
	return Amount{v: new(big.Int).Add(a.val(), b.val())}
}

// Sub returns a - b. Returns an error if b > a (would underflow).
func (a Amount) Sub(b Amount) (Amount, error) {
	result := new(big.Int).Sub(a.val(), b.val())
	if result.Sign() < 0 {
		return Amount{}, ErrInsufficientFunds
	}
	return Amount{v: result}, nil
}

// Cmp compares a and b: -1 if a < b, 0 if a == b, +1 if a > b.
func (a Amount) Cmp(b Amount) int {
	return a.val().Cmp(b.val())
}

// Equal returns true if a == b.
func (a Amount) Equal(b Amount) bool {
	return a.val().Cmp(b.val()) == 0
}

// GT returns true if a > b.
func (a Amount) GT(b Amount) bool {
	return a.val().Cmp(b.val()) > 0
}

// LT returns true if a < b.
func (a Amount) LT(b Amount) bool {
	return a.val().Cmp(b.val()) < 0
}

// IsZero returns true if the amount is zero.
func (a Amount) IsZero() bool {
	return a.v == nil || a.v.Sign() == 0
}

// String returns the base-10 representation.
func (a Amount) String() string {
	return a.val().String()
}

// MarshalJSON encodes the amount as a JSON quoted string (CosmWasm convention).
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON decodes an amount from a JSON string or number.
func (a *Amount) UnmarshalJSON(data []byte) error {
	// Try quoted string first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return fmt.Errorf("invalid amount string: %q", s)
		}
		if v.Sign() < 0 {
			return fmt.Errorf("negative amount: %q", s)
		}
		a.v = v
		return nil
	}

	// Fall back to JSON number.
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("amount must be a string or number, got %s", string(data))
	}
	v, ok := new(big.Int).SetString(n.String(), 10)
	if !ok {
		return fmt.Errorf("invalid amount number: %s", n)
	}
	if v.Sign() < 0 {
		return fmt.Errorf("negative amount: %s", n)
	}
	a.v = v
	return nil
}
