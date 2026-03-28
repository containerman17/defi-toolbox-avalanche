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
	"github.com/ava-labs/libevm/crypto"
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

func (f *wsFetcher) FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error) {
	params := map[string]interface{}{"address": addr.Hex(), "slot": slot.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getStorageAt", params)
	if err != nil { return common.Hash{}, err }
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil { return common.Hash{}, err }
	return common.HexToHash(vr.Value), nil
}

func (f *wsFetcher) FetchBalance(addr common.Address) (*uint256.Int, error) {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getBalance", params)
	if err != nil { return nil, err }
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil { return nil, err }
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok { return nil, fmt.Errorf("bad hex: %s", vr.Value) }
	val, _ := uint256.FromBig(bi)
	return val, nil
}

func (f *wsFetcher) FetchNonce(addr common.Address) (uint64, error) {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getNonce", params)
	if err != nil { return 0, err }
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil { return 0, err }
	n, _ := strconv.ParseUint(strings.TrimPrefix(vr.Value, "0x"), 16, 64)
	return n, nil
}

func (f *wsFetcher) FetchCode(addr common.Address) ([]byte, error) {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": f.block}
	result, err := f.call("state_getCode", params)
	if err != nil { return nil, err }
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil { return nil, err }
	if vr.Value == "" || vr.Value == "0x" { return nil, nil }
	code, _ := hex.DecodeString(strings.TrimPrefix(vr.Value, "0x"))
	return code, nil
}

func (f *wsFetcher) FetchBlockHash(num uint64) (common.Hash, error) { return common.Hash{}, nil }

// ─── Types ──────────────────────────────────────────────────────────

var ROUTER = router.DeployedRouter
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

type quoteKey struct {
	pool common.Address
	dir  int
}

type typeStats struct {
	Quotes   int     // total quotes (2 per pool)
	HotMs    float64 // hot pass execution time
	Match    int     // result == EVM ground truth
	Mismatch int     // result != EVM ground truth
	NonZero  int     // EVM ground truth was non-zero
	Formula  int     // quotes handled by formula
}

type blockResult struct {
	blockNum    uint64
	byType      map[int]*typeStats
	mismatchSet map[quoteKey]bool // true = mismatched on this block
	mismatchLog []string
}

var typeNames = map[int]string{
	0: "uniswap_v3", 1: "algebra", 2: "lfj_v1", 3: "lfj_v2",
	4: "dodo", 5: "woofi_v2", 6: "balancer_v3", 7: "pharaoh_v1",
	8: "v2", 9: "uniswap_v4", 10: "erc4626", 12: "wombat",
	13: "platypus", 16: "balancer_v2", 17: "cavalre", 18: "kyber_dmm",
	19: "synapse", 20: "trident",
}

// ─── Per-block benchmark ────────────────────────────────────────────

func runBlockBenchmark(
	blockNum uint64,
	registry *formulas.Registry,
	pools []pathfinder.Pool,
	overrides []pathfinder.ParsedOverride,
	skipFormulas bool,
	stateServerHost string,
) (*blockResult, error) {
	stateServerURL := fmt.Sprintf("ws://%s/debug/%d", stateServerHost, blockNum)

	f, state, cfg, err := connectStateServer(stateServerURL)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", blockNum, err)
	}
	defer f.conn.Close()

	// Register Balancer V3/V2 pools (EVM calls against this block's state)
	registerBalancerV3Pools(pools, state, cfg, registry)
	registerBalancerV2Pools(pools, state, cfg, registry)

	// Apply token overrides to this block's statedb
	baseWithOverrides := pathfinder.ApplyOverridesFlat(state, overrides)

	// Build PoolManager with this block's statedb
	poolReader := func(addr common.Address, key common.Hash) common.Hash {
		return state.GetState(addr, key)
	}
	pm := formulas.NewPoolManager(registry, poolReader)
	pm.SetBlockTimestamp(cfg.Timestamp)
	pm.SetEVMCaller(func(to common.Address, data []byte) ([]byte, bool) {
		result, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, to, data)
		return result, err == nil
	})
	for i := range pools {
		if len(pools[i].Tokens) >= 2 {
			pm.SetPoolTokens(pools[i].Address, pools[i].Tokens[0], pools[i].Tokens[1])
		}
		pm.SetPoolType(pools[i].Address, pools[i].PoolType, pools[i].Dex)
	}

	// ─── Pass 1: EVM ground truth (not timed) ───
	evmGround := make(map[quoteKey]uint256.Int)
	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(baseWithOverrides)

	fmt.Fprintf(os.Stderr, "[benchmark] pass 1 (EVM ground truth)...")
	p1t0 := time.Now()
	for i := range pools {
		pool := &pools[i]
		for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
			if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
				continue
			}
			tokenIn := pool.Tokens[tokenIdx[0]]
			tokenOut := pool.Tokens[tokenIdx[1]]
			amountIn := uint256.NewInt(1_000_000_000_000_000_000)
			calldata := pathfinder.EncodeSwapSingleWithExtra(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn, pool.ExtraData)
			cs.Reset()
			ret, gasUsed, evmErr := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
			key := quoteKey{pool.Address, tokenIdx[0]}
			if evmErr == nil && len(ret) >= 32 {
				var out uint256.Int
				out.SetBytes(ret[:32])
				evmGround[key] = out
			}
			if len(pools) == 1 {
				fmt.Fprintf(os.Stderr, "\n  [DEBUG] pool=%s dir=%d evmErr=%v retLen=%d gasUsed=%d ret=%x", pool.Address.Hex(), tokenIdx[0], evmErr, len(ret), gasUsed, ret)
			}
		}
	}
	fmt.Fprintf(os.Stderr, " %dms\n", time.Since(p1t0).Milliseconds())

	// ─── Pass 2: Warm-up (not timed) ───
	fmt.Fprintf(os.Stderr, "[benchmark] pass 2 (warm-up)...")
	p2t0 := time.Now()
	for i := range pools {
		pool := &pools[i]
		for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
			if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
				continue
			}
			tokenIn := pool.Tokens[tokenIdx[0]]
			tokenOut := pool.Tokens[tokenIdx[1]]
			zeroForOne := tokenIn.Cmp(tokenOut) < 0
			amountIn := uint256.NewInt(1_000_000_000_000_000_000)

			if !skipFormulas {
				pm.Get(pool.Address).Quote(amountIn, zeroForOne)
			} else {
				calldata := pathfinder.EncodeSwapSingleWithExtra(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn, pool.ExtraData)
				cs.Reset()
				evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
			}
		}
	}
	fmt.Fprintf(os.Stderr, " %dms\n", time.Since(p2t0).Milliseconds())

	// ─── Pass 3: Hot pass (timed + correctness) ───
	byType := make(map[int]*typeStats)
	getStats := func(poolType int) *typeStats {
		if s, ok := byType[poolType]; ok {
			return s
		}
		s := &typeStats{}
		byType[poolType] = s
		return s
	}

	fmt.Fprintf(os.Stderr, "[benchmark] pass 3 (hot pass)...\n")

	mismatchSet := make(map[quoteKey]bool)
	var mismatchLog []string
	t0 := time.Now()

	for i := range pools {
		pool := &pools[i]
		for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
			if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
				continue
			}
			ts := getStats(pool.PoolType)
			ts.Quotes++
			tokenIn := pool.Tokens[tokenIdx[0]]
			tokenOut := pool.Tokens[tokenIdx[1]]
			zeroForOne := tokenIn.Cmp(tokenOut) < 0
			amountIn := uint256.NewInt(1_000_000_000_000_000_000)

			key := quoteKey{pool.Address, tokenIdx[0]}
			evmResult := evmGround[key]
			if !evmResult.IsZero() {
				ts.NonZero++
			}

			qt0 := time.Now()
			result := pm.Get(pool.Address).Quote(amountIn, zeroForOne)
			ts.Formula++
			ts.HotMs += float64(time.Since(qt0).Nanoseconds()) / 1e6

			if result.Eq(&evmResult) {
				ts.Match++
			} else {
				var diff uint256.Int
				if result.Gt(&evmResult) {
					diff.Sub(&result, &evmResult)
				} else {
					diff.Sub(&evmResult, &result)
				}
				var scaled uint256.Int
				if pool.PoolType == 3 {
					scaled.Mul(&diff, uint256.NewInt(100_000))
				} else {
					scaled.Mul(&diff, uint256.NewInt(100_000_000))
				}
				denom := &evmResult
				if result.Gt(&evmResult) { denom = &result }
				if !denom.IsZero() && scaled.Lt(denom) {
					ts.Match++
				} else {
					ts.Mismatch++
					mismatchSet[key] = true
					if len(mismatchLog) < 200 {
						mismatchLog = append(mismatchLog, fmt.Sprintf("  MISMATCH %s dir=%d result=%s evm=%s",
							pool.Address.Hex(), tokenIdx[0], result.Dec(), evmResult.Dec()))
					}
				}
			}
		}
	}

	_ = t0 // used above for timing

	return &blockResult{
		blockNum:    blockNum,
		byType:      byType,
		mismatchSet: mismatchSet,
		mismatchLog: mismatchLog,
	}, nil
}

