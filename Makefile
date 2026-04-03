.PHONY: build build-wasm build-state-server clean

BIN = evm-quoter/bin

build: build-wasm

build-wasm:
	GOOS=js GOARCH=wasm go build -tags nethttpomit,osusergo,netgo -o cmd/wasm-sdk/quoter.wasm ./cmd/wasm-sdk
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" cmd/wasm-sdk/wasm_exec.js

build-state-server:
	go build -o $(BIN)/state-server ./cmd/state-server

clean:
	rm -f $(BIN)/state-server cmd/wasm-sdk/quoter.wasm cmd/wasm-sdk/wasm_exec.js
