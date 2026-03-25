// cmd/discover — Pure Go formula registry discovery
//
// Assigns formula IDs to pools by quoting each via EVM.
// If EVM returns non-zero output → pool gets a formula ID.
// If EVM reverts or returns 0 → pool gets -1 (invalid/FoT).
//
// Usage:
//   go run ./cmd/discover/ [--write] [--state-server ws://localhost:7449] [--limit 5000]

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
}

var formulaNames = map[int]string{
	0:  "V2 constant product",
	1:  "Pharaoh V1",
	2:  "V3 tick-walking",
	3:  "LFJ V2 Liquidity Book",
	4:  "Algebra V1 Integral",
	5:  "DODO PMM",
	6:  "V4 PoolManager",
	-1: "invalid (FoT/broken)",
}

var (
	ROUTER       = common.HexToAddress("0x000000000000000000000000cafebabe00facade")
	DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
)

// Starter tokens — only these have known-good balance override slots.
// Discovery quotes each pool using a starter token as input.
var starterTokens = map[common.Address]bool{
	common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"): true, // USDC
	common.HexToAddress("0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"): true, // USDT
	common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"): true, // WAVAX
}

// ─── State server connection (same as cmd/benchmark) ───

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
	stateServerURL := "ws://localhost:7449"
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
			formulas.RegisterV4Pool(strings.ToLower(p.Address.Hex()), poolId, tickSpacing, fee, 0)
			v4Count++
		}
	}
	if v4Count > 0 {
		fmt.Fprintf(os.Stderr, "[discover] registered %d V4 pools\n", v4Count)
	}

	type poolResult struct {
		addr      common.Address
		formulaID int
		ok        bool
	}

	// Direct registry for V2/LFJ_V1 pools: check slot 8 reserves directly from state.
	// No EVM needed — if reserves are non-zero, the V2 formula works.
	slot8 := common.HexToHash("0x8")
	var directResults []poolResult
	for _, p := range pools {
		fid, ok := formulaMap[p.PoolType]
		if !ok || fid != 0 || len(p.Tokens) < 2 { continue }
		if _, known := registry.GetFormulaID(p.Address); known { continue }
		val := state.GetState(p.Address, slot8)
		if val == (common.Hash{}) { continue }
		data := val.Bytes()
		hasReserves := false
		for _, b := range data[4:32] {
			if b != 0 { hasReserves = true; break }
		}
		if !hasReserves { continue }
		registry.SetFormulaID(p.Address, 0)
		directResults = append(directResults, poolResult{p.Address, 0, true})
	}
	// Direct registry for V3/Pharaoh V3 pools: check slot 0 for sqrtPriceX96
	slot0 := common.HexToHash("0x0")
	for _, p := range pools {
		fid, ok := formulaMap[p.PoolType]
		if !ok || fid != 2 || len(p.Tokens) < 2 { continue } // V3 only
		if _, known := registry.GetFormulaID(p.Address); known { continue }
		val := state.GetState(p.Address, slot0)
		if val == (common.Hash{}) { continue }
		// slot0 has sqrtPriceX96 in lower 160 bits — check if non-zero
		data := val.Bytes()
		hasPrice := false
		for _, b := range data[12:32] { // lower 160 bits
			if b != 0 { hasPrice = true; break }
		}
		if !hasPrice { continue }
		registry.SetFormulaID(p.Address, 2)
		directResults = append(directResults, poolResult{p.Address, 2, true})
	}

	// Direct registry for Pharaoh V1: check if pool has reserves via known slots
	for _, p := range pools {
		fid, ok := formulaMap[p.PoolType]
		if !ok || fid != 1 || len(p.Tokens) < 2 { continue }
		if _, known := registry.GetFormulaID(p.Address); known { continue }
		// Pharaoh V1 uses various reserve slots — try common ones
		found := false
		for _, s := range []int{8, 9, 10, 11} {
			val := state.GetState(p.Address, common.BigToHash(big.NewInt(int64(s))))
			if val != (common.Hash{}) {
				data := val.Bytes()
				for _, b := range data[4:32] {
					if b != 0 { found = true; break }
				}
				if found { break }
			}
		}
		if !found { continue }
		registry.SetFormulaID(p.Address, 1)
		directResults = append(directResults, poolResult{p.Address, 1, true})
	}

	// Direct registry for Algebra: check slot 2 (globalState)
	slot2 := common.HexToHash("0x2")
	for _, p := range pools {
		fid, ok := formulaMap[p.PoolType]
		if !ok || fid != 4 || len(p.Tokens) < 2 { continue }
		if _, known := registry.GetFormulaID(p.Address); known { continue }
		val := state.GetState(p.Address, slot2)
		if val == (common.Hash{}) { continue }
		data := val.Bytes()
		hasState := false
		for _, b := range data[12:32] {
			if b != 0 { hasState = true; break }
		}
		if !hasState { continue }
		registry.SetFormulaID(p.Address, 4)
		directResults = append(directResults, poolResult{p.Address, 4, true})
	}

	if len(directResults) > 0 {
		fmt.Fprintf(os.Stderr, "[discover] directly registered %d pools from storage slots\n", len(directResults))
	}

	// Filter to formula-eligible types
	var eligible []pathfinder.Pool
	for _, p := range pools {
		if _, ok := formulaMap[p.PoolType]; ok && len(p.Tokens) >= 2 {
			eligible = append(eligible, p)
		}
	}
	fmt.Fprintf(os.Stderr, "[discover] %d formula-eligible\n", len(eligible))

	// Build overrides and apply
	overrides := router.BuildOverrides(ROUTER, pools)
	base := pathfinder.ApplyOverridesFlat(state, overrides)

	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(base)

	// Warm pass
	fmt.Fprintf(os.Stderr, "[discover] warm pass...\n")
	for _, p := range eligible {
		amountIn := uint256.NewInt(1_000_000_000_000_000_000)
		calldata := pathfinder.EncodeSwapSingle(p.Address, p.PoolType, p.Tokens[0], p.Tokens[1], amountIn)
		cs.Reset()
		evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
	}

	// Discovery pass
	fmt.Fprintf(os.Stderr, "[discover] quoting %d pools via EVM...\n", len(eligible))
	t0 := time.Now()

	type stats struct {
		ok   int
		fail int
	}
	byFormula := make(map[int]*stats)

	var results []poolResult

	for _, p := range eligible {
		formulaID := formulaMap[p.PoolType]

		// Try each direction — use token-specific amount if known, otherwise 1e18
		valid := false
		for _, dir := range [][2]int{{0, 1}, {1, 0}} {
			tokenIn := p.Tokens[dir[0]]
			tokenOut := p.Tokens[dir[1]]

			// Get amount for this token — need override for tokenIn
			amountIn, hasAmount := tokenAmounts[tokenIn]
			if !hasAmount {
				continue // can't test without known amount/override
			}

			calldata := pathfinder.EncodeSwapSingle(p.Address, p.PoolType, tokenIn, tokenOut, amountIn)
			cs.Reset()
			ret, _, evmErr := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
			if evmErr == nil && len(ret) >= 32 {
				var out uint256.Int
				out.SetBytes(ret[:32])
				if !out.IsZero() {
					valid = true
					break
				}
			}
		}

		if valid {
			results = append(results, poolResult{p.Address, formulaID, true})
			if byFormula[formulaID] == nil {
				byFormula[formulaID] = &stats{}
			}
			byFormula[formulaID].ok++
		} else {
			// Check if pool has a known FoT token — if so, the formula can handle it
			// with FoT adjustment instead of marking as -1.
			fotRecoverable := false
			for _, t := range p.Tokens {
				tHex := strings.ToLower(t.Hex())
				// Skip rebasing and formula-issue tokens — they must stay -1
				if formulas.FotRebasingTokens[tHex] || formulas.FotFormulaIssueTokens[tHex] {
					fotRecoverable = false
					break
				}
				if formulas.IsFotToken(tHex) {
					fotRecoverable = true
				}
			}
			// Also check if pool is FoT-exempt (fee doesn't apply, so EVM failure is real)
			poolHexStr := strings.ToLower(p.Address.Hex())
			if formulas.IsFotExemptPool(poolHexStr) {
				fotRecoverable = false
			}

			if fotRecoverable {
				results = append(results, poolResult{p.Address, formulaID, true})
				if byFormula[formulaID] == nil {
					byFormula[formulaID] = &stats{}
				}
				byFormula[formulaID].ok++
			} else {
				results = append(results, poolResult{p.Address, -1, false})
				if byFormula[-1] == nil {
					byFormula[-1] = &stats{}
				}
				byFormula[-1].fail++
			}
		}
	}

	elapsed := time.Since(t0)
	fmt.Fprintf(os.Stderr, "[discover] done in %dms\n\n", elapsed.Milliseconds())

	// Print summary
	fmt.Fprintf(os.Stderr, "Results:\n")
	for _, id := range []int{-1, 0, 1, 2, 3, 4, 5, 6} {
		s := byFormula[id]
		if s == nil {
			continue
		}
		name := formulaNames[id]
		if id == -1 {
			fmt.Fprintf(os.Stderr, "  %s: %d failed\n", name, s.fail)
		} else {
			fmt.Fprintf(os.Stderr, "  %s: %d validated\n", name, s.ok)
		}
	}

	// Build registry content
	var lines []string
	lines = append(lines, "# Formula registry — auto-generated by cmd/discover")
	lines = append(lines, "# format: pool_address:formula_id")
	lines = append(lines, "# 0=V2, 1=PharaohV1, 2=V3, 3=LFJV2, 4=Algebra, 5=DODO, 6=V4, -1=invalid")
	for _, r := range results {
		lines = append(lines, fmt.Sprintf("%s:%d", strings.ToLower(r.addr.Hex()), r.formulaID))
	}

	if doWrite {
		registryPath := "formulas/registry.txt"

		// Merge mode: read existing registry, only add/update entries.
		// Never overwrite a valid entry with -1 (pool may work with different amounts).
		existing := make(map[string]int)
		if data, err := os.ReadFile(registryPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					var id int
					fmt.Sscanf(parts[1], "%d", &id)
					existing[parts[0]] = id
				}
			}
		}

		// Combine EVM-validated results with directly registered V2 pools
		allResults := append(results, directResults...)

		added, updated := 0, 0
		for _, r := range allResults {
			addr := strings.ToLower(r.addr.Hex())
			oldID, exists := existing[addr]
			if !exists {
				// New pool — add it
				existing[addr] = r.formulaID
				added++
			} else if r.ok && oldID == -1 {
				// Was invalid, now valid — update
				existing[addr] = r.formulaID
				updated++
			}
			// Don't overwrite valid entries with -1
		}

		// Write merged registry
		var outLines []string
		outLines = append(outLines, "# Formula registry — auto-generated by cmd/discover")
		outLines = append(outLines, "# format: pool_address:formula_id")
		outLines = append(outLines, "# 0=V2, 1=PharaohV1, 2=V3, 3=LFJV2, 4=Algebra, 5=DODO, -1=invalid")
		for addr, id := range existing {
			outLines = append(outLines, fmt.Sprintf("%s:%d", addr, id))
		}

		err := os.WriteFile(registryPath, []byte(strings.Join(outLines, "\n")+"\n"), 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to write: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\nMerged into %s: %d added, %d updated (total %d entries)\n",
			registryPath, added, updated, len(existing))
	} else {
		fmt.Fprintf(os.Stderr, "\nDry run — pass --write to merge (%d new results)\n", len(results))
	}
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