// printTypeTable prints the per-type breakdown table to stderr and returns totals.
func printTypeTable(byType map[int]*typeStats, poolCount int) (totalQuotes, totalMatch, totalMismatch, totalNonZero, totalFmla int, totalHotMs float64) {
	fmt.Fprintf(os.Stderr, "\n%-16s %6s %8s %6s %8s %6s %6s %8s\n",
		"TYPE", "POOLS", "QUOTES", "FMLA", "MS", "MATCH", "MISS", "NONZERO%")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 74))

	type sortEntry struct {
		poolType int
		ms       float64
	}
	var sorted []sortEntry
	for pt, s := range byType {
		sorted = append(sorted, sortEntry{pt, s.HotMs})
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].ms > sorted[i].ms {
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
		pc := s.Quotes / 2
		nzPct := 0.0
		if s.Quotes > 0 { nzPct = float64(s.NonZero) / float64(s.Quotes) * 100 }
		fmt.Fprintf(os.Stderr, "%-16s %6d %8d %6d %8.1f %6d %6d %7.1f%%\n",
			name, pc, s.Quotes, s.Formula, s.HotMs, s.Match, s.Mismatch, nzPct)
		totalQuotes += s.Quotes
		totalMatch += s.Match
		totalMismatch += s.Mismatch
		totalNonZero += s.NonZero
		totalFmla += s.Formula
		totalHotMs += s.HotMs
	}

	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 74))
	totalNzPct := 0.0
	if totalQuotes > 0 { totalNzPct = float64(totalNonZero) / float64(totalQuotes) * 100 }
	fmt.Fprintf(os.Stderr, "%-16s %6d %8d %6d %8.1f %6d %6d %7.1f%%\n",
		"TOTAL", poolCount, totalQuotes, totalFmla, totalHotMs, totalMatch, totalMismatch, totalNzPct)
	return
}

