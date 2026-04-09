package nitrovm

// Account represents an on-chain account (EOA or contract).
type Account struct {
	Address  Address
	Balance  uint64
	Nonce    uint64
	CodeHash []byte // nil for EOAs
}
