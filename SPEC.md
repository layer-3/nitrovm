# NitroVM Specification

## 1. Runtime

NitroVM executes WebAssembly modules using the [CosmWasm VM](https://github.com/CosmWasm/cosmwasm) (wasmer) via [wasmvm](https://github.com/CosmWasm/wasmvm) CGO bindings. All contract execution is deterministic and sandboxed.

The CosmWasm VM handles:
- **Determinism** — float instructions, SIMD, threads, and non-deterministic opcodes are rejected at upload
- **Gas metering** — injected into WASM bytecode via wasmer middleware at basic block boundaries
- **Memory safety** — linear memory is bounded and isolated per contract instance

### 1.1 Gas Metering

Every WASM instruction consumes gas, paid in YELLOW (the native token). A transaction specifies a gas limit; execution halts if the limit is exceeded. CosmWasm's wasmer middleware instruments bytecode to deduct gas at each basic block entry. Host function calls carry additional costs:

| Operation       | Cost   |
|-----------------|--------|
| WASM instruction| 1      |
| Storage read    | 200    |
| Storage write   | 5000   |
| Hash (per byte) | 3      |
| Signature verify| 3000   |

> Costs are preliminary and subject to tuning.

## 2. Accounts and Addresses

Addresses are 20-byte values displayed as EVM-style hex (`0x` prefix, 40 hex chars). Both externally owned accounts (EOAs) and contracts share the same address space.

Each account has:

| Field   | Description                      |
|---------|----------------------------------|
| Address | 20-byte identifier               |
| Balance | YELLOW token balance (uint256)   |
| Nonce   | Transaction counter (EOA only)   |
| CodeHash| Hash of stored WASM bytecode (contracts only) |

## 3. Contract Lifecycle

### 3.1 Store

Upload compiled WASM bytecode to the runtime. Returns a **code ID** (content-addressed hash of the bytecode). The same bytecode is stored only once.

```
StoreCode(wasm []byte) -> codeID
```

### 3.2 Instantiate

Create a new contract instance from a stored code ID. Assigns a unique address, initializes storage, and calls the contract's `instantiate` entry point with the provided init message.

```
Instantiate(codeID, initMsg []byte, label string, funds Coins) -> InstantiateResult
```

Returns:

| Field           | Description                                 |
|-----------------|---------------------------------------------|
| ContractAddress | Deterministic address derived from creator, codeID, and instance counter |
| GasUsed         | Gas consumed during instantiation           |
| Data            | Optional binary data returned by the contract |
| Attributes      | Key-value attributes emitted by the contract |
| Events          | Typed events emitted by the contract        |

The caller may attach YELLOW tokens (`funds`) that are transferred to the new contract's balance.

### 3.3 Execute (Call)

Invoke a contract function by sending calldata to a contract address.

```
Execute(contractAddress, calldata []byte, funds Coins) -> ExecuteResult
```

Returns:

| Field      | Description                                 |
|------------|---------------------------------------------|
| GasUsed    | Gas consumed during execution               |
| Data       | Optional binary data returned by the contract |
| Attributes | Key-value attributes emitted by the contract |
| Events     | Typed events emitted by the contract        |

The caller may attach YELLOW tokens. The contract reads calldata, performs logic, reads/writes storage, and returns a result or error. Any sub-messages returned in the contract response are dispatched after execution (see §3.5).

### 3.4 Query

Read-only call that does not modify state. No gas cost to the caller (but internally metered to prevent abuse).

```
Query(contractAddress, queryMsg []byte) -> result []byte
```

### 3.5 Sub-Message Dispatch

Contracts communicate with other contracts and the runtime by returning **sub-messages** in their `Response`. After a contract's `execute` or `instantiate` entry point returns, NitroVM dispatches each sub-message in order.

Supported message types:

| Message             | Description                                      |
|---------------------|--------------------------------------------------|
| `BankMsg::Send`        | Transfer YELLOW tokens from the contract to an address |
| `WasmMsg::Execute`     | Call another contract's `execute` entry point     |
| `WasmMsg::Instantiate` | Spawn a new contract instance from a stored code ID |

The sender of a dispatched sub-message is the calling contract's address. Sub-messages may themselves return further sub-messages, enabling chains of cross-contract calls.

`WasmMsg::Instantiate` references codes by sequential uint64 ID (assigned in upload order), matching the CosmWasm convention. The newly created contract address is emitted in an `instantiate` event.

**Recursion limit:** dispatch depth is capped at **10** to prevent infinite loops. Exceeding this limit aborts the transaction.

Events emitted by sub-calls are collected and appended to the parent execution result.

### 3.6 Reply Callbacks

Contracts can react to the outcome of their sub-messages using the `ReplyOn` field and the `reply` entry point. Each sub-message carries an `ID` (arbitrary uint64 chosen by the contract) and an optional `Payload` (opaque bytes, max 128 KiB). After a sub-message is dispatched, NitroVM checks `ReplyOn` to decide whether to invoke the calling contract's `reply(env, reply)` entry point.

| ReplyOn        | On Success          | On Error                       |
|----------------|---------------------|--------------------------------|
| `ReplyNever`   | skip reply          | abort transaction              |
| `ReplySuccess` | invoke reply        | abort transaction              |
| `ReplyError`   | skip reply          | rollback state + invoke reply  |
| `ReplyAlways`  | invoke reply        | rollback state + invoke reply  |

The `Reply` object passed to the entry point contains:

| Field     | Description                                                   |
|-----------|---------------------------------------------------------------|
| `id`      | The `SubMsg.ID` — used to match which sub-message triggered the reply |
| `payload` | Echo of `SubMsg.Payload`                                      |
| `result`  | `Ok { events, data }` on success, or `Err "message"` on failure |
| `gas_used`| Gas consumed by the sub-message                               |

**Error catching:** When `ReplyOn` is `ReplyError` or `ReplyAlways` and the sub-message fails, NitroVM rolls back all state changes from the failed sub-message (both in-memory balances and contract storage via savepoints) before invoking `reply`. This allows the contract to handle the failure gracefully.

**Data propagation:** If the `reply` handler sets `Data` in its response, it **replaces** the parent execution's `Data` field. This enables patterns like extracting a return value from a sub-message.

**Reply chaining:** The `reply` handler itself returns a `Response` that may contain further sub-messages. These are dispatched recursively, subject to the same depth limit of 10.

**Reply errors are not catchable:** If the `reply` entry point itself returns an error, the entire transaction aborts. Only the original sub-message error can be caught via `ReplyError`/`ReplyAlways`.

**Not yet supported:**

- `WasmMsg::Migrate` — migrating contracts

## 4. Storage

### 4.1 Model

Each contract has isolated key-value storage. Keys and values are arbitrary byte slices.

| Operation | Signature                                  |
|-----------|--------------------------------------------|
| Get       | `storage_get(key []byte) -> value []byte`  |
| Set       | `storage_set(key []byte, value []byte)`    |
| Delete    | `storage_delete(key []byte)`               |
| Range     | `storage_range(start, end []byte, order) -> iterator` |

### 4.2 Adapter Interface

Storage is abstracted behind an adapter interface to support multiple backends.

```go
type StorageAdapter interface {
    Get(contractAddr Address, key []byte) ([]byte, error)
    Set(contractAddr Address, key []byte, value []byte) error
    Delete(contractAddr Address, key []byte) error
    Range(contractAddr Address, start, end []byte, order Order) (Iterator, error)
    Close() error
}
```

Initial implementation: **SQLite** (one table per contract, or a single table with composite keys).

## 5. Host Functions

CosmWasm-compatible host imports provided by NitroVM. All data crosses the WASM boundary via `Region` structs in linear memory (offset, capacity, length).

### 5.1 Storage

- `env.db_read(key_ptr) -> val_ptr`
- `env.db_write(key_ptr, val_ptr)`
- `env.db_remove(key_ptr)`
- `env.db_scan(start_ptr, end_ptr, order) -> iterator_id`
- `env.db_next(iterator_id) -> kv_pair_ptr`

### 5.2 Address Handling

- `env.addr_validate(addr_ptr) -> u32`
- `env.addr_canonicalize(human_ptr, canonical_ptr) -> u32`
- `env.addr_humanize(canonical_ptr, human_ptr) -> u32`

### 5.3 Crypto

These functions are implemented natively by the CosmWasm VM (libwasmvm) via the `cosmwasm-crypto` crate. No Go-side implementation is required — the VM registers them as WASM imports automatically.

- `env.secp256k1_verify(msg_hash_ptr, sig_ptr, pubkey_ptr) -> u32`
- `env.secp256k1_recover_pubkey(msg_hash_ptr, sig_ptr, recovery_param) -> u64`
- `env.ed25519_verify(msg_ptr, sig_ptr, pubkey_ptr) -> u32`
- `env.ed25519_batch_verify(msgs_ptr, sigs_ptr, pubkeys_ptr) -> u32`

### 5.4 Chain Queries

- `env.query_chain(request_ptr) -> response_ptr` — Query chain state

Supported query types:

| Query                    | Description                                      |
|--------------------------|--------------------------------------------------|
| `BankQuery::Balance`     | Get balance of a single denom for an address     |
| `BankQuery::AllBalances` | Get all YELLOW token balances for an address     |
| `WasmQuery::Smart`       | Call another contract's `query` entry point (cross-contract read) |

### 5.5 Debug

- `env.debug(msg_ptr)` — Print debug message (disabled in production)
- `env.abort(msg_ptr, msg_len, file_ptr, file_len)` — Abort execution with error

## 6. Contract ABI

Contracts are written in Rust using the [cosmwasm-std](https://crates.io/crates/cosmwasm-std) crate and compiled to `wasm32-unknown-unknown`.

Contracts must export the following CosmWasm entry points:

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
- **wasm-opt** from [Binaryen](https://github.com/WebAssembly/binaryen) (required post-processing step):
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
- **Hash:** keccak256 (Ethereum-compatible, `sha3.NewLegacyKeccak256()`)
- **Address derivation:** `keccak256(uncompressed_pubkey[1:])[12:]` — last 20 bytes of the hash of the 64-byte public key (without the `0x04` prefix)

### 8.3 Transaction Envelope

All transactions share a single RLP-encoded structure:

| Field    | Type      | Description |
|----------|-----------|-------------|
| ChainID  | string    | Chain identifier for replay protection |
| Nonce    | uint64    | Sender's sequential nonce |
| GasLimit | uint64    | Maximum gas for this transaction |
| GasPrice | uint64    | Price per unit of gas (in smallest YELLOW unit) |
| Type     | uint8     | 1=Store, 2=Instantiate, 3=Execute |
| Code     | bytes     | WASM bytecode (Store only) |
| CodeID   | bytes     | Code hash (Instantiate only) |
| Label    | string    | Contract label (Instantiate only) |
| Contract | [20]byte  | Target contract address (Execute only) |
| Msg      | bytes     | JSON message (Instantiate, Execute) |
| Funds    | []Coin    | Attached funds as `[[denom, amount], ...]` |

Fields irrelevant to the transaction type encode as zero-values.

### 8.4 Signing Process

1. RLP-encode the transaction fields in the order above
2. Compute `hash = keccak256(rlp_bytes)`
3. Sign `hash` with the sender's secp256k1 private key (compact format: V, R, S)
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
5. Validate chain ID matches server's chain
6. Validate gas price >= server's `--min-gas-price`
7. Nonce must match sender's current nonce (strict sequential)

### 8.6 Gas Fees

After successful execution, the gas fee is deducted from the signer's balance:

```
gas_fee = gas_used * gas_price
```

If the signer cannot afford the gas fee, the entire transaction is rolled back (using VM snapshot + storage savepoint). Gas price of 0 means no fee deduction.

### 8.7 Nonce Rules

- Every sender address has a sequential nonce starting at 0
- Each signed transaction must include a nonce equal to the sender's current nonce
- Nonce is incremented after successful execution (store, instantiate, or execute)
- Replay attacks are prevented: re-submitting a transaction with a used nonce is rejected

### 8.8 Key Management

The CLI stores private keys as hex-encoded 32-byte files at `~/.nitrovm/key` (overridable via `--keyfile` flag or `NITRO_KEYFILE` env). Generate with `nitroctl keygen`.

### 8.9 Legacy Mode

When the server runs without `--require-sig`, both signed and unsigned requests are accepted. Unsigned requests use the legacy JSON format with an explicit `sender` field. When `--require-sig` is set, only signed requests are accepted for store, instantiate, and execute endpoints.

## 9. Future Work

- Contract migration and admin controls (`WasmMsg::Migrate`)
- IBC support for cross-chain messaging
