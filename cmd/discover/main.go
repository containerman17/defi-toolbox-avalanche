// cmd/discover — Formula registry discovery with multi-amount verification
//
// For each pool not in registry.txt, probes formula vs EVM with up to 10 amounts.
// If all amounts match exactly → assigns the formula ID.
// If any disagree → assigns -1 (broken formula).
// Existing registry entries are never overwritten (append-only).
//
// Usage:
//   go run ./cmd/discover/ [--write] [--state-server ws://localhost:7449/live] [--limit 5000]

package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
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

// Pool type → formula ID mapping
var formulaMap = map[int]int{
	0: 2, // uniswap_v3, pharaoh_v3 → V3
	1: 4, // algebra → Algebra
	2: 0, // lfj_v1 → V2 constant product
	3: 3, // lfj_v2 → LFJ V2
	4: 5, // dodo → DODO
	7: 1, // pharaoh_v1 → Pharaoh V1
	8: 0, // v2 family → V2 constant product
	9: 6, // uniswap_v4 → V4 (singleton PoolManager)
	6: 7, // balancer_v3 → Balancer V3 (Weighted + Stable via Vault)
}

var formulaNames = map[int]string{
	0:  "V2 constant product",
	1:  "Pharaoh V1",
	2:  "V3 tick-walking",
	3:  "LFJ V2 Liquidity Book",
	4:  "Algebra V1 Integral",
	5:  "DODO PMM",
	6:  "V4 PoolManager",
	7:  "Balancer V3",
	-1: "invalid (formula mismatch)",
}

var (
	ROUTER       = router.DeployedRouter
	DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
)

// ─── State server connection ───

type wsFetcher struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage
	block   uint64
}

