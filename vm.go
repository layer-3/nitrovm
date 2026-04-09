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

	// Sequential code IDs for WasmMsg::Instantiate compatibility.
	// CosmWasm contracts reference codes by uint64 sequence number.
	codeSeq   uint64
	codeBySeq map[uint64]cosmwasm.Checksum // seq -> checksum
	seqByCode map[string]uint64            // hex(checksum) -> seq

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
		codeBySeq:   make(map[uint64]cosmwasm.Checksum),
		seqByCode:   make(map[string]uint64),
		blockHeight: 1,
		blockTime:   uint64(time.Now().UnixNano()),
	}, nil
}

// StoreCode uploads WASM bytecode. Returns the code ID (checksum) and gas used.
// If sender and nonce are non-nil, validates and increments the sender's nonce.
func (n *NitroVM) StoreCode(code []byte, sender *Address, nonce *uint64) ([]byte, uint64, error) {
	if nonce != nil && sender != nil {
		if expected := n.GetNonce(*sender); *nonce != expected {
			return nil, 0, fmt.Errorf("%w: got %d, expected %d", ErrInvalidNonce, *nonce, expected)
		}
	}

	// wasmvm charges 420_000 gas per byte for compilation
	gasLimit := uint64(len(code)) * GasCostStoreCodePerByte
	checksum, gasUsed, err := n.vm.StoreCode(code, gasLimit)
	if err != nil {
		return nil, 0, fmt.Errorf("store code: %w", err)
	}

	n.mu.Lock()
	hexID := hex.EncodeToString(checksum)
	n.codes[hexID] = checksum
	if _, exists := n.seqByCode[hexID]; !exists {
		n.codeSeq++
		n.codeBySeq[n.codeSeq] = checksum
		n.seqByCode[hexID] = n.codeSeq
	}
	if sender != nil && nonce != nil {
		n.getOrCreateAccount(*sender).Nonce++
	}
	n.mu.Unlock()

	return []byte(checksum), gasUsed, nil
}

