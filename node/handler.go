package node

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/crypto"
	"github.com/layer-3/nitrovm/storage"
)

func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxStoreBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	stx, sender, err := s.parseSigned(body)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if stx.Tx.Type != crypto.TxStore {
		writeErr(w, http.StatusBadRequest, "tx type must be store")
		return
	}

	// Serialize state-changing operations to prevent nonce TOCTOU.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	wasm := stx.Tx.Code
	nonce := stx.Tx.Nonce
	gasPrice := stx.Tx.GasPrice

	// Always snapshot so we can rollback on any failure.
	snap := s.vm.Snapshot()
	var ts storage.TransactionalStore
	if t, ok := s.store.(storage.TransactionalStore); ok {
		ts = t
		if err := ts.Savepoint("gasfee_store"); err != nil {
			writeErr(w, http.StatusInternalServerError, "savepoint: "+err.Error())
			return
		}
	}
	rollback := func() {
		s.vm.Restore(snap)
		if ts != nil {
			ts.RollbackTo("gasfee_store")
		}
	}

	codeID, gasUsed, err := s.vm.StoreCode(wasm, &sender, &nonce)
	if err != nil {
		rollback()
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	codeHex := hex.EncodeToString(codeID)
	codeSeq, _ := s.vm.GetCodeSeq(codeHex)
	dbTx, err := s.db.Begin()
	if err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, "begin tx: "+err.Error())
		return
	}

	var gasFeeStr string
	if gasPrice > 0 {
		gasFeeStr, err = s.deductGasFee(dbTx, sender, gasUsed, gasPrice)
		if err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusBadRequest, "gas fee: "+err.Error())
			return
		}
	}

	if _, err := dbTx.Exec("INSERT OR REPLACE INTO codes (code_id, wasm) VALUES (?, ?)", codeHex, wasm); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist code: "+err.Error())
		return
	}
	if _, err := dbTx.Exec("INSERT OR IGNORE INTO code_seqs (seq_id, code_id) VALUES (?, ?)", codeSeq, codeHex); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist code seq: "+err.Error())
		return
	}
	if err := s.persistBalance(dbTx, sender); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist balance: "+err.Error())
		return
	}
	if err := s.tickOp(dbTx); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "tick op: "+err.Error())
		return
	}
	if err := dbTx.Commit(); err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}

	// Success — release storage savepoint.
	if ts != nil {
		ts.ReleaseSavepoint("gasfee_store")
	}

	resp := map[string]any{"code_id": codeHex, "code_seq": codeSeq, "gas_used": gasUsed, "sender": sender.Hex()}
	if gasFeeStr != "" {
		resp["gas_fee"] = gasFeeStr
	}
	writeJSON(w, resp)
}