type stateServerMessage struct {
	Type        string          `json:"type,omitempty"`
	BlockNumber uint64          `json:"blockNumber,omitempty"`
	Timestamp   uint64          `json:"timestamp,omitempty"`
	BaseFee     uint64          `json:"baseFee,omitempty"`
	GasLimit    uint64          `json:"gasLimit,omitempty"`
	Entries     [][2]string     `json:"entries,omitempty"`
	JSONRPC     string          `json:"jsonrpc,omitempty"`
	ID          int             `json:"id,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
}

type valueResult struct {
	Value string `json:"value"`
}

func connectStateServer(url string) (*statedb.StateDB, statedb.EVMConfig, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, statedb.EVMConfig{}, fmt.Errorf("ws connect: %w", err)
	}

	f := &wsFetcher{conn: conn, pending: make(map[int]chan json.RawMessage)}
	state := statedb.NewStateDB(f)

	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, statedb.EVMConfig{}, fmt.Errorf("read initial_dump: %w", err)
	}
	var dump stateServerMessage
	if err := json.Unmarshal(msg, &dump); err != nil {
		return nil, statedb.EVMConfig{}, fmt.Errorf("parse initial_dump: %w", err)
	}
	if dump.Type != "initial_dump" {
		return nil, statedb.EVMConfig{}, fmt.Errorf("expected initial_dump, got %s", dump.Type)
	}

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

	fmt.Fprintf(os.Stderr, "[discover] loaded block %d: %d storage, %d accounts\n",
		dump.BlockNumber, storageCount, len(accountData))

	go f.readLoop(state)

	cfg := statedb.EVMConfig{
		BlockNumber: dump.BlockNumber,
		Timestamp:   dump.Timestamp,
		ChainID:     43114,
		BaseFee:     dump.BaseFee,
		GasLimit:    dump.GasLimit,
	}

	return state, cfg, nil
}

func (f *wsFetcher) readLoop(state *statedb.StateDB) {
	for {
		_, msg, err := f.conn.ReadMessage()
		if err != nil { return }
		var m stateServerMessage
		if json.Unmarshal(msg, &m) != nil { continue }
		if m.Type == "block_diff" { continue }
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

func main() {
	stateServerURL := "ws://localhost:7449/live"
	poolLimit := 5000
	doWrite := false

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
		if arg == "--write" {
			doWrite = true
		}
	}

	state, cfg, err := connectStateServer(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}

	pools := poolcollector.EmbeddedPools(poolLimit)
	registry := formulas.LoadEmbeddedRegistry()
	fmt.Fprintf(os.Stderr, "[discover] %d pools loaded\n", len(pools))

	// Load token amounts (amount equal to ~1 AVAX per token)
	tokenAmounts := loadTokenAmounts()
	fmt.Fprintf(os.Stderr, "[discover] %d token amounts loaded\n", len(tokenAmounts))

	// Register V4 pools from ExtraData
	v4Count := 0
	for _, p := range pools {
		if p.PoolType != 9 || p.ExtraData == "" { continue }
		var poolIdHex string
		var fee uint32
		var tickSpacing int32
		for _, kv := range strings.Split(p.ExtraData, ",") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 { continue }
			switch parts[0] {
			case "id": poolIdHex = parts[1]
			case "fee": var f int; fmt.Sscanf(parts[1], "%d", &f); fee = uint32(f)
			case "ts": var t int; fmt.Sscanf(parts[1], "%d", &t); tickSpacing = int32(t)
			}
		}
		if poolIdHex != "" && tickSpacing != 0 {
			var poolId [32]byte
			copy(poolId[:], common.FromHex(poolIdHex))
			formulas.RegisterV4Pool(strings.ToLower(p.Address.Hex()), poolId, tickSpacing, fee, 0, common.Address{})
			v4Count++
		}
	}
	if v4Count > 0 {
		fmt.Fprintf(os.Stderr, "[discover] registered %d V4 pools\n", v4Count)
	}

	// Register Balancer V3 pools by reading parameters from EVM/storage
	balV3Count := registerBalancerV3PoolsFromState(pools, state, cfg, registry)
	if balV3Count > 0 {
		fmt.Fprintf(os.Stderr, "[discover] registered %d Balancer V3 pools\n", balV3Count)
	}

	// Build overrides and apply
	overrides := router.BuildTokenOverrides(ROUTER, pools)
	base := pathfinder.ApplyOverridesFlat(state, overrides)

	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(base)

	// Build StorageReader for formula quoting (uses same state as EVM)
	storageReader := func(addr common.Address, key common.Hash) common.Hash {
		return base.GetState(addr, key)
	}

	// Build PoolManager for formula quoting
	pm := formulas.NewPoolManager(registry, storageReader)
	pm.SetBlockTimestamp(cfg.Timestamp)
	pm.SetEVMCaller(func(to common.Address, data []byte) ([]byte, bool) {
		cs.Reset()
		ret, _, err := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, to, data)
		if err != nil {
			return nil, false
		}
		return ret, true
	})

	// Register pool tokens and types in PoolManager
	for _, p := range pools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
			pm.SetPoolType(p.Address, p.PoolType, p.Dex)
		}
	}

	// Filter to formula-eligible pools not already in registry
	type candidate struct {
		pool      pathfinder.Pool
		formulaID int
	}
	var candidates []candidate
	skipped := 0
	for _, p := range pools {
		fid, ok := formulaMap[p.PoolType]
		if !ok || len(p.Tokens) < 2 {
			continue
		}
		if _, known := registry.GetFormulaID(p.Address); known {
			skipped++
			continue
		}
		candidates = append(candidates, candidate{pool: p, formulaID: fid})
	}
	fmt.Fprintf(os.Stderr, "[discover] %d candidates (%d skipped, already in registry)\n", len(candidates), skipped)

	// Warm pass — run one EVM call per pool to populate state cache
	fmt.Fprintf(os.Stderr, "[discover] warm pass...\n")
	for _, c := range candidates {
		p := c.pool
		amountIn := uint256.NewInt(1_000_000_000_000_000_000)
		calldata := pathfinder.EncodeSwapSingleWithExtra(p.Address, p.PoolType, p.Tokens[0], p.Tokens[1], amountIn, p.ExtraData)
		cs.Reset()
		evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
	}

	// Discovery pass with multi-amount verification
	fmt.Fprintf(os.Stderr, "[discover] verifying %d pools (formula vs EVM, up to 10 amounts)...\n", len(candidates))
	t0 := time.Now()

	type fillResult struct {
		addr      common.Address
		formulaID int
	}
	var results []fillResult

	filled := 0
	matched := 0
	mismatched := 0

	for _, c := range candidates {
		p := c.pool

		// Try both directions: token0→token1 and token1→token0
		bestFormulaID := -1

		for _, dir := range [][2]int{{0, 1}, {1, 0}} {
			tokenIn := p.Tokens[dir[0]]
			tokenOut := p.Tokens[dir[1]]
			zeroForOne := dir[0] == 0

			baseAmount, hasAmount := tokenAmounts[tokenIn]
			if !hasAmount {
				continue
			}

			// Step 1: EVM probe with base amount
			evmOut := evmQuote(evmCtx, cs, p.Address, p.PoolType, tokenIn, tokenOut, baseAmount, p.ExtraData)

			// Step 2: Formula probe with base amount
			pq := pm.BuildQuoterForFormulaID(p.Address, c.formulaID)
			formulaOut := formulaQuote(pq, baseAmount, zeroForOne)

			// Step 3: Check if they match (including both being zero)
			if !amountsEqual(evmOut, formulaOut) {
				continue
			}

			// Step 4: Multi-amount verification (10 amounts)
			allMatch := true
			for mult := uint64(1); mult <= 10; mult++ {
				testAmount := new(uint256.Int).Mul(baseAmount, uint256.NewInt(mult))

				evmResult := evmQuote(evmCtx, cs, p.Address, p.PoolType, tokenIn, tokenOut, testAmount, p.ExtraData)

				// Rebuild formula quoter for each test (clean state)
				pq = pm.BuildQuoterForFormulaID(p.Address, c.formulaID)
				fResult := formulaQuote(pq, testAmount, zeroForOne)

				if !amountsEqual(evmResult, fResult) {
					allMatch = false
					break
				}
			}

			if allMatch {
				bestFormulaID = c.formulaID
				break // Found a matching direction, done
			}
		}

		results = append(results, fillResult{addr: p.Address, formulaID: bestFormulaID})
		filled++
		if bestFormulaID >= 0 {
			matched++
		} else {
			mismatched++
		}
	}

	elapsed := time.Since(t0)
	fmt.Fprintf(os.Stderr, "[discover] done in %dms\n\n", elapsed.Milliseconds())

	// Print stats
	fmt.Fprintf(os.Stderr, "Stats:\n")
	fmt.Fprintf(os.Stderr, "  Pools filled:   %d\n", filled)
	fmt.Fprintf(os.Stderr, "  Formula match:  %d\n", matched)
	fmt.Fprintf(os.Stderr, "  Formula fail:   %d (assigned -1)\n", mismatched)
	fmt.Fprintf(os.Stderr, "  Skipped:        %d (already in registry)\n", skipped)

	// Per-formula breakdown
	byFormula := make(map[int]int)
	for _, r := range results {
		byFormula[r.formulaID]++
	}
	fmt.Fprintf(os.Stderr, "\nBy formula:\n")
	for _, id := range []int{0, 1, 2, 3, 4, 5, 6, 7, -1} {
		cnt := byFormula[id]
		if cnt == 0 { continue }
		name := formulaNames[id]
		fmt.Fprintf(os.Stderr, "  %s: %d\n", name, cnt)
	}

	if doWrite {
		registryPath := "formulas/registry.txt"

		// Read existing registry
		existing := make(map[string]int)
		if data, err := os.ReadFile(registryPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") { continue }
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					var id int
					fmt.Sscanf(parts[1], "%d", &id)
					existing[parts[0]] = id
				}
			}
		}

		added := 0
		for _, r := range results {
			addr := strings.ToLower(r.addr.Hex())
			if _, exists := existing[addr]; exists {
				continue // NEVER overwrite existing entries
			}
			existing[addr] = r.formulaID
			added++
		}

		// Write merged registry
		var outLines []string
		outLines = append(outLines, "# Formula registry — auto-generated by cmd/discover")
		outLines = append(outLines, "# format: pool_address:formula_id")
		outLines = append(outLines, "# 0=V2, 1=PharaohV1, 2=V3, 3=LFJV2, 4=Algebra, 5=DODO, 6=V4, 7=BalancerV3, -1=invalid")
		for addr, id := range existing {
			outLines = append(outLines, fmt.Sprintf("%s:%d", addr, id))
		}

		err := os.WriteFile(registryPath, []byte(strings.Join(outLines, "\n")+"\n"), 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to write: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\nAppended to %s: %d new entries (total %d)\n",
			registryPath, added, len(existing))
	} else {
		fmt.Fprintf(os.Stderr, "\nDry run — pass --write to append (%d new results)\n", len(results))
	}
}

// evmQuote runs a single-pool swap via EVM. Returns nil on revert (treated as zero).
func evmQuote(evmCtx *statedb.CachedContext, cs *statedb.CallState, pool common.Address, poolType int, tokenIn, tokenOut common.Address, amount *uint256.Int, extraData string) *uint256.Int {
	calldata := pathfinder.EncodeSwapSingleWithExtra(pool, poolType, tokenIn, tokenOut, amount, extraData)
	cs.Reset()
	ret, _, evmErr := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
	if evmErr != nil || len(ret) < 32 {
		return uint256.NewInt(0) // revert → zero
	}
	var out uint256.Int
	out.SetBytes(ret[:32])
	return &out
}

// formulaQuote runs a quote via PoolManager. Returns nil on failure (treated as zero).
func formulaQuote(pq formulas.PoolQuoter, amount *uint256.Int, zeroForOne bool) *uint256.Int {
	if pq == nil {
		return uint256.NewInt(0)
	}
	out := pq.Quote(amount, zeroForOne)
	if out.IsZero() {
		return uint256.NewInt(0)
	}
	return &out
}

// amountsEqual compares two amounts. Both nil/zero counts as equal.
func amountsEqual(a, b *uint256.Int) bool {
	if a == nil { a = uint256.NewInt(0) }
	if b == nil { b = uint256.NewInt(0) }
	return a.Eq(b)
}

// loadTokenAmounts reads formulas/data/token_amounts.txt and returns
// a map of token address → amount (as *uint256.Int) equal to ~1 AVAX.
func loadTokenAmounts() map[common.Address]*uint256.Int {
	data, err := os.ReadFile("formulas/data/token_amounts.txt")
	if err != nil {
		return map[common.Address]*uint256.Int{}
	}
	result := make(map[common.Address]*uint256.Int)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		addr := common.HexToAddress(parts[0])
		amt := new(uint256.Int)
		amt.SetFromHex(parts[1])
		if !amt.IsZero() {
			result[addr] = amt
		}
	}
	return result
}

// registerBalancerV3PoolsFromState registers Balancer V3 pools by reading parameters
// from pool contract storage/EVM. Returns the count of registered pools.
func registerBalancerV3PoolsFromState(pools []pathfinder.Pool, state *statedb.StateDB, cfg statedb.EVMConfig, registry *formulas.Registry) int {
	count := 0
	for _, p := range pools {
		if p.PoolType != 6 || len(p.Tokens) < 2 {
			continue
		}
		// Only handle 2-token pools for now (>2 tokens need more complex index mapping)
		if len(p.Tokens) != 2 {
			continue
		}

		poolAddr := strings.ToLower(p.Address.Hex())

		// Try getAmplificationParameter() → selector 0x6daccffa
		// Returns (uint256 value, bool isUpdating, uint256 precision)
		ampSelector := common.FromHex("0x6daccffa")
		ampResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, ampSelector)
		if err == nil && len(ampResult) >= 96 {
			// StablePool: parse amp value
			ampVal := new(big.Int).SetBytes(ampResult[0:32])
			if ampVal.Sign() > 0 {
				info := &formulas.BalancerV3PoolInfo{
					PoolType:  formulas.BalV3Stable,
					NumTokens: len(p.Tokens),
					Tokens:    p.Tokens,
					Amp:       ampVal,
				}
				formulas.RegisterBalancerV3Pool(poolAddr, info)
				count++
				continue
			}
		}

		// Try getNormalizedWeights() → selector 0xf89f27ed
		// Returns uint256[] (dynamic array of weights)
		weightsSelector := common.FromHex("0xf89f27ed")
		weightsResult, _, err := statedb.ExecuteCall(state, cfg, DUMMY_SENDER, p.Address, weightsSelector)
		if err == nil && len(weightsResult) >= 96 {
			// WeightedPool: parse weights array
			// ABI: offset (32 bytes) + length (32 bytes) + length * 32 bytes
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
	return count
}
