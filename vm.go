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
) (*InstantiateResult, error) {
	n.mu.Lock()
	checksum, ok := n.codes[hex.EncodeToString(codeID)]
	if !ok {
		n.mu.Unlock()
		return nil, ErrCodeNotFound
	}

	n.instanceCount++
	addr := CreateContractAddress(sender, codeID, n.instanceCount)
	n.contracts[addr] = &contractMeta{Checksum: checksum, Label: label, Creator: sender}

	if err := n.transferFunds(sender, addr, funds); err != nil {
		delete(n.contracts, addr)
		n.instanceCount--
		n.mu.Unlock()
		return nil, err
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
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	if result.Err != "" {
		return nil, fmt.Errorf("%w: %s", ErrContractError, result.Err)
	}

	res := &InstantiateResult{
		ContractAddress: addr,
		GasUsed:         gasUsed,
	}
	if result.Ok != nil {
		res.Data = result.Ok.Data
		res.Attributes = result.Ok.Attributes
		res.Events = result.Ok.Events
	}
	return res, nil
}

// Execute calls a contract function.
func (n *NitroVM) Execute(
	contract, sender Address,
	msg []byte,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
) (*ExecuteResult, error) {
	return n.executeInternal(contract, sender, msg, funds, gasLimit, 0)
}

func (n *NitroVM) executeInternal(
	contract, sender Address,
	msg []byte,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	depth int,
) (*ExecuteResult, error) {
	n.mu.RLock()
	ci, ok := n.contracts[contract]
	if !ok {
		n.mu.RUnlock()
		return nil, ErrContractNotFound
	}
	checksum := ci.Checksum
	n.mu.RUnlock()

	if len(funds) > 0 {
		n.mu.Lock()
		if err := n.transferFunds(sender, contract, funds); err != nil {
			n.mu.Unlock()
			return nil, err
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
		return nil, fmt.Errorf("execute: %w", err)
	}
	if result.Err != "" {
		return nil, fmt.Errorf("%w: %s", ErrContractError, result.Err)
	}

	// Track sender nonce on successful execution.
	n.mu.Lock()
	n.getOrCreateAccount(sender).Nonce++
	n.mu.Unlock()

	res := &ExecuteResult{GasUsed: gasUsed}
	if result.Ok != nil {
		res.Data = result.Ok.Data
		res.Attributes = result.Ok.Attributes
		res.Events = result.Ok.Events

		// Dispatch sub-messages.
		if len(result.Ok.Messages) > 0 {
			subEvents, err := n.dispatchMessages(contract, result.Ok.Messages, gasLimit, depth)
			if err != nil {
				return nil, fmt.Errorf("dispatch: %w", err)
			}
			res.Events = append(res.Events, subEvents...)
		}
	}
	return res, nil
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
func (n *NitroVM) GetBalance(addr Address) Amount {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if acct, ok := n.accounts[addr]; ok {
		return acct.Balance
	}
	return Amount{}
}

// SetBalance sets the YELLOW token balance for an address.
func (n *NitroVM) SetBalance(addr Address, balance Amount) {
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

// TickOp advances the operation counter and updates the timestamp.
// Called by the server after each state-changing operation.
func (n *NitroVM) TickOp() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blockHeight++
	n.blockTime = uint64(time.Now().UnixNano())
}

// GetOpSeq returns the current operation sequence number.
func (n *NitroVM) GetOpSeq() uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.blockHeight
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

// ListCodes returns all stored code IDs as hex strings.
func (n *NitroVM) ListCodes() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.codes))
	for hexID := range n.codes {
		out = append(out, hexID)
	}
	return out
}

// ListContracts returns metadata for all registered contracts.
func (n *NitroVM) ListContracts() []ContractInfo {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]ContractInfo, 0, len(n.contracts))
	for addr, meta := range n.contracts {
		out = append(out, ContractInfo{
			Address: addr.Hex(),
			CodeID:  hex.EncodeToString(meta.Checksum),
			Label:   meta.Label,
			Creator: meta.Creator.Hex(),
		})
	}
	return out
}

