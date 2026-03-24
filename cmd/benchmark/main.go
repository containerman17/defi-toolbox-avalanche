package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"defi-toolbox/formulas"
	"defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"
)

// ─── Minimal state-server fetcher (copied from cmd/native) ─────────

type wsFetcher struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage
	block   uint64
}

type stateServerMessage struct {
	Type        string      `json:"type,omitempty"`
	BlockNumber uint64      `json:"blockNumber,omitempty"`
	Timestamp   uint64      `json:"timestamp,omitempty"`
	BaseFee     uint64      `json:"baseFee,omitempty"`
	GasLimit    uint64      `json:"gasLimit,omitempty"`
	Entries     [][2]string `json:"entries,omitempty"`
	JSONRPC     string      `json:"jsonrpc,omitempty"`
	ID          int         `json:"id,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
}

type valueResult struct {
	Value string `json:"value"`
}

func connectStateServer(url string) (*wsFetcher, *statedb.StateDB, statedb.EVMConfig, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, nil, statedb.EVMConfig{}, fmt.Errorf("ws connect: %w", err)
	}

	f := &wsFetcher{conn: conn, pending: make(map[int]chan json.RawMessage)}
	state := statedb.NewStateDB(f)

	// Read initial_dump
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, nil, statedb.EVMConfig{}, fmt.Errorf("read initial_dump: %w", err)
	}
	var dump stateServerMessage
	if err := json.Unmarshal(msg, &dump); err != nil {
		return nil, nil, statedb.EVMConfig{}, fmt.Errorf("parse initial_dump: %w", err)
	}
	if dump.Type != "initial_dump" {
		return nil, nil, statedb.EVMConfig{}, fmt.Errorf("expected initial_dump, got %s", dump.Type)
	}

	f.block = dump.BlockNumber
	storageCount := 0
	accountData := make(map[string]map[string]string)

	for _, entry := range dump.Entries {
		key, value := entry[0], entry[1]
		if strings.HasPrefix(key, "s:") {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) == 3 {
				state.SetStorageSlot(common.HexToAddress(parts[1]), common.HexToHash(parts[2]), common.HexToHash(value))
				storageCount++
			}
		} else if strings.HasPrefix(key, "b:") {
			addr := key[2:]
			if accountData[addr] == nil { accountData[addr] = make(map[string]string) }
			accountData[addr]["balance"] = value
		} else if strings.HasPrefix(key, "n:") {
			addr := key[2:]
			if accountData[addr] == nil { accountData[addr] = make(map[string]string) }
			accountData[addr]["nonce"] = value
		} else if strings.HasPrefix(key, "c:") {
			addr := key[2:]
			if accountData[addr] == nil { accountData[addr] = make(map[string]string) }
			accountData[addr]["code"] = value
		}
	}

	for addrStr, data := range accountData {
		addr := common.HexToAddress(addrStr)
		balance := uint256.NewInt(0)
		if b, ok := data["balance"]; ok {
			if bi, ok := new(big.Int).SetString(strings.TrimPrefix(b, "0x"), 16); ok && bi != nil {
				balance, _ = uint256.FromBig(bi)
			}
		}
		var nonce uint64
		if n, ok := data["nonce"]; ok {
			if ni, ok := new(big.Int).SetString(strings.TrimPrefix(n, "0x"), 16); ok && ni != nil {
				nonce = ni.Uint64()
			}
		}
		var code []byte
		if c, ok := data["code"]; ok && c != "0x" && c != "" {
			code, _ = hex.DecodeString(strings.TrimPrefix(c, "0x"))
		}
		state.SetAccount(addr, balance, nonce, code)
	}

	fmt.Fprintf(os.Stderr, "[benchmark] initial_dump: block=%d, %d storage, %d accounts\n", f.block, storageCount, len(accountData))

	go f.readLoop(state)

	cfg := statedb.EVMConfig{
		BlockNumber: dump.BlockNumber,
		Timestamp:   dump.Timestamp,
		ChainID:     43114,
		BaseFee:     dump.BaseFee,
		GasLimit:    dump.GasLimit,
	}

	return f, state, cfg, nil
}

func (f *wsFetcher) readLoop(state *statedb.StateDB) {
	for {
		_, msg, err := f.conn.ReadMessage()
		if err != nil { return }
		var m stateServerMessage
		if json.Unmarshal(msg, &m) != nil { continue }
		if m.Type == "block_diff" {
			for _, entry := range m.Entries {
				key, value := entry[0], entry[1]
				if strings.HasPrefix(key, "s:") {
					parts := strings.SplitN(key, ":", 3)
					if len(parts) == 3 {
						state.SetStorageSlot(common.HexToAddress(parts[1]), common.HexToHash(parts[2]), common.HexToHash(value))
					}
				}
			}
			continue
		}
		if m.ID > 0 {
			f.mu.Lock()
			ch, ok := f.pending[m.ID]
			if ok { delete(f.pending, m.ID) }
			f.mu.Unlock()
			if ok {
				if m.Error != nil && string(m.Error) != "null" { ch <- m.Error } else { ch <- m.Result }
			}
		}
	}
}

func (f *wsFetcher) call(method string, params interface{}) (json.RawMessage, error) {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	ch := make(chan json.RawMessage, 1)
	f.pending[id] = ch
	data, _ := json.Marshal(struct {
		JSONRPC string      `json:"jsonrpc"`
		ID      int         `json:"id"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params"`
	}{"2.0", id, method, params})
	err := f.conn.WriteMessage(websocket.TextMessage, data)
	f.mu.Unlock()
	if err != nil { return nil, err }
	select {
	case result := <-ch: return result, nil
	case <-time.After(30 * time.Second): return nil, fmt.Errorf("timeout")
	}
}

