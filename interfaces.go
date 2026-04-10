package nitrovm

import (
	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm/core"
)

// VM defines the public contract runtime interface.
type VM interface {
	StoreCode(code []byte, sender *core.Address, nonce *uint64) ([]byte, uint64, error)
	Instantiate(codeID []byte, sender core.Address, msg []byte, label string, funds []wasmvmtypes.Coin, gasLimit uint64, nonce *uint64) (*core.InstantiateResult, error)
	Execute(contract, sender core.Address, msg []byte, funds []wasmvmtypes.Coin, gasLimit uint64, nonce *uint64) (*core.ExecuteResult, error)
	Query(contract core.Address, msg []byte, gasLimit uint64) ([]byte, uint64, error)
	GetBalance(addr core.Address) core.Amount
	SetBalance(addr core.Address, balance core.Amount)
	GetNonce(addr core.Address) uint64
	SetNonce(addr core.Address, nonce uint64)
	SetBlockInfo(height, timeNanos uint64)
	TickOp()
	GetOpSeq() uint64
	RegisterContract(addr, creator core.Address, checksum []byte, label string)
	SetInstanceCount(count uint64)
	ListCodes() []string
	ListContracts() []core.ContractInfo
	GetContractInfo(addr core.Address) *core.ContractInfo
	GetCodeSeq(hexCodeID string) (uint64, bool)
	SetCodeSeq(seq uint64)
	DeductGasFee(sender core.Address, gasUsed, gasPrice uint64) error
	ChainID() string
	Snapshot() any
	Restore(snap any)
	Close()
}
