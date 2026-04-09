package nitrovm

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	cosmwasm "github.com/CosmWasm/wasmvm/v2"
	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"
)

type contractMeta struct {
	Checksum cosmwasm.Checksum
	Label    string
	Creator  Address
}

// NitroVM is a WebAssembly runtime for executing smart contracts.
type NitroVM struct {
	vm      *cosmwasm.VM
	storage StorageAdapter
	api     wasmvmtypes.GoAPI
	chainID string

	accounts  map[Address]*Account
	contracts map[Address]*contractMeta
	codes     map[string]cosmwasm.Checksum // hex(checksum) -> checksum

	blockHeight uint64
	blockTime   uint64 // unix nanoseconds

	instanceCount uint64
	mu            sync.RWMutex
}

// New creates a new NitroVM instance.
func New(cfg Config, storage StorageAdapter) (*NitroVM, error) {
	vm, err := cosmwasm.NewVM(
		cfg.DataDir,
		[]string{"iterator", "staking", "stargate"},
		cfg.MemoryLimit,
		cfg.PrintDebug,
		cfg.CacheSize,
	)
	if err != nil {
		return nil, fmt.Errorf("create wasmvm: %w", err)
	}

	return &NitroVM{
		vm:          vm,
		storage:     storage,
		api:         newGoAPI(),
		chainID:     cfg.ChainID,
		accounts:    make(map[Address]*Account),
		contracts:   make(map[Address]*contractMeta),
		codes:       make(map[string]cosmwasm.Checksum),
		blockHeight: 1,
		blockTime:   uint64(time.Now().UnixNano()),
	}, nil
}

// StoreCode uploads WASM bytecode. Returns the code ID (checksum).
func (n *NitroVM) StoreCode(code []byte) ([]byte, error) {
	// wasmvm charges 420_000 gas per byte for compilation
	gasLimit := uint64(len(code)) * 420_000
	checksum, _, err := n.vm.StoreCode(code, gasLimit)
	if err != nil {
		return nil, fmt.Errorf("store code: %w", err)
	}

	n.mu.Lock()
	n.codes[hex.EncodeToString(checksum)] = checksum
	n.mu.Unlock()

	return []byte(checksum), nil
}

// Instantiate creates a new contract instance from a stored code ID.
func (n *NitroVM) Instantiate(
	codeID []byte,
	sender Address,
	msg []byte,
	label string,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
) (Address, uint64, error) {
	n.mu.Lock()
	checksum, ok := n.codes[hex.EncodeToString(codeID)]
	if !ok {
		n.mu.Unlock()
		return Address{}, 0, ErrCodeNotFound
	}

	n.instanceCount++
	addr := CreateContractAddress(sender, codeID, n.instanceCount)
	n.contracts[addr] = &contractMeta{Checksum: checksum, Label: label, Creator: sender}

	if err := n.transferFunds(sender, addr, funds); err != nil {
		delete(n.contracts, addr)
		n.instanceCount--
		n.mu.Unlock()
		return Address{}, 0, err
	}
	n.mu.Unlock()

	env := n.makeEnv(addr)
	info := wasmvmtypes.MessageInfo{Sender: sender.Hex(), Funds: funds}
	store := newContractKVStore(addr, n.storage)
	gasMeter := NewGasMeter(gasLimit)
	querier := newChainQuerier(n)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}

	result, gasUsed, err := n.vm.Instantiate(checksum, env, info, msg, store, n.api, querier, gasMeter, gasLimit, deserCost)
	if err != nil {
		return Address{}, gasUsed, fmt.Errorf("instantiate: %w", err)
	}
	if result.Err != "" {
		return Address{}, gasUsed, fmt.Errorf("%w: %s", ErrContractError, result.Err)
	}

	return addr, gasUsed, nil
}

