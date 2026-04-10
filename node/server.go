package node

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm"
	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/crypto"
	"github.com/layer-3/nitrovm/storage"
)

type coin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

func toWasmCoins(coins []coin) []wasmvmtypes.Coin {
	if len(coins) == 0 {
		return nil
	}
	out := make([]wasmvmtypes.Coin, len(coins))
	for i, c := range coins {
		out[i] = wasmvmtypes.Coin{Denom: c.Denom, Amount: c.Amount}
	}
	return out
}

func rlpToWasmCoins(funds []crypto.RLPCoin) []wasmvmtypes.Coin {
	if len(funds) == 0 {
		return nil
	}
	coins := make([]wasmvmtypes.Coin, len(funds))
	for i, f := range funds {
		coins[i] = wasmvmtypes.Coin{Denom: f.Denom, Amount: f.Amount}
	}
	return coins
}

const defaultGasLimit = uint64(500_000_000_000)

// Body size limits to prevent OOM from oversized requests.
const (
	maxStoreBodySize   = 20 << 20 // 20 MB — WASM is hex-encoded inside JSON
	maxRequestBodySize = 2 << 20  // 2 MB for regular JSON requests
)

// Network represents the deployment environment.
type Network string

const (
	Devnet  Network = "devnet"
	Testnet Network = "testnet"
	Mainnet Network = "mainnet"
)

// Config holds server configuration.
type Config struct {
	Addr        string  // listen address
	DataDir     string  // data directory for VM cache and SQLite state
	Network     Network // devnet, testnet, or mainnet
	MinGasPrice uint64  // minimum accepted gas price
}

// Server wraps a NitroVM instance with HTTP handlers and SQLite persistence.
type Server struct {
	vm          nitrovm.VM
	db          *sql.DB                // metadata persistence (codes, contracts, accounts, meta)
	store       storage.StorageAdapter // contract KV storage (for simulate savepoints)
	network     Network
	minGasPrice uint64
	httpSrv     *http.Server
}

// New creates a new Server. Opens databases, creates the VM, and restores state.
func New(cfg Config, metaDB *sql.DB, store storage.StorageAdapter, vm nitrovm.VM) *Server {
	network := cfg.Network
	if network == "" {
		network = Devnet
	}
	return &Server{
		vm:          vm,
		db:          metaDB,
		store:       store,
		network:     network,
		minGasPrice: cfg.MinGasPrice,
	}
}

// Restore restores server state from the metadata database.
func (s *Server) Restore() error {
	return s.restore()
}

// Handler returns the HTTP handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /store", s.handleStore)
	mux.HandleFunc("POST /instantiate", s.handleInstantiate)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("POST /query", s.handleQuery)
	mux.HandleFunc("GET /balance/", s.handleBalance)
	if s.network == Devnet {
		mux.HandleFunc("POST /faucet", s.handleFaucet)
	}
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /account/", s.handleAccount)
	mux.HandleFunc("POST /simulate", s.handleSimulate)
	mux.HandleFunc("GET /codes", s.handleListCodes)
	mux.HandleFunc("GET /contracts", s.handleListContracts)
	mux.HandleFunc("GET /contract/", s.handleContractInfo)
	return mux
}

// Start begins listening. Blocks until the server is shut down.
func (s *Server) Start(addr string) error {
	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("nitrolite listening on %s (network=%s)", addr, s.network)
	err := s.httpSrv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close shuts down the HTTP server and releases resources.
func (s *Server) Close() {
	if s.httpSrv != nil {
		s.httpSrv.Close()
	}
	s.vm.Close()
	s.db.Close()
}

// --- Signed Transaction Support ---

// parseSigned parses and validates a signed transaction from a JSON body.
// All state-changing requests must be signed.
func (s *Server) parseSigned(body []byte) (*crypto.SignedTransaction, core.Address, error) {
	var envelope struct {
		Tx string `json:"tx"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Tx == "" {
		return nil, core.Address{}, fmt.Errorf("invalid request: expected JSON with signed \"tx\" field")
	}

	// Decode hex -> bytes.
	txBytes, err := hex.DecodeString(strings.TrimPrefix(envelope.Tx, "0x"))
	if err != nil {
		return nil, core.Address{}, fmt.Errorf("bad tx hex: %w", err)
	}

	// Decode signed transaction.
	stx, err := crypto.DecodeSignedTx(txBytes)
	if err != nil {
		return nil, core.Address{}, fmt.Errorf("decode signed tx: %w", err)
	}

	// Recover sender.
	sender, err := crypto.RecoverSender(stx)
	if err != nil {
		return nil, core.Address{}, err
	}

	// Validate chain ID.
	if stx.Tx.ChainID != s.vm.ChainID() {
		return nil, core.Address{}, fmt.Errorf("%w: got %q, want %q", core.ErrInvalidChainID, stx.Tx.ChainID, s.vm.ChainID())
	}

	// Validate gas price.
	if stx.Tx.GasPrice < s.minGasPrice {
		return nil, core.Address{}, fmt.Errorf("%w: got %d, min %d", core.ErrGasPriceTooLow, stx.Tx.GasPrice, s.minGasPrice)
	}

	return stx, sender, nil
}

// deductGasFee deducts gas fees and persists the balance update.
// If deduction fails, returns an error (caller should rollback).
func (s *Server) deductGasFee(dbTx dbExecer, sender core.Address, gasUsed, gasPrice uint64) (string, error) {
	if gasPrice == 0 {
		return "0", nil
	}
	if err := s.vm.DeductGasFee(sender, gasUsed, gasPrice); err != nil {
		return "", err
	}
	fee := core.NewAmount(gasUsed).Mul(core.NewAmount(gasPrice))
	s.persistBalance(dbTx, sender)
	return fee.String(), nil
}

// --- JSON Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
