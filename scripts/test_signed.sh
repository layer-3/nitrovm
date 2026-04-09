#!/usr/bin/env bash
# E2E test for transaction signing: keygen, faucet, signed store/instantiate/execute.
set -euo pipefail

DATA_DIR=$(mktemp -d)
KEY_DIR=$(mktemp -d)
WASM="contracts/token/target/wasm32-unknown-unknown/release/token.wasm"
PORT=26658

NITROLITE="./cmd/nitrolite/nitrolite"
NITROCTL="./cmd/nitroctl/nitroctl"

export NITRO_SERVER="http://localhost:$PORT"
export NITRO_KEYFILE="$KEY_DIR/key"

cleanup() {
    [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    rm -rf "$DATA_DIR" "$KEY_DIR"
}
trap cleanup EXIT

echo "=== Building binaries ==="
CGO_ENABLED=1 go build -o "$NITROLITE" ./cmd/nitrolite
CGO_ENABLED=1 go build -o "$NITROCTL" ./cmd/nitroctl

echo "=== Starting nitrolite (data: $DATA_DIR) ==="
$NITROLITE --data-dir "$DATA_DIR" --addr ":$PORT" &
SERVER_PID=$!
sleep 1

if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "FAIL: server did not start"
    exit 1
fi

echo "=== Generating keypair ==="
KEYGEN_OUT=$($NITROCTL keygen)
echo "$KEYGEN_OUT"
ADDRESS=$($NITROCTL address)
echo "Address: $ADDRESS"

# Verify address format.
[[ "$ADDRESS" =~ ^0x[0-9a-f]{40}$ ]] || { echo "FAIL: bad address format: $ADDRESS"; exit 1; }

echo "=== Funding account via faucet ==="
$NITROCTL faucet "$ADDRESS" 1000000000000

BAL=$($NITROCTL balance "$ADDRESS")
echo "Balance: $BAL"
echo "$BAL" | grep -q '1000000000000' || { echo "FAIL: expected balance 1000000000000"; exit 1; }

echo "=== Signed store ==="
CODE_ID=$($NITROCTL store "$WASM")
echo "Code ID: $CODE_ID"
[ -n "$CODE_ID" ] || { echo "FAIL: empty code ID"; exit 1; }

echo "=== Check nonce after store ==="
ACCT=$($NITROCTL account "$ADDRESS")
echo "Account: $ACCT"
echo "$ACCT" | grep -q '"nonce":1' || { echo "FAIL: expected nonce=1 after store"; exit 1; }

echo "=== Signed instantiate ==="
BOB="0x0000000000000000000000000000000000000099"
CONTRACT=$($NITROCTL instantiate "$CODE_ID" \
    "{\"name\":\"Signed Token\",\"symbol\":\"SIG\",\"decimals\":18,\"initial_balances\":[{\"address\":\"$ADDRESS\",\"amount\":\"5000\"}]}" \
    "signed-token")
echo "Contract: $CONTRACT"
[ -n "$CONTRACT" ] || { echo "FAIL: empty contract address"; exit 1; }

echo "=== Check nonce after instantiate ==="
ACCT=$($NITROCTL account "$ADDRESS")
echo "Account: $ACCT"
echo "$ACCT" | grep -q '"nonce":2' || { echo "FAIL: expected nonce=2 after instantiate"; exit 1; }

echo "=== Query token balance ==="
QBAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$ADDRESS\"}}")
echo "Token balance: $QBAL"
echo "$QBAL" | grep -q '"5000"' || { echo "FAIL: expected token balance 5000"; exit 1; }

echo "=== Signed execute (transfer 200 to Bob) ==="
$NITROCTL execute "$CONTRACT" \
    "{\"transfer\":{\"recipient\":\"$BOB\",\"amount\":\"200\"}}"

echo "=== Check nonce after execute ==="
ACCT=$($NITROCTL account "$ADDRESS")
echo "Account: $ACCT"
echo "$ACCT" | grep -q '"nonce":3' || { echo "FAIL: expected nonce=3 after execute"; exit 1; }

echo "=== Verify transfer ==="
QBAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$BOB\"}}")
echo "Bob token balance: $QBAL"
echo "$QBAL" | grep -q '"200"' || { echo "FAIL: expected Bob=200"; exit 1; }

QBAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$ADDRESS\"}}")
echo "Sender token balance: $QBAL"
echo "$QBAL" | grep -q '"4800"' || { echo "FAIL: expected sender=4800"; exit 1; }

echo "=== Signed deploy (store+instantiate in one step) ==="
CONTRACT2=$($NITROCTL deploy "$WASM" \
    "{\"name\":\"Deploy Token\",\"symbol\":\"DEP\",\"decimals\":18,\"initial_balances\":[{\"address\":\"$ADDRESS\",\"amount\":\"9999\"}]}" \
    "deploy-token")
echo "Contract2: $CONTRACT2"
[ -n "$CONTRACT2" ] || { echo "FAIL: empty contract from deploy"; exit 1; }

echo "=== Check nonce after deploy (store+instantiate = +2) ==="
ACCT=$($NITROCTL account "$ADDRESS")
echo "Account: $ACCT"
echo "$ACCT" | grep -q '"nonce":5' || { echo "FAIL: expected nonce=5 after deploy"; exit 1; }

echo "=== Verify deploy contract ==="
QBAL=$($NITROCTL query "$CONTRACT2" "{\"balance\":{\"address\":\"$ADDRESS\"}}")
echo "Deploy token balance: $QBAL"
echo "$QBAL" | grep -q '"9999"' || { echo "FAIL: expected 9999 from deployed contract"; exit 1; }

echo ""
echo "=== ALL SIGNED TESTS PASSED ==="
