.PHONY: build build-native build-wasm build-state-server clean

BIN = evm-quoter/bin

build: build-native build-wasm

build-native:
	go build -o $(BIN)/harness-native ./cmd/native

build-wasm:
	GOOS=js GOARCH=wasm go build -tags nethttpomit,osusergo,netgo -o cmd/quoter-example/wasm/quoter.wasm ./cmd/quoter-example/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" cmd/quoter-example/wasm/wasm_exec.js

build-state-server:
	go build -o $(BIN)/state-server ./cmd/state-server

clean:
	rm -f $(BIN)/harness-native $(BIN)/harness.wasm $(BIN)/state-server
