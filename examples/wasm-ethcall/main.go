//go:build js && wasm

package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall/js"
	"time"

	"defi-toolbox/cmd/quoter-example/shared"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/vm"
)

var (
	ls        *statedb.LiveState
	transport *shared.BrowserTransport
)

func main() {
	js.Global().Set("connect", js.FuncOf(connectFn))
	js.Global().Set("ethCall", js.FuncOf(ethCallFn))
	js.Global().Set("subscribeBlocks", js.FuncOf(subscribeBlocksFn))
	js.Global().Set("getBlock", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if ls == nil {
			return 0
		}
		return int(ls.Block())
	}))

	fmt.Fprintf(os.Stderr, "[wasm] ethcall example ready, call connect(url)\n")
	select {}
}

// connect(url) — connects to state server, loads full state.
func connectFn(this js.Value, args []js.Value) interface{} {
	if len(args) < 1 {
		return jsError("connect requires url argument")
	}
	url := args[0].String()

	handler := js.FuncOf(func(this js.Value, promiseArgs []js.Value) interface{} {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			var err error
			ls, transport, err = shared.ConnectBrowser(url)
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			resolve.Invoke(js.Null())
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

// ethCall(to, data) — runs eth_call locally in the WASM EVM.
// Returns a Promise that resolves to {result, gasUsed, ms}.
func ethCallFn(this js.Value, args []js.Value) interface{} {
	if ls == nil {
		return jsError("not connected")
	}
	if len(args) < 2 {
		return jsError("ethCall requires (to, data)")
	}
	to := common.HexToAddress(args[0].String())
	data := common.FromHex(args[1].String())

	handler := js.FuncOf(func(this js.Value, promiseArgs []js.Value) interface{} {
		resolve := promiseArgs[0]
		go func() {
			ls.RLock()
			cfg := ls.EVMConfig()
			state := ls.State()
			ctx := statedb.GetCachedContext(cfg)

			// Use block gas limit (typically 40M) instead of the default 5M,
			// needed for complex contracts like LBQuoter.
			gasLimit := cfg.GasLimit
			if gasLimit == 0 {
				gasLimit = 40_000_000
			}

			t0 := time.Now()
			result, gasUsed, err := ctx.ExecuteWithGas(
				state, common.Address{}, to, data, gasLimit,
			)
			elapsed := time.Since(t0)
			ls.RUnlock()

			obj := map[string]interface{}{
				"ms": elapsed.Seconds() * 1000,
			}
			if err != nil {
				obj["error"] = err.Error()
				obj["gasUsed"] = int(gasUsed)
				// On revert, result contains revert data
				if errors.Is(err, vm.ErrExecutionReverted) {
					obj["revertData"] = "0x" + hex.EncodeToString(result)
				}
				fmt.Fprintf(os.Stderr, "[wasm] ethCall reverted: %v (gas used: %d)\n", err, gasUsed)
			} else {
				obj["gasUsed"] = int(gasUsed)
				obj["result"] = "0x" + hex.EncodeToString(result)
			}
			b, _ := json.Marshal(obj)
			resolve.Invoke(js.Global().Get("JSON").Call("parse", string(b)))
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

// subscribeBlocks(callback) — calls callback(block, timestamp) on each new block.
func subscribeBlocksFn(this js.Value, args []js.Value) interface{} {
	if ls == nil {
		return jsError("not connected")
	}
	if len(args) < 1 {
		return jsError("subscribeBlocks requires a callback")
	}
	cb := args[0]
	ls.SetOnBlock(func(ls *statedb.LiveState, entries [][2]string) {
		cb.Invoke(int(ls.Block()), int(ls.Timestamp()))
	})
	return js.Null()
}

func jsError(msg string) interface{} {
	return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New(msg))
}
