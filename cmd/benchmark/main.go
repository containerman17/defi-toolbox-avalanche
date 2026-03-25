package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"runtime/pprof"
	"strconv"
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

	// TEMPORARY: skip initial_dump loading to test without cache
	skipDump := false
	for _, arg := range os.Args {
		if arg == "--no-dump" { skipDump = true }
	}

	for _, entry := range dump.Entries {
		if skipDump { break }
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

func (f *wsFetcher) FetchNonce(addr common.Address) uint64 {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getNonce", params)
	if err != nil { return 0 }
	var vr valueResult
	if json.Unmarshal(result, &vr) != nil { return 0 }
	n, _ := strconv.ParseUint(strings.TrimPrefix(vr.Value, "0x"), 16, 64)
	return n
}

func (f *wsFetcher) FetchCode(addr common.Address) []byte {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getCode", params)
	if err != nil { return nil }
	var vr valueResult
	if json.Unmarshal(result, &vr) != nil { return nil }
	if vr.Value == "" || vr.Value == "0x" { return nil }
	code, _ := hex.DecodeString(strings.TrimPrefix(vr.Value, "0x"))
	return code
}

func (f *wsFetcher) FetchBlockHash(num uint64) common.Hash { return common.Hash{} }

// ─── Main ──────────────────────────────────────────────────────────

var ROUTER = common.HexToAddress("0x000000000000000000000000cafebabe00facade")
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

func main() {
	stateServerURL := "ws://localhost:7449"
	poolLimit := 4000
	skipFormulas := false

	correctnessMode := false
	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) { stateServerURL = os.Args[i+1] }
		if arg == "--limit" && i+1 < len(os.Args) { fmt.Sscanf(os.Args[i+1], "%d", &poolLimit) }
		if arg == "--skip-formulas" { skipFormulas = true }
		if arg == "--correctness" { correctnessMode = true }
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

	// Register V4 pools from ExtraData
	registerV4Pools(pools)

	// Register Balancer V3 pools from state
	registerBalancerV3Pools(pools, state, cfg, registry)

	// Register Balancer V2 pools from state
	registerBalancerV2Pools(pools, state, cfg, registry)

	// Build overrides for all tokens
	overrides := router.BuildOverrides(ROUTER, pools)

	// Apply overrides flat — no overlay indirection, CallState reads one layer
	baseWithOverrides := pathfinder.ApplyOverridesFlat(state, overrides)

	// ─── Correctness mode: compare formula vs EVM for every pool ───
	if correctnessMode {
		fmt.Fprintf(os.Stderr, "[correctness] comparing formula vs EVM for %d pools...\n", len(pools))
		evmCtx := statedb.GetCachedContext(cfg)
		cs := statedb.NewCallState(baseWithOverrides)
		poolReader := func(addr common.Address, key common.Hash) common.Hash { return state.GetState(addr, key) }
		pm := formulas.NewPoolManager(registry, poolReader)
		for i := range pools {
			if len(pools[i].Tokens) >= 2 {
				pm.SetPoolTokens(pools[i].Address, pools[i].Tokens[0], pools[i].Tokens[1])
			}
			pm.SetPoolType(pools[i].Address, pools[i].PoolType, pools[i].Dex)
		}

		// Warm pass
		for i := range pools {
			p := &pools[i]
			if len(p.Tokens) < 2 { continue }
			amountIn := uint256.NewInt(1_000_000_000_000_000_000)
			zeroForOne := p.Tokens[0].Cmp(p.Tokens[1]) < 0
			if q := pm.Get(p.Address); q != nil { q.Quote(amountIn, zeroForOne) }
			cd := pathfinder.EncodeSwapSingle(p.Address, p.PoolType, p.Tokens[0], p.Tokens[1], amountIn)
			cs.Reset()
			evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, cd)
		}

		var total, match, mismatch, formulaOnly, evmOnly, bothFail int
		for i := range pools {
			p := &pools[i]
			if len(p.Tokens) < 2 { continue }
			for _, dir := range [][2]int{{0, 1}, {1, 0}} {
				tokenIn, tokenOut := p.Tokens[dir[0]], p.Tokens[dir[1]]
				amountIn := uint256.NewInt(1_000_000_000_000_000_000)
				zeroForOne := tokenIn.Cmp(tokenOut) < 0
				total++

				// Formula result
				var formulaOut *uint256.Int
				if q := pm.Get(p.Address); q != nil {
					formulaOut, _ = q.Quote(amountIn, zeroForOne)
				}

				// EVM result
				cd := pathfinder.EncodeSwapSingle(p.Address, p.PoolType, tokenIn, tokenOut, amountIn)
				cs.Reset()
				ret, _, evmErr := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, cd)
				var evmOut *uint256.Int
				if evmErr == nil && len(ret) >= 32 {
					var out uint256.Int
					out.SetBytes(ret[:32])
					if !out.IsZero() { evmOut = &out }
				}

				if formulaOut == nil && evmOut == nil {
					bothFail++
				} else if formulaOut == nil && evmOut != nil {
					evmOnly++
				} else if formulaOut != nil && evmOut == nil {
					formulaOnly++
				} else if formulaOut.Eq(evmOut) {
					match++
				} else {
					// Check within 0.01 PPM (1/1,000,000 of a percent = 1e-8 relative)
					var diff uint256.Int
					if formulaOut.Gt(evmOut) {
						diff.Sub(formulaOut, evmOut)
					} else {
						diff.Sub(evmOut, formulaOut)
					}
					// diff/evmOut < 1e-8  ⟺  diff * 1e8 < evmOut
					var scaled uint256.Int
					scaled.Mul(&diff, uint256.NewInt(100_000_000))
					if scaled.Lt(evmOut) {
						match++ // within 0.01 PPM tolerance
					} else {
						mismatch++
						if mismatch <= 200 {
							fmt.Fprintf(os.Stderr, "  MISMATCH %s dir=%d formula=%s evm=%s\n",
								p.Address.Hex()[:12], dir[0], formulaOut.Dec(), evmOut.Dec())
						}
					}
				}
			}
		}
		pct := 0.0
		if match+mismatch > 0 { pct = float64(match) / float64(match+mismatch) * 100 }
		fmt.Fprintf(os.Stderr, "\n[correctness] %d total, %d match, %d mismatch, %d formula-only, %d evm-only, %d both-fail\n",
			total, match, mismatch, formulaOnly, evmOnly, bothFail)
		fmt.Fprintf(os.Stderr, "[correctness] %.1f%% correct (%d/%d)\n", pct, match, match+mismatch)

		// Log result
		gitHash := "unknown"
		if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
			gitHash = strings.TrimSpace(string(out))
		}
		ts := time.Now().Format("2006-01-02_15:04")
		result := fmt.Sprintf("%.0f", pct)
		line := fmt.Sprintf("time=%s git=%s result=%s match=%d mismatch=%d formula_only=%d evm_only=%d\n",
			ts, gitHash, result, match, mismatch, formulaOnly, evmOnly)
		f, err := os.OpenFile("benchmark_results/evm_correctness.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil { f.WriteString(line); f.Close() }
		fmt.Printf(`{"correctness": %s, "match": %d, "mismatch": %d}`+"\n", result, match, mismatch)
		return
	}

	// Pre-build PoolManager for struct-based quoting
	warmReader := func(addr common.Address, key common.Hash) common.Hash {
		return state.GetState(addr, key)
	}
	warmPM := formulas.NewPoolManager(registry, warmReader)
	for i := range pools {
		if len(pools[i].Tokens) >= 2 {
			warmPM.SetPoolTokens(pools[i].Address, pools[i].Tokens[0], pools[i].Tokens[1])
		}
		warmPM.SetPoolType(pools[i].Address, pools[i].PoolType, pools[i].Dex)
	}

	// Warm passes before the hot (timed) pass.
	// Default 2: first builds pool structs + JUMPDEST caches, second warms CPU caches.
	numPasses := 2
	for i, arg := range os.Args {
		if arg == "--passes" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &numPasses)
		}
	}
	for pass := 1; pass <= numPasses; pass++ {
		passT0 := time.Now()
		quoteAll(baseWithOverrides, cfg, registry, pools, skipFormulas, state, warmPM)
		fmt.Fprintf(os.Stderr, "[benchmark] pass %d: %dms\n", pass, time.Since(passT0).Milliseconds())
	}

	// Per-type stats
	type typeStats struct {
		FormulaCount  int     `json:"formulaCount"`
		EVMCount      int     `json:"evmCount"`
		OkCount       int     `json:"okCount"`
		FailCount     int     `json:"failCount"`
		FormulaMs     float64 `json:"formulaMs"`
		EVMMs         float64 `json:"evmMs"`
		FormulaReadNs int64   // total nanoseconds in storage reads
		FormulaMathNs int64   // total nanoseconds in math (total - reads)
		FormulaReads  int     // total storage read count
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
		4: "dodo", 5: "woofi_v2", 6: "balancer_v3", 7: "pharaoh_v1",
		8: "v2", 9: "uniswap_v4", 10: "erc4626", 12: "wombat",
		13: "platypus", 16: "balancer_v2", 17: "cavalre", 18: "kyber_dmm",
		19: "synapse", 20: "trident",
	}

	// Hot pass with timing — uses CallState (thin overlay) + CallerContract (JUMPDEST sharing)
	fmt.Fprintf(os.Stderr, "[benchmark] hot pass...\n")

	// Profiling for hot pass
	var cpuProfile, memProfile, profileMode string
	for i, arg := range os.Args {
		if arg == "--cpuprofile" && i+1 < len(os.Args) { cpuProfile = os.Args[i+1] }
		if arg == "--memprofile" && i+1 < len(os.Args) { memProfile = os.Args[i+1] }
		if arg == "--profile-mode" && i+1 < len(os.Args) { profileMode = os.Args[i+1] }
	}
	if cpuProfile != "" {
		f, _ := os.Create(cpuProfile)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if memProfile != "" {
		defer func() {
			f, _ := os.Create(memProfile)
			pprof.WriteHeapProfile(f)
			f.Close()
		}()
	}
	// profileMode: "formulas-only" skips EVM, "evm-only" skips formulas (same as --skip-formulas)
	if profileMode == "evm-only" {
		skipFormulas = true
	}

	t0 := time.Now()

	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(baseWithOverrides)

	// Pool manager: struct-based quoting (reads state once, quotes from memory)
	poolReader := func(addr common.Address, key common.Hash) common.Hash {
		return state.GetState(addr, key)
	}
	pm := formulas.NewPoolManager(registry, poolReader)
	for i := range pools {
		if len(pools[i].Tokens) >= 2 {
			pm.SetPoolTokens(pools[i].Address, pools[i].Tokens[0], pools[i].Tokens[1])
		}
		pm.SetPoolType(pools[i].Address, pools[i].PoolType, pools[i].Dex)
	}

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
			zeroForOne := tokenIn.Cmp(tokenOut) < 0

			// Try pool struct first (pure math, zero state access, no calldata needed)
			if !skipFormulas {
				if quoter := pm.Get(pool.Address); quoter != nil {
					ft0 := time.Now()
					if out, ok := quoter.Quote(amountIn, zeroForOne); ok {
						elapsed := time.Since(ft0).Nanoseconds()
						ts.FormulaCount++
						ts.FormulaMs += float64(elapsed) / 1e6
						ts.FormulaMathNs += elapsed
						ts.OkCount++
						_ = out
						continue
					}
					// Struct exists but returned zero — skip TryQuote fallback
					ts.FailCount++
					continue
				}
				// No struct (LFJ V2, Algebra) — fallback to function-based formula
				calldata := pathfinder.EncodeSwapSingle(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn)
				var readCount int
				var readNs int64
				ft0 := time.Now()
				reader := func(addr common.Address, key common.Hash) common.Hash {
					rt0 := time.Now()
					val := state.GetState(addr, key)
					readNs += time.Since(rt0).Nanoseconds()
					readCount++
					return val
				}
				if ret, ok := registry.TryQuote(reader, calldata); ok {
					totalNs := time.Since(ft0).Nanoseconds()
					mathNs := totalNs - readNs
					ts.FormulaCount++
					ts.FormulaMs += float64(totalNs) / 1e6
					ts.FormulaReadNs += readNs
					ts.FormulaMathNs += mathNs
					ts.FormulaReads += readCount
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

			calldata := pathfinder.EncodeSwapSingle(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn)


			// EVM fallback — skip if profiling formulas only
			if profileMode == "formulas-only" {
				ts.FailCount++
				continue
			}
			et0 := time.Now()
			cs.Reset()
			ret, _, evmErr := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
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
	fmt.Fprintf(os.Stderr, "\n%-16s %6s %8s %8s %8s %8s %6s %6s %8s\n",
		"TYPE", "POOLS", "FORMULA", "F_MS", "READ_MS", "MATH_MS", "READS", "EVM", "E_MS")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 90))

	var totalFormula, totalEVM, totalOk, totalFail int
	var totalFormulaMs, totalEVMMs float64
	var totalReadNs, totalMathNs int64
	var totalReads int

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
		poolCount := (s.FormulaCount + s.EVMCount) / 2
		readMs := float64(s.FormulaReadNs) / 1e6
		mathMs := float64(s.FormulaMathNs) / 1e6
		readsPerQuote := 0
		if s.FormulaCount > 0 {
			readsPerQuote = s.FormulaReads / s.FormulaCount
		}
		fmt.Fprintf(os.Stderr, "%-16s %6d %8d %8.1f %8.1f %8.1f %6d %6d %8.1f\n",
			name, poolCount, s.FormulaCount, s.FormulaMs, readMs, mathMs, readsPerQuote, s.EVMCount, s.EVMMs)
		totalFormula += s.FormulaCount
		totalEVM += s.EVMCount
		totalFormulaMs += s.FormulaMs
		totalEVMMs += s.EVMMs
		totalOk += s.OkCount
		totalFail += s.FailCount
		totalReadNs += s.FormulaReadNs
		totalMathNs += s.FormulaMathNs
		totalReads += s.FormulaReads
	}

	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 90))
	totalQuotes := totalFormula + totalEVM
	fmt.Fprintf(os.Stderr, "%-16s %6d %8d %8.1f %8.1f %8.1f %6s %6d %8.1f\n",
		"TOTAL", len(pools), totalFormula, totalFormulaMs,
		float64(totalReadNs)/1e6, float64(totalMathNs)/1e6, "", totalEVM, totalEVMMs)

	// JSON output
	msPerPool := totalMs / float64(len(pools))
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
		"msPerPool":    fmt.Sprintf("%.3f", msPerPool),
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))

	// Append to benchmark_results/evm_speed.log for regression tracking
	var logResult string
	for i, arg := range os.Args {
		if arg == "--log" && i+1 < len(os.Args) { logResult = os.Args[i+1] }
	}
	if logResult == "" && !skipFormulas && profileMode == "" {
		logResult = "benchmark_results/evm_speed.log"
	}
	if logResult != "" {
		gitHash := "unknown"
		if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
			gitHash = strings.TrimSpace(string(out))
		}
		ts := time.Now().Format("2006-01-02_15:04")
		line := fmt.Sprintf("time=%s git=%s result=%.3f pools=%d formulas=%d evm=%d ok=%d\n",
			ts, gitHash, msPerPool, len(pools), totalFormula, totalEVM, totalOk)
		os.MkdirAll("benchmark_results", 0o755)
		f, err := os.OpenFile(logResult, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.WriteString(line)
			f.Close()
			fmt.Fprintf(os.Stderr, "\nLogged to %s: %.3f ms/pool\n", logResult, msPerPool)
		}
	}
}

