# NitroVM Specification

## Overview

NitroVM is the smart contract execution environment for [Clearnet](https://clearnet.yellow.com), a decentralized ledger built on a distributed hash table (DHT) rather than a traditional blockchain. Clearnet provides an abstraction layer on top of existing blockchains, unifying cross-chain state without requiring inter-chain communication protocols. NitroVM runs on Clearnet nodes and provides deterministic, sandboxed execution of WebAssembly smart contracts.

YELLOW is the native token of the network.

## 1. Runtime

NitroVM executes WebAssembly modules using the [CosmWasm VM](https://github.com/CosmWasm/cosmwasm) via [wasmvm](https://github.com/CosmWasm/wasmvm) CGO bindings. All contract execution is deterministic and sandboxed.

The CosmWasm VM provides:
- **Determinism** — float instructions, SIMD, threads, and non-deterministic opcodes are rejected at upload.
- **Gas metering** — injected into WASM bytecode via wasmer middleware at basic block boundaries.
- **Memory safety** — linear memory is bounded and isolated per contract instance.

### 1.1 Gas Metering

Every WASM instruction consumes gas, paid in YELLOW (the native token). A transaction specifies a gas limit; execution halts if the limit is exceeded.

| Operation             | Cost        |
|-----------------------|-------------|
| WASM instruction      | 1           |
| Storage read          | 200         |
| Storage write         | 5 000       |
| Hash (per byte)       | 3           |
| Signature verify      | 3 000       |
| Store code (per byte) | 420 000     |

> Costs are preliminary and subject to tuning.

### 1.2 Block Context

The runtime maintains a block context comprising:

| Field       | Description                                    |
|-------------|------------------------------------------------|
| BlockHeight | Monotonically increasing block number          |
| BlockTime   | Unix timestamp in nanoseconds                  |
| OpSeq       | Operation sequence counter, incremented per tx |

Contracts receive the current block height and time in their `Env` parameter. The operation sequence is used for event ordering.

## 2. Accounts and Addresses

Addresses are 20-byte values displayed as EVM-style hex (`0x` prefix, 40 hex chars). Both externally owned accounts (EOAs) and contracts share the same address space.

Each account has:

| Field    | Description                                    |
|----------|------------------------------------------------|
| Address  | 20-byte identifier                             |
| Balance  | YELLOW token balance (uint256)                 |
| Nonce    | Transaction counter (EOA only)                 |
| CodeHash | Hash of stored WASM bytecode (contracts only)  |

## 3. Contract Lifecycle

### 3.1 Store

Upload compiled WASM bytecode to the runtime. Returns a **code ID** (content-addressed SHA-256 hash of the bytecode). The same bytecode is stored only once. Each code ID is additionally assigned a sequential integer for use by `WasmMsg::Instantiate` (see §3.5).

```
StoreCode(wasm []byte, sender Address, nonce uint64) -> (codeID, gasUsed)
```

Gas cost: 420 000 per byte of WASM input.

### 3.2 Instantiate

Create a new contract instance from a stored code ID. Assigns a deterministic address, initializes storage, and calls the contract's `instantiate` entry point with the provided init message.

```
Instantiate(codeID, sender, msg, label, funds, gasLimit, nonce) -> InstantiateResult
```

**Address derivation:** The contract address is the last 20 bytes of `SHA-256(creator || codeID || instanceCounter)`, where `instanceCounter` is a global counter incremented on each instantiation.

Returns:

| Field           | Description                                           |
|-----------------|-------------------------------------------------------|
| ContractAddress | Deterministic address (see above)                     |
| GasUsed         | Gas consumed during instantiation                     |
| Data            | Optional binary data returned by the contract         |
| Attributes      | Key-value attributes emitted by the contract          |
| Events          | Typed events emitted by the contract                  |

The caller MAY attach YELLOW tokens (`funds`) that are transferred to the new contract's balance before the entry point is called.

### 3.3 Execute

Invoke a contract function by sending a message to a contract address.

```
Execute(contract, sender, msg, funds, gasLimit, nonce) -> ExecuteResult
```

Returns:

| Field      | Description                                           |
|------------|-------------------------------------------------------|
| GasUsed    | Gas consumed during execution                         |
| Data       | Optional binary data returned by the contract         |
| Attributes | Key-value attributes emitted by the contract          |
| Events     | Typed events emitted by the contract                  |

The caller MAY attach YELLOW tokens. Any sub-messages returned in the contract response are dispatched after execution (see §3.5).

### 3.4 Query

Read-only call. Does not modify state. No gas cost to the caller (but internally metered to prevent abuse).

```
Query(contract, msg, gasLimit) -> (data, gasUsed)
```

### 3.5 Sub-Message Dispatch

Contracts communicate with other contracts and the runtime by returning **sub-messages** in their `Response`. After a contract's `execute` or `instantiate` entry point returns, NitroVM dispatches each sub-message in order.

Supported message types:

| Message                | Description                                                   |
|------------------------|---------------------------------------------------------------|
| `BankMsg::Send`        | Transfer YELLOW tokens from the contract to an address        |
| `WasmMsg::Execute`     | Call another contract's `execute` entry point                 |
| `WasmMsg::Instantiate` | Spawn a new contract instance from a stored code ID           |

The sender of a dispatched sub-message is the calling contract's address. Sub-messages MAY themselves return further sub-messages, enabling chains of cross-contract calls.

`WasmMsg::Instantiate` references codes by sequential uint64 ID (assigned in upload order), matching the CosmWasm convention.

**Recursion limit:** Dispatch depth MUST NOT exceed **10**. Exceeding this limit aborts the transaction.

Events emitted by sub-calls are collected and appended to the parent execution result.

### 3.6 Reply Callbacks

Contracts MAY react to the outcome of their sub-messages using the `ReplyOn` field and the `reply` entry point. Each sub-message carries an `ID` (arbitrary uint64 chosen by the contract) and an optional `Payload` (opaque bytes, max 128 KiB).

| ReplyOn        | On Success          | On Error                       |
|----------------|---------------------|--------------------------------|
| `ReplyNever`   | skip reply          | abort transaction              |
| `ReplySuccess` | invoke reply        | abort transaction              |
| `ReplyError`   | skip reply          | rollback state + invoke reply  |
| `ReplyAlways`  | invoke reply        | rollback state + invoke reply  |

The `Reply` object passed to the entry point contains:

| Field     | Description                                                        |
|-----------|--------------------------------------------------------------------|
| `id`      | The `SubMsg.ID` — matches which sub-message triggered the reply    |
| `payload` | Echo of `SubMsg.Payload`                                           |
| `result`  | `Ok { events, data }` on success, or `Err "message"` on failure   |
| `gas_used`| Gas consumed by the sub-message                                    |

**Error catching:** When `ReplyOn` is `ReplyError` or `ReplyAlways` and the sub-message fails, NitroVM MUST roll back all state changes from the failed sub-message (both in-memory balances and contract storage via savepoints) before invoking `reply`.

**Data propagation:** If the `reply` handler sets `Data` in its response, it MUST replace the parent execution's `Data` field.

**Reply chaining:** The `reply` handler itself returns a `Response` that MAY contain further sub-messages. These are dispatched recursively, subject to the same depth limit.

**Reply errors are not catchable:** If the `reply` entry point itself returns an error, the entire transaction MUST abort.

## 4. Storage

### 4.1 Model

Each contract has isolated key-value storage. Keys and values are arbitrary byte slices.

| Operation | Signature                                        |
|-----------|--------------------------------------------------|
| Get       | `storage_get(key) -> value`                      |
| Set       | `storage_set(key, value)`                        |
| Delete    | `storage_delete(key)`                            |
| Range     | `storage_range(start, end, order) -> iterator`   |

### 4.2 Adapter Interface

Storage is abstracted behind an adapter interface to support multiple backends:

| Method | Description                                               |
|--------|-----------------------------------------------------------|
| Get    | Read a value by contract address and key                  |
| Set    | Write a value by contract address and key                 |
| Delete | Remove a key by contract address                          |
| Range  | Ordered iteration over a key range (ascending/descending) |
| Close  | Release backend resources                                 |

### 4.3 Transactional Storage

Backends that support transactional semantics expose savepoint operations:

| Method          | Description                                        |
|-----------------|----------------------------------------------------|
| Savepoint       | Create a named savepoint                           |
| RollbackTo      | Roll back all changes since the named savepoint    |
| ReleaseSavepoint| Commit a savepoint (merge into parent transaction) |

Savepoints are used by the reply mechanism (§3.6) to roll back failed sub-message state changes.

### 4.4 Backends

| Backend | Persistence | Transaction Support | Use Case                  |
|---------|-------------|---------------------|---------------------------|
| SQLite  | Disk        | Yes (SAVEPOINT)     | Production                |
| Memory  | None        | Yes (snapshot/copy) | Testing                   |

## 5. Host Functions

CosmWasm-compatible host imports provided by NitroVM. All data crosses the WASM boundary via `Region` structs in linear memory (offset, capacity, length).

### 5.1 Storage

- `env.db_read(key_ptr) -> val_ptr`
- `env.db_write(key_ptr, val_ptr)`
- `env.db_remove(key_ptr)`
- `env.db_scan(start_ptr, end_ptr, order) -> iterator_id`
- `env.db_next(iterator_id) -> kv_pair_ptr`

### 5.2 Address Handling

- `env.addr_validate(addr_ptr) -> u32` — Validate a hex address string
- `env.addr_canonicalize(human_ptr, canonical_ptr) -> u32` — `"0x..."` to 20-byte canonical
- `env.addr_humanize(canonical_ptr, human_ptr) -> u32` — 20-byte canonical to `"0x..."`

### 5.3 Crypto

Implemented natively by the CosmWasm VM (libwasmvm) via the `cosmwasm-crypto` crate. No host-side implementation is required.

- `env.secp256k1_verify(msg_hash_ptr, sig_ptr, pubkey_ptr) -> u32`
- `env.secp256k1_recover_pubkey(msg_hash_ptr, sig_ptr, recovery_param) -> u64`
- `env.ed25519_verify(msg_ptr, sig_ptr, pubkey_ptr) -> u32`
- `env.ed25519_batch_verify(msgs_ptr, sigs_ptr, pubkeys_ptr) -> u32`

### 5.4 Chain Queries

- `env.query_chain(request_ptr) -> response_ptr`

Supported query types:

| Query                    | Description                                                      |
|--------------------------|------------------------------------------------------------------|
| `BankQuery::Balance`     | Get balance of a single denom for an address                     |
| `BankQuery::AllBalances` | Get all YELLOW token balances for an address                     |
| `WasmQuery::Smart`       | Call another contract's `query` entry point (cross-contract read) |

### 5.5 Debug

- `env.debug(msg_ptr)` — Print debug message (disabled in production)
- `env.abort(msg_ptr, msg_len, file_ptr, file_len)` — Abort execution with error

## 6. Contract ABI

Contracts are written in Rust using the [cosmwasm-std](https://crates.io/crates/cosmwasm-std) crate and compiled to `wasm32-unknown-unknown`.

Required exports:

```
instantiate(env_ptr, info_ptr, msg_ptr) -> result_ptr
execute(env_ptr, info_ptr, msg_ptr) -> result_ptr
query(env_ptr, msg_ptr) -> result_ptr
reply(env_ptr, msg_ptr) -> result_ptr       // optional — required if using ReplyOn
allocate(size) -> ptr
deallocate(ptr)
```

- `env_ptr` — block height, timestamp, contract address
- `info_ptr` — sender address, attached funds
- `msg_ptr` — JSON-encoded contract message

`allocate`/`deallocate` are used by the host to manage memory regions for passing data across the WASM boundary.

## 7. Building Contracts

### 7.1 Prerequisites

- **Rust** with the `wasm32-unknown-unknown` target:
  ```
  rustup target add wasm32-unknown-unknown
  ```
- **wasm-opt** from [Binaryen](https://github.com/WebAssembly/binaryen):
  ```
  brew install binaryen      # macOS
  apt install binaryen        # Debian/Ubuntu
  ```

### 7.2 Contract Structure

A minimal contract lives under `contracts/<name>/` with this layout:

```
contracts/<name>/
├── .cargo/
│   └── config.toml          # WASM linker flags
├── Cargo.toml
└── src/
    ├── lib.rs                # Re-exports modules
    ├── msg.rs                # InstantiateMsg, ExecuteMsg, QueryMsg, responses
    ├── state.rs              # Storage helpers (raw cosmwasm or cw-storage-plus)
    ├── error.rs              # ContractError enum
    └── contract.rs           # Entry points: instantiate, execute, query
```

### 7.3 Cargo.toml

```toml
[package]
name = "<contract-name>"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib", "rlib"]

[dependencies]
cosmwasm-std = { version = "2", default-features = false, features = ["std"] }
cosmwasm-schema = "2"
schemars = "0.8"
serde = { version = "1", default-features = false, features = ["derive"] }
thiserror = "2"

[profile.release]
opt-level = "z"
debug = false
lto = true
codegen-units = 1
panic = "abort"
strip = true
overflow-checks = true
```

Key settings:
- `crate-type = ["cdylib"]` — produces a WASM module with the correct exports.
- `opt-level = "z"` + `lto = true` + `strip = true` — minimizes binary size.
- `panic = "abort"` — avoids unwinding machinery that bloats WASM.

### 7.4 Cargo Config

Place in `contracts/<name>/.cargo/config.toml`:

```toml
[target.wasm32-unknown-unknown]
rustflags = ["-C", "link-arg=-s"]
```

### 7.5 Build & Post-Process

**Step 1 — Compile to WASM:**

```
cargo build --release --target wasm32-unknown-unknown \
  --manifest-path contracts/<name>/Cargo.toml
```

Output: `contracts/<name>/target/wasm32-unknown-unknown/release/<name>.wasm`

**Step 2 — Lower bulk-memory operations:**

wasmvm's wasmer engine does not support WebAssembly bulk-memory instructions (`memory.copy`, `memory.fill`). Rust 1.82+ emits these by default for `wasm32-unknown-unknown`. Use `wasm-opt` to lower them to MVP-compatible loops:

```
wasm-opt \
  --enable-bulk-memory --enable-sign-ext --enable-mutable-globals \
  --llvm-memory-copy-fill-lowering \
  -Oz \
  <input>.wasm -o <output>.wasm
```

- `--enable-bulk-memory` — allows wasm-opt to parse the input containing bulk-memory ops.
- `--llvm-memory-copy-fill-lowering` — replaces `memory.copy`/`memory.fill` with scalar loops and disables the bulk-memory feature flag in the output.
- `-Oz` — optimizes for size.

**Both steps combined (Makefile):**

```make
build-contract:
	cargo build --release --target wasm32-unknown-unknown \
	  --manifest-path contracts/<name>/Cargo.toml
	wasm-opt --enable-bulk-memory --enable-sign-ext --enable-mutable-globals \
	  --llvm-memory-copy-fill-lowering -Oz \
	  contracts/<name>/target/wasm32-unknown-unknown/release/<name>.wasm \
	  -o contracts/<name>/target/wasm32-unknown-unknown/release/<name>.wasm
```

### 7.6 Verification

Load the WASM into NitroVM via `StoreCode`. If the binary still contains unsupported instructions, `StoreCode` returns a validation error. A successful store confirms the contract is ready for instantiation.

## 8. Transaction Signing

### 8.1 Overview

State-changing operations (store, instantiate, execute) support cryptographically signed transactions. The signer's address is derived from their secp256k1 public key — it is never passed explicitly.

### 8.2 Signing Algorithm

- **Curve:** secp256k1 ECDSA (same as Ethereum)
- **Hash:** keccak256 (Ethereum-compatible)
- **Address derivation:** `keccak256(uncompressed_pubkey[1:])[12:]` — last 20 bytes of the hash of the 64-byte public key (without the `0x04` prefix)

### 8.3 Transaction Envelope

All transactions share a single RLP-encoded structure:

| Field    | Type      | Description                                       |
|----------|-----------|---------------------------------------------------|
| ChainID  | string    | Chain identifier for replay protection            |
| Nonce    | uint64    | Sender's sequential nonce                         |
| GasLimit | uint64    | Maximum gas for this transaction                  |
| GasPrice | uint64    | Price per unit of gas (in smallest YELLOW unit)   |
| Type     | uint8     | 1=Store, 2=Instantiate, 3=Execute                |
| Code     | bytes     | WASM bytecode (Store only)                        |
| CodeID   | bytes     | Code hash (Instantiate only)                      |
| Label    | string    | Contract label (Instantiate only)                 |
| Contract | [20]byte  | Target contract address (Execute only)            |
| Msg      | bytes     | JSON message (Instantiate, Execute)               |
| Funds    | []Coin    | Attached funds as `[[denom, amount], ...]`        |

Fields irrelevant to the transaction type encode as zero-values.

### 8.4 Signing Process

1. RLP-encode the transaction fields in the order above
2. Compute `hash = keccak256(rlp_bytes)`
3. Sign `hash` with the sender's secp256k1 private key → (V, R, S)
4. Transmit as: `RLP(tx) || V (1 byte) || R (32 bytes) || S (32 bytes)`

### 8.5 Server Verification

The server detects signed requests by the presence of a `"tx"` field in the JSON body:

```json
{"tx": "<hex-encoded signed tx bytes>"}
```

Verification steps:
1. Hex-decode and split into RLP bytes + signature (last 65 bytes)
2. RLP-decode the transaction
3. Recover the public key via `ecrecover(hash, V, R, S)`
4. Derive address from recovered public key
5. Validate: chain ID MUST match server's chain
6. Validate: gas price MUST be >= server's minimum gas price
7. Validate: nonce MUST equal sender's current nonce

### 8.6 Gas Fees

After successful execution, the gas fee is deducted from the signer's balance:

```
gas_fee = gas_used * gas_price
```

If the signer cannot afford the gas fee, the entire transaction MUST be rolled back (VM snapshot + storage savepoint). A gas price of 0 means no fee deduction.

### 8.7 Nonce Rules

- Every sender address has a sequential nonce starting at 0.
- Each signed transaction MUST include a nonce equal to the sender's current nonce.
- The nonce is incremented after successful execution.
- Re-submitting a transaction with a used nonce MUST be rejected.

### 8.8 Key Management

The CLI stores private keys as hex-encoded 32-byte files at `~/.nitrovm/key` (overridable via `--keyfile` flag or `NITRO_KEYFILE` env). Generate with `nitroctl keygen`.

### 8.9 Legacy Mode

When the server runs without `--require-sig`, both signed and unsigned requests are accepted. Unsigned requests use the legacy JSON format with an explicit `sender` field. When `--require-sig` is set, only signed requests are accepted for store, instantiate, and execute endpoints.

## 9. HTTP API

The server exposes a JSON-over-HTTP API. All request and response bodies are JSON-encoded.

### 9.1 State-Changing Endpoints

These endpoints accept either a signed transaction (`{"tx": "<hex>"}`) or, in legacy mode, a plain JSON body with an explicit `sender` field.

| Endpoint       | Method | Description                              |
|----------------|--------|------------------------------------------|
| `/store`       | POST   | Upload WASM bytecode                     |
| `/instantiate` | POST   | Create a contract instance               |
| `/execute`     | POST   | Call a contract function                  |

**POST /store**

Request (signed): `{"tx": "<hex>"}`

Response:
```json
{
  "code_id": "<hex checksum>",
  "code_seq": 1,
  "gas_used": 100000,
  "sender": "0x...",
  "gas_fee": "100000"
}
```

**POST /instantiate**

Request (signed): `{"tx": "<hex>"}`

Response:
```json
{
  "contract": "0x...",
  "gas_used": 50000,
  "sender": "0x...",
  "data": "<base64>",
  "attributes": [{"key": "...", "value": "..."}],
  "events": [{"type": "...", "attributes": [...]}],
  "gas_fee": "50000"
}
```

**POST /execute**

Request (signed): `{"tx": "<hex>"}`

Response:
```json
{
  "data": "<base64>",
  "gas_used": 30000,
  "sender": "0x...",
  "attributes": [{"key": "...", "value": "..."}],
  "events": [{"type": "...", "attributes": [...]}],
  "gas_fee": "30000"
}
```

### 9.2 Read-Only Endpoints

| Endpoint              | Method | Description                                 |
|-----------------------|--------|---------------------------------------------|
| `/query`              | POST   | Query a contract (read-only)                |
| `/balance/<address>`  | GET    | Get YELLOW balance for an address           |
| `/account/<address>`  | GET    | Get account info (balance, nonce, contract) |
| `/status`             | GET    | Server status (chain_id, block_height, counts) |
| `/codes`              | GET    | List all stored code IDs                    |
| `/contracts`          | GET    | List all contract instances                 |
| `/contract/<address>` | GET    | Get contract info (code_id, label, creator) |
| `/events`             | GET    | Query events (filterable)                   |
| `/simulate`           | POST   | Dry-run execute or instantiate              |

**POST /query**

Request:
```json
{
  "contract": "0x...",
  "msg": {}
}
```

Response:
```json
{
  "data": {},
  "gas_used": 5000
}
```

**GET /events**

Query parameters:

| Parameter  | Description                           |
|------------|---------------------------------------|
| `contract` | Filter by contract address            |
| `type`     | Filter by event type                  |
| `sender`   | Filter by sender address              |
| `limit`    | Maximum number of events to return    |

Response:
```json
{
  "events": [
    {
      "id": 1,
      "op_seq": 1,
      "tx_type": "execute",
      "contract": "0x...",
      "sender": "0x...",
      "event_type": "transfer",
      "attributes": [{"key": "...", "value": "..."}],
      "created_at": 1234567890
    }
  ]
}
```

**POST /simulate**

Performs a dry-run of an execute or instantiate operation without persisting state changes. The request format matches `/execute` or `/instantiate`. Returns the same response shape including gas used, attributes, and events.

### 9.3 Faucet

| Endpoint  | Method | Description                              |
|-----------|--------|------------------------------------------|
| `/faucet` | POST   | Set an account's YELLOW balance          |

Available only when the server is running in **devnet** mode. Request:

```json
{
  "address": "0x...",
  "amount": "1000000"
}
```

## 10. CLI (`nitroctl`)

### 10.1 Global Flags

| Flag          | Default               | Env Var          | Description                    |
|---------------|-----------------------|------------------|--------------------------------|
| `--server`    | `http://localhost:26657` | `NITRO_SERVER` | Server URL                     |
| `--keyfile`   | `~/.nitrovm/key`      | `NITRO_KEYFILE`  | Path to private key file       |
| `--gas-limit` | (none)                |                  | Gas limit for the transaction  |
| `--gas-price` | `1`                   |                  | Gas price per unit             |
| `--nonce`     | (auto-fetched)        |                  | Override sender nonce          |
| `--funds`     | (none)                |                  | Attach funds (e.g. `100YELLOW`) |

### 10.2 Commands

**Key management:**

| Command   | Description                                   |
|-----------|-----------------------------------------------|
| `keygen`  | Generate a new secp256k1 keypair, save to keyfile |
| `address` | Display the address derived from the keyfile   |

**Contract operations (signed):**

| Command                            | Description                                |
|------------------------------------|--------------------------------------------|
| `store <file.wasm>`               | Upload WASM bytecode                        |
| `instantiate <code-id> '<msg>' [label]` | Create a contract instance            |
| `execute <contract> '<msg>'`       | Call a contract function                   |
| `deploy <file.wasm> '<msg>' [label]` | Store + instantiate in two transactions  |

**Read-only operations (unsigned):**

| Command                  | Description                         |
|--------------------------|-------------------------------------|
| `query <contract> '<msg>'` | Query a contract                  |
| `balance <address>`      | Check YELLOW balance                |
| `account <address>`      | Show account info (balance, nonce)  |
| `status`                 | Show server status                  |
| `list-codes`             | List all stored code IDs            |
| `list-contracts`         | List all contract instances         |
| `info <address>`         | Show contract details               |
| `events [filters]`       | Query events (--contract, --type, --sender, --limit) |

**Devnet only:**

| Command                    | Description                      |
|----------------------------|----------------------------------|
| `faucet <address> <amount>` | Set an account's YELLOW balance |

## 11. Persistence

### 11.1 State Recovery

The server persists all state to disk so that it can be restored across restarts. Two stores are maintained:

| Store           | Contents                                                     |
|-----------------|--------------------------------------------------------------|
| Metadata DB     | Stored codes (bytecode), code sequences, contract registry, account balances/nonces, events, global counters |
| Contract KV DB  | Per-contract key-value storage                               |

On startup, the server replays stored codes into the CosmWasm VM, rebuilds the contract registry, and restores all account state. No re-execution of historical transactions is required.

### 11.2 Event Log

All events emitted during contract execution (attributes and typed events) are persisted with their operation sequence number, transaction type, contract address, and sender. Events are queryable via the `/events` endpoint with filtering by contract, event type, and sender.

## 12. Configuration

### 12.1 Server Flags

| Flag              | Default        | Description                                      |
|-------------------|----------------|--------------------------------------------------|
| `--addr`          | `:26657`       | Listen address                                   |
| `--data-dir`      | `~/.nitrovm`   | Data directory for databases                     |
| `--chain-id`      | `nitrovm-1`    | Chain identifier (used in tx signing)            |
| `--network`       | `devnet`       | Network mode: `devnet`, `testnet`, or `mainnet`  |
| `--require-sig`   | `false`        | Reject unsigned state-changing requests          |
| `--min-gas-price` | `0`            | Minimum gas price for signed transactions        |
| `--memory-limit`  | `256`          | WASM memory limit in MB                          |
| `--cache-size`    | `100`          | Compiled WASM cache size in MB                   |
| `--print-debug`   | `false`        | Enable contract debug output                     |

### 12.2 Network Modes

| Mode      | Faucet | Require Sig | Description                         |
|-----------|--------|-------------|-------------------------------------|
| `devnet`  | Yes    | Optional    | Local development, faucet enabled   |
| `testnet` | No     | Optional    | Public test environment             |
| `mainnet` | No     | Optional    | Production environment              |

## 13. Future Work

- Contract migration and admin controls (`WasmMsg::Migrate`)