func (f *wsFetcher) FetchStorage(addr common.Address, slot common.Hash) common.Hash {
	params := map[string]interface{}{"address": addr.Hex(), "slot": slot.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getStorageAt", params)
	if err != nil { return common.Hash{} }
	var vr valueResult
	if json.Unmarshal(result, &vr) != nil { return common.Hash{} }
	return common.HexToHash(vr.Value)
}

func (f *wsFetcher) FetchBalance(addr common.Address) *uint256.Int {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getBalance", params)
	if err != nil { return uint256.NewInt(0) }
	var vr valueResult
	if json.Unmarshal(result, &vr) != nil { return uint256.NewInt(0) }
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok { return uint256.NewInt(0) }
	val, _ := uint256.FromBig(bi)
	return val
}

func (f *wsFetcher) FetchNonce(addr common.Address) uint64 { return 0 }
func (f *wsFetcher) FetchCode(addr common.Address) []byte { return nil }
func (f *wsFetcher) FetchBlockHash(num uint64) common.Hash { return common.Hash{} }

// ─── Main ──────────────────────────────────────────────────────────

var ROUTER = common.HexToAddress("0x000000000000000000000000cafebabe00facade")
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

func main() {
	stateServerURL := "ws://localhost:7449"
	poolLimit := 4000
	skipFormulas := false

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) { stateServerURL = os.Args[i+1] }
		if arg == "--limit" && i+1 < len(os.Args) { fmt.Sscanf(os.Args[i+1], "%d", &poolLimit) }
		if arg == "--skip-formulas" { skipFormulas = true }
	}

	_, state, cfg, err := connectStateServer(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}

	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)

	validated, invalid := registry.RegistryStats()
	fmt.Fprintf(os.Stderr, "[benchmark] registry: %d validated, %d invalid\n", validated, invalid)
	fmt.Fprintf(os.Stderr, "[benchmark] pools: %d\n", len(pools))

	// Build overrides for all tokens
	overrides := router.BuildOverrides(ROUTER, pools)

	// Apply overrides once
	baseWithOverrides := pathfinder.ApplyOverrides(state, overrides)

	// Warm pass
	fmt.Fprintf(os.Stderr, "[benchmark] warm pass...\n")
	quoteAll(baseWithOverrides, cfg, registry, pools)

	// Per-type stats
	type typeStats struct {
		FormulaCount int     `json:"formulaCount"`
		EVMCount     int     `json:"evmCount"`
		OkCount      int     `json:"okCount"`
		FailCount    int     `json:"failCount"`
		FormulaMs    float64 `json:"formulaMs"`
		EVMMs        float64 `json:"evmMs"`
	}
	byType := make(map[int]*typeStats)
	getStats := func(poolType int) *typeStats {
		if s, ok := byType[poolType]; ok {
			return s
		}
		s := &typeStats{}
		byType[poolType] = s
		return s
	}

	// Pool type names for display
	typeNames := map[int]string{
		0: "uniswap_v3", 1: "algebra", 2: "lfj_v1", 3: "lfj_v2",
		4: "dodo", 5: "woofi", 6: "balancer_v3", 7: "pharaoh_v1",
		8: "v2", 9: "uniswap_v4", 10: "erc4626", 12: "wombat",
	}

	// Hot pass with timing
	fmt.Fprintf(os.Stderr, "[benchmark] hot pass...\n")
	t0 := time.Now()

	for i := range pools {
		pool := &pools[i]
		for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
			if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
				continue
			}
			tokenIn := pool.Tokens[tokenIdx[0]]
			tokenOut := pool.Tokens[tokenIdx[1]]
			amountIn := uint256.NewInt(1_000_000_000_000_000_000)
			ts := getStats(pool.PoolType)

			calldata := pathfinder.EncodeSwapSingle(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn)

			// Try formula
			if !skipFormulas {
				reader := func(addr common.Address, key common.Hash) common.Hash {
					return state.GetState(addr, key)
				}
				ft0 := time.Now()
				if ret, ok := registry.TryQuote(reader, calldata); ok {
					elapsed := float64(time.Since(ft0).Microseconds()) / 1000.0
					ts.FormulaCount++
					ts.FormulaMs += elapsed
					var out uint256.Int
					out.SetBytes(ret)
					if !out.IsZero() {
						ts.OkCount++
					} else {
						ts.FailCount++
					}
					continue
				}
			}

			// EVM fallback
			et0 := time.Now()
			execState := baseWithOverrides.NewOverlay()
			ret, _, evmErr := statedb.ExecuteCall(execState, cfg, DUMMY_SENDER, ROUTER, calldata)
			elapsed := float64(time.Since(et0).Microseconds()) / 1000.0
			ts.EVMCount++
			ts.EVMMs += elapsed
			if evmErr == nil && len(ret) >= 32 {
				var out uint256.Int
				out.SetBytes(ret[:32])
				if !out.IsZero() {
					ts.OkCount++
				} else {
					ts.FailCount++
				}
			} else {
				ts.FailCount++
			}
		}
	}

	totalMs := float64(time.Since(t0).Milliseconds())

	// Print per-type breakdown
	fmt.Fprintf(os.Stderr, "\n%-16s %6s %8s %8s %6s %8s %8s\n", "TYPE", "POOLS", "FORMULA", "F_MS", "EVM", "E_MS", "TOTAL_MS")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 70))

	var totalFormula, totalEVM, totalOk, totalFail int
	var totalFormulaMs, totalEVMMs float64

	// Sort by total time descending
	type sortEntry struct {
		poolType int
		totalMs  float64
	}
	var sorted []sortEntry
	for pt, s := range byType {
		sorted = append(sorted, sortEntry{pt, s.FormulaMs + s.EVMMs})
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].totalMs > sorted[i].totalMs {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	for _, e := range sorted {
		s := byType[e.poolType]
		name := typeNames[e.poolType]
		if name == "" {
			name = fmt.Sprintf("type_%d", e.poolType)
		}
		poolCount := (s.FormulaCount + s.EVMCount) / 2 // each pool quoted in both directions
		fmt.Fprintf(os.Stderr, "%-16s %6d %8d %8.1f %6d %8.1f %8.1f\n",
			name, poolCount, s.FormulaCount, s.FormulaMs, s.EVMCount, s.EVMMs, s.FormulaMs+s.EVMMs)
		totalFormula += s.FormulaCount
		totalEVM += s.EVMCount
		totalFormulaMs += s.FormulaMs
		totalEVMMs += s.EVMMs
		totalOk += s.OkCount
		totalFail += s.FailCount
	}

	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 70))
	totalQuotes := totalFormula + totalEVM
	fmt.Fprintf(os.Stderr, "%-16s %6d %8d %8.1f %6d %8.1f %8.1f\n",
		"TOTAL", len(pools), totalFormula, totalFormulaMs, totalEVM, totalEVMMs, totalMs)

	// JSON output
	result := map[string]interface{}{
		"pools":        len(pools),
		"totalQuotes":  totalQuotes,
		"okCount":      totalOk,
		"failCount":    totalFail,
		"totalMs":      totalMs,
		"formulaCount": totalFormula,
		"evmCount":     totalEVM,
		"formulaMs":    fmt.Sprintf("%.1f", totalFormulaMs),
		"evmMs":        fmt.Sprintf("%.1f", totalEVMMs),
		"msPerQuote":   fmt.Sprintf("%.3f", totalMs/float64(totalQuotes)),
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}

func quoteAll(base *statedb.StateDB, cfg statedb.EVMConfig, registry *formulas.Registry, pools []pathfinder.Pool) {
	for i := range pools {
		pool := &pools[i]
		if len(pool.Tokens) < 2 { continue }
		calldata := pathfinder.EncodeSwapSingle(pool.Address, pool.PoolType, pool.Tokens[0], pool.Tokens[1], uint256.NewInt(1_000_000_000_000_000_000))
		reader := func(addr common.Address, key common.Hash) common.Hash { return base.GetState(addr, key) }
		if _, ok := registry.TryQuote(reader, calldata); ok { continue }
		execState := base.NewOverlay()
		statedb.ExecuteCall(execState, cfg, DUMMY_SENDER, ROUTER, calldata)
	}
}