// Execute calls a contract function.
func (n *NitroVM) Execute(
	contract, sender Address,
	msg []byte,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
) ([]byte, uint64, error) {
	n.mu.RLock()
	ci, ok := n.contracts[contract]
	if !ok {
		n.mu.RUnlock()
		return nil, 0, ErrContractNotFound
	}
	checksum := ci.Checksum
	n.mu.RUnlock()

	if len(funds) > 0 {
		n.mu.Lock()
		if err := n.transferFunds(sender, contract, funds); err != nil {
			n.mu.Unlock()
			return nil, 0, err
		}
		n.mu.Unlock()
	}

	env := n.makeEnv(contract)
	info := wasmvmtypes.MessageInfo{Sender: sender.Hex(), Funds: funds}
	store := newContractKVStore(contract, n.storage)
	gasMeter := NewGasMeter(gasLimit)
	querier := newChainQuerier(n)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}

	result, gasUsed, err := n.vm.Execute(checksum, env, info, msg, store, n.api, querier, gasMeter, gasLimit, deserCost)
	if err != nil {
		return nil, gasUsed, fmt.Errorf("execute: %w", err)
	}
	if result.Err != "" {
		return nil, gasUsed, fmt.Errorf("%w: %s", ErrContractError, result.Err)
	}

	var data []byte
	if result.Ok != nil {
		data = result.Ok.Data
	}
	return data, gasUsed, nil
}

// Query performs a read-only contract query.
func (n *NitroVM) Query(contract Address, msg []byte, gasLimit uint64) ([]byte, uint64, error) {
	n.mu.RLock()
	ci, ok := n.contracts[contract]
	if !ok {
		n.mu.RUnlock()
		return nil, 0, ErrContractNotFound
	}
	checksum := ci.Checksum
	n.mu.RUnlock()

	env := n.makeEnv(contract)
	store := newContractKVStore(contract, n.storage)
	gasMeter := NewGasMeter(gasLimit)
	querier := newChainQuerier(n)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}

	result, gasUsed, err := n.vm.Query(checksum, env, msg, store, n.api, querier, gasMeter, gasLimit, deserCost)
	if err != nil {
		return nil, gasUsed, fmt.Errorf("query: %w", err)
	}
	if result.Err != "" {
		return nil, gasUsed, fmt.Errorf("%w: %s", ErrContractError, result.Err)
	}

	return result.Ok, gasUsed, nil
}

// GetBalance returns the YELLOW token balance for an address.
func (n *NitroVM) GetBalance(addr Address) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if acct, ok := n.accounts[addr]; ok {
		return acct.Balance
	}
	return 0
}

// SetBalance sets the YELLOW token balance for an address.
func (n *NitroVM) SetBalance(addr Address, balance uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.getOrCreateAccount(addr).Balance = balance
}

// SetBlockInfo updates the current block context.
func (n *NitroVM) SetBlockInfo(height, timeNanos uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blockHeight = height
	n.blockTime = timeNanos
}

// RegisterContract adds a contract to the internal registry without executing
// the instantiate entry point. Used for restoring state from persistence.
func (n *NitroVM) RegisterContract(addr, creator Address, checksum []byte, label string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.contracts[addr] = &contractMeta{Checksum: checksum, Label: label, Creator: creator}
}

// SetInstanceCount restores the instance counter for deterministic address generation.
func (n *NitroVM) SetInstanceCount(count uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.instanceCount = count
}

// Close releases all VM and storage resources.
func (n *NitroVM) Close() {
	n.vm.Cleanup()
	n.storage.Close()
}

func (n *NitroVM) makeEnv(contract Address) wasmvmtypes.Env {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return wasmvmtypes.Env{
		Block: wasmvmtypes.BlockInfo{
			Height:  n.blockHeight,
			Time:    wasmvmtypes.Uint64(n.blockTime),
			ChainID: n.chainID,
		},
		Contract: wasmvmtypes.ContractInfo{
			Address: contract.Hex(),
		},
	}
}

// transferFunds moves YELLOW tokens between accounts. Caller must hold n.mu write lock.
func (n *NitroVM) transferFunds(from, to Address, funds []wasmvmtypes.Coin) error {
	for _, coin := range funds {
		if coin.Denom != "YELLOW" {
			continue
		}
		var amount uint64
		if _, err := fmt.Sscanf(coin.Amount, "%d", &amount); err != nil || amount == 0 {
			continue
		}
		fromAcct := n.getOrCreateAccount(from)
		if fromAcct.Balance < amount {
			return ErrInsufficientFunds
		}
		toAcct := n.getOrCreateAccount(to)
		fromAcct.Balance -= amount
		toAcct.Balance += amount
	}
	return nil
}

func (n *NitroVM) getOrCreateAccount(addr Address) *Account {
	acct, ok := n.accounts[addr]
	if !ok {
		acct = &Account{Address: addr}
		n.accounts[addr] = acct
	}
	return acct
}
