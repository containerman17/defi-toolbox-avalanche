package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"

	"encoding/json"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// ─── Stdin/stdout JSON-RPC ─────────────────────────────────────────

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type ethCallParams struct {
	To             string                       `json:"to"`
	Data           string                       `json:"data"`
	From           string                       `json:"from"`
	StateOverrides map[string]stateOverrideEntry `json:"stateOverrides,omitempty"`
}

type batchCallParams struct {
	StateOverrides map[string]stateOverrideEntry `json:"stateOverrides,omitempty"`
	Calls          []batchCallEntry              `json:"calls"`
	SkipFormulas   bool                          `json:"skipFormulas,omitempty"`
}

type batchCallEntry struct {
	To   string `json:"to"`
	Data string `json:"data"`
	From string `json:"from"`
}

type batchCallResult struct {
	ReturnData string `json:"returnData"`
	GasUsed    uint64 `json:"gasUsed"`
	Error      string `json:"error,omitempty"`
}

type stateOverrideEntry struct {
	Code      string            `json:"code,omitempty"`
	Balance   string            `json:"balance,omitempty"`
	Nonce     *uint64           `json:"nonce,omitempty"`
	StateDiff map[string]string `json:"stateDiff,omitempty"`
}

type response struct {
	ID     int         `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// parsedOverride holds pre-parsed override data for fast per-call application.
type parsedOverride struct {
	addr    common.Address
	balance *uint256.Int
	nonce   uint64
	code    []byte
	slots   []struct {
		slot  common.Hash
		value common.Hash
	}
}

// parseOverrides pre-parses state overrides (once per batch).
func parseOverrides(raw map[string]stateOverrideEntry) []parsedOverride {
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
			po.slots = append(po.slots, struct {
				slot  common.Hash
				value common.Hash
			}{common.HexToHash(slotHex), common.HexToHash(valueHex)})
		}
		result = append(result, po)
	}
	return result
}

// applyParsedOverrides creates a fresh overlay and applies pre-parsed overrides.
func applyParsedOverrides(base *statedb.StateDB, overrides []parsedOverride) *statedb.StateDB {
	if len(overrides) == 0 {
		return base
	}
	overlay := base.NewOverlay()
	for _, po := range overrides {
		if po.code != nil {
			overlay.SetAccount(po.addr, po.balance, po.nonce, po.code)
		}
		for _, s := range po.slots {
			overlay.SetStorageSlot(po.addr, s.slot, s.value)
		}
	}
	return overlay
}

// applyOverrides creates an overlay with state overrides applied (parses from raw).
func applyOverrides(base *statedb.StateDB, raw map[string]stateOverrideEntry) *statedb.StateDB {
	return applyParsedOverrides(base, parseOverrides(raw))
}

var stdoutMu sync.Mutex

// writeStdout writes a line to stdout, safe for concurrent use from readLoop and main.
func writeStdout(data []byte) {
	stdoutMu.Lock()
	os.Stdout.Write(data)
	os.Stdout.Write([]byte("\n"))
	stdoutMu.Unlock()
}

func main() {
	counter := &statedb.Counter{}

	// Check for flags in args
	stateServerURL := ""
	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
	}

	// Load formula registry
	registry := formulas.LoadEmbeddedRegistry()
	validated, invalid := registry.RegistryStats()
	if validated+invalid > 0 {
		fmt.Fprintf(os.Stderr, "[native] formula registry: %d validated, %d invalid\n", validated, invalid)
	}

	// Pre-compute pools, graph, and overrides for find_route
	embeddedPools := poolcollector.EmbeddedPools(7500)
	embeddedAdj := pf.BuildAdjacency(embeddedPools, registry)
	embeddedOverrides := router.BuildTokenOverrides(
		router.DeployedRouter,
		embeddedPools,
	)
	fmt.Fprintf(os.Stderr, "[native] pre-computed: %d pools, %d overrides\n", len(embeddedPools), len(embeddedOverrides))

	var ls *statedb.LiveState
	var state *statedb.StateDB

	if stateServerURL != "" {
		var err error
		ls, err = statedb.Connect(stateServerURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect to state server: %v\n", err)
			os.Exit(1)
		}
		state = ls.State()
		fmt.Fprintf(os.Stderr, "[native] connected to state server %s\n", stateServerURL)
	}

	// Allocate state if no state server (for standalone prefill usage)
	if state == nil {
		state = statedb.NewStateDB(nil)
	}

	// Create PoolManager backed by state, register pools
	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)
	for _, p := range embeddedPools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	if ls != nil {
		pm.SetBlockTimestamp(ls.Timestamp())
		ls.SetOnBlock(func(liveState *statedb.LiveState, entries [][2]string) {
			// Invalidate pools whose storage changed
			for _, entry := range entries {
				key := entry[0]
				if strings.HasPrefix(key, "s:") {
					parts := strings.SplitN(key, ":", 3)
					if len(parts) == 3 {
						addr := common.HexToAddress(parts[1])
						slot := common.HexToHash(parts[2])
						pm.InvalidateBySlot(addr, slot)
					}
				}
			}
			pm.SetBlockTimestamp(liveState.Timestamp())
			// Notify JS of new block via stdout
			note, _ := json.Marshal(map[string]interface{}{
				"type": "block", "blockNumber": liveState.Block(), "timestamp": liveState.Timestamp(),
			})
			writeStdout(note)
		})
	}
	fmt.Fprintf(os.Stderr, "[native] pool manager ready for %d pools\n", len(embeddedPools))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024*1024), 64*1024*1024) // 64MB buffer (large batches)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "invalid json: %v\n", err)
			continue
		}

		// Lock for reading state during the entire request
		if ls != nil {
			ls.RLock()
		}

		var resp response
		resp.ID = req.ID

		switch req.Method {
		case "increment":
			resp.Result = counter.Increment()

		case "eth_call":
			var params ethCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = fmt.Sprintf("invalid params: %v", err)
				break
			}

			from := common.HexToAddress(params.From)
			to := common.HexToAddress(params.To)
			data, err := hex.DecodeString(strings.TrimPrefix(params.Data, "0x"))
			if err != nil {
				resp.Error = fmt.Sprintf("invalid data: %v", err)
				break
			}

			execState := applyOverrides(state, params.StateOverrides)
			var cfg statedb.EVMConfig
			if ls != nil {
				cfg = ls.EVMConfig()
			}

			ret, gasUsed, evmErr := statedb.ExecuteCall(execState, cfg, from, to, data)

			result := map[string]interface{}{
				"returnData": "0x" + hex.EncodeToString(ret),
				"gasUsed":    gasUsed,
			}
			if evmErr != nil {
				result["error"] = evmErr.Error()
			}
			resp.Result = result

		case "eth_call_batch":
			var params batchCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = fmt.Sprintf("invalid params: %v", err)
				break
			}

			parsed := parseOverrides(params.StateOverrides)
			var cfg statedb.EVMConfig
			if ls != nil {
				cfg = ls.EVMConfig()
			}

			// Apply overrides once — EVM calls use CallState overlay on top
			baseWithOverrides := applyParsedOverrides(state, parsed)

			evmCtx := statedb.GetCachedContext(cfg)
			cs := statedb.NewCallState(baseWithOverrides)

			results := make([]batchCallResult, len(params.Calls))
			for i, call := range params.Calls {
				data, err := hex.DecodeString(strings.TrimPrefix(call.Data, "0x"))
				if err != nil {
					results[i] = batchCallResult{Error: fmt.Sprintf("invalid data: %v", err)}
					continue
				}

				// Try formula shortcut (unless skipFormulas is set).
				// Reads from base state — pool reserves are not affected by overrides.
				if !params.SkipFormulas {
					reader := func(addr common.Address, key common.Hash) common.Hash { return state.GetState(addr, key) }
					if ret, ok := registry.TryQuote(reader, data); ok {
						results[i] = batchCallResult{
							ReturnData: "0x" + hex.EncodeToString(ret),
							GasUsed:    0,
						}
						continue
					}
				}

				// EVM path — thin CallState overlay with JUMPDEST sharing
				from := common.HexToAddress(call.From)
				to := common.HexToAddress(call.To)
				cs.Reset()
				ret, gasUsed, evmErr := evmCtx.ExecuteWithCallState(cs, from, to, data)
				results[i] = batchCallResult{
					ReturnData: "0x" + hex.EncodeToString(ret),
					GasUsed:    gasUsed,
				}
				if evmErr != nil {
					results[i].Error = evmErr.Error()
				}
			}

			resp.Result = map[string]interface{}{
				"results": results,
			}

		case "find_route":
			var params struct {
				TokenIn     string `json:"tokenIn"`
				TokenOut    string `json:"tokenOut"`
				AmountIn    string `json:"amountIn"`
				MaxHops     int    `json:"maxHops"`
				FormulaOnly bool   `json:"formulaOnly"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = fmt.Sprintf("invalid params: %v", err)
				break
			}

			tokenIn := common.HexToAddress(params.TokenIn)
			tokenOut := common.HexToAddress(params.TokenOut)
			amtBig, ok := new(big.Int).SetString(strings.TrimPrefix(params.AmountIn, "0x"), 16)
			if !ok {
				resp.Error = "invalid amountIn"
				break
			}
			amountIn, _ := uint256.FromBig(amtBig)

			var cfg statedb.EVMConfig
			if ls != nil {
				cfg = ls.EVMConfig()
				pm.SetBlockTimestamp(ls.Timestamp())
			}

			maxHops := params.MaxHops
			if maxHops <= 0 {
				maxHops = 4
			}

			route := pf.FindBestRoute(pm, embeddedAdj, embeddedPools, state, cfg, router.DeployedRouter, embeddedOverrides, tokenIn, tokenOut, amountIn, maxHops)
			if route == nil {
				resp.Result = map[string]interface{}{"route": nil}
			} else {
				resp.Result = route
			}

		default:
			resp.Error = "unknown method: " + req.Method
		}

		out, _ := json.Marshal(resp)
		writeStdout(out)

		if ls != nil {
			ls.RUnlock()
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
		os.Exit(1)
	}
}
