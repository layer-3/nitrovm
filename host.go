package nitrovm

// host.go contains the CosmWasm host function implementations used by the VM:
//   - contractKVStore: adapts StorageAdapter to wasmvm's KVStore interface
//   - GoAPI: EVM-style address canonicalize/humanize/validate
//   - chainQuerier: handles query_chain for bank balances and cross-contract calls

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"
)

// ---------------------------------------------------------------------------
// KVStore adapter
// ---------------------------------------------------------------------------

// contractKVStore wraps a StorageAdapter for a specific contract address,
// implementing wasmvmtypes.KVStore.
type contractKVStore struct {
	addr    Address
	adapter StorageAdapter
}

func newContractKVStore(addr Address, adapter StorageAdapter) wasmvmtypes.KVStore {
	return &contractKVStore{addr: addr, adapter: adapter}
}

func (s *contractKVStore) Get(key []byte) []byte {
	val, _ := s.adapter.Get(s.addr, key)
	return val
}

func (s *contractKVStore) Set(key, value []byte) {
	_ = s.adapter.Set(s.addr, key, value)
}

func (s *contractKVStore) Delete(key []byte) {
	_ = s.adapter.Delete(s.addr, key)
}

func (s *contractKVStore) Iterator(start, end []byte) wasmvmtypes.Iterator {
	iter, err := s.adapter.Range(s.addr, start, end, Ascending)
	if err != nil {
		return &emptyIterator{}
	}
	return &wasmIterator{inner: iter, start: start, end: end}
}

func (s *contractKVStore) ReverseIterator(start, end []byte) wasmvmtypes.Iterator {
	iter, err := s.adapter.Range(s.addr, start, end, Descending)
	if err != nil {
		return &emptyIterator{}
	}
	return &wasmIterator{inner: iter, start: start, end: end}
}

type wasmIterator struct {
	inner StorageIterator
	start []byte
	end   []byte
}

func (it *wasmIterator) Domain() ([]byte, []byte) { return it.start, it.end }
func (it *wasmIterator) Valid() bool               { return it.inner.Valid() }
func (it *wasmIterator) Next()                     { it.inner.Next() }
func (it *wasmIterator) Key() []byte               { return it.inner.Key() }
func (it *wasmIterator) Value() []byte             { return it.inner.Value() }
func (it *wasmIterator) Close() error              { return it.inner.Close() }
func (it *wasmIterator) Error() error              { return nil }

type emptyIterator struct{}

func (it *emptyIterator) Domain() ([]byte, []byte) { return nil, nil }
func (it *emptyIterator) Valid() bool               { return false }
func (it *emptyIterator) Next()                     {}
func (it *emptyIterator) Key() []byte               { return nil }
func (it *emptyIterator) Value() []byte             { return nil }
func (it *emptyIterator) Close() error              { return nil }
func (it *emptyIterator) Error() error              { return nil }

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
		return "", 0, fmt.Errorf("invalid canonical address length: %d", len(canonical))
	}
	return "0x" + hex.EncodeToString(canonical), 0, nil
}

func canonicalizeAddress(human string) ([]byte, uint64, error) {
	s := strings.TrimPrefix(strings.TrimPrefix(human, "0x"), "0X")
	if len(s) != 40 {
		return nil, 0, fmt.Errorf("invalid address length: %d hex chars, want 40", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid hex: %w", err)
	}
	return b, 0, nil
}

func validateAddress(human string) (uint64, error) {
	s := strings.TrimPrefix(strings.TrimPrefix(human, "0x"), "0X")
	if len(s) != 40 {
		return 0, fmt.Errorf("invalid address length: %d hex chars, want 40", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return 0, fmt.Errorf("invalid hex: %w", err)
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// Querier — query_chain host function
// ---------------------------------------------------------------------------

type chainQuerier struct {
	vm *NitroVM
}

func newChainQuerier(vm *NitroVM) wasmvmtypes.Querier {
	return &chainQuerier{vm: vm}
}

func (q *chainQuerier) Query(request wasmvmtypes.QueryRequest, gasLimit uint64) ([]byte, error) {
	if request.Bank != nil {
		return q.bankQuery(request.Bank)
	}
	if request.Wasm != nil {
		return q.wasmQuery(request.Wasm, gasLimit)
	}
	return nil, fmt.Errorf("unsupported query type")
}

func (q *chainQuerier) GasConsumed() uint64 { return 0 }

func (q *chainQuerier) bankQuery(query *wasmvmtypes.BankQuery) ([]byte, error) {
	if query.Balance != nil {
		addr, err := HexToAddress(query.Balance.Address)
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
		addr, err := HexToAddress(query.AllBalances.Address)
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
		addr, err := HexToAddress(query.Smart.ContractAddr)
		if err != nil {
			return nil, err
		}
		result, _, err := q.vm.Query(addr, query.Smart.Msg, gasLimit)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("unsupported wasm query")
}
