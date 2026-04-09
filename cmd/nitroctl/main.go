package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/layer-3/nitrovm"
)

var (
	serverURL    = "http://localhost:26657"
	flagGasLimit uint64 = 500_000_000_000
	flagNonce    *uint64
	flagFunds    string
	flagKeyfile  string
	flagGasPrice uint64 = 1
)

// doGet performs an HTTP GET (abstracted for fetchNonce/fetchChainID).
func doGet(url string) (*http.Response, error) {
	return http.Get(url)
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
  store <file.wasm>                                Store contract code (signed)
  instantiate <code-id> '<msg>' [label]            Create contract instance (signed)
  execute <contract> '<msg>'                       Call contract (signed)
  query <contract> '<msg>'                         Query contract (read-only)
  deploy <file.wasm> '<msg>' [label]               Store + instantiate (signed)
  balance <address>                                Check YELLOW balance
  account <address>                                Show full account info
  faucet <address> <amount>                        Set YELLOW balance
  events [--contract addr] [--type t] [--sender s] [--limit n]  Query events
  list-codes                                       List stored code IDs
  list-contracts                                   List all contracts
  info <address>                                   Show contract details
  status                                           Show server status

When a keyfile exists, store/instantiate/execute/deploy auto-sign transactions.
The sender address is derived from the private key — no explicit sender arg needed.`)
	os.Exit(1)
}

// tryLoadKey attempts to load the private key, returning nil if no keyfile.
func tryLoadKey() *secp256k1.PrivateKey {
	path := flagKeyfile
	if path == "" {
		path = getDefaultKeyPath()
	}
	key, err := loadPrivateKey(path)
	if err != nil {
		return nil
	}
	return key
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
func getNonce(addr nitrovm.Address) (uint64, error) {
	if flagNonce != nil {
		return *flagNonce, nil
	}
	return fetchNonce(addr)
}

// parseFunds parses the --funds flag value (e.g. "100YELLOW") into RLPCoins.
func parseFundsRLP() []nitrovm.RLPCoin {
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
	return []nitrovm.RLPCoin{{Denom: flagFunds[i:], Amount: flagFunds[:i]}}
}

// parseFunds parses the --funds flag value (e.g. "100YELLOW") into a JSON-ready slice.
func parseFunds() []map[string]string {
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
	return []map[string]string{{"denom": flagFunds[i:], "amount": flagFunds[:i]}}
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
	addr := nitrovm.DeriveAddress(key)

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
	fmt.Println(nitrovm.DeriveAddress(key).Hex())
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

	key := tryLoadKey()
	if key != nil {
		// Signed mode.
		sender := nitrovm.DeriveAddress(key)
		chainID, err := fetchChainID()
		if err != nil {
			return fmt.Errorf("fetch chain ID: %w", err)
		}
		nonce, err := getNonce(sender)
		if err != nil {
			return fmt.Errorf("fetch nonce: %w", err)
		}
		tx := &nitrovm.Transaction{
			ChainID:  chainID,
			Nonce:    nonce,
			GasLimit: flagGasLimit,
			GasPrice: flagGasPrice,
			Type:     nitrovm.TxStore,
			Code:     wasm,
		}
		body, err := signAndMarshal(tx, key)
		if err != nil {
			return err
		}
		resp, err := http.Post(serverURL+"/store", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var errCheck struct{ Error string `json:"error"` }
		json.Unmarshal(raw, &errCheck)
		if errCheck.Error != "" {
			return fmt.Errorf("%s", errCheck.Error)
		}
		var result struct{ CodeID string `json:"code_id"` }
		json.Unmarshal(raw, &result)
		fmt.Println(result.CodeID)
		return nil
	}

	// Legacy unsigned mode.
	resp, err := http.Post(serverURL+"/store", "application/wasm", bytes.NewReader(wasm))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		CodeID string `json:"code_id"`
		Error  string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Error != "" {
		return fmt.Errorf("%s", result.Error)
	}
	fmt.Println(result.CodeID)
	return nil
}

func cmdInstantiate(args []string) error {
	key := tryLoadKey()

	if key != nil {
		// Signed mode: instantiate <code-id> '<msg>' [label]
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

		sender := nitrovm.DeriveAddress(key)
		chainID, err := fetchChainID()
		if err != nil {
			return fmt.Errorf("fetch chain ID: %w", err)
		}
		nonce, err := getNonce(sender)
		if err != nil {
			return fmt.Errorf("fetch nonce: %w", err)
		}
		tx := &nitrovm.Transaction{
			ChainID:  chainID,
			Nonce:    nonce,
			GasLimit: flagGasLimit,
			GasPrice: flagGasPrice,
			Type:     nitrovm.TxInstantiate,
			CodeID:   codeID,
			Label:    label,
			Msg:      msg,
			Funds:    parseFundsRLP(),
		}
		body, err := signAndMarshal(tx, key)
		if err != nil {
			return err
		}
		resp, err := http.Post(serverURL+"/instantiate", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var errCheck struct{ Error string `json:"error"` }
		json.Unmarshal(raw, &errCheck)
		if errCheck.Error != "" {
			return fmt.Errorf("%s", errCheck.Error)
		}
		var result struct{ Contract string `json:"contract"` }
		json.Unmarshal(raw, &result)
		fmt.Println(result.Contract)
		return nil
	}

	// Legacy mode: instantiate <code-id> <sender> '<msg>' [label]
	if len(args) < 3 {
		return fmt.Errorf("usage: instantiate <code-id> <sender> '<msg>' [label]")
	}
	label := "default"
	if len(args) >= 4 {
		label = args[3]
	}
	req := map[string]any{
		"code_id": args[0],
		"sender":  args[1],
		"msg":     json.RawMessage(args[2]),
		"label":   label,
	}
	if flagGasLimit != 500_000_000_000 {
		req["gas_limit"] = flagGasLimit
	}
	if flagNonce != nil {
		req["nonce"] = *flagNonce
	}
	if funds := parseFunds(); funds != nil {
		req["funds"] = funds
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(serverURL+"/instantiate", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Contract string `json:"contract"`
		Error    string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Error != "" {
		return fmt.Errorf("%s", result.Error)
	}
	fmt.Println(result.Contract)
	return nil
}

func cmdExecute(args []string) error {
	key := tryLoadKey()

	if key != nil {
		// Signed mode: execute <contract> '<msg>'
		if len(args) < 2 {
			return fmt.Errorf("usage: execute <contract> '<msg>'")
		}
		contract, err := nitrovm.HexToAddress(args[0])
		if err != nil {
			return fmt.Errorf("bad contract: %w", err)
		}
		msg := []byte(args[1])

		sender := nitrovm.DeriveAddress(key)
		chainID, err := fetchChainID()
		if err != nil {
			return fmt.Errorf("fetch chain ID: %w", err)
		}
		nonce, err := getNonce(sender)
		if err != nil {
			return fmt.Errorf("fetch nonce: %w", err)
		}
		tx := &nitrovm.Transaction{
			ChainID:  chainID,
			Nonce:    nonce,
			GasLimit: flagGasLimit,
			GasPrice: flagGasPrice,
			Type:     nitrovm.TxExecute,
			Contract: contract,
			Msg:      msg,
			Funds:    parseFundsRLP(),
		}
		body, err := signAndMarshal(tx, key)
		if err != nil {
			return err
		}
		resp, err := http.Post(serverURL+"/execute", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var errCheck struct{ Error string `json:"error"` }
		json.Unmarshal(raw, &errCheck)
		if errCheck.Error != "" {
			return fmt.Errorf("%s", errCheck.Error)
		}
		fmt.Println(strings.TrimSpace(string(raw)))
		return nil
	}

	// Legacy mode: execute <contract> <sender> '<msg>'
	if len(args) < 3 {
		return fmt.Errorf("usage: execute <contract> <sender> '<msg>'")
	}
	req := map[string]any{
		"contract": args[0],
		"sender":   args[1],
		"msg":      json.RawMessage(args[2]),
	}
	if flagGasLimit != 500_000_000_000 {
		req["gas_limit"] = flagGasLimit
	}
	if flagNonce != nil {
		req["nonce"] = *flagNonce
	}
	if funds := parseFunds(); funds != nil {
		req["funds"] = funds
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(serverURL+"/execute", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var errCheck struct {
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &errCheck)
	if errCheck.Error != "" {
		return fmt.Errorf("%s", errCheck.Error)
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdDeploy(args []string) error {
	key := tryLoadKey()

	if key != nil {
		// Signed mode: deploy <file.wasm> '<msg>' [label]
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

		sender := nitrovm.DeriveAddress(key)
		chainID, err := fetchChainID()
		if err != nil {
			return fmt.Errorf("fetch chain ID: %w", err)
		}
		nonce, err := getNonce(sender)
		if err != nil {
			return fmt.Errorf("fetch nonce: %w", err)
		}

		// Step 1: Store code (signed).
		storeTx := &nitrovm.Transaction{
			ChainID:  chainID,
			Nonce:    nonce,
			GasLimit: flagGasLimit,
			GasPrice: flagGasPrice,
			Type:     nitrovm.TxStore,
			Code:     wasm,
		}
		storeBody, err := signAndMarshal(storeTx, key)
		if err != nil {
			return err
		}
		resp, err := http.Post(serverURL+"/store", "application/json", bytes.NewReader(storeBody))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var storeResult struct {
			CodeID string `json:"code_id"`
			Error  string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&storeResult)
		if storeResult.Error != "" {
			return fmt.Errorf("store: %s", storeResult.Error)
		}
		codeID, _ := hex.DecodeString(storeResult.CodeID)

		// Step 2: Instantiate (signed, nonce incremented).
		instTx := &nitrovm.Transaction{
			ChainID:  chainID,
			Nonce:    nonce + 1,
			GasLimit: flagGasLimit,
			GasPrice: flagGasPrice,
			Type:     nitrovm.TxInstantiate,
			CodeID:   codeID,
			Label:    label,
			Msg:      []byte(msg),
			Funds:    parseFundsRLP(),
		}
		instBody, err := signAndMarshal(instTx, key)
		if err != nil {
			return err
		}
		resp2, err := http.Post(serverURL+"/instantiate", "application/json", bytes.NewReader(instBody))
		if err != nil {
			return err
		}
		defer resp2.Body.Close()
		var instResult struct {
			Contract string `json:"contract"`
			Error    string `json:"error"`
		}
		json.NewDecoder(resp2.Body).Decode(&instResult)
		if instResult.Error != "" {
			return fmt.Errorf("instantiate: %s", instResult.Error)
		}
		fmt.Println(instResult.Contract)
		return nil
	}

	// Legacy mode: deploy <file.wasm> <sender> '<msg>' [label]
	if len(args) < 3 {
		return fmt.Errorf("usage: deploy <file.wasm> <sender> '<msg>' [label]")
	}
	wasmPath, sender, msg := args[0], args[1], args[2]
	label := "default"
	if len(args) >= 4 {
		label = args[3]
	}

	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return err
	}
	resp, err := http.Post(serverURL+"/store", "application/wasm", bytes.NewReader(wasm))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var storeResult struct {
		CodeID string `json:"code_id"`
		Error  string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&storeResult)
	if storeResult.Error != "" {
		return fmt.Errorf("store: %s", storeResult.Error)
	}

	req := map[string]any{
		"code_id": storeResult.CodeID,
		"sender":  sender,
		"msg":     json.RawMessage(msg),
		"label":   label,
	}
	if flagGasLimit != 500_000_000_000 {
		req["gas_limit"] = flagGasLimit
	}
	if flagNonce != nil {
		req["nonce"] = *flagNonce
	}
	if funds := parseFunds(); funds != nil {
		req["funds"] = funds
	}
	body, _ := json.Marshal(req)
	resp2, err := http.Post(serverURL+"/instantiate", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	var instResult struct {
		Contract string `json:"contract"`
		Error    string `json:"error"`
	}
	json.NewDecoder(resp2.Body).Decode(&instResult)
	if instResult.Error != "" {
		return fmt.Errorf("instantiate: %s", instResult.Error)
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
	body, _ := json.Marshal(req)
	resp, err := http.Post(serverURL+"/query", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var errCheck struct {
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &errCheck)
	if errCheck.Error != "" {
		return fmt.Errorf("%s", errCheck.Error)
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdBalance(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: balance <address>")
	}
	resp, err := http.Get(serverURL + "/balance/" + args[0])
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Balance string `json:"balance"`
		Error   string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Error != "" {
		return fmt.Errorf("%s", result.Error)
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
	body, _ := json.Marshal(map[string]any{
		"address": args[0],
		"amount":  args[1],
	})
	resp, err := http.Post(serverURL+"/faucet", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Error != "" {
		return fmt.Errorf("%s", result.Error)
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
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdListCodes() error {
	resp, err := http.Get(serverURL + "/codes")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdListContracts() error {
	resp, err := http.Get(serverURL + "/contracts")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdInfo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: info <address>")
	}
	resp, err := http.Get(serverURL + "/contract/" + args[0])
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var errCheck struct {
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &errCheck)
	if errCheck.Error != "" {
		return fmt.Errorf("%s", errCheck.Error)
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdAccount(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: account <address>")
	}
	resp, err := http.Get(serverURL + "/account/" + args[0])
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var errCheck struct {
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &errCheck)
	if errCheck.Error != "" {
		return fmt.Errorf("%s", errCheck.Error)
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func cmdStatus() error {
	resp, err := http.Get(serverURL + "/status")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}
