package runtime

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	cosmwasm "github.com/CosmWasm/wasmvm/v2"
	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm"
	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage"
)

// Compile-time check: NitroVM implements nitrovm.VM.
var _ nitrovm.VM = (*NitroVM)(nil)

type contractMeta struct {
	Checksum cosmwasm.Checksum
	Label    string
	Creator  core.Address
}

// NitroVM is a WebAssembly runtime for executing smart contracts.
type NitroVM struct {
	vm      *cosmwasm.VM
	storage storage.StorageAdapter
	api     wasmvmtypes.GoAPI
	chainID string

	accounts  map[core.Address]*core.Account
	contracts map[core.Address]*contractMeta
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

	// replyFn invokes the reply entry point on a contract. Defaults to
	// calling wasmvm.VM.Reply; overridable in tests.
	replyFn func(
		checksum cosmwasm.Checksum,
		env wasmvmtypes.Env,
		reply wasmvmtypes.Reply,
		store wasmvmtypes.KVStore,
		gasLimit uint64,
	) (*wasmvmtypes.ContractResult, uint64, error)
}

// New creates a new NitroVM instance.
func New(cfg core.Config, store storage.StorageAdapter) (*NitroVM, error) {
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

	n := &NitroVM{
		vm:          vm,
		storage:     store,
		api:         newGoAPI(),
		chainID:     cfg.ChainID,
		accounts:    make(map[core.Address]*core.Account),
		contracts:   make(map[core.Address]*contractMeta),
		codes:       make(map[string]cosmwasm.Checksum),
		codeBySeq:   make(map[uint64]cosmwasm.Checksum),
		seqByCode:   make(map[string]uint64),
		blockHeight: 1,
		blockTime:   uint64(time.Now().UnixNano()),
	}
	n.replyFn = n.defaultReply
	return n, nil
}

// validateNonce checks that the provided nonce matches the sender's current nonce.
// Returns nil if nonce is nil (skip check).
func (n *NitroVM) validateNonce(sender core.Address, nonce *uint64) error {
	if nonce == nil {
		return nil
	}
	if expected := n.GetNonce(sender); *nonce != expected {
		return fmt.Errorf("%w: got %d, expected %d", core.ErrInvalidNonce, *nonce, expected)
	}
	return nil
}

// StoreCode uploads WASM bytecode. Returns the code ID (checksum) and gas used.
// If sender and nonce are non-nil, validates and increments the sender's nonce.
func (n *NitroVM) StoreCode(code []byte, sender *core.Address, nonce *uint64) ([]byte, uint64, error) {
	if len(code) > MaxCodeSize {
		return nil, 0, fmt.Errorf("%w: %d bytes exceeds %d byte limit", core.ErrCodeTooLarge, len(code), MaxCodeSize)
	}

	if sender != nil {
		if err := n.validateNonce(*sender, nonce); err != nil {
			return nil, 0, err
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
	sender core.Address,
	msg []byte,
	label string,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	nonce *uint64,
) (*core.InstantiateResult, error) {
	if err := n.validateNonce(sender, nonce); err != nil {
		return nil, err
	}
	return n.instantiateInternal(codeID, sender, msg, label, funds, gasLimit, 0)
}

func (n *NitroVM) instantiateInternal(
	codeID []byte,
	sender core.Address,
	msg []byte,
	label string,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	depth int,
) (*core.InstantiateResult, error) {
	n.mu.Lock()
	checksum, ok := n.codes[hex.EncodeToString(codeID)]
	if !ok {
		n.mu.Unlock()
		return nil, core.ErrCodeNotFound
	}

	n.instanceCount++
	addr := core.CreateContractAddress(sender, codeID, n.instanceCount)
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
	querier := newChainQuerier(n, gasMeter)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}

	result, gasUsed, err := n.vm.Instantiate(checksum, env, info, msg, store, n.api, querier, gasMeter, gasLimit, deserCost)
	if store.err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrStorageError, store.err)
	}
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	if result.Err != "" {
		return nil, fmt.Errorf("%w: %s", core.ErrContractError, result.Err)
	}

	// Increment sender nonce on successful instantiation.
	n.mu.Lock()
	n.getOrCreateAccount(sender).Nonce++
	n.mu.Unlock()

	res := &core.InstantiateResult{
		ContractAddress: addr,
		GasUsed:         gasUsed,
	}
	if result.Ok != nil {
		res.Data = result.Ok.Data
		res.Attributes = result.Ok.Attributes
		res.Events = result.Ok.Events

		// Dispatch sub-messages returned by the instantiate handler.
		if len(result.Ok.Messages) > 0 {
			dr, err := n.dispatchMessages(addr, result.Ok.Messages, gasLimit, depth)
			if err != nil {
				return nil, fmt.Errorf("dispatch: %w", err)
			}
			res.Events = append(res.Events, dr.Events...)
			if dr.DataOverride != nil {
				res.Data = *dr.DataOverride
			}
		}
	}
	return res, nil
}

