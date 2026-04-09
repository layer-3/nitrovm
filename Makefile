.PHONY: build build-contract test test-script clean

WASM_TARGET = wasm32-unknown-unknown
TOKEN_DIR   = contracts/token
TOKEN_WASM  = $(TOKEN_DIR)/target/$(WASM_TARGET)/release/token.wasm

# Build server and client binaries.
build:
	CGO_ENABLED=1 go build -o cmd/nitrolite/nitrolite ./cmd/nitrolite
	CGO_ENABLED=1 go build -o cmd/nitroctl/nitroctl ./cmd/nitroctl

# Build the token contract WASM and lower bulk-memory ops for wasmvm compatibility.
# Requires: cargo, wasm-opt (brew install binaryen)
build-contract:
	cargo build --release --target $(WASM_TARGET) --manifest-path $(TOKEN_DIR)/Cargo.toml
	wasm-opt --enable-bulk-memory --enable-sign-ext --enable-mutable-globals \
		--llvm-memory-copy-fill-lowering -Oz \
		$(TOKEN_WASM) -o $(TOKEN_WASM)

# Run Go unit + integration tests.
test: build-contract
	CGO_ENABLED=1 go test -v -count=1 github.com/layer-3/nitrovm

# Run end-to-end CLI functional test (server + client).
test-script: build-contract
	bash scripts/test_token.sh

clean:
	cargo clean --manifest-path $(TOKEN_DIR)/Cargo.toml
	rm -f cmd/nitrolite/nitrolite cmd/nitroctl/nitroctl