// Instantiate creates a new contract instance from a stored code ID.
// If nonce is non-nil, it must match the sender's current nonce.
func (n *NitroVM) Instantiate(
	codeID []byte,
	sender Address,
	msg []byte,
	label string,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	nonce *uint64,
) (*InstantiateResult, error) {
	if nonce != nil {
		if expected := n.GetNonce(sender); *nonce != expected {
			return nil, fmt.Errorf("%w: got %d, expected %d", ErrInvalidNonce, *nonce, expected)
		}
	}
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

	// Increment sender nonce on successful instantiation.
	n.mu.Lock()
	n.getOrCreateAccount(sender).Nonce++
	n.mu.Unlock()

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
// If nonce is non-nil, it must match the sender's current nonce.
func (n *NitroVM) Execute(
	contract, sender Address,
	msg []byte,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	nonce *uint64,
) (*ExecuteResult, error) {
	if nonce != nil {
		if expected := n.GetNonce(sender); *nonce != expected {
			return nil, fmt.Errorf("%w: got %d, expected %d", ErrInvalidNonce, *nonce, expected)
		}
	}
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

// GetCodeSeq returns the sequential code ID for a given hex checksum.
func (n *NitroVM) GetCodeSeq(hexCodeID string) (uint64, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	seq, ok := n.seqByCode[hexCodeID]
	return seq, ok
}

// SetCodeSeq restores the sequential code ID counter.
func (n *NitroVM) SetCodeSeq(seq uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.codeSeq = seq
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

		case msg.Wasm != nil && msg.Wasm.Instantiate != nil:
			imsg := msg.Wasm.Instantiate
			n.mu.RLock()
			checksum, ok := n.codeBySeq[imsg.CodeID]
			n.mu.RUnlock()
			if !ok {
				return events, fmt.Errorf("dispatch wasm instantiate: code_id %d not found", imsg.CodeID)
			}
			res, err := n.Instantiate([]byte(checksum), contractAddr, imsg.Msg, imsg.Label, toCoins(imsg.Funds), gasLimit, nil)
			if err != nil {
				return events, fmt.Errorf("dispatch wasm instantiate: %w", err)
			}
			events = append(events, wasmvmtypes.Event{
				Type: "instantiate",
				Attributes: wasmvmtypes.Array[wasmvmtypes.EventAttribute]{
					{Key: "_contract_address", Value: res.ContractAddress.Hex()},
					{Key: "code_id", Value: fmt.Sprintf("%d", imsg.CodeID)},
				},
			})
			events = append(events, res.Events...)

		default:
			return events, fmt.Errorf("%w: only bank.send, wasm.execute, and wasm.instantiate are supported", ErrUnsupportedMsg)
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

// DeductGasFee deducts gas_used * gas_price from the sender's balance.
// Returns ErrInsufficientFunds if the sender cannot afford the fee.
func (n *NitroVM) DeductGasFee(sender Address, gasUsed, gasPrice uint64) error {
	if gasPrice == 0 {
		return nil
	}
	fee := NewAmount(gasUsed).Mul(NewAmount(gasPrice))
	n.mu.Lock()
	defer n.mu.Unlock()
	acct := n.getOrCreateAccount(sender)
	newBal, err := acct.Balance.Sub(fee)
	if err != nil {
		return ErrInsufficientFunds
	}
	acct.Balance = newBal
	return nil
}

// ChainID returns the configured chain identifier.
func (n *NitroVM) ChainID() string { return n.chainID }

// VMSnapshot holds a deep copy of mutable VM state for simulate/rollback.
type VMSnapshot struct {
	accounts      map[Address]*Account
	contracts     map[Address]*contractMeta
	codes         map[string]cosmwasm.Checksum
	codeSeq       uint64
	codeBySeq     map[uint64]cosmwasm.Checksum
	seqByCode     map[string]uint64
	instanceCount uint64
	blockHeight   uint64
	blockTime     uint64
}

// Snapshot captures the current mutable state. Caller must hold n.mu.
func (n *NitroVM) Snapshot() VMSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()

	accts := make(map[Address]*Account, len(n.accounts))
	for k, v := range n.accounts {
		cp := *v
		cp.Balance = v.Balance // Amount wraps *big.Int, copy the struct
		accts[k] = &cp
	}
	contracts := make(map[Address]*contractMeta, len(n.contracts))
	for k, v := range n.contracts {
		cp := *v
		contracts[k] = &cp
	}
	codes := make(map[string]cosmwasm.Checksum, len(n.codes))
	for k, v := range n.codes {
		codes[k] = v
	}
	codeBySeq := make(map[uint64]cosmwasm.Checksum, len(n.codeBySeq))
	for k, v := range n.codeBySeq {
		codeBySeq[k] = v
	}
	seqByCode := make(map[string]uint64, len(n.seqByCode))
	for k, v := range n.seqByCode {
		seqByCode[k] = v
	}

	return VMSnapshot{
		accounts:      accts,
		contracts:     contracts,
		codes:         codes,
		codeSeq:       n.codeSeq,
		codeBySeq:     codeBySeq,
		seqByCode:     seqByCode,
		instanceCount: n.instanceCount,
		blockHeight:   n.blockHeight,
		blockTime:     n.blockTime,
	}
}

// Restore replaces mutable state from a snapshot.
func (n *NitroVM) Restore(snap VMSnapshot) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.accounts = snap.accounts
	n.contracts = snap.contracts
	n.codes = snap.codes
	n.codeSeq = snap.codeSeq
	n.codeBySeq = snap.codeBySeq
	n.seqByCode = snap.seqByCode
	n.instanceCount = snap.instanceCount
	n.blockHeight = snap.blockHeight
	n.blockTime = snap.blockTime
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
