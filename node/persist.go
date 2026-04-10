package node

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	wasmvmtypes "github.com/CosmWasm/wasmvm/v2/types"

	"github.com/layer-3/nitrovm/core"
)

// dbExecer abstracts *sql.DB and *sql.Tx for persistence helpers.
type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// CreateMetaTables creates the required metadata tables in the SQLite database.
func CreateMetaTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS codes (code_id TEXT PRIMARY KEY, wasm BLOB);
		CREATE TABLE IF NOT EXISTS code_seqs (seq_id INTEGER PRIMARY KEY, code_id TEXT NOT NULL);
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

func (s *Server) restore() error {
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
		if _, _, err := s.vm.StoreCode(wasm, nil, nil); err != nil {
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
		addr, err := core.HexToAddress(addrHex)
		if err != nil {
			return fmt.Errorf("restore contract %s: bad address: %w", addrHex, err)
		}
		creator, err := core.HexToAddress(creatorHex)
		if err != nil {
			return fmt.Errorf("restore contract %s: bad creator: %w", addrHex, err)
		}
		checksum, err := hex.DecodeString(codeHex)
		if err != nil {
			return fmt.Errorf("restore contract %s: bad code_id hex: %w", addrHex, err)
		}
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
		balance, err := core.NewAmountFromString(balStr)
		if err != nil {
			return fmt.Errorf("restore account %s: bad balance %q: %w", addrHex, balStr, err)
		}
		addr, err := core.HexToAddress(addrHex)
		if err != nil {
			return fmt.Errorf("restore account %s: bad address: %w", addrHex, err)
		}
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

	// Restore code sequence counter.
	var maxSeq uint64
	if err := s.db.QueryRow("SELECT COALESCE(MAX(seq_id), 0) FROM code_seqs").Scan(&maxSeq); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("restore code seq: %w", err)
	}
	if maxSeq > 0 {
		s.vm.SetCodeSeq(maxSeq)
		log.Printf("restored code_seq=%d", maxSeq)
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

// getInstanceCount approximates the instance count by counting contract rows.
func (s *Server) getInstanceCount() (int, error) {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM contracts").Scan(&count); err != nil {
		return 0, fmt.Errorf("count contracts: %w", err)
	}
	return count, nil
}

func (s *Server) tickOp(db dbExecer) error {
	s.vm.TickOp()
	_, err := db.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('op_seq', ?)",
		fmt.Sprintf("%d", s.vm.GetOpSeq()))
	return err
}

func (s *Server) persistBalance(db dbExecer, addr core.Address) error {
	bal := s.vm.GetBalance(addr)
	nonce := s.vm.GetNonce(addr)
	_, err := db.Exec("INSERT OR REPLACE INTO accounts (address, balance, nonce) VALUES (?, ?, ?)", addr.Hex(), bal.String(), nonce)
	return err
}

func (s *Server) persistEvents(db dbExecer, txType, contract, sender string, attrs []wasmvmtypes.EventAttribute, events []wasmvmtypes.Event) error {
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
		attrJSON, err := json.Marshal(evt.Attributes)
		if err != nil {
			return fmt.Errorf("marshal event attrs: %w", err)
		}
		if _, err := db.Exec(
			"INSERT INTO events (op_seq, tx_type, contract, sender, event_type, attributes, created_at) VALUES (?,?,?,?,?,?,?)",
			opSeq, txType, contract, sender, evt.Type, string(attrJSON), now,
		); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
	}
	return nil
}