// registerV4Pools parses V4 pool ExtraData and registers them with the formula system.
func registerV4Pools(pools []pathfinder.Pool) {
	count := 0
	for _, p := range pools {
		if p.PoolType != 9 || p.ExtraData == "" {
			continue
		}
		// ExtraData format: id=0x...,fee=18,ts=1,hooks=0x...
		var poolIdHex string
		var fee uint32
		var tickSpacing int32
		var hooks string
		for _, kv := range strings.Split(p.ExtraData, ",") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "id":
				poolIdHex = parts[1]
			case "fee":
				var f int
				fmt.Sscanf(parts[1], "%d", &f)
				fee = uint32(f)
			case "ts":
				var t int
				fmt.Sscanf(parts[1], "%d", &t)
				tickSpacing = int32(t)
			case "hooks":
				hooks = parts[1]
			}
		}
		if poolIdHex == "" || tickSpacing == 0 {
			continue
		}
		var poolId [32]byte
		idBytes := common.FromHex(poolIdHex)
		copy(poolId[:], idBytes)

		var hookFeePpm uint32
		var hooksAddr common.Address
		if hooks != "" {
			hooksAddr = common.HexToAddress(hooks)
		}

		poolAddr := strings.ToLower(p.Address.Hex())
		formulas.RegisterV4Pool(poolAddr, poolId, tickSpacing, fee, hookFeePpm, hooksAddr)
		count++
	}
	if count > 0 {
		fmt.Fprintf(os.Stderr, "[benchmark] registered %d V4 pools\n", count)
	}
}

