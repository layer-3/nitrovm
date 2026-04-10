package core

import "testing"

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
