#!/usr/bin/env bash
set -euo pipefail

ALICE="0x0000000000000000000000000000000000000001"
BOB="0x0000000000000000000000000000000000000002"
DATA_DIR=$(mktemp -d)
WASM="contracts/token/target/wasm32-unknown-unknown/release/token.wasm"
PORT=26657

NITROLITE="./cmd/nitrolite/nitrolite"
NITROCTL="./cmd/nitroctl/nitroctl"

export NITRO_SERVER="http://localhost:$PORT"

cleanup() {
    [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    rm -rf "$DATA_DIR"
}
trap cleanup EXIT

echo "=== Building binaries ==="
CGO_ENABLED=1 go build -o "$NITROLITE" ./cmd/nitrolite
CGO_ENABLED=1 go build -o "$NITROCTL" ./cmd/nitroctl

echo "=== Starting nitrolite (data: $DATA_DIR) ==="
$NITROLITE --data-dir "$DATA_DIR" --addr ":$PORT" &
SERVER_PID=$!
sleep 1

# Verify server is up.
if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "FAIL: server did not start"
    exit 1
fi

echo "=== Storing token contract ==="
CODE_ID=$($NITROCTL store "$WASM")
echo "Code ID: $CODE_ID"

echo "=== Instantiating token (Alice=1000) ==="
CONTRACT=$($NITROCTL instantiate "$CODE_ID" "$ALICE" \
    "{\"name\":\"Yellow Token\",\"symbol\":\"YLW\",\"decimals\":18,\"initial_balances\":[{\"address\":\"$ALICE\",\"amount\":\"1000\"}]}" \
    "yellow-token")
echo "Contract: $CONTRACT"

echo "=== Query Alice balance ==="
ALICE_BAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$ALICE\"}}")
echo "Alice: $ALICE_BAL"
echo "$ALICE_BAL" | grep -q '"1000"' || { echo "FAIL: expected Alice=1000"; exit 1; }

echo "=== Query Bob balance (should be 0) ==="
BOB_BAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$BOB\"}}")
echo "Bob: $BOB_BAL"
echo "$BOB_BAL" | grep -q '"0"' || { echo "FAIL: expected Bob=0"; exit 1; }

echo "=== Alice sends 100 to Bob ==="
$NITROCTL execute "$CONTRACT" "$ALICE" \
    "{\"transfer\":{\"recipient\":\"$BOB\",\"amount\":\"100\"}}"

echo "=== Query balances after transfer ==="
ALICE_BAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$ALICE\"}}")
echo "Alice: $ALICE_BAL"
echo "$ALICE_BAL" | grep -q '"900"' || { echo "FAIL: expected Alice=900"; exit 1; }

BOB_BAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$BOB\"}}")
echo "Bob: $BOB_BAL"
echo "$BOB_BAL" | grep -q '"100"' || { echo "FAIL: expected Bob=100"; exit 1; }

echo "=== Alice sends 5 micro payments (1 each) to Bob ==="
for i in 1 2 3 4 5; do
    $NITROCTL execute "$CONTRACT" "$ALICE" \
        "{\"transfer\":{\"recipient\":\"$BOB\",\"amount\":\"1\"}}"
    echo "  payment $i/5 sent"
done

# Alice: 900 - 5 = 895, Bob: 100 + 5 = 105
ALICE_BAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$ALICE\"}}")
echo "Alice after micro payments: $ALICE_BAL"
echo "$ALICE_BAL" | grep -q '"895"' || { echo "FAIL: expected Alice=895"; exit 1; }

BOB_BAL=$($NITROCTL query "$CONTRACT" "{\"balance\":{\"address\":\"$BOB\"}}")
echo "Bob after micro payments: $BOB_BAL"
echo "$BOB_BAL" | grep -q '"105"' || { echo "FAIL: expected Bob=105"; exit 1; }

echo "=== Query token info ==="
INFO=$($NITROCTL query "$CONTRACT" '{"token_info":{}}')
echo "Info: $INFO"
echo "$INFO" | grep -q '"Yellow Token"' || { echo "FAIL: bad token name"; exit 1; }

echo ""
echo "=== ALL TESTS PASSED ==="
