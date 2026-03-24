.PHONY: build build-native build-wasm start-state-server clean

build: build-native build-wasm

build-native:
	cd go && go build -o ../bin/harness-native ./cmd/native

build-wasm:
	cd go && GOOS=js GOARCH=wasm go build -tags nethttpomit,osusergo,netgo -o ../bin/harness.wasm ./cmd/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" bin/wasm_exec.js

start-state-server:
	cd ../state-server && go run . $(ARGS)

clean:
	rm -rf bin/
