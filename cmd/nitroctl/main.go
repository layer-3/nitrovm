package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
)

var (
	serverURL    = "http://localhost:26657"
	flagGasLimit *uint64
	flagFunds    string
)

func main() {
	if u := os.Getenv("NITRO_SERVER"); u != "" {
		serverURL = u
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
			flagGasLimit = &v
			args = args[2:]
		case "--funds":
			flagFunds = args[1]
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
  --gas-limit <n>         Gas limit for the operation
  --funds <amount><denom> Attach funds (e.g. 100YELLOW)

Commands:
  store <file.wasm>                                Store contract code
  instantiate <code-id> <sender> '<msg>' [label]   Create contract instance
  execute <contract> <sender> '<msg>'              Call contract
  query <contract> '<msg>'                         Query contract (read-only)
  balance <address>                                Check YELLOW balance
  faucet <address> <amount>                        Set YELLOW balance
  events [--contract addr] [--type t] [--sender s] [--limit n]  Query events
  list-codes                                       List stored code IDs
  list-contracts                                   List all contracts
  info <address>                                   Show contract details`)
	os.Exit(1)
}

// parseFunds parses the --funds flag value (e.g. "100YELLOW") into a JSON-ready slice.
func parseFunds() []map[string]string {
	if flagFunds == "" {
		return nil
	}
	// Split at boundary between digits and non-digits.
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

func cmdStore(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: store <file.wasm>")
	}
	wasm, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
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
	if flagGasLimit != nil {
		req["gas_limit"] = *flagGasLimit
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
	if len(args) < 3 {
		return fmt.Errorf("usage: execute <contract> <sender> '<msg>'")
	}
	req := map[string]any{
		"contract": args[0],
		"sender":   args[1],
		"msg":      json.RawMessage(args[2]),
	}
	if flagGasLimit != nil {
		req["gas_limit"] = *flagGasLimit
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

func cmdQuery(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: query <contract> '<msg>'")
	}
	req := map[string]any{
		"contract": args[0],
		"msg":      json.RawMessage(args[1]),
	}
	if flagGasLimit != nil {
		req["gas_limit"] = *flagGasLimit
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
	// Validate amount is a valid non-negative decimal integer.
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