func quoteAll(base *statedb.StateDB, cfg statedb.EVMConfig, registry *formulas.Registry, pools []pathfinder.Pool, skipFormulas bool, rawState *statedb.StateDB, pm *formulas.PoolManager) {
	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(base)
	for i := range pools {
		pool := &pools[i]
		for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
			if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
				continue
			}
			tokenIn := pool.Tokens[tokenIdx[0]]
			tokenOut := pool.Tokens[tokenIdx[1]]
			amountIn := uint256.NewInt(1_000_000_000_000_000_000)
			zeroForOne := tokenIn.Cmp(tokenOut) < 0

			if !skipFormulas {
				// Try pool struct
				if quoter := pm.Get(pool.Address); quoter != nil {
					quoter.Quote(amountIn, zeroForOne)
					continue // formula quoter exists — skip EVM even if quote returns 0 (empty pool)
				}
				// Fallback to function-based formula
				calldata := pathfinder.EncodeSwapSingle(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn)
				reader := func(addr common.Address, key common.Hash) common.Hash { return rawState.GetState(addr, key) }
				if _, ok := registry.TryQuote(reader, calldata); ok {
					continue
				}
			}

			calldata := pathfinder.EncodeSwapSingle(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn)
			cs.Reset()
			evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
		}
	}
}