// Execute calls a contract function.
// If nonce is non-nil, it must match the sender's current nonce.
func (n *NitroVM) Execute(
	contract, sender core.Address,
	msg []byte,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	nonce *uint64,
) (*core.ExecuteResult, error) {
	if err := n.validateNonce(sender, nonce); err != nil {
		return nil, err
	}
	return n.executeInternal(contract, sender, msg, funds, gasLimit, 0)
}

func (n *NitroVM) executeInternal(
	contract, sender core.Address,
	msg []byte,
	funds []wasmvmtypes.Coin,
	gasLimit uint64,
	depth int,
) (*core.ExecuteResult, error) {
	n.mu.RLock()
	ci, ok := n.contracts[contract]
	if !ok {
		n.mu.RUnlock()
		return nil, core.ErrContractNotFound
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
	querier := newChainQuerier(n, gasMeter)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}

	result, gasUsed, err := n.vm.Execute(checksum, env, info, msg, store, n.api, querier, gasMeter, gasLimit, deserCost)
	if store.err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrStorageError, store.err)
	}
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	if result.Err != "" {
		return nil, fmt.Errorf("%w: %s", core.ErrContractError, result.Err)
	}

	// Track sender nonce on successful execution.
	n.mu.Lock()
	n.getOrCreateAccount(sender).Nonce++
	n.mu.Unlock()

	res := &core.ExecuteResult{GasUsed: gasUsed}
	if result.Ok != nil {
		res.Data = result.Ok.Data
		res.Attributes = result.Ok.Attributes
		res.Events = result.Ok.Events

		// Dispatch sub-messages.
		if len(result.Ok.Messages) > 0 {
			dr, err := n.dispatchMessages(contract, result.Ok.Messages, gasLimit, depth)
			if err != nil {
				return nil, fmt.Errorf("dispatch: %w", err)
			}
			res.Events = append(res.Events, dr.Events...)
			if dr.DataOverride != nil {
				res.Data = *dr.DataOverride
			}
		}
	}
	return res, nil
}

// Query performs a read-only contract query.
func (n *NitroVM) Query(contract core.Address, msg []byte, gasLimit uint64) ([]byte, uint64, error) {
	n.mu.RLock()
	ci, ok := n.contracts[contract]
	if !ok {
		n.mu.RUnlock()
		return nil, 0, core.ErrContractNotFound
	}
	checksum := ci.Checksum
	n.mu.RUnlock()

	env := n.makeEnv(contract)
	store := newContractKVStore(contract, n.storage)
	gasMeter := NewGasMeter(gasLimit)
	querier := newChainQuerier(n, gasMeter)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}

	result, gasUsed, err := n.vm.Query(checksum, env, msg, store, n.api, querier, gasMeter, gasLimit, deserCost)
	if store.err != nil {
		return nil, gasUsed, fmt.Errorf("%w: %v", core.ErrStorageError, store.err)
	}
	if err != nil {
		return nil, gasUsed, fmt.Errorf("query: %w", err)
	}
	if result.Err != "" {
		return nil, gasUsed, fmt.Errorf("%w: %s", core.ErrContractError, result.Err)
	}

	return result.Ok, gasUsed, nil
}