func (s *Server) handleInstantiate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	stx, sender, err := s.parseSigned(body)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if stx.Tx.Type != crypto.TxInstantiate {
		writeErr(w, http.StatusBadRequest, "tx type must be instantiate")
		return
	}

	// Serialize state-changing operations to prevent nonce TOCTOU.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	codeID := stx.Tx.CodeID
	msg := stx.Tx.Msg
	label := stx.Tx.Label
	funds := rlpToWasmCoins(stx.Tx.Funds)
	gl := stx.Tx.GasLimit
	nonce := stx.Tx.Nonce
	gasPrice := stx.Tx.GasPrice

	// Always snapshot so we can rollback on any failure.
	snap := s.vm.Snapshot()
	var ts storage.TransactionalStore
	if t, ok := s.store.(storage.TransactionalStore); ok {
		ts = t
		if err := ts.Savepoint("gasfee_instantiate"); err != nil {
			writeErr(w, http.StatusInternalServerError, "savepoint: "+err.Error())
			return
		}
	}
	rollback := func() {
		s.vm.Restore(snap)
		if ts != nil {
			ts.RollbackTo("gasfee_instantiate")
		}
	}

	res, gasUsed, err := s.vm.Instantiate(codeID, sender, msg, label, funds, gl, &nonce)
	if err != nil {
		rollback()
		// Charge gas even on failure if gas was consumed.
		// Deduct AFTER rollback so in-memory and DB state agree.
		if gasPrice > 0 && gasUsed > 0 {
			if feeErr := s.vm.DeductGasFee(sender, gasUsed, gasPrice); feeErr == nil {
				s.persistBalance(s.db, sender)
			}
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Capture all addresses modified during instantiation (including sub-message
	// recipients) so we can persist their balances below.
	touched := s.vm.TouchedAddresses()

	dbTx, err := s.db.Begin()
	if err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, "begin tx: "+err.Error())
		return
	}

	var gasFeeStr string
	if gasPrice > 0 {
		gasFeeStr, err = s.deductGasFee(dbTx, sender, gasUsed, gasPrice)
		if err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusBadRequest, "gas fee: "+err.Error())
			return
		}
	}

	codeHex := hex.EncodeToString(codeID)
	// Persist the newly created contract and any contracts spawned via sub-messages.
	for _, ci := range s.vm.ListContracts() {
		if _, err := dbTx.Exec("INSERT OR IGNORE INTO contracts (address, code_id, label, creator) VALUES (?,?,?,?)",
			ci.Address, ci.CodeID, ci.Label, ci.Creator); err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusInternalServerError, "persist contract: "+err.Error())
			return
		}
	}
	_ = codeHex // used indirectly via ListContracts
	instanceCount := s.vm.GetInstanceCount()
	if _, err := dbTx.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('instance_count', ?)",
		fmt.Sprintf("%d", instanceCount)); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist instance count: "+err.Error())
		return
	}
	for _, addr := range touched {
		if err := s.persistBalance(dbTx, addr); err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusInternalServerError, "persist balance: "+err.Error())
			return
		}
	}
	if err := s.persistEvents(dbTx, "instantiate", res.ContractAddress.Hex(), sender.Hex(), res.Attributes, res.Events); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist events: "+err.Error())
		return
	}
	if err := s.tickOp(dbTx); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "tick op: "+err.Error())
		return
	}
	if err := dbTx.Commit(); err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}

	// Success — release storage savepoint.
	if ts != nil {
		ts.ReleaseSavepoint("gasfee_instantiate")
	}

	resp := map[string]any{"contract": res.ContractAddress.Hex(), "gas_used": gasUsed, "sender": sender.Hex()}
	if gasFeeStr != "" {
		resp["gas_fee"] = gasFeeStr
	}
	if len(res.Attributes) > 0 {
		resp["attributes"] = res.Attributes
	}
	if len(res.Events) > 0 {
		resp["events"] = res.Events
	}
	writeJSON(w, resp)
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	stx, sender, err := s.parseSigned(body)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if stx.Tx.Type != crypto.TxExecute {
		writeErr(w, http.StatusBadRequest, "tx type must be execute")
		return
	}

	// Serialize state-changing operations to prevent nonce TOCTOU.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	contract := stx.Tx.Contract
	msg := stx.Tx.Msg
	funds := rlpToWasmCoins(stx.Tx.Funds)
	gl := stx.Tx.GasLimit
	nonce := stx.Tx.Nonce
	gasPrice := stx.Tx.GasPrice

	// Always snapshot so we can rollback on any failure.
	snap := s.vm.Snapshot()
	var ts storage.TransactionalStore
	if t, ok := s.store.(storage.TransactionalStore); ok {
		ts = t
		if err := ts.Savepoint("gasfee_execute"); err != nil {
			writeErr(w, http.StatusInternalServerError, "savepoint: "+err.Error())
			return
		}
	}
	rollback := func() {
		s.vm.Restore(snap)
		if ts != nil {
			ts.RollbackTo("gasfee_execute")
		}
	}

	// Capture known contracts before execution to diff afterward.
	knownContracts := make(map[string]struct{})
	for _, ci := range s.vm.ListContracts() {
		knownContracts[ci.Address] = struct{}{}
	}

	res, gasUsed, err := s.vm.Execute(contract, sender, msg, funds, gl, &nonce)
	if err != nil {
		rollback()
		// Charge gas even on failure if gas was consumed.
		// Deduct AFTER rollback so in-memory and DB state agree.
		if gasPrice > 0 && gasUsed > 0 {
			if feeErr := s.vm.DeductGasFee(sender, gasUsed, gasPrice); feeErr == nil {
				s.persistBalance(s.db, sender)
			}
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Capture all addresses modified during execution (including sub-message
	// recipients) so we can persist their balances below.
	touched := s.vm.TouchedAddresses()

	dbTx, err := s.db.Begin()
	if err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, "begin tx: "+err.Error())
		return
	}

	var gasFeeStr string
	if gasPrice > 0 {
		gasFeeStr, err = s.deductGasFee(dbTx, sender, gasUsed, gasPrice)
		if err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusBadRequest, "gas fee: "+err.Error())
			return
		}
	}

	for _, addr := range touched {
		if err := s.persistBalance(dbTx, addr); err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusInternalServerError, "persist balance: "+err.Error())
			return
		}
	}
	// Persist only newly created contracts (spawned via sub-message dispatch).
	for _, ci := range s.vm.ListContracts() {
		if _, exists := knownContracts[ci.Address]; exists {
			continue
		}
		if _, err := dbTx.Exec("INSERT OR IGNORE INTO contracts (address, code_id, label, creator) VALUES (?,?,?,?)",
			ci.Address, ci.CodeID, ci.Label, ci.Creator); err != nil {
			dbTx.Rollback()
			rollback()
			writeErr(w, http.StatusInternalServerError, "persist contract: "+err.Error())
			return
		}
	}
	// Persist instance count if new contracts were created.
	if _, err := dbTx.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('instance_count', ?)",
		fmt.Sprintf("%d", s.vm.GetInstanceCount())); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist instance count: "+err.Error())
		return
	}
	if err := s.persistEvents(dbTx, "execute", contract.Hex(), sender.Hex(), res.Attributes, res.Events); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "persist events: "+err.Error())
		return
	}
	if err := s.tickOp(dbTx); err != nil {
		dbTx.Rollback()
		rollback()
		writeErr(w, http.StatusInternalServerError, "tick op: "+err.Error())
		return
	}
	if err := dbTx.Commit(); err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}

	// Success — release storage savepoint.
	if ts != nil {
		ts.ReleaseSavepoint("gasfee_execute")
	}

	resp := map[string]any{"data": res.Data, "gas_used": gasUsed, "sender": sender.Hex()}
	if gasFeeStr != "" {
		resp["gas_fee"] = gasFeeStr
	}
	if len(res.Attributes) > 0 {
		resp["attributes"] = res.Attributes
	}
	if len(res.Events) > 0 {
		resp["events"] = res.Events
	}
	writeJSON(w, resp)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req struct {
		Contract string          `json:"contract"`
		Msg      json.RawMessage `json:"msg"`
		GasLimit *uint64         `json:"gas_limit,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	contract, err := core.HexToAddress(req.Contract)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad contract: "+err.Error())
		return
	}
	gl := defaultGasLimit
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

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	addrHex := strings.TrimPrefix(r.URL.Path, "/balance/")
	addr, err := core.HexToAddress(addrHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address: "+err.Error())
		return
	}
	bal := s.vm.GetBalance(addr)
	writeJSON(w, map[string]any{"balance": bal})
}

func (s *Server) handleFaucet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req struct {
		Address string      `json:"address"`
		Amount  core.Amount `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	addr, err := core.HexToAddress(req.Address)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address: "+err.Error())
		return
	}
	s.vm.SetBalance(addr, req.Amount)
	nonce := s.vm.GetNonce(addr)
	if _, err := s.db.Exec("INSERT OR REPLACE INTO accounts (address, balance, nonce) VALUES (?, ?, ?)", req.Address, req.Amount.String(), nonce); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist account: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
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
		if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
			writeErr(w, http.StatusInternalServerError, "unmarshal attrs: "+err.Error())
			return
		}
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

