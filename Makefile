.PHONY: build build-contract test test-script clean

WASM_TARGET = wasm32-unknown-unknown
TOKEN_DIR   = contracts/token
TOKEN_WASM  = $(TOKEN_DIR)/target/$(WASM_TARGET)/release/token.wasm
ESCROW_DIR  = contracts/escrow
ESCROW_WASM = $(ESCROW_DIR)/target/$(WASM_TARGET)/release/escrow.wasm

# Build server and client binaries.
build:
	CGO_ENABLED=1 go build -o cmd/nitrolite/nitrolite ./cmd/nitrolite
	CGO_ENABLED=1 go build -o cmd/nitroctl/nitroctl ./cmd/nitroctl

# Build the token and escrow contract WASMs and lower bulk-memory ops for wasmvm compatibility.
# Requires: cargo, wasm-opt (brew install binaryen)
build-contract:
	cargo build --release --target $(WASM_TARGET) --manifest-path $(TOKEN_DIR)/Cargo.toml
	wasm-opt --enable-bulk-memory --enable-sign-ext --enable-mutable-globals \
		--llvm-memory-copy-fill-lowering -Oz \
		$(TOKEN_WASM) -o $(TOKEN_WASM)
	cargo build --release --target $(WASM_TARGET) --manifest-path $(ESCROW_DIR)/Cargo.toml
	wasm-opt --enable-bulk-memory --enable-sign-ext --enable-mutable-globals \
		--llvm-memory-copy-fill-lowering -Oz \
		$(ESCROW_WASM) -o $(ESCROW_WASM)

# Run Go unit + integration tests.
test: build-contract
	CGO_ENABLED=1 go test -v -count=1 ./...

# Run end-to-end CLI functional test (server + client, signed transactions).
test-script: build-contract
	bash scripts/test_signed.sh

clean:
	cargo clean --manifest-path $(TOKEN_DIR)/Cargo.toml
	cargo clean --manifest-path $(ESCROW_DIR)/Cargo.toml
	rm -f cmd/nitrolite/nitrolite cmd/nitroctl/nitroctl
