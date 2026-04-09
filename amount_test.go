package nitrovm

import (
	"encoding/json"
	"testing"
)

func TestAmountZeroValue(t *testing.T) {
	var a Amount
	if !a.IsZero() {
		t.Error("zero value should be zero")
	}
	if s := a.String(); s != "0" {
		t.Errorf("zero value String() = %q, want \"0\"", s)
	}
}

func TestNewAmount(t *testing.T) {
	a := NewAmount(42)
	if a.String() != "42" {
		t.Errorf("NewAmount(42).String() = %q", a.String())
	}
	if a.IsZero() {
		t.Error("NewAmount(42) should not be zero")
	}

	z := NewAmount(0)
	if !z.IsZero() {
		t.Error("NewAmount(0) should be zero")
	}
}

func TestNewAmountFromString(t *testing.T) {
	// Valid large number (max uint256).
	a, err := NewAmountFromString("115792089237316195423570985008687907853269984665640564039457584007913129639935")
	if err != nil {
		t.Fatal(err)
	}
	if a.IsZero() {
		t.Error("max uint256 should not be zero")
	}

	// Negative.
	if _, err := NewAmountFromString("-1"); err == nil {
		t.Error("expected error for negative amount")
	}

	// Garbage.
	if _, err := NewAmountFromString("abc"); err == nil {
		t.Error("expected error for non-numeric string")
	}

	// Empty.
	if _, err := NewAmountFromString(""); err == nil {
		t.Error("expected error for empty string")
	}
}

func TestMustNewAmount(t *testing.T) {
	a := MustNewAmount("12345")
	if a.String() != "12345" {
		t.Errorf("MustNewAmount = %q", a.String())
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewAmount should panic on invalid input")
		}
	}()
	MustNewAmount("bad")
}

func TestAmountAdd(t *testing.T) {
	a := NewAmount(100)
	b := NewAmount(200)
	c := a.Add(b)
	if c.String() != "300" {
		t.Errorf("100 + 200 = %s", c.String())
	}
	// Original unchanged.
	if a.String() != "100" {
		t.Errorf("a mutated to %s", a.String())
	}
}

func TestAmountSub(t *testing.T) {
	a := NewAmount(500)
	b := NewAmount(200)
	c, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.String() != "300" {
		t.Errorf("500 - 200 = %s", c.String())
	}

	// Underflow.
	_, err = b.Sub(a)
	if err == nil {
		t.Error("expected underflow error")
	}

	// Equal.
	z, err := a.Sub(a)
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsZero() {
		t.Errorf("500 - 500 = %s, want 0", z.String())
	}
}

func TestAmountComparisons(t *testing.T) {
	a := NewAmount(100)
	b := NewAmount(200)
	c := NewAmount(100)

	if !a.Equal(c) {
		t.Error("100 should equal 100")
	}
	if a.Equal(b) {
		t.Error("100 should not equal 200")
	}
	if !a.LT(b) {
		t.Error("100 should be < 200")
	}
	if !b.GT(a) {
		t.Error("200 should be > 100")
	}
	if a.GT(c) {
		t.Error("100 should not be > 100")
	}
}

func TestAmountJSON(t *testing.T) {
	a := NewAmount(12345)

	// Marshal produces quoted string.
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"12345"` {
		t.Errorf("marshal = %s, want \"12345\"", string(data))
	}

	// Unmarshal from quoted string.
	var b Amount
	if err := json.Unmarshal([]byte(`"67890"`), &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "67890" {
		t.Errorf("unmarshal string = %s", b.String())
	}

	// Unmarshal from JSON number.
	var c Amount
	if err := json.Unmarshal([]byte(`42`), &c); err != nil {
		t.Fatal(err)
	}
	if c.String() != "42" {
		t.Errorf("unmarshal number = %s", c.String())
	}

	// Unmarshal negative string should fail.
	var d Amount
	if err := json.Unmarshal([]byte(`"-5"`), &d); err == nil {
		t.Error("expected error for negative JSON string")
	}

	// Zero value marshals correctly.
	var z Amount
	data, err = json.Marshal(z)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"0"` {
		t.Errorf("zero marshal = %s", string(data))
	}
}

func TestAmountJSONRoundTrip(t *testing.T) {
	type wrapper struct {
		Balance Amount `json:"balance"`
	}
	orig := wrapper{Balance: NewAmount(999)}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var decoded wrapper
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !orig.Balance.Equal(decoded.Balance) {
		t.Errorf("round-trip: got %s, want %s", decoded.Balance, orig.Balance)
	}
}
