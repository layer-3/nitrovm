package main

import (
	"database/sql"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	_ "github.com/mattn/go-sqlite3"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/node"
	"github.com/layer-3/nitrovm/runtime"
	"github.com/layer-3/nitrovm/storage/memory"
)

func main() {
	addr := flag.String("addr", ":26657", "listen address")
	dataDir := flag.String("data-dir", ".", "data directory for VM cache and SQLite state")
	network := flag.String("network", "devnet", "network mode: devnet, testnet, or mainnet")
	minGasPrice := flag.Uint64("min-gas-price", 0, "minimum accepted gas price")
	flag.Parse()

	net := node.Network(*network)
	if net != node.Devnet && net != node.Testnet && net != node.Mainnet {
		log.Fatalf("invalid network: %q (must be devnet, testnet, or mainnet)", *network)
	}

	// Open state DB (metadata + contract storage persistence).
	dbPath := filepath.Join(*dataDir, "state.db")
	metaDB, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err := node.CreateMetaTables(metaDB); err != nil {
		log.Fatalf("create tables: %v", err)
	}

	// In-memory contract KV storage; flushed to SQLite on each successful commit.
	store := memory.New()

	cfg := core.DefaultConfig()
	cfg.DataDir = *dataDir
	cfg.PrintDebug = false

	vm, err := runtime.New(cfg, store)
	if err != nil {
		log.Fatalf("create vm: %v", err)
	}

	s := node.New(node.Config{
		Addr:        *addr,
		DataDir:     *dataDir,
		Network:     net,
		MinGasPrice: *minGasPrice,
	}, metaDB, vm)

	if err := s.Restore(); err != nil {
		log.Fatalf("restore state: %v", err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down")
		s.Close()
	}()

	log.Printf("nitrolite data: %s", *dataDir)
	if err := s.Start(*addr); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