// GetBalance returns the YELLOW token balance for an address.
func (n *NitroVM) GetBalance(addr core.Address) core.Amount {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if acct, ok := n.accounts[addr]; ok {
		return acct.Balance
	}
	return core.Amount{}
}

// SetBalance sets the YELLOW token balance for an address.
func (n *NitroVM) SetBalance(addr core.Address, balance core.Amount) {
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
func (n *NitroVM) RegisterContract(addr, creator core.Address, checksum []byte, label string) {
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

// GetInstanceCount returns the current instance counter value.
func (n *NitroVM) GetInstanceCount() uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.instanceCount
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
func (n *NitroVM) ListContracts() []core.ContractInfo {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]core.ContractInfo, 0, len(n.contracts))
	for addr, meta := range n.contracts {
		out = append(out, core.ContractInfo{
			Address: addr.Hex(),
			CodeID:  hex.EncodeToString(meta.Checksum),
			Label:   meta.Label,
			Creator: meta.Creator.Hex(),
		})
	}
	return out
}

// GetContractInfo returns metadata for a single contract, or nil if not found.
func (n *NitroVM) GetContractInfo(addr core.Address) *core.ContractInfo {
	n.mu.RLock()
	defer n.mu.RUnlock()
	meta, ok := n.contracts[addr]
	if !ok {
		return nil
	}
	return &core.ContractInfo{
		Address: addr.Hex(),
		CodeID:  hex.EncodeToString(meta.Checksum),
		Label:   meta.Label,
		Creator: meta.Creator.Hex(),
	}
}

// GetNonce returns the nonce for an address.
func (n *NitroVM) GetNonce(addr core.Address) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if acct, ok := n.accounts[addr]; ok {
		return acct.Nonce
	}
	return 0
}

