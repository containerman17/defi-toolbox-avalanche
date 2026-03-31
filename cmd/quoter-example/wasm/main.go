//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall/js"

	"defi-toolbox/cmd/quoter-example/shared"
)

var quoter *shared.Quoter

func main() {
	js.Global().Set("connect", js.FuncOf(connectFn))
	js.Global().Set("quote", js.FuncOf(quoteFn))

	fmt.Fprintf(os.Stderr, "[wasm] quoter ready, call connect(url) then quote(tokenIn, tokenOut, amountIn)\n")
	select {} // block forever
}

// connect(url, poolLimit?) — connects to state server, builds quoter.
func connectFn(this js.Value, args []js.Value) interface{} {
	if len(args) < 1 {
		return jsError("connect requires url argument")
	}
	url := args[0].String()
	poolLimit := 5000
	if len(args) >= 2 {
		poolLimit = args[1].Int()
	}
	maxHops := 4
	if len(args) >= 3 {
		maxHops = args[2].Int()
	}

	// Return a Promise so JS can await it.
	handler := js.FuncOf(func(this js.Value, promiseArgs []js.Value) interface{} {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			ls, err := shared.ConnectBrowser(url)
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			quoter = shared.NewQuoter(ls, poolLimit, maxHops)
			quoter.StartBlockLoop()
			resolve.Invoke(js.Null())
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

// quote(tokenIn, tokenOut, amountIn) — returns JSON string with quote results.
func quoteFn(this js.Value, args []js.Value) interface{} {
	if quoter == nil {
		return jsError("not connected, call connect() first")
	}
	if len(args) < 3 {
		return jsError("quote requires tokenIn, tokenOut, amountIn")
	}

	req := shared.QuoteRequest{
		TokenIn:  args[0].String(),
		TokenOut: args[1].String(),
		AmountIn: args[2].String(),
	}

	// Return a Promise.
	handler := js.FuncOf(func(this js.Value, promiseArgs []js.Value) interface{} {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			resp, err := quoter.Quote(req)
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			b, _ := json.Marshal(resp)
			resolve.Invoke(js.Global().Get("JSON").Call("parse", string(b)))
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

func jsError(msg string) interface{} {
	return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New(msg))
}