// registerBalancerV3Pools discovers and registers Balancer V3 pool parameters via EVM calls.
func registerBalancerV3Pools(pools []pathfinder.Pool, state *statedb.StateDB, cfg statedb.EVMConfig, registry *formulas.Registry) {
	count := 0
	for _, p := range pools {
		if p.PoolType != 6 || len(p.Tokens) < 2 {
			continue
		}
		// Only handle 2-token pools for now
		if len(p.Tokens) != 2 {
			continue
		}

		poolAddr := strings.ToLower(p.Address.Hex())

		// Try getAmplificationParameter() → selector 0x6daccffa
		ampSelector := common.FromHex("0x6daccffa")
		ampResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, ampSelector)
		if err == nil && len(ampResult) >= 96 {
			ampVal := new(big.Int).SetBytes(ampResult[0:32])
			if ampVal.Sign() > 0 {
				info := &formulas.BalancerV3PoolInfo{
					PoolType:  formulas.BalV3Stable,
					NumTokens: len(p.Tokens),
					Tokens:    p.Tokens,
					Amp:       ampVal,
				}
				formulas.RegisterBalancerV3Pool(poolAddr, info)
				registry.SetFormulaID(p.Address, formulas.FormulaBalancerV3)
				count++
				continue
			}
		}

		// Try getNormalizedWeights() → selector 0xf89f27ed
		weightsSelector := common.FromHex("0xf89f27ed")
		weightsResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, weightsSelector)
		if err == nil && len(weightsResult) >= 96 {
			if len(weightsResult) >= 64 {
				numWeights := new(big.Int).SetBytes(weightsResult[32:64]).Int64()
				if numWeights == int64(len(p.Tokens)) && len(weightsResult) >= 64+int(numWeights)*32 {
					weights := make([]*big.Int, numWeights)
					allValid := true
					for i := int64(0); i < numWeights; i++ {
						off := 64 + i*32
						weights[i] = new(big.Int).SetBytes(weightsResult[off : off+32])
						if weights[i].Sign() <= 0 {
							allValid = false
							break
						}
					}
					if allValid {
						info := &formulas.BalancerV3PoolInfo{
							PoolType:  formulas.BalV3Weighted,
							NumTokens: len(p.Tokens),
							Tokens:    p.Tokens,
							Weights:   weights,
						}
						formulas.RegisterBalancerV3Pool(poolAddr, info)
						registry.SetFormulaID(p.Address, formulas.FormulaBalancerV3)
						count++
						continue
					}
				}
			}
		}
	}
	if count > 0 {
		fmt.Fprintf(os.Stderr, "[benchmark] registered %d Balancer V3 pools\n", count)
	}
}

