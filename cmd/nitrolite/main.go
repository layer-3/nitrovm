package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"

	"github.com/layer-3/nitrovm"
	"github.com/layer-3/nitrovm/storage/sqlite"
)

const gasLimit = uint64(500_000_000_000)

// server wraps a NitroVM instance with HTTP handlers and SQLite persistence.
type server struct {
	vm *nitrovm.NitroVM
	db *sql.DB // metadata persistence (codes, contracts, accounts, meta)
}

func main() {
	addr := flag.String("addr", ":26657", "listen address")
	dataDir := flag.String("data-dir", ".", "data directory for VM cache and SQLite state")
	flag.Parse()

	// Open metadata DB (same file as contract storage, separate connection).
	dbPath := *dataDir + "/state.db"
	metaDB, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		log.Fatalf("open meta db: %v", err)
	}
	if err := createMetaTables(metaDB); err != nil {
		log.Fatalf("create meta tables: %v", err)
	}

	// Contract KV storage (NitroVM's StorageAdapter).
	store, err := sqlite.New(dbPath)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}

	cfg := nitrovm.DefaultConfig()
	cfg.DataDir = *dataDir
	cfg.PrintDebug = false

	vm, err := nitrovm.New(cfg, store)
	if err != nil {
		log.Fatalf("create vm: %v", err)
	}

	s := &server{vm: vm, db: metaDB}
	if err := s.restore(); err != nil {
		log.Fatalf("restore state: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /store", s.handleStore)
	mux.HandleFunc("POST /instantiate", s.handleInstantiate)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("POST /query", s.handleQuery)
	mux.HandleFunc("GET /balance/", s.handleBalance)
	mux.HandleFunc("POST /faucet", s.handleFaucet)

	srv := &http.Server{Addr: *addr, Handler: mux}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down")
		srv.Close()
	}()

	log.Printf("nitrolite listening on %s (data: %s)", *addr, *dataDir)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	vm.Close()
	metaDB.Close()
}

// --- HTTP Handlers ---

func (s *server) handleStore(w http.ResponseWriter, r *http.Request) {
	wasm, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	codeID, err := s.vm.StoreCode(wasm)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	codeHex := hex.EncodeToString(codeID)
	s.db.Exec("INSERT OR REPLACE INTO codes (code_id, wasm) VALUES (?, ?)", codeHex, wasm)
	writeJSON(w, map[string]any{"code_id": codeHex})
}

func (s *server) handleInstantiate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodeID string          `json:"code_id"`
		Sender string          `json:"sender"`
		Msg    json.RawMessage `json:"msg"`
		Label  string          `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	codeID, err := hex.DecodeString(req.CodeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad code_id hex")
		return
	}
	sender, err := nitrovm.HexToAddress(req.Sender)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad sender: "+err.Error())
		return
	}
	addr, gasUsed, err := s.vm.Instantiate(codeID, sender, req.Msg, req.Label, nil, gasLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Persist contract + bump instance count.
	s.db.Exec("INSERT OR REPLACE INTO contracts (address, code_id, label, creator) VALUES (?,?,?,?)",
		addr.Hex(), req.CodeID, req.Label, req.Sender)
	s.db.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('instance_count', ?)",
		fmt.Sprintf("%d", s.getInstanceCount()))
	writeJSON(w, map[string]any{"contract": addr.Hex(), "gas_used": gasUsed})
}

func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contract string          `json:"contract"`
		Sender   string          `json:"sender"`
		Msg      json.RawMessage `json:"msg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	contract, err := nitrovm.HexToAddress(req.Contract)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad contract: "+err.Error())
		return
	}
	sender, err := nitrovm.HexToAddress(req.Sender)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad sender: "+err.Error())
		return
	}
	data, gasUsed, err := s.vm.Execute(contract, sender, req.Msg, nil, gasLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"data": data, "gas_used": gasUsed})
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contract string          `json:"contract"`
		Msg      json.RawMessage `json:"msg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	contract, err := nitrovm.HexToAddress(req.Contract)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad contract: "+err.Error())
		return
	}
	result, _, err := s.vm.Query(contract, req.Msg, gasLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

func (s *server) handleBalance(w http.ResponseWriter, r *http.Request) {
	addrHex := strings.TrimPrefix(r.URL.Path, "/balance/")
	addr, err := nitrovm.HexToAddress(addrHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address: "+err.Error())
		return
	}
	bal := s.vm.GetBalance(addr)
	writeJSON(w, map[string]any{"balance": bal})
}

func (s *server) handleFaucet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
		Amount  uint64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	addr, err := nitrovm.HexToAddress(req.Address)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address: "+err.Error())
		return
	}
	s.vm.SetBalance(addr, req.Amount)
	s.db.Exec("INSERT OR REPLACE INTO accounts (address, balance) VALUES (?, ?)", req.Address, req.Amount)
	writeJSON(w, map[string]any{})
}

// --- State Persistence ---

func createMetaTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS codes (code_id TEXT PRIMARY KEY, wasm BLOB);
		CREATE TABLE IF NOT EXISTS contracts (address TEXT PRIMARY KEY, code_id TEXT, label TEXT, creator TEXT);
		CREATE TABLE IF NOT EXISTS accounts (address TEXT PRIMARY KEY, balance INTEGER);
		CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
	`)
	return err
}

func (s *server) restore() error {
	// Restore codes.
	rows, err := s.db.Query("SELECT code_id, wasm FROM codes")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var codeHex string
		var wasm []byte
		if err := rows.Scan(&codeHex, &wasm); err != nil {
			return err
		}
		if _, err := s.vm.StoreCode(wasm); err != nil {
			return fmt.Errorf("restore code %s: %w", codeHex, err)
		}
		log.Printf("restored code %s", codeHex)
	}

	// Restore contracts.
	rows2, err := s.db.Query("SELECT address, code_id, label, creator FROM contracts")
	if err != nil {
		return err
	}
	defer rows2.Close()
	for rows2.Next() {
		var addrHex, codeHex, label, creatorHex string
		if err := rows2.Scan(&addrHex, &codeHex, &label, &creatorHex); err != nil {
			return err
		}
		addr, _ := nitrovm.HexToAddress(addrHex)
		creator, _ := nitrovm.HexToAddress(creatorHex)
		checksum, _ := hex.DecodeString(codeHex)
		s.vm.RegisterContract(addr, creator, checksum, label)
		log.Printf("restored contract %s", addrHex)
	}

	// Restore accounts.
	rows3, err := s.db.Query("SELECT address, balance FROM accounts")
	if err != nil {
		return err
	}
	defer rows3.Close()
	for rows3.Next() {
		var addrHex string
		var balance uint64
		if err := rows3.Scan(&addrHex, &balance); err != nil {
			return err
		}
		addr, _ := nitrovm.HexToAddress(addrHex)
		s.vm.SetBalance(addr, balance)
	}

	// Restore instance count.
	var countStr string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key = 'instance_count'").Scan(&countStr)
	if err == nil {
		if n, err := strconv.ParseUint(countStr, 10, 64); err == nil {
			s.vm.SetInstanceCount(n)
			log.Printf("restored instance_count=%d", n)
		}
	}

	return nil
}

// getInstanceCount reads the current counter by attempting a dummy instantiate.
// We approximate by counting rows in the contracts table.
func (s *server) getInstanceCount() int {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM contracts").Scan(&count)
	return count
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
