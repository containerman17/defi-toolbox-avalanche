.PHONY: build build-native build-wasm build-wasm-ethcall build-state-server clean

BIN = evm-quoter/bin

build: build-native build-wasm

build-native:
	go build -o $(BIN)/harness-native ./cmd/native

build-wasm:
	GOOS=js GOARCH=wasm go build -tags nethttpomit,osusergo,netgo -o $(BIN)/harness.wasm ./cmd/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" $(BIN)/wasm_exec.js

build-wasm-ethcall:
	GOOS=js GOARCH=wasm go build -tags nethttpomit,osusergo,netgo -o examples/wasm-ethcall/ethcall.wasm ./examples/wasm-ethcall
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" examples/wasm-ethcall/wasm_exec.js

build-state-server:
	go build -o $(BIN)/state-server ./cmd/state-server

clean:
	rm -f $(BIN)/harness-native $(BIN)/harness.wasm $(BIN)/state-server
