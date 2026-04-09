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
	"time"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"
	_ "github.com/mattn/go-sqlite3"

	"github.com/layer-3/nitrovm"
	"github.com/layer-3/nitrovm/storage/sqlite"
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
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /codes", s.handleListCodes)
	mux.HandleFunc("GET /contracts", s.handleListContracts)
	mux.HandleFunc("GET /contract/", s.handleContractInfo)

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
	s.tickOp()
	writeJSON(w, map[string]any{"code_id": codeHex})
}

func (s *server) handleInstantiate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodeID   string          `json:"code_id"`
		Sender   string          `json:"sender"`
		Msg      json.RawMessage `json:"msg"`
		Label    string          `json:"label"`
		Funds    []coin          `json:"funds,omitempty"`
		GasLimit *uint64         `json:"gas_limit,omitempty"`
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
	gl := gasLimit
	if req.GasLimit != nil {
		gl = *req.GasLimit
	}
	funds := toWasmCoins(req.Funds)
	res, err := s.vm.Instantiate(codeID, sender, req.Msg, req.Label, funds, gl)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Persist contract + bump instance count.
	s.db.Exec("INSERT OR REPLACE INTO contracts (address, code_id, label, creator) VALUES (?,?,?,?)",
		res.ContractAddress.Hex(), req.CodeID, req.Label, req.Sender)
	s.db.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('instance_count', ?)",
		fmt.Sprintf("%d", s.getInstanceCount()))
	s.persistBalance(sender)
	s.persistBalance(res.ContractAddress)
	s.persistEvents("instantiate", res.ContractAddress.Hex(), req.Sender, res.Attributes, res.Events)
	s.tickOp()
	resp := map[string]any{"contract": res.ContractAddress.Hex(), "gas_used": res.GasUsed}
	if len(res.Attributes) > 0 {
		resp["attributes"] = res.Attributes
	}
	if len(res.Events) > 0 {
		resp["events"] = res.Events
	}
	writeJSON(w, resp)
}

func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contract string          `json:"contract"`
		Sender   string          `json:"sender"`
		Msg      json.RawMessage `json:"msg"`
		Funds    []coin          `json:"funds,omitempty"`
		GasLimit *uint64         `json:"gas_limit,omitempty"`
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
	gl := gasLimit
	if req.GasLimit != nil {
		gl = *req.GasLimit
	}
	funds := toWasmCoins(req.Funds)
	res, err := s.vm.Execute(contract, sender, req.Msg, funds, gl)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.persistBalance(sender)
	s.persistBalance(contract)
	s.persistEvents("execute", req.Contract, req.Sender, res.Attributes, res.Events)
	s.tickOp()
	resp := map[string]any{"data": res.Data, "gas_used": res.GasUsed}
	if len(res.Attributes) > 0 {
		resp["attributes"] = res.Attributes
	}
	if len(res.Events) > 0 {
		resp["events"] = res.Events
	}
	writeJSON(w, resp)
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contract string          `json:"contract"`
		Msg      json.RawMessage `json:"msg"`
		GasLimit *uint64         `json:"gas_limit,omitempty"`
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
	gl := gasLimit
	if req.GasLimit != nil {
		gl = *req.GasLimit
	}
	result, gasUsed, err := s.vm.Query(contract, req.Msg, gl)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"data": json.RawMessage(result), "gas_used": gasUsed})
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
		Address string        `json:"address"`
		Amount  nitrovm.Amount `json:"amount"`
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
	s.db.Exec("INSERT OR REPLACE INTO accounts (address, balance, nonce) VALUES (?, ?, 0)", req.Address, req.Amount.String())
	writeJSON(w, map[string]any{})
}

func (s *server) persistBalance(addr nitrovm.Address) {
	bal := s.vm.GetBalance(addr)
	nonce := s.vm.GetNonce(addr)
	s.db.Exec("INSERT OR REPLACE INTO accounts (address, balance, nonce) VALUES (?, ?, ?)", addr.Hex(), bal.String(), nonce)
}

// --- State Persistence ---

func createMetaTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS codes (code_id TEXT PRIMARY KEY, wasm BLOB);
		CREATE TABLE IF NOT EXISTS contracts (address TEXT PRIMARY KEY, code_id TEXT, label TEXT, creator TEXT);
		CREATE TABLE IF NOT EXISTS accounts (address TEXT PRIMARY KEY, balance TEXT);
		CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
		CREATE TABLE IF NOT EXISTS events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			op_seq     INTEGER NOT NULL,
			tx_type    TEXT NOT NULL,
			contract   TEXT NOT NULL,
			sender     TEXT NOT NULL,
			event_type TEXT NOT NULL,
			attributes TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_events_contract ON events(contract);
		CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
	`)
	if err != nil {
		return err
	}
	// Migration: add nonce column if missing.
	db.Exec("ALTER TABLE accounts ADD COLUMN nonce INTEGER DEFAULT 0")
	return nil
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
	rows3, err := s.db.Query("SELECT address, balance, COALESCE(nonce, 0) FROM accounts")
	if err != nil {
		return err
	}
	defer rows3.Close()
	for rows3.Next() {
		var addrHex, balStr string
		var nonce uint64
		if err := rows3.Scan(&addrHex, &balStr, &nonce); err != nil {
			return err
		}
		balance, err := nitrovm.NewAmountFromString(balStr)
		if err != nil {
			return fmt.Errorf("restore account %s: bad balance %q: %w", addrHex, balStr, err)
		}
		addr, _ := nitrovm.HexToAddress(addrHex)
		s.vm.SetBalance(addr, balance)
		s.vm.SetNonce(addr, nonce)
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

	// Restore operation sequence.
	var seqStr string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key = 'op_seq'").Scan(&seqStr)
	if err == nil {
		if n, err := strconv.ParseUint(seqStr, 10, 64); err == nil {
			s.vm.SetBlockInfo(n, uint64(time.Now().UnixNano()))
			log.Printf("restored op_seq=%d", n)
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

func (s *server) tickOp() {
	s.vm.TickOp()
	s.db.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('op_seq', ?)",
		fmt.Sprintf("%d", s.vm.GetOpSeq()))
}

func (s *server) persistEvents(txType, contract, sender string, attrs []wasmvmtypes.EventAttribute, events []wasmvmtypes.Event) {
	opSeq := s.vm.GetOpSeq()
	now := time.Now().Unix()

	var allEvents []wasmvmtypes.Event
	if len(attrs) > 0 {
		allEvents = append(allEvents, wasmvmtypes.Event{
			Type:       "wasm",
			Attributes: attrs,
		})
	}
	allEvents = append(allEvents, events...)

	for _, evt := range allEvents {
		attrJSON, _ := json.Marshal(evt.Attributes)
		s.db.Exec(
			"INSERT INTO events (op_seq, tx_type, contract, sender, event_type, attributes, created_at) VALUES (?,?,?,?,?,?,?)",
			opSeq, txType, contract, sender, evt.Type, string(attrJSON), now,
		)
	}
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	contract := q.Get("contract")
	eventType := q.Get("type")
	sender := q.Get("sender")

	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n >= 0 {
		offset = n
	}

	query := "SELECT id, op_seq, tx_type, contract, sender, event_type, attributes, created_at FROM events WHERE 1=1"
	var args []any
	if contract != "" {
		query += " AND contract = ?"
		args = append(args, contract)
	}
	if eventType != "" {
		query += " AND event_type = ?"
		args = append(args, eventType)
	}
	if sender != "" {
		query += " AND sender = ?"
		args = append(args, sender)
	}
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query events: "+err.Error())
		return
	}
	defer rows.Close()

	var events []map[string]any
	for rows.Next() {
		var id, opSeq, createdAt int64
		var txType, contractAddr, senderAddr, evtType, attrsJSON string
		if err := rows.Scan(&id, &opSeq, &txType, &contractAddr, &senderAddr, &evtType, &attrsJSON, &createdAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		var attrs []map[string]string
		json.Unmarshal([]byte(attrsJSON), &attrs)
		events = append(events, map[string]any{
			"id":         id,
			"op_seq":     opSeq,
			"tx_type":    txType,
			"contract":   contractAddr,
			"sender":     senderAddr,
			"event_type": evtType,
			"attributes": attrs,
			"created_at": createdAt,
		})
	}
	if events == nil {
		events = []map[string]any{}
	}
	writeJSON(w, map[string]any{"events": events})
}

func (s *server) handleListCodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"codes": s.vm.ListCodes()})
}

func (s *server) handleListContracts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"contracts": s.vm.ListContracts()})
}

func (s *server) handleContractInfo(w http.ResponseWriter, r *http.Request) {
	addrHex := strings.TrimPrefix(r.URL.Path, "/contract/")
	addr, err := nitrovm.HexToAddress(addrHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address: "+err.Error())
		return
	}
	info := s.vm.GetContractInfo(addr)
	if info == nil {
		writeErr(w, http.StatusNotFound, "contract not found")
		return
	}
	bal := s.vm.GetBalance(addr)
	writeJSON(w, map[string]any{
		"address": info.Address,
		"code_id": info.CodeID,
		"label":   info.Label,
		"creator": info.Creator,
		"balance": bal,
	})
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
