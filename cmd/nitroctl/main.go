package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

var serverURL = "http://localhost:26657"

func main() {
	if u := os.Getenv("NITRO_SERVER"); u != "" {
		serverURL = u
	}

	args := os.Args[1:]
	// Consume --server flag if present.
	if len(args) >= 2 && args[0] == "--server" {
		serverURL = args[1]
		args = args[2:]
	}

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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: nitroctl [--server URL] <command> [args...]

Commands:
  store <file.wasm>                                Store contract code
  instantiate <code-id> <sender> '<msg>' [label]   Create contract instance
  execute <contract> <sender> '<msg>'              Call contract
  query <contract> '<msg>'                         Query contract (read-only)
  balance <address>                                Check YELLOW balance
  faucet <address> <amount>                        Set YELLOW balance`)
	os.Exit(1)
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
	body, _ := json.Marshal(map[string]any{
		"code_id": args[0],
		"sender":  args[1],
		"msg":     json.RawMessage(args[2]),
		"label":   label,
	})
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
	body, _ := json.Marshal(map[string]any{
		"contract": args[0],
		"sender":   args[1],
		"msg":      json.RawMessage(args[2]),
	})
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
	body, _ := json.Marshal(map[string]any{
		"contract": args[0],
		"msg":      json.RawMessage(args[1]),
	})
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
		Balance uint64 `json:"balance"`
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
	amount, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("bad amount: %w", err)
	}
	body, _ := json.Marshal(map[string]any{
		"address": args[0],
		"amount":  amount,
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