func (s *Server) handleListCodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"codes": s.vm.ListCodes()})
}

func (s *Server) handleListContracts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"contracts": s.vm.ListContracts()})
}

func (s *Server) handleContractInfo(w http.ResponseWriter, r *http.Request) {
	addrHex := strings.TrimPrefix(r.URL.Path, "/contract/")
	addr, err := core.HexToAddress(addrHex)
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"chain_id":       s.vm.ChainID(),
		"block_height":   s.vm.GetOpSeq(),
		"code_count":     len(s.vm.ListCodes()),
		"contract_count": len(s.vm.ListContracts()),
	})
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	addrHex := strings.TrimPrefix(r.URL.Path, "/account/")
	addr, err := core.HexToAddress(addrHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address: "+err.Error())
		return
	}
	resp := map[string]any{
		"address": addr.Hex(),
		"balance": s.vm.GetBalance(addr),
		"nonce":   s.vm.GetNonce(addr),
	}
	if info := s.vm.GetContractInfo(addr); info != nil {
		resp["code_id"] = info.CodeID
		resp["label"] = info.Label
		resp["creator"] = info.Creator
	}
	writeJSON(w, resp)
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req struct {
		Type     string          `json:"type"`
		CodeID   string          `json:"code_id,omitempty"`
		Contract string          `json:"contract,omitempty"`
		Sender   string          `json:"sender"`
		Msg      json.RawMessage `json:"msg"`
		Label    string          `json:"label,omitempty"`
		Funds    []coin          `json:"funds,omitempty"`
		GasLimit *uint64         `json:"gas_limit,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.Type != "execute" && req.Type != "instantiate" {
		writeErr(w, http.StatusBadRequest, `"type" must be "execute" or "instantiate"`)
		return
	}

	// Serialize with state-changing ops to prevent snapshot/restore races.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Snapshot VM in-memory state.
	snap := s.vm.Snapshot()
	defer s.vm.Restore(snap)

	// Savepoint storage if supported.
	if ts, ok := s.store.(storage.TransactionalStore); ok {
		if err := ts.Savepoint("simulate"); err != nil {
			writeErr(w, http.StatusInternalServerError, "savepoint: "+err.Error())
			return
		}
		defer ts.RollbackTo("simulate")
	}

	const maxSimulateGas uint64 = 50_000_000
	gl := maxSimulateGas
	if req.GasLimit != nil && *req.GasLimit < maxSimulateGas {
		gl = *req.GasLimit
	}
	funds := toWasmCoins(req.Funds)

	switch req.Type {
	case "execute":
		contract, err := core.HexToAddress(req.Contract)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad contract: "+err.Error())
			return
		}
		sender, err := core.HexToAddress(req.Sender)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad sender: "+err.Error())
			return
		}
		res, gasUsed, err := s.vm.Execute(contract, sender, req.Msg, funds, gl, nil)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		resp := map[string]any{"data": res.Data, "gas_used": gasUsed}
		if len(res.Attributes) > 0 {
			resp["attributes"] = res.Attributes
		}
		if len(res.Events) > 0 {
			resp["events"] = res.Events
		}
		writeJSON(w, resp)

	case "instantiate":
		codeID, err := hex.DecodeString(req.CodeID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad code_id hex")
			return
		}
		sender, err := core.HexToAddress(req.Sender)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad sender: "+err.Error())
			return
		}
		res, gasUsed, err := s.vm.Instantiate(codeID, sender, req.Msg, req.Label, funds, gl, nil)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		resp := map[string]any{"contract": res.ContractAddress.Hex(), "gas_used": gasUsed}
		if len(res.Attributes) > 0 {
			resp["attributes"] = res.Attributes
		}
		if len(res.Events) > 0 {
			resp["events"] = res.Events
		}
		writeJSON(w, resp)
	}
}