// GetContractInfo returns metadata for a single contract, or nil if not found.
func (n *NitroVM) GetContractInfo(addr Address) *ContractInfo {
	n.mu.RLock()
	defer n.mu.RUnlock()
	meta, ok := n.contracts[addr]
	if !ok {
		return nil
	}
	return &ContractInfo{
		Address: addr.Hex(),
		CodeID:  hex.EncodeToString(meta.Checksum),
		Label:   meta.Label,
		Creator: meta.Creator.Hex(),
	}
}

// GetNonce returns the nonce for an address.
func (n *NitroVM) GetNonce(addr Address) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if acct, ok := n.accounts[addr]; ok {
		return acct.Nonce
	}
	return 0
}

// SetNonce restores the nonce for an address.
func (n *NitroVM) SetNonce(addr Address, nonce uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.getOrCreateAccount(addr).Nonce = nonce
}

// maxDispatchDepth limits recursive sub-message dispatch to prevent infinite loops.
const maxDispatchDepth = 10

// dispatchMessages processes sub-messages returned by contract execution.
// Supports BankMsg::Send and WasmMsg::Execute. Returns collected events.
func (n *NitroVM) dispatchMessages(
	contractAddr Address,
	msgs []wasmvmtypes.SubMsg,
	gasLimit uint64,
	depth int,
) ([]wasmvmtypes.Event, error) {
	if depth >= maxDispatchDepth {
		return nil, ErrMaxDispatchDepth
	}

	var events []wasmvmtypes.Event
	for _, sub := range msgs {
		msg := sub.Msg

		switch {
		case msg.Bank != nil && msg.Bank.Send != nil:
			send := msg.Bank.Send
			to, err := HexToAddress(send.ToAddress)
			if err != nil {
				return events, fmt.Errorf("dispatch bank send: bad address: %w", err)
			}
			n.mu.Lock()
			err = n.transferFunds(contractAddr, to, toCoins(send.Amount))
			n.mu.Unlock()
			if err != nil {
				return events, fmt.Errorf("dispatch bank send: %w", err)
			}
			events = append(events, wasmvmtypes.Event{
				Type: "transfer",
				Attributes: wasmvmtypes.Array[wasmvmtypes.EventAttribute]{
					{Key: "sender", Value: contractAddr.Hex()},
					{Key: "recipient", Value: send.ToAddress},
					{Key: "amount", Value: coinString(send.Amount)},
				},
			})

		case msg.Wasm != nil && msg.Wasm.Execute != nil:
			wmsg := msg.Wasm.Execute
			target, err := HexToAddress(wmsg.ContractAddr)
			if err != nil {
				return events, fmt.Errorf("dispatch wasm execute: bad address: %w", err)
			}
			subRes, err := n.executeInternal(target, contractAddr, wmsg.Msg, toCoins(wmsg.Funds), gasLimit, depth+1)
			if err != nil {
				return events, fmt.Errorf("dispatch wasm execute: %w", err)
			}
			events = append(events, subRes.Events...)

		default:
			return events, fmt.Errorf("%w: only bank.send and wasm.execute are supported", ErrUnsupportedMsg)
		}
	}
	return events, nil
}

// toCoins converts wasmvmtypes.Array[Coin] to []Coin.
func toCoins(arr wasmvmtypes.Array[wasmvmtypes.Coin]) []wasmvmtypes.Coin {
	return []wasmvmtypes.Coin(arr)
}

// coinString produces a human-readable summary of coins for event attributes.
func coinString(coins wasmvmtypes.Array[wasmvmtypes.Coin]) string {
	s := ""
	for i, c := range coins {
		if i > 0 {
			s += ","
		}
		s += c.Amount + c.Denom
	}
	return s
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
		amt, err := NewAmountFromString(coin.Amount)
		if err != nil || amt.IsZero() {
			continue
		}
		fromAcct := n.getOrCreateAccount(from)
		newFromBal, err := fromAcct.Balance.Sub(amt)
		if err != nil {
			return ErrInsufficientFunds
		}
		toAcct := n.getOrCreateAccount(to)
		fromAcct.Balance = newFromBal
		toAcct.Balance = toAcct.Balance.Add(amt)
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
