package core

// Account represents an on-chain account (EOA or contract).
type Account struct {
	Address  Address
	Balance  Amount
	Nonce    uint64
	CodeHash []byte // nil for EOAs
}
