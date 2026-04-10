package runtime

// host.go contains the CosmWasm host function implementations used by the VM:
//   - contractKVStore: adapts StorageAdapter to wasmvm's KVStore interface
//   - GoAPI: EVM-style address canonicalize/humanize/validate
//   - chainQuerier: handles query_chain for bank balances and cross-contract calls

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/storage"
)

// ---------------------------------------------------------------------------
// KVStore adapter
// ---------------------------------------------------------------------------

// contractKVStore wraps a StorageAdapter for a specific contract address,
// implementing wasmvmtypes.KVStore. The first storage error is captured in
// the err field so callers can check it after execution.
type contractKVStore struct {
	addr    core.Address
	adapter storage.StorageAdapter
	err     error // first storage error encountered
}

func newContractKVStore(addr core.Address, adapter storage.StorageAdapter) *contractKVStore {
	return &contractKVStore{addr: addr, adapter: adapter}
}

func (s *contractKVStore) Get(key []byte) []byte {
	if s.err != nil {
		return nil
	}
	val, err := s.adapter.Get(s.addr, key)
	if err != nil {
		s.err = fmt.Errorf("storage get (contract=%s): %w", s.addr.Hex(), err)
		log.Printf("%v", s.err)
	}
	return val
}

func (s *contractKVStore) Set(key, value []byte) {
	if s.err != nil {
		return
	}
	if err := s.adapter.Set(s.addr, key, value); err != nil {
		s.err = fmt.Errorf("storage set (contract=%s): %w", s.addr.Hex(), err)
		log.Printf("%v", s.err)
	}
}

func (s *contractKVStore) Delete(key []byte) {
	if s.err != nil {
		return
	}
	if err := s.adapter.Delete(s.addr, key); err != nil {
		s.err = fmt.Errorf("storage delete (contract=%s): %w", s.addr.Hex(), err)
		log.Printf("%v", s.err)
	}
}

func (s *contractKVStore) Iterator(start, end []byte) wasmvmtypes.Iterator {
	iter, err := s.adapter.Range(s.addr, start, end, storage.Ascending)
	if err != nil {
		return &emptyIterator{}
	}
	return &wasmIterator{inner: iter, start: start, end: end}
}

func (s *contractKVStore) ReverseIterator(start, end []byte) wasmvmtypes.Iterator {
	iter, err := s.adapter.Range(s.addr, start, end, storage.Descending)
	if err != nil {
		return &emptyIterator{}
	}
	return &wasmIterator{inner: iter, start: start, end: end}
}

type wasmIterator struct {
	inner storage.StorageIterator
	start []byte
	end   []byte
}

func (it *wasmIterator) Domain() ([]byte, []byte) { return it.start, it.end }
func (it *wasmIterator) Valid() bool              { return it.inner.Valid() }
func (it *wasmIterator) Next()                    { it.inner.Next() }
func (it *wasmIterator) Key() []byte              { return it.inner.Key() }
func (it *wasmIterator) Value() []byte            { return it.inner.Value() }
func (it *wasmIterator) Close() error             { return it.inner.Close() }
func (it *wasmIterator) Error() error             { return nil }

type emptyIterator struct{}

func (it *emptyIterator) Domain() ([]byte, []byte) { return nil, nil }
func (it *emptyIterator) Valid() bool              { return false }
func (it *emptyIterator) Next()                    {}
func (it *emptyIterator) Key() []byte              { return nil }
func (it *emptyIterator) Value() []byte            { return nil }
func (it *emptyIterator) Close() error             { return nil }
func (it *emptyIterator) Error() error             { return nil }

// ---------------------------------------------------------------------------
// GoAPI — EVM-style address handling
// ---------------------------------------------------------------------------

func newGoAPI() wasmvmtypes.GoAPI {
	return wasmvmtypes.GoAPI{
		HumanizeAddress:     humanizeAddress,
		CanonicalizeAddress: canonicalizeAddress,
		ValidateAddress:     validateAddress,
	}
}

func humanizeAddress(canonical []byte) (string, uint64, error) {
	if len(canonical) != 20 {
		return "", GasCostAddrOp, fmt.Errorf("invalid canonical address length: %d", len(canonical))
	}
	return "0x" + hex.EncodeToString(canonical), GasCostAddrOp, nil
}

func canonicalizeAddress(human string) ([]byte, uint64, error) {
	s := core.TrimHexPrefix(human)
	if len(s) != 40 {
		return nil, GasCostAddrOp, fmt.Errorf("invalid address length: %d hex chars, want 40", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, GasCostAddrOp, fmt.Errorf("invalid hex: %w", err)
	}
	return b, GasCostAddrOp, nil
}

func validateAddress(human string) (uint64, error) {
	_, gas, err := canonicalizeAddress(human)
	return gas, err
}

// ---------------------------------------------------------------------------
// Querier — query_chain host function
// ---------------------------------------------------------------------------

type chainQuerier struct {
	vm       *NitroVM
	gasMeter *GasMeter
}

func newChainQuerier(vm *NitroVM, gasMeter *GasMeter) *chainQuerier {
	return &chainQuerier{vm: vm, gasMeter: gasMeter}
}

func (q *chainQuerier) Query(request wasmvmtypes.QueryRequest, gasLimit uint64) ([]byte, error) {
	if request.Bank != nil {
		if err := q.gasMeter.ConsumeGas(GasCostStorageRead); err != nil {
			return nil, fmt.Errorf("query out of gas: %w", err)
		}
		return q.bankQuery(request.Bank)
	}
	if request.Wasm != nil {
		return q.wasmQuery(request.Wasm, gasLimit)
	}
	return nil, fmt.Errorf("unsupported query type")
}

func (q *chainQuerier) GasConsumed() uint64 { return q.gasMeter.GasConsumed() }

func (q *chainQuerier) bankQuery(query *wasmvmtypes.BankQuery) ([]byte, error) {
	if query.Balance != nil {
		addr, err := core.HexToAddress(query.Balance.Address)
		if err != nil {
			return nil, err
		}
		bal := q.vm.GetBalance(addr)
		return json.Marshal(wasmvmtypes.BalanceResponse{
			Amount: wasmvmtypes.Coin{
				Denom:  query.Balance.Denom,
				Amount: bal.String(),
			},
		})
	}
	if query.AllBalances != nil {
		addr, err := core.HexToAddress(query.AllBalances.Address)
		if err != nil {
			return nil, err
		}
		bal := q.vm.GetBalance(addr)
		return json.Marshal(wasmvmtypes.AllBalancesResponse{
			Amount: wasmvmtypes.Array[wasmvmtypes.Coin]{
				{Denom: "YELLOW", Amount: bal.String()},
			},
		})
	}
	return nil, fmt.Errorf("unsupported bank query")
}

func (q *chainQuerier) wasmQuery(query *wasmvmtypes.WasmQuery, gasLimit uint64) ([]byte, error) {
	if query.Smart != nil {
		addr, err := core.HexToAddress(query.Smart.ContractAddr)
		if err != nil {
			return nil, err
		}
		result, gasUsed, err := q.vm.Query(addr, query.Smart.Msg, gasLimit)
		if consumeErr := q.gasMeter.ConsumeGas(gasUsed); consumeErr != nil {
			return nil, fmt.Errorf("cross-contract query out of gas: %w", consumeErr)
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("unsupported wasm query")
}
