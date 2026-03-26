//go:build js && wasm

package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"syscall/js"

	"defi-toolbox/router"
	"defi-toolbox/statedb"
	"defi-toolbox/formulas"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// ─── JS Promise-based fetcher ─────────────────────────────────────
// Go calls js.Global().Call("fetchStorageAsync", ...) which returns a Promise.
// We register a .then() callback that sends the result to a Go channel.
// Blocking on the channel yields the Go goroutine back to the JS event loop,
// so JS can resolve the Promise (WS round trip to state server).

type jsFetcher struct {
	misses atomic.Int64
}

func awaitPromise(promise js.Value) string {
	ch := make(chan string, 1)
	var thenCb, catchCb js.Func
	thenCb = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) > 0 {
			ch <- args[0].String()
		} else {
			ch <- ""
		}
		thenCb.Release()
		catchCb.Release()
		return nil
	})
	catchCb = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		msg := "unknown error"
		if len(args) > 0 {
			msg = args[0].String()
		}
		ch <- "ERROR:" + msg
		thenCb.Release()
		catchCb.Release()
		return nil
	})
	promise.Call("then", thenCb).Call("catch", catchCb)
	return <-ch
}

func (f *jsFetcher) FetchStorage(addr common.Address, slot common.Hash) common.Hash {
	f.misses.Add(1)
	promise := js.Global().Call("__goFetchStorageAsync", addr.Hex(), slot.Hex())
	result := awaitPromise(promise)
	if strings.HasPrefix(result, "ERROR:") {
		fmt.Printf("[wasm] FetchStorage error: %s\n", result)
		return common.Hash{}
	}
	return common.HexToHash(result)
}

func (f *jsFetcher) FetchBalance(addr common.Address) *uint256.Int {
	f.misses.Add(1)
	promise := js.Global().Call("__goFetchBalanceAsync", addr.Hex())
	result := awaitPromise(promise)
	if strings.HasPrefix(result, "ERROR:") {
		return uint256.NewInt(0)
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(result, "0x"), 16)
	if !ok {
		return uint256.NewInt(0)
	}
	val, _ := uint256.FromBig(bi)
	return val
}

func (f *jsFetcher) FetchNonce(addr common.Address) uint64 {
	f.misses.Add(1)
	promise := js.Global().Call("__goFetchNonceAsync", addr.Hex())
	result := awaitPromise(promise)
	if strings.HasPrefix(result, "ERROR:") {
		return 0
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(result, "0x"), 16)
	if !ok {
		return 0
	}
	return bi.Uint64()
}

func (f *jsFetcher) FetchCode(addr common.Address) []byte {
	f.misses.Add(1)
	promise := js.Global().Call("__goFetchCodeAsync", addr.Hex())
	result := awaitPromise(promise)
	if strings.HasPrefix(result, "ERROR:") {
		return nil
	}
	if result == "" || result == "0x" {
		return nil
	}
	code, _ := hex.DecodeString(strings.TrimPrefix(result, "0x"))
	return code
}

func (f *jsFetcher) FetchBlockHash(num uint64) common.Hash {
	f.misses.Add(1)
	promise := js.Global().Call("__goFetchBlockHashAsync", fmt.Sprintf("0x%x", num))
	result := awaitPromise(promise)
	if strings.HasPrefix(result, "ERROR:") {
		return common.Hash{}
	}
	return common.HexToHash(result)
}

// ─── Global state ──────────────────────────────────────────────────

var (
	state    *statedb.StateDB
	fetcher  = &jsFetcher{}
	counter  = &statedb.Counter{}
	evmCfg   statedb.EVMConfig
	registry = formulas.NewRegistry() // empty by default
)

