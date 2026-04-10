package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/layer-3/nitrovm/core"
	"github.com/layer-3/nitrovm/crypto"
)

var (
	serverURL    = "http://localhost:26657"
	flagGasLimit uint64 = 500_000_000_000
	flagNonce    *uint64
	flagFunds    string
	flagKeyfile  string
	flagGasPrice uint64 = 1
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// readResponse reads the HTTP response body, checks for error status codes
// and JSON error fields, and returns the raw bytes.
func readResponse(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string `json:"error"` }
		if json.Unmarshal(raw, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("server error (HTTP %d): %s", resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("server error: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var errCheck struct{ Error string `json:"error"` }
	if err := json.Unmarshal(raw, &errCheck); err == nil && errCheck.Error != "" {
		return nil, fmt.Errorf("%s", errCheck.Error)
	}
	return raw, nil
}

func main() {
	if u := os.Getenv("NITRO_SERVER"); u != "" {
		serverURL = u
	}
	if k := os.Getenv("NITRO_KEYFILE"); k != "" {
		flagKeyfile = k
	}

	args := os.Args[1:]
	// Consume global flags before command.
	for len(args) >= 2 {
		switch args[0] {
		case "--server":
			serverURL = args[1]
			args = args[2:]
		case "--gas-limit":
			v, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bad --gas-limit: %v\n", err)
				os.Exit(1)
			}
			flagGasLimit = v
			args = args[2:]
		case "--funds":
			flagFunds = args[1]
			args = args[2:]
		case "--nonce":
			v, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bad --nonce: %v\n", err)
				os.Exit(1)
			}
			flagNonce = &v
			args = args[2:]
		case "--keyfile":
			flagKeyfile = args[1]
			args = args[2:]
		case "--gas-price":
			v, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bad --gas-price: %v\n", err)
				os.Exit(1)
			}
			flagGasPrice = v
			args = args[2:]
		default:
			goto done
		}
	}
done:

	if len(args) == 0 {
		usage()
	}

	var err error
	switch args[0] {
	case "keygen":
		err = cmdKeygen()
	case "address":
		err = cmdAddress()
	case "store":
		err = cmdStore(args[1:])
	case "instantiate":
		err = cmdInstantiate(args[1:])
	case "execute":
		err = cmdExecute(args[1:])
	case "query":
		err = cmdQuery(args[1:])
	case "balance":
		err = cmdBalance(args[1:])
	case "faucet":
		err = cmdFaucet(args[1:])
	case "events":
		err = cmdEvents(args[1:])
	case "list-codes":
		err = cmdListCodes()
	case "list-contracts":
		err = cmdListContracts()
	case "info":
		err = cmdInfo(args[1:])
	case "deploy":
		err = cmdDeploy(args[1:])
	case "account":
		err = cmdAccount(args[1:])
	case "status":
		err = cmdStatus()
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: nitroctl [flags] <command> [args...]

Flags:
  --server <url>          Server URL (default http://localhost:26657, or NITRO_SERVER env)
  --keyfile <path>        Private key file (default ~/.nitrovm/key, or NITRO_KEYFILE env)
  --gas-limit <n>         Gas limit for the operation
  --gas-price <n>         Gas price (default 1)
  --nonce <n>             Override sender nonce
  --funds <amount><denom> Attach funds (e.g. 100YELLOW)

Commands:
  keygen                                           Generate new keypair
  address                                          Show address from keyfile
  store <file.wasm>                                Store contract code
  instantiate <code-id> '<msg>' [label]            Create contract instance
  execute <contract> '<msg>'                       Call contract
  query <contract> '<msg>'                         Query contract (read-only)
  deploy <file.wasm> '<msg>' [label]               Store + instantiate
  balance <address>                                Check YELLOW balance
  account <address>                                Show full account info
  faucet <address> <amount>                        Set YELLOW balance (devnet only)
  events [--contract addr] [--type t] [--sender s] [--limit n]  Query events
  list-codes                                       List stored code IDs
  list-contracts                                   List all contracts
  info <address>                                   Show contract details
  status                                           Show server status

All state-changing operations require a keyfile. Run 'nitroctl keygen' to create one.
The sender address is derived from the private key.`)
	os.Exit(1)
}

// mustLoadKey loads the private key or exits with an error.
func mustLoadKey() *secp256k1.PrivateKey {
	path := flagKeyfile
	if path == "" {
		path = getDefaultKeyPath()
	}
	key, err := loadPrivateKey(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\nRun 'nitroctl keygen' to create a key, or use --keyfile\n", err)
		os.Exit(1)
	}
	return key
}

// getNonce fetches the nonce from server, or uses the override flag.
func getNonce(addr core.Address) (uint64, error) {
	if flagNonce != nil {
		return *flagNonce, nil
	}
	return fetchNonce(addr)
}

// parseFundsRLP parses the --funds flag value (e.g. "100YELLOW") into RLPCoins.
func parseFundsRLP() []crypto.RLPCoin {
	if flagFunds == "" {
		return nil
	}
	i := 0
	for i < len(flagFunds) && (flagFunds[i] >= '0' && flagFunds[i] <= '9') {
		i++
	}
	if i == 0 || i == len(flagFunds) {
		fmt.Fprintf(os.Stderr, "bad --funds format %q, expected <amount><denom> e.g. 100YELLOW\n", flagFunds)
		os.Exit(1)
	}
	return []crypto.RLPCoin{{Denom: flagFunds[i:], Amount: flagFunds[:i]}}
}

// --- Key Commands ---

func cmdKeygen() error {
	path := flagKeyfile
	if path == "" {
		path = getDefaultKeyPath()
	}

	// Generate random 32 bytes.
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	key := secp256k1.PrivKeyFromBytes(b[:])
	addr := crypto.DeriveAddress(key)

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(b[:])+"\n"), 0600); err != nil {
		return err
	}
	fmt.Printf("Address: %s\nKey saved to %s\n", addr.Hex(), path)
	return nil
}

func cmdAddress() error {
	key := mustLoadKey()
	fmt.Println(crypto.DeriveAddress(key).Hex())
	return nil
}

// --- Signed Commands ---

func cmdStore(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: store <file.wasm>")
	}
	wasm, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}

	key := mustLoadKey()
	sender := crypto.DeriveAddress(key)
	chainID, err := fetchChainID()
	if err != nil {
		return fmt.Errorf("fetch chain ID: %w", err)
	}
	nonce, err := getNonce(sender)
	if err != nil {
		return fmt.Errorf("fetch nonce: %w", err)
	}
	tx := &crypto.Transaction{
		ChainID:  chainID,
		Nonce:    nonce,
		GasLimit: flagGasLimit,
		GasPrice: flagGasPrice,
		Type:     crypto.TxStore,
		Code:     wasm,
	}
	body, err := signAndMarshal(tx, key)
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(serverURL+"/store", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	var result struct{ CodeID string `json:"code_id"` }
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Println(result.CodeID)
	return nil
}

func cmdInstantiate(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: instantiate <code-id> '<msg>' [label]")
	}
	codeID, err := hex.DecodeString(args[0])
	if err != nil {
		return fmt.Errorf("bad code-id hex: %w", err)
	}
	msg := []byte(args[1])
	label := "default"
	if len(args) >= 3 {
		label = args[2]
	}

	key := mustLoadKey()
	sender := crypto.DeriveAddress(key)
	chainID, err := fetchChainID()
	if err != nil {
		return fmt.Errorf("fetch chain ID: %w", err)
	}
	nonce, err := getNonce(sender)
	if err != nil {
		return fmt.Errorf("fetch nonce: %w", err)
	}
	tx := &crypto.Transaction{
		ChainID:  chainID,
		Nonce:    nonce,
		GasLimit: flagGasLimit,
		GasPrice: flagGasPrice,
		Type:     crypto.TxInstantiate,
		CodeID:   codeID,
		Label:    label,
		Msg:      msg,
		Funds:    parseFundsRLP(),
	}
	body, err := signAndMarshal(tx, key)
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(serverURL+"/instantiate", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	var result struct{ Contract string `json:"contract"` }
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Println(result.Contract)
	return nil
}

func cmdExecute(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: execute <contract> '<msg>'")
	}
	contract, err := core.HexToAddress(args[0])
	if err != nil {
		return fmt.Errorf("bad contract: %w", err)
	}
	msg := []byte(args[1])

	key := mustLoadKey()
	sender := crypto.DeriveAddress(key)
	chainID, err := fetchChainID()
	if err != nil {
		return fmt.Errorf("fetch chain ID: %w", err)
	}
	nonce, err := getNonce(sender)
	if err != nil {
		return fmt.Errorf("fetch nonce: %w", err)
	}
	tx := &crypto.Transaction{
		ChainID:  chainID,
		Nonce:    nonce,
		GasLimit: flagGasLimit,
		GasPrice: flagGasPrice,
		Type:     crypto.TxExecute,
		Contract: contract,
		Msg:      msg,
		Funds:    parseFundsRLP(),
	}
	body, err := signAndMarshal(tx, key)
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(serverURL+"/execute", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdDeploy(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: deploy <file.wasm> '<msg>' [label]")
	}
	wasmPath, msg := args[0], args[1]
	label := "default"
	if len(args) >= 3 {
		label = args[2]
	}

	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return err
	}

	key := mustLoadKey()
	sender := crypto.DeriveAddress(key)
	chainID, err := fetchChainID()
	if err != nil {
		return fmt.Errorf("fetch chain ID: %w", err)
	}
	nonce, err := getNonce(sender)
	if err != nil {
		return fmt.Errorf("fetch nonce: %w", err)
	}

	// Step 1: Store code (signed).
	storeTx := &crypto.Transaction{
		ChainID:  chainID,
		Nonce:    nonce,
		GasLimit: flagGasLimit,
		GasPrice: flagGasPrice,
		Type:     crypto.TxStore,
		Code:     wasm,
	}
	storeBody, err := signAndMarshal(storeTx, key)
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(serverURL+"/store", "application/json", bytes.NewReader(storeBody))
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	var storeResult struct {
		CodeID string `json:"code_id"`
	}
	if err := json.Unmarshal(raw, &storeResult); err != nil {
		return fmt.Errorf("parse store response: %w", err)
	}
	codeID, err := hex.DecodeString(storeResult.CodeID)
	if err != nil {
		return fmt.Errorf("bad code_id hex: %w", err)
	}

	// Step 2: Instantiate (signed, nonce incremented).
	instTx := &crypto.Transaction{
		ChainID:  chainID,
		Nonce:    nonce + 1,
		GasLimit: flagGasLimit,
		GasPrice: flagGasPrice,
		Type:     crypto.TxInstantiate,
		CodeID:   codeID,
		Label:    label,
		Msg:      []byte(msg),
		Funds:    parseFundsRLP(),
	}
	instBody, err := signAndMarshal(instTx, key)
	if err != nil {
		return err
	}
	resp2, err := httpClient.Post(serverURL+"/instantiate", "application/json", bytes.NewReader(instBody))
	if err != nil {
		return err
	}
	raw2, err := readResponse(resp2)
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}
	var instResult struct {
		Contract string `json:"contract"`
	}
	if err := json.Unmarshal(raw2, &instResult); err != nil {
		return fmt.Errorf("parse instantiate response: %w", err)
	}
	fmt.Println(instResult.Contract)
	return nil
}

// --- Read-only / Unsigned Commands ---

func cmdQuery(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: query <contract> '<msg>'")
	}
	req := map[string]any{
		"contract": args[0],
		"msg":      json.RawMessage(args[1]),
	}
	if flagGasLimit != 500_000_000_000 {
		req["gas_limit"] = flagGasLimit
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	resp, err := httpClient.Post(serverURL+"/query", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdBalance(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: balance <address>")
	}
	resp, err := httpClient.Get(serverURL + "/balance/" + args[0])
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	var result struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Println(result.Balance)
	return nil
}

func cmdFaucet(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: faucet <address> <amount>")
	}
	if _, ok := new(big.Int).SetString(args[1], 10); !ok {
		return fmt.Errorf("bad amount: %q is not a valid integer", args[1])
	}
	body, err := json.Marshal(map[string]any{
		"address": args[0],
		"amount":  args[1],
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	resp, err := httpClient.Post(serverURL+"/faucet", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if _, err := readResponse(resp); err != nil {
		return err
	}
	return nil
}

func cmdEvents(args []string) error {
	var params []string
	for len(args) >= 2 {
		switch args[0] {
		case "--contract":
			params = append(params, "contract="+args[1])
			args = args[2:]
		case "--type":
			params = append(params, "type="+args[1])
			args = args[2:]
		case "--sender":
			params = append(params, "sender="+args[1])
			args = args[2:]
		case "--limit":
			params = append(params, "limit="+args[1])
			args = args[2:]
		default:
			return fmt.Errorf("unknown flag: %s", args[0])
		}
	}
	u := serverURL + "/events"
	if len(params) > 0 {
		u += "?" + strings.Join(params, "&")
	}
	resp, err := httpClient.Get(u)
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdListCodes() error {
	resp, err := httpClient.Get(serverURL + "/codes")
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdListContracts() error {
	resp, err := httpClient.Get(serverURL + "/contracts")
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdInfo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: info <address>")
	}
	resp, err := httpClient.Get(serverURL + "/contract/" + args[0])
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdAccount(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: account <address>")
	}
	resp, err := httpClient.Get(serverURL + "/account/" + args[0])
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdStatus() error {
	resp, err := httpClient.Get(serverURL + "/status")
	if err != nil {
		return err
	}
	raw, err := readResponse(resp)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}
