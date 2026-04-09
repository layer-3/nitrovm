package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/layer-3/nitrovm"
)

// getDefaultKeyPath returns ~/.nitrovm/key.
func getDefaultKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".nitrovm/key"
	}
	return filepath.Join(home, ".nitrovm", "key")
}

// loadPrivateKey reads a hex-encoded private key from a file.
func loadPrivateKey(path string) (*secp256k1.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keyfile %s: %w", path, err)
	}
	hexStr := strings.TrimSpace(string(data))
	hexStr = strings.TrimPrefix(hexStr, "0x")
	hexStr = strings.TrimPrefix(hexStr, "0X")
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("bad hex in keyfile: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("keyfile must contain 32 bytes, got %d", len(b))
	}
	return secp256k1.PrivKeyFromBytes(b), nil
}

// fetchNonce queries the server for the current nonce of an address.
func fetchNonce(addr nitrovm.Address) (uint64, error) {
	resp, err := doGet(serverURL + "/account/" + addr.Hex())
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result struct {
		Nonce uint64 `json:"nonce"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Error != "" {
		return 0, fmt.Errorf("%s", result.Error)
	}
	return result.Nonce, nil
}

// fetchChainID queries the server for the chain ID.
func fetchChainID() (string, error) {
	resp, err := doGet(serverURL + "/status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		ChainID string `json:"chain_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.ChainID, nil
}

// signAndMarshal signs a transaction and returns the JSON body for the server.
func signAndMarshal(tx *nitrovm.Transaction, key *secp256k1.PrivateKey) ([]byte, error) {
	stx, err := nitrovm.SignTx(tx, key)
	if err != nil {
		return nil, fmt.Errorf("sign tx: %w", err)
	}
	encoded, err := nitrovm.EncodeSignedTx(stx)
	if err != nil {
		return nil, fmt.Errorf("encode signed tx: %w", err)
	}
	body, err := json.Marshal(map[string]string{
		"tx": hex.EncodeToString(encoded),
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}