func main() {
	state = statedb.NewStateDB(fetcher)
	evmCfg = statedb.EVMConfig{
		BlockNumber: uint64(router.DeployedBlock),
		Timestamp:   0,
		ChainID:     43114,
	}

	// Counter (Prototype 1 compatibility)
	js.Global().Set("increment", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		return counter.Increment()
	}))

	// Set block number, timestamp, baseFee, gasLimit
	js.Global().Set("__goSetBlock", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) >= 1 {
			evmCfg.BlockNumber = uint64(args[0].Int())
		}
		if len(args) >= 2 {
			evmCfg.Timestamp = uint64(args[1].Int())
		}
		if len(args) >= 3 {
			evmCfg.BaseFee = uint64(args[2].Int())
		}
		if len(args) >= 4 {
			evmCfg.GasLimit = uint64(args[3].Int())
		}
		return nil
	}))

	// Prefill storage slot
	js.Global().Set("__goPrefillStorage", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 3 {
			return nil
		}
		addr := common.HexToAddress(args[0].String())
		slot := common.HexToHash(args[1].String())
		value := common.HexToHash(args[2].String())
		state.SetStorageSlot(addr, slot, value)
		return nil
	}))

	// Prefill account
	js.Global().Set("__goPrefillAccount", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 4 {
			return nil
		}
		addr := common.HexToAddress(args[0].String())
		balStr := args[1].String()
		nonce := uint64(args[2].Int())
		codeHex := args[3].String()

		balance := uint256.NewInt(0)
		if bi, ok := new(big.Int).SetString(strings.TrimPrefix(balStr, "0x"), 16); ok && bi != nil {
			balance, _ = uint256.FromBig(bi)
		}

		var code []byte
		if codeHex != "" && codeHex != "0x" {
			code, _ = hex.DecodeString(strings.TrimPrefix(codeHex, "0x"))
		}

		state.SetAccount(addr, balance, nonce, code)
		return nil
	}))

	// Pre-parsed override for fast per-call application
	type parsedOverride struct {
		addr    common.Address
		balance *uint256.Int
		nonce   uint64
		code    []byte
		slots   []struct{ slot, value common.Hash }
	}

	parseOverridesJSON := func(overridesJSON string) []parsedOverride {
		if overridesJSON == "" {
			return nil
		}
		var raw map[string]struct {
			Code      string            `json:"code,omitempty"`
			Balance   string            `json:"balance,omitempty"`
			Nonce     *uint64           `json:"nonce,omitempty"`
			StateDiff map[string]string `json:"stateDiff,omitempty"`
		}
		if err := json.Unmarshal([]byte(overridesJSON), &raw); err != nil {
			return nil
		}
		result := make([]parsedOverride, 0, len(raw))
		for addrHex, entry := range raw {
			po := parsedOverride{addr: common.HexToAddress(addrHex)}
			if entry.Code != "" && entry.Code != "0x" {
				po.code, _ = hex.DecodeString(strings.TrimPrefix(entry.Code, "0x"))
				po.balance = uint256.NewInt(0)
				if entry.Balance != "" {
					if bi, ok := new(big.Int).SetString(strings.TrimPrefix(entry.Balance, "0x"), 16); ok {
						po.balance, _ = uint256.FromBig(bi)
					}
				}
				if entry.Nonce != nil {
					po.nonce = *entry.Nonce
				}
			}
			for slotHex, valueHex := range entry.StateDiff {
				po.slots = append(po.slots, struct{ slot, value common.Hash }{common.HexToHash(slotHex), common.HexToHash(valueHex)})
			}
			result = append(result, po)
		}
		return result
	}

	applyParsed := func(parsed []parsedOverride) *statedb.StateDB {
		if len(parsed) == 0 {
			return state
		}
		overlay := state.NewOverlay()
		for _, po := range parsed {
			if po.code != nil {
				overlay.SetAccount(po.addr, po.balance, po.nonce, po.code)
			}
			for _, s := range po.slots {
				overlay.SetStorageSlot(po.addr, s.slot, s.value)
			}
		}
		return overlay
	}

	// Single eth_call — Args: from, to, data, callback, [overridesJSON]
	js.Global().Set("__goEthCall", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 4 {
			return js.Null()
		}
		from := args[0].String()
		to := args[1].String()
		dataHex := args[2].String()
		callback := args[3]
		var ovrJSON string
		if len(args) >= 5 && !args[4].IsNull() && !args[4].IsUndefined() {
			ovrJSON = args[4].String()
		}

		go func() {
			fromAddr := common.HexToAddress(from)
			toAddr := common.HexToAddress(to)
			data, err := hex.DecodeString(strings.TrimPrefix(dataHex, "0x"))
			if err != nil {
				callback.Invoke(fmt.Sprintf(`{"error":"invalid data hex: %v"}`, err))
				return
			}

			execState := applyParsed(parseOverridesJSON(ovrJSON))
			ret, gasUsed, evmErr := statedb.ExecuteCall(execState, evmCfg, fromAddr, toAddr, data)

			retHex := "0x" + hex.EncodeToString(ret)
			if evmErr != nil {
				callback.Invoke(fmt.Sprintf(`{"returnData":"%s","gasUsed":%d,"error":"%s"}`, retHex, gasUsed, evmErr.Error()))
			} else {
				callback.Invoke(fmt.Sprintf(`{"returnData":"%s","gasUsed":%d}`, retHex, gasUsed))
			}
		}()

		return nil
	}))

	// Batch eth_call — Args: callsJSON, overridesJSON, callback
	// callsJSON: [{"to":"0x...","data":"0x...","from":"0x..."},...]
	// Returns JSON array of results
	js.Global().Set("__goEthCallBatch", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 3 {
			return js.Null()
		}
		callsJSON := args[0].String()
		ovrJSON := args[1].String()
		callback := args[2]

		go func() {
			var calls []struct {
				To   string `json:"to"`
				Data string `json:"data"`
				From string `json:"from"`
			}
			if err := json.Unmarshal([]byte(callsJSON), &calls); err != nil {
				callback.Invoke(fmt.Sprintf(`{"error":"invalid calls JSON: %v"}`, err))
				return
			}

			parsed := parseOverridesJSON(ovrJSON)

			results := make([]string, len(calls))
			for i, call := range calls {
				data, err := hex.DecodeString(strings.TrimPrefix(call.Data, "0x"))
				if err != nil {
					results[i] = fmt.Sprintf(`{"error":"invalid data: %v"}`, err)
					continue
				}

				// Try formula shortcut
				reader := func(addr common.Address, key common.Hash) common.Hash { return state.GetState(addr, key) }
				if ret, ok := registry.TryQuote(reader, data); ok {
					retHex := "0x" + hex.EncodeToString(ret)
					results[i] = fmt.Sprintf(`{"returnData":"%s","gasUsed":0}`, retHex)
					continue
				}

				from := common.HexToAddress(call.From)
				to := common.HexToAddress(call.To)
				execState := applyParsed(parsed)
				ret, gasUsed, evmErr := statedb.ExecuteCall(execState, evmCfg, from, to, data)
				retHex := "0x" + hex.EncodeToString(ret)
				if evmErr != nil {
					results[i] = fmt.Sprintf(`{"returnData":"%s","gasUsed":%d,"error":"%s"}`, retHex, gasUsed, evmErr.Error())
				} else {
					results[i] = fmt.Sprintf(`{"returnData":"%s","gasUsed":%d}`, retHex, gasUsed)
				}
			}

			var buf strings.Builder
			buf.WriteString(`{"results":[`)
			for i, r := range results {
				if i > 0 {
					buf.WriteByte(',')
				}
				buf.WriteString(r)
			}
			buf.WriteString(`]}`)
			callback.Invoke(buf.String())
		}()

		return nil
	}))

	// Signal that we're ready
	js.Global().Set("__goReady", js.ValueOf(true))

	// Keep Go runtime alive
	select {}
}
