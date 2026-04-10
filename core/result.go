package core

import wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

// ExecuteResult holds the full response from a contract execution.
type ExecuteResult struct {
	Data       []byte
	Attributes []wasmvmtypes.EventAttribute
	Events     []wasmvmtypes.Event
	GasUsed    uint64
}

// ContractInfo holds metadata about a contract instance.
type ContractInfo struct {
	Address string `json:"address"`
	CodeID  string `json:"code_id"`
	Label   string `json:"label"`
	Creator string `json:"creator"`
}

// InstantiateResult holds the full response from a contract instantiation.
type InstantiateResult struct {
	ContractAddress Address
	Data            []byte
	Attributes      []wasmvmtypes.EventAttribute
	Events          []wasmvmtypes.Event
	GasUsed         uint64
}