// SetNonce restores the nonce for an address.
func (n *NitroVM) SetNonce(addr core.Address, nonce uint64) {
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

// dispatchResult holds the output of sub-message dispatch.
type dispatchResult struct {
	Events       []wasmvmtypes.Event
	DataOverride *[]byte // non-nil if a reply handler set Data
}

// defaultReply invokes the reply entry point via wasmvm.
func (n *NitroVM) defaultReply(
	checksum cosmwasm.Checksum,
	env wasmvmtypes.Env,
	reply wasmvmtypes.Reply,
	store wasmvmtypes.KVStore,
	gasLimit uint64,
) (*wasmvmtypes.ContractResult, uint64, error) {
	gasMeter := NewGasMeter(gasLimit)
	querier := newChainQuerier(n, gasMeter)
	deserCost := wasmvmtypes.UFraction{Numerator: 1, Denominator: 1}
	return n.vm.Reply(checksum, env, reply, store, n.api, querier, gasMeter, gasLimit, deserCost)
}

// dispatchMessages processes sub-messages returned by contract execution.
// Handles ReplyOn semantics: invokes the calling contract's reply entry point
// based on the sub-message's ReplyOn mode and dispatch outcome.
func (n *NitroVM) dispatchMessages(
	contractAddr core.Address,
	msgs []wasmvmtypes.SubMsg,
	gasLimit uint64,
	depth int,
) (*dispatchResult, error) {
	if depth >= maxDispatchDepth {
		return nil, core.ErrMaxDispatchDepth
	}

	dr := &dispatchResult{}
	for i, sub := range msgs {
		canCatchError := sub.ReplyOn == wasmvmtypes.ReplyError || sub.ReplyOn == wasmvmtypes.ReplyAlways

		// Snapshot state before dispatch when the error may be caught.
		var vmSnap any
		var ts storage.TransactionalStore
		spName := fmt.Sprintf("reply_d%d_i%d", depth, i)
		if canCatchError {
			vmSnap = n.Snapshot()
			if t, ok := n.storage.(storage.TransactionalStore); ok {
				ts = t
				if err := ts.Savepoint(spName); err != nil {
					return nil, fmt.Errorf("savepoint: %w", err)
				}
			}
		}

		subEvents, subData, dispatchErr := n.dispatchSingleMessage(contractAddr, sub.Msg, gasLimit, depth)

		if dispatchErr != nil {
			if canCatchError {
				// Rollback the failed sub-message's state changes.
				n.Restore(vmSnap)
				if ts != nil {
					_ = ts.RollbackTo(spName)
				}

				// Invoke reply with error result.
				reply := wasmvmtypes.Reply{
					ID:      sub.ID,
					Payload: sub.Payload,
					Result:  wasmvmtypes.SubMsgResult{Err: dispatchErr.Error()},
				}
				replyEvents, replyData, err := n.invokeReply(contractAddr, reply, gasLimit, depth)
				if err != nil {
					return nil, err
				}
				dr.Events = append(dr.Events, replyEvents...)
				if replyData != nil {
					dr.DataOverride = replyData
				}
			} else {
				// ReplyNever or ReplySuccess: error aborts.
				return nil, dispatchErr
			}
		} else {
			// Success path.
			if canCatchError && ts != nil {
				_ = ts.ReleaseSavepoint(spName)
			}

			if sub.ReplyOn == wasmvmtypes.ReplySuccess || sub.ReplyOn == wasmvmtypes.ReplyAlways {
				reply := wasmvmtypes.Reply{
					ID:      sub.ID,
					Payload: sub.Payload,
					Result: wasmvmtypes.SubMsgResult{
						Ok: &wasmvmtypes.SubMsgResponse{
							Events: subEvents,
							Data:   subData,
						},
					},
				}
				replyEvents, replyData, err := n.invokeReply(contractAddr, reply, gasLimit, depth)
				if err != nil {
					return nil, err
				}
				dr.Events = append(dr.Events, subEvents...)
				dr.Events = append(dr.Events, replyEvents...)
				if replyData != nil {
					dr.DataOverride = replyData
				}
			} else {
				// ReplyNever or ReplyError on success: just collect events.
				dr.Events = append(dr.Events, subEvents...)
			}
		}
	}
	return dr, nil
}

// dispatchSingleMessage dispatches one sub-message and returns its events,
// response data, and any error.
func (n *NitroVM) dispatchSingleMessage(
	contractAddr core.Address,
	msg wasmvmtypes.CosmosMsg,
	gasLimit uint64,
	depth int,
) (events []wasmvmtypes.Event, data []byte, err error) {
	switch {
	case msg.Bank != nil && msg.Bank.Send != nil:
		send := msg.Bank.Send
		to, addrErr := core.HexToAddress(send.ToAddress)
		if addrErr != nil {
			return nil, nil, fmt.Errorf("dispatch bank send: bad address: %w", addrErr)
		}
		n.mu.Lock()
		txErr := n.transferFunds(contractAddr, to, toCoins(send.Amount))
		n.mu.Unlock()
		if txErr != nil {
			return nil, nil, fmt.Errorf("dispatch bank send: %w", txErr)
		}
		events = append(events, wasmvmtypes.Event{
			Type: "transfer",
			Attributes: wasmvmtypes.Array[wasmvmtypes.EventAttribute]{
				{Key: "sender", Value: contractAddr.Hex()},
				{Key: "recipient", Value: send.ToAddress},
				{Key: "amount", Value: coinString(send.Amount)},
			},
		})
		return events, nil, nil

	case msg.Wasm != nil && msg.Wasm.Execute != nil:
		wmsg := msg.Wasm.Execute
		target, addrErr := core.HexToAddress(wmsg.ContractAddr)
		if addrErr != nil {
			return nil, nil, fmt.Errorf("dispatch wasm execute: bad address: %w", addrErr)
		}
		subRes, execErr := n.executeInternal(target, contractAddr, wmsg.Msg, toCoins(wmsg.Funds), gasLimit, depth+1)
		if execErr != nil {
			return nil, nil, fmt.Errorf("dispatch wasm execute: %w", execErr)
		}
		if len(subRes.Attributes) > 0 {
			events = append(events, wasmvmtypes.Event{
				Type:       "wasm",
				Attributes: subRes.Attributes,
			})
		}
		events = append(events, subRes.Events...)
		return events, subRes.Data, nil

	case msg.Wasm != nil && msg.Wasm.Instantiate != nil:
		imsg := msg.Wasm.Instantiate
		n.mu.RLock()
		checksum, ok := n.codeBySeq[imsg.CodeID]
		n.mu.RUnlock()
		if !ok {
			return nil, nil, fmt.Errorf("dispatch wasm instantiate: code_id %d not found", imsg.CodeID)
		}
		res, instErr := n.instantiateInternal([]byte(checksum), contractAddr, imsg.Msg, imsg.Label, toCoins(imsg.Funds), gasLimit, depth)
		if instErr != nil {
			return nil, nil, fmt.Errorf("dispatch wasm instantiate: %w", instErr)
		}
		events = append(events, wasmvmtypes.Event{
			Type: "instantiate",
			Attributes: wasmvmtypes.Array[wasmvmtypes.EventAttribute]{
				{Key: "_contract_address", Value: res.ContractAddress.Hex()},
				{Key: "code_id", Value: fmt.Sprintf("%d", imsg.CodeID)},
			},
		})
		if len(res.Attributes) > 0 {
			events = append(events, wasmvmtypes.Event{
				Type:       "wasm",
				Attributes: res.Attributes,
			})
		}
		events = append(events, res.Events...)
		return events, res.Data, nil

	default:
		return nil, nil, fmt.Errorf("%w: only bank.send, wasm.execute, and wasm.instantiate are supported", core.ErrUnsupportedMsg)
	}
}

// invokeReply calls the reply entry point on a contract and recursively
// dispatches any sub-messages the reply handler returns.
func (n *NitroVM) invokeReply(
	contractAddr core.Address,
	reply wasmvmtypes.Reply,
	gasLimit uint64,
	depth int,
) (events []wasmvmtypes.Event, dataOverride *[]byte, err error) {
	n.mu.RLock()
	ci, ok := n.contracts[contractAddr]
	if !ok {
		n.mu.RUnlock()
		return nil, nil, core.ErrContractNotFound
	}
	checksum := ci.Checksum
	n.mu.RUnlock()

	env := n.makeEnv(contractAddr)
	store := newContractKVStore(contractAddr, n.storage)

	result, _, replyErr := n.replyFn(checksum, env, reply, store, gasLimit)
	if replyErr != nil {
		return nil, nil, fmt.Errorf("%w: %v", core.ErrReplyFailed, replyErr)
	}
	if result.Err != "" {
		return nil, nil, fmt.Errorf("%w: %s", core.ErrReplyFailed, result.Err)
	}

	if result.Ok != nil {
		// Wrap reply attributes into a "reply" event.
		if len(result.Ok.Attributes) > 0 {
			events = append(events, wasmvmtypes.Event{
				Type:       "reply",
				Attributes: result.Ok.Attributes,
			})
		}
		events = append(events, result.Ok.Events...)

		if result.Ok.Data != nil {
			dataOverride = &result.Ok.Data
		}

		// Recursively dispatch sub-messages returned by the reply handler.
		if len(result.Ok.Messages) > 0 {
			dr, dispatchErr := n.dispatchMessages(contractAddr, result.Ok.Messages, gasLimit, depth+1)
			if dispatchErr != nil {
				return nil, nil, fmt.Errorf("reply dispatch: %w", dispatchErr)
			}
			events = append(events, dr.Events...)
			if dr.DataOverride != nil {
				dataOverride = dr.DataOverride
			}
		}
	}
	return events, dataOverride, nil
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
func (n *NitroVM) DeductGasFee(sender core.Address, gasUsed, gasPrice uint64) error {
	if gasPrice == 0 {
		return nil
	}
	fee := core.NewAmount(gasUsed).Mul(core.NewAmount(gasPrice))
	n.mu.Lock()
	defer n.mu.Unlock()
	acct := n.getOrCreateAccount(sender)
	newBal, err := acct.Balance.Sub(fee)
	if err != nil {
		return core.ErrInsufficientFunds
	}
	acct.Balance = newBal
	return nil
}

// ChainID returns the configured chain identifier.
func (n *NitroVM) ChainID() string { return n.chainID }

// vmSnapshot holds a deep copy of mutable VM state for simulate/rollback.
type vmSnapshot struct {
	accounts      map[core.Address]*core.Account
	contracts     map[core.Address]*contractMeta
	codes         map[string]cosmwasm.Checksum
	codeSeq       uint64
	codeBySeq     map[uint64]cosmwasm.Checksum
	seqByCode     map[string]uint64
	instanceCount uint64
	blockHeight   uint64
	blockTime     uint64
}

// Snapshot captures the current mutable state.
// Returns an opaque handle that can be passed to Restore.
func (n *NitroVM) Snapshot() any {
	n.mu.Lock()
	defer n.mu.Unlock()

	accts := make(map[core.Address]*core.Account, len(n.accounts))
	for k, v := range n.accounts {
		cp := *v
		cp.Balance = v.Balance.DeepCopy()
		accts[k] = &cp
	}
	contracts := make(map[core.Address]*contractMeta, len(n.contracts))
	for k, v := range n.contracts {
		cp := *v
		contracts[k] = &cp
	}
	codes := make(map[string]cosmwasm.Checksum, len(n.codes))
	for k, v := range n.codes {
		codes[k] = append(cosmwasm.Checksum(nil), v...)
	}
	codeBySeq := make(map[uint64]cosmwasm.Checksum, len(n.codeBySeq))
	for k, v := range n.codeBySeq {
		codeBySeq[k] = append(cosmwasm.Checksum(nil), v...)
	}
	seqByCode := make(map[string]uint64, len(n.seqByCode))
	for k, v := range n.seqByCode {
		seqByCode[k] = v
	}

	return vmSnapshot{
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

// Restore replaces mutable state from a snapshot previously returned by Snapshot.
func (n *NitroVM) Restore(snap any) {
	s := snap.(vmSnapshot)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.accounts = s.accounts
	n.contracts = s.contracts
	n.codes = s.codes
	n.codeSeq = s.codeSeq
	n.codeBySeq = s.codeBySeq
	n.seqByCode = s.seqByCode
	n.instanceCount = s.instanceCount
	n.blockHeight = s.blockHeight
	n.blockTime = s.blockTime
}

// Close releases all VM and storage resources.
func (n *NitroVM) Close() {
	n.vm.Cleanup()
	n.storage.Close()
}

func (n *NitroVM) makeEnv(contract core.Address) wasmvmtypes.Env {
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
func (n *NitroVM) transferFunds(from, to core.Address, funds []wasmvmtypes.Coin) error {
	for _, coin := range funds {
		if coin.Denom != "YELLOW" {
			return fmt.Errorf("%w: %q", core.ErrUnsupportedDenom, coin.Denom)
		}
		amt, err := core.NewAmountFromString(coin.Amount)
		if err != nil {
			return fmt.Errorf("invalid fund amount %q: %w", coin.Amount, err)
		}
		if amt.IsZero() {
			continue
		}
		fromAcct := n.getOrCreateAccount(from)
		newFromBal, err := fromAcct.Balance.Sub(amt)
		if err != nil {
			return core.ErrInsufficientFunds
		}
		toAcct := n.getOrCreateAccount(to)
		fromAcct.Balance = newFromBal
		toAcct.Balance = toAcct.Balance.Add(amt)
	}
	return nil
}

func (n *NitroVM) getOrCreateAccount(addr core.Address) *core.Account {
	acct, ok := n.accounts[addr]
	if !ok {
		acct = &core.Account{Address: addr}
		n.accounts[addr] = acct
	}
	return acct
}