// ─── Main ──────────────────────────────────────────────────────────

func main() {
	poolLimit := 4000
	skipFormulas := false
	numBlocks := 1
	var singlePool string
	stateServerHost := "localhost:7449"

	for i, arg := range os.Args {
		if arg == "--limit" && i+1 < len(os.Args) { fmt.Sscanf(os.Args[i+1], "%d", &poolLimit) }
		if arg == "--skip-formulas" { skipFormulas = true }
		if arg == "--blocks" && i+1 < len(os.Args) { fmt.Sscanf(os.Args[i+1], "%d", &numBlocks) }
		if arg == "--pool" && i+1 < len(os.Args) { singlePool = os.Args[i+1] }
		if arg == "--state-server" && i+1 < len(os.Args) { stateServerHost = os.Args[i+1] }
	}
	if numBlocks < 1 { numBlocks = 1 }

	// Compute block numbers
	blocks := make([]uint64, numBlocks)
	for i := 0; i < numBlocks; i++ {
		blocks[i] = uint64(router.DeployedBlock) + uint64(i)*10000
	}

	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)

	// Filter to single pool if --pool specified
	if singlePool != "" {
		target := common.HexToAddress(singlePool)
		var filtered []pathfinder.Pool
		for i := range pools {
			if pools[i].Address == target {
				filtered = append(filtered, pools[i])
				break
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(os.Stderr, "pool %s not found in pool list\n", singlePool)
			os.Exit(1)
		}
		pools = filtered
	}

	validated, invalid := registry.RegistryStats()
	fmt.Fprintf(os.Stderr, "[benchmark] registry: %d validated, %d invalid\n", validated, invalid)
	fmt.Fprintf(os.Stderr, "[benchmark] pools: %d, blocks: %d\n", len(pools), numBlocks)

	// Register V4 pools from ExtraData (block-independent)
	registerV4Pools(pools)

	// Build token overrides (block-independent)
	overrides := router.BuildTokenOverrides(ROUTER, pools)

	// Profiling flags
	var cpuProfile, memProfile string
	for i, arg := range os.Args {
		if arg == "--cpuprofile" && i+1 < len(os.Args) { cpuProfile = os.Args[i+1] }
		if arg == "--memprofile" && i+1 < len(os.Args) { memProfile = os.Args[i+1] }
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

	// Run benchmark for each block
	results := make([]*blockResult, 0, numBlocks)
	for idx, blockNum := range blocks {
		if numBlocks > 1 {
			fmt.Fprintf(os.Stderr, "\n=== Block %d (%d/%d) ===\n", blockNum, idx+1, numBlocks)
		}

		res, err := runBlockBenchmark(blockNum, registry, pools, overrides, skipFormulas, stateServerHost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		results = append(results, res)

		// Print mismatches for this block
		for _, line := range res.mismatchLog {
			fmt.Fprintln(os.Stderr, line)
		}

		// Print per-type breakdown for this block
		totalQuotes, totalMatch, totalMismatch, totalNonZero, _, _ := printTypeTable(res.byType, len(pools))
		correctPct := 0.0
		if totalMatch+totalMismatch > 0 { correctPct = float64(totalMatch) / float64(totalMatch+totalMismatch) * 100 }
		totalNzPct := 0.0
		if totalQuotes > 0 { totalNzPct = float64(totalNonZero) / float64(totalQuotes) * 100 }
		fmt.Fprintf(os.Stderr, "\n[result] block %d: %.1f%% correct, %d match, %d mismatch, %.1f%% non-zero\n",
			blockNum, correctPct, totalMatch, totalMismatch, totalNzPct)
	}

	// ─── Cross-block aggregation (only when N > 1) ───
	if numBlocks > 1 {
		fmt.Fprintf(os.Stderr, "\n=== AGGREGATE (%d blocks) ===\n", numBlocks)

		// A pool+direction is "correct" only if it matched EVM on ALL blocks
		aggByType := make(map[int]*typeStats)
		aggGetStats := func(poolType int) *typeStats {
			if s, ok := aggByType[poolType]; ok { return s }
			s := &typeStats{}
			aggByType[poolType] = s
			return s
		}

		// Use first block's structure as template for iteration
		first := results[0]
		for i := range pools {
			pool := &pools[i]
			for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
				if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
					continue
				}
				key := quoteKey{pool.Address, tokenIdx[0]}
				ts := aggGetStats(pool.PoolType)
				ts.Quotes++

				// Check if mismatched on any block
				anyMismatch := false
				for _, res := range results {
					if res.mismatchSet[key] {
						anyMismatch = true
						break
					}
				}
				if anyMismatch {
					ts.Mismatch++
				} else {
					ts.Match++
				}

				// Aggregate non-zero and formula/EVM from first block
				if firstStats, ok := first.byType[pool.PoolType]; ok {
					_ = firstStats // NonZero counted per-key below
				}
			}
		}

		// Aggregate NonZero, Formula, HotMs from first block's byType
		for pt, fs := range first.byType {
			ts := aggGetStats(pt)
			ts.NonZero = fs.NonZero
			ts.Formula = fs.Formula
			ts.HotMs = fs.HotMs
		}

		totalQuotes, totalMatch, totalMismatch, totalNonZero, _, _ := printTypeTable(aggByType, len(pools))
		correctPct := 0.0
		if totalMatch+totalMismatch > 0 { correctPct = float64(totalMatch) / float64(totalMatch+totalMismatch) * 100 }
		totalNzPct := 0.0
		if totalQuotes > 0 { totalNzPct = float64(totalNonZero) / float64(totalQuotes) * 100 }
		fmt.Fprintf(os.Stderr, "\n[result] AGGREGATE: %.1f%% correct, %d match, %d mismatch, %.1f%% non-zero\n",
			correctPct, totalMatch, totalMismatch, totalNzPct)
	}

	// ─── JSON output ───
	// Use first block's stats for single-block backward compat
	first := results[0]
	firstQ, firstMatch, firstMismatch, firstNonZero, firstFmla, firstHotMs := 0, 0, 0, 0, 0, 0.0
	for _, s := range first.byType {
		firstQ += s.Quotes
		firstMatch += s.Match
		firstMismatch += s.Mismatch
		firstNonZero += s.NonZero
		firstFmla += s.Formula
		firstHotMs += s.HotMs
	}
	firstCorrectPct := 0.0
	if firstMatch+firstMismatch > 0 { firstCorrectPct = float64(firstMatch) / float64(firstMatch+firstMismatch) * 100 }
	firstNzPct := 0.0
	if firstQ > 0 { firstNzPct = float64(firstNonZero) / float64(firstQ) * 100 }
	msPerPool := firstHotMs / float64(len(pools))

	jsonResult := map[string]interface{}{
		"pools":       len(pools),
		"quotes":      firstQ,
		"formula":     firstFmla,
		"totalMs":     fmt.Sprintf("%.1f", firstHotMs),
		"msPerPool":   fmt.Sprintf("%.4f", msPerPool),
		"match":       firstMatch,
		"mismatch":    firstMismatch,
		"correctness": fmt.Sprintf("%.1f", firstCorrectPct),
		"nonZeroPct":  fmt.Sprintf("%.1f", firstNzPct),
	}

	if numBlocks > 1 {
		jsonResult["blocks"] = numBlocks

		// Per-block array
		perBlock := make([]map[string]interface{}, 0, numBlocks)
		for _, res := range results {
			bq, bm, bmm, bnz, bms := 0, 0, 0, 0, 0.0
			for _, s := range res.byType {
				bq += s.Quotes
				bm += s.Match
				bmm += s.Mismatch
				bnz += s.NonZero
				bms += s.HotMs
			}
			bc := 0.0
			if bm+bmm > 0 { bc = float64(bm) / float64(bm+bmm) * 100 }
			bnzp := 0.0
			if bq > 0 { bnzp = float64(bnz) / float64(bq) * 100 }
			perBlock = append(perBlock, map[string]interface{}{
				"block":       res.blockNum,
				"match":       bm,
				"mismatch":    bmm,
				"correctness": fmt.Sprintf("%.1f", bc),
				"nonZeroPct":  fmt.Sprintf("%.1f", bnzp),
				"totalMs":     fmt.Sprintf("%.1f", bms),
			})
		}
		jsonResult["perBlock"] = perBlock

		// Aggregate correctness
		aggMatch, aggMismatch := 0, 0
		for i := range pools {
			pool := &pools[i]
			for _, tokenIdx := range [][2]int{{0, 1}, {1, 0}} {
				if tokenIdx[0] >= len(pool.Tokens) || tokenIdx[1] >= len(pool.Tokens) {
					continue
				}
				key := quoteKey{pool.Address, tokenIdx[0]}
				anyMismatch := false
				for _, res := range results {
					if res.mismatchSet[key] {
						anyMismatch = true
						break
					}
				}
				if anyMismatch {
					aggMismatch++
				} else {
					aggMatch++
				}
			}
		}
		aggPct := 0.0
		if aggMatch+aggMismatch > 0 { aggPct = float64(aggMatch) / float64(aggMatch+aggMismatch) * 100 }
		jsonResult["aggregateMatch"] = aggMatch
		jsonResult["aggregateMismatch"] = aggMismatch
		jsonResult["aggregateCorrectness"] = fmt.Sprintf("%.1f", aggPct)
	}

	out, _ := json.MarshalIndent(jsonResult, "", "  ")
	fmt.Println(string(out))

	// Append to benchmark_results/benchmark.log
	var logResult string
	for i, arg := range os.Args {
		if arg == "--log" && i+1 < len(os.Args) { logResult = os.Args[i+1] }
	}
	if logResult == "" && !skipFormulas {
		logResult = "benchmark_results/benchmark.log"
	}
	if logResult != "" {
		gitHash := "unknown"
		if gitOut, gitErr := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); gitErr == nil {
			gitHash = strings.TrimSpace(string(gitOut))
		}
		logTs := time.Now().Format("2006-01-02_15:04")
		line := fmt.Sprintf("time=%s git=%s blocks=%d ms_per_pool=%.4f total_ms=%.1f pools=%d formula=%d match=%d mismatch=%d correctness=%.1f nonzero=%.1f\n",
			logTs, gitHash, numBlocks, msPerPool, firstHotMs, len(pools), firstFmla, firstMatch, firstMismatch, firstCorrectPct, firstNzPct)
		os.MkdirAll("benchmark_results", 0o755)
		logF, logErr := os.OpenFile(logResult, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if logErr == nil {
			logF.WriteString(line)
			logF.Close()
			fmt.Fprintf(os.Stderr, "\nLogged to %s\n", logResult)
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

// balV3VaultAddr is the Balancer V3 vault on Avalanche C-Chain.
var balV3VaultAddr = common.HexToAddress("0xba1333333333a1ba1108e8412f11850a5c319ba9")

// balV3ReadTokenInfo reads TokenInfo for a token from vault storage.
// TokenInfo is packed as: byte0=tokenType, bytes1-20=rateProvider, byte21=paysYieldFees.
// Stored in _poolTokenInfo[pool][token] at vault slot 4.
func balV3ReadTokenInfo(state *statedb.StateDB, pool, token common.Address) (tokenType uint8, rateProvider common.Address) {
	// outer slot: keccak256(pool ++ 4)
	outerKey := make([]byte, 64)
	copy(outerKey[12:32], pool.Bytes())
	outerKey[63] = 4
	outerSlot := crypto.Keccak256Hash(outerKey)

	// inner slot: keccak256(token ++ outerSlot)
	innerKey := make([]byte, 64)
	copy(innerKey[12:32], token.Bytes())
	copy(innerKey[32:64], outerSlot.Bytes())
	innerSlot := crypto.Keccak256Hash(innerKey)

	packed := state.GetState(balV3VaultAddr, innerSlot)
	val := new(big.Int).SetBytes(packed[:])

	// byte 0 (LSB): tokenType
	tokenType = uint8(val.Uint64() & 0xff)
	// bytes 1-20: rateProvider address
	rpInt := new(big.Int).And(new(big.Int).Rsh(val, 8), new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1)))
	rateProvider = common.BigToAddress(rpInt)
	return
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

		// Read per-token types and rate providers from vault storage.
		tokenTypes := make([]formulas.BalV3TokenType, len(p.Tokens))
		rateProviders := make([]common.Address, len(p.Tokens))
		for i, tok := range p.Tokens {
			tt, rp := balV3ReadTokenInfo(state, p.Address, tok)
			tokenTypes[i] = formulas.BalV3TokenType(tt)
			rateProviders[i] = rp
		}

		// Try getAmplificationParameter() → selector 0x6daccffa
		ampSelector := common.FromHex("0x6daccffa")
		ampResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, ampSelector)
		if err == nil && len(ampResult) >= 96 {
			ampVal := new(big.Int).SetBytes(ampResult[0:32])
			if ampVal.Sign() > 0 {
				info := &formulas.BalancerV3PoolInfo{
					PoolType:      formulas.BalV3Stable,
					NumTokens:     len(p.Tokens),
					Tokens:        p.Tokens,
					Amp:           ampVal,
					TokenTypes:    tokenTypes,
					RateProviders: rateProviders,
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
							PoolType:      formulas.BalV3Weighted,
							NumTokens:     len(p.Tokens),
							Tokens:        p.Tokens,
							Weights:       weights,
							TokenTypes:    tokenTypes,
							RateProviders: rateProviders,
						}
						formulas.RegisterBalancerV3Pool(poolAddr, info)
						registry.SetFormulaID(p.Address, formulas.FormulaBalancerV3)
						count++
						continue
					}
				}
			}
		}

		// Pool type 6 but neither Stable nor Weighted (e.g. GyroECLP).
		// Register FormulaBalancerV3 without a pool info entry so that
		// newBalancerV3Pool returns nil, and PoolManager.Get() caches a
		// deadPoolQuoter — preventing EVM fallback entirely.
		registry.SetFormulaID(p.Address, formulas.FormulaBalancerV3)
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

