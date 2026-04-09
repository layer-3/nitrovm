# NitroVM

A WebAssembly runtime for executing smart contracts, inspired by the EVM execution model.

## Overview

NitroVM provides a sandboxed WASM-based execution environment for smart contracts with familiar blockchain primitives: contract addresses, persistent storage, gas metering, and native asset ownership. It uses the [CosmWasm](https://cosmwasm.com/) VM (wasmer) via [wasmvm](https://github.com/CosmWasm/wasmvm) CGO bindings.

## Key Features

- **CosmWasm Runtime** — Contracts compile to WebAssembly and run in a deterministic sandbox powered by [wasmer](https://wasmer.io/) via CosmWasm's VM, with built-in gas metering, float rejection, and opcode validation
- **EVM-style Addresses** — Contracts are identified by 20-byte hex addresses (`0x...`)
- **Native Token (YELLOW)** — Gas metering and value transfers use the native YELLOW token
- **Persistent Storage** — Key-value storage per contract, backed by pluggable storage adapters (SQLite initially)
- **Two-step Deployment** — Store contract code, then instantiate with init parameters (the CosmWasm model)
- **Host Functions** — CosmWasm-compatible host imports for storage, crypto, address handling, and chain queries

## Contract Languages

Contracts are written in **Rust** using the [cosmwasm-std](https://crates.io/crates/cosmwasm-std) crate.

## Contract Lifecycle

```
Store Code  -->  Instantiate  -->  Call
(upload wasm)    (create instance   (execute contract
                  with init data,    functions via
                  assign address)    calldata)
```

## Architecture

```
 Calldata
    |
    v
+-----------+      +-------------------+
|  NitroVM  | ---> | CosmWasm Host Fns |
|  (Go)     |      | (storage, crypto, |
+-----------+      |  addr, queries)   |
    |              +-------------------+
    v
+-----------+      +----------------+
|  wasmvm   | ---> | CosmWasm VM    |
|  (CGO)    |      | (wasmer, gas   |
+-----------+      |  metering,     |
                   |  validation)   |
                   +----------------+
    |
    v
+-----------+
|  Storage  |
|  Adapter  |
+-----------+
    |
    v
+-----------+
|  SQLite   |
+-----------+
```

## Status

Early development. See [SPEC.md](SPEC.md) for the detailed technical specification.