// registerBalancerV2Pools discovers and registers Balancer V2 weighted pool parameters via EVM calls.
// It uses getNormalizedWeights() and getSwapFeePercentage() on each pool contract.
// Balances are read from Vault storage at quote time (not cached here).
func registerBalancerV2Pools(pools []pathfinder.Pool, state *statedb.StateDB, cfg statedb.EVMConfig, registry *formulas.Registry) {
	count := 0
	for _, p := range pools {
		if p.PoolType != 16 || len(p.Tokens) < 2 {
			continue
		}
		// Parse poolId from ExtraData (@poolId=0x...)
		var poolIdHex string
		for _, kv := range strings.Split(p.ExtraData, ",") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 && parts[0] == "poolId" {
				poolIdHex = parts[1]
			}
		}
		if poolIdHex == "" {
			continue
		}
		poolIdBytes := common.FromHex(poolIdHex)
		if len(poolIdBytes) != 32 {
			continue
		}
		var poolId [32]byte
		copy(poolId[:], poolIdBytes)

		poolAddr := strings.ToLower(p.Address.Hex())
		numTokens := len(p.Tokens)

		// Only handle 2-token pools: >2 tokens requires knowing which token pair
		// is being swapped, which the PoolQuoter interface doesn't expose.
		if numTokens != 2 {
			continue
		}

		// Determine specialization from poolId bytes 20-21 (big-endian).
		spec := (int(poolId[20]) << 8) | int(poolId[21])
		if spec != 1 && spec != 2 {
			// Only MinimalSwapInfo (1) and TwoToken (2) supported.
			continue
		}

		// Fetch normalized weights: getNormalizedWeights() selector = 0xf89f27ed
		weightsSelector := common.FromHex("0xf89f27ed")
		weightsResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, weightsSelector)
		if err != nil || len(weightsResult) < 96 {
			continue
		}
		// ABI decode: uint256[] — offset(32) + length(32) + data(32*n)
		numWeights := new(big.Int).SetBytes(weightsResult[32:64]).Int64()
		if numWeights != int64(numTokens) || len(weightsResult) < 64+int(numWeights)*32 {
			continue
		}
		weights := make([]*big.Int, numWeights)
		allValid := true
		for i := int64(0); i < numWeights; i++ {
			off := 64 + i*32
			weights[i] = new(big.Int).SetBytes(weightsResult[off : off+32])
			if weights[i].Sign() <= 0 {
				allValid = false
				break
			}
		}
		if !allValid {
			continue
		}

		// Fetch swap fee: getSwapFeePercentage() selector = 0x55c67628
		feeSelector := common.FromHex("0x55c67628")
		feeResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, feeSelector)
		if err != nil || len(feeResult) < 32 {
			continue
		}
		swapFee := new(big.Int).SetBytes(feeResult[0:32])
		if swapFee.Sign() <= 0 {
			continue
		}

		// Build per-token scaling factors from token decimal counts.
		// scalingFactor = 10^(18 - decimals).  We fetch decimals() from each token.
		scalingFactors := make([]*big.Int, numTokens)
		decimalsSelector := common.FromHex("0x313ce567") // decimals()
		sfValid := true
		for i, tok := range p.Tokens {
			decResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, tok, decimalsSelector)
			if err != nil || len(decResult) < 32 {
				sfValid = false
				break
			}
			dec := new(big.Int).SetBytes(decResult[24:32]).Int64() // last byte is sufficient
			if dec < 0 || dec > 18 {
				sfValid = false
				break
			}
			exp := int64(18) - dec
			scalingFactors[i] = new(big.Int).Exp(big.NewInt(10), big.NewInt(exp), nil)
		}
		if !sfValid {
			continue
		}

		info := &formulas.BalancerV2PoolInfo{
			PoolId:            poolId,
			Specialization:    spec,
			NumTokens:         numTokens,
			Tokens:            p.Tokens,
			Weights:           weights,
			SwapFeePercentage: swapFee,
			ScalingFactors:    scalingFactors,
		}
		formulas.RegisterBalancerV2Pool(poolAddr, info)
		registry.SetFormulaID(p.Address, formulas.FormulaBalancerV2)
		count++
	}
	if count > 0 {
		fmt.Fprintf(os.Stderr, "[benchmark] registered %d Balancer V2 pools\n", count)
	}
}

