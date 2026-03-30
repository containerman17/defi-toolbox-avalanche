package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
var USDC = common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E")

const RPC = "http://localhost:9650/ext/bc/C/rpc"

var chainID = big.NewInt(43114)

// ── Pool edge for adjacency ──

type poolEdge struct {
	poolIdx  uint16
	tokenOut common.Address
	dir      bool // zeroForOne
}

// ── Hub config ──

type hubConfig struct {
	token      common.Address
	label      string
	sizes      []*uint256.Int // starting amounts for formula BFS
	maxBalance *uint256.Int
}

// ── BFS data structures ──

// bfsEntry is one frontier node: an amount of some token reached via a specific pool.
type bfsEntry struct {
	token    common.Address
	amount   uint256.Int
	parentID int32  // index into flat entries array (-1 for root)
	pool     uint16 // pool index that produced this
	dir      bool
}

// bfsPath is a reconstructed path from backtracking.
type bfsPath struct {
	pools      []uint16
	dirs       []bool
	amountIn   *uint256.Int
	formulaOut uint256.Int
}

// ── Formula BFS ──

const maxLayers = 4
const topPerToken = 3

// formulaBFS runs a Bellman-Ford-style BFS from hub token through all pools.
// Returns candidates: paths that return to hub with output > 0.
func formulaBFS(
	pm *formulas.PoolManager,
	adj map[common.Address][]poolEdge,
	pools []pf.Pool,
	hub common.Address,
	startAmount *uint256.Int,
) []bfsPath {
	// Flat array of all entries for backtracking
	allEntries := []bfsEntry{{token: hub, amount: *startAmount, parentID: -1}}

	// Frontier: token → indices into allEntries (max topPerToken per token)
	type frontier = map[common.Address][]int32

	current := frontier{hub: {0}} // index 0 = the root entry
	var candidates []bfsPath

	for layer := 1; layer <= maxLayers; layer++ {
		next := make(frontier)

		for token, entryIDs := range current {
			edges := adj[token]
			for _, eid := range entryIDs {
				entry := &allEntries[eid]
				if entry.amount.IsZero() {
					continue
				}
				for _, edge := range edges {
					out := pm.Quote(pools[edge.poolIdx].Address, &entry.amount, edge.dir)
					if out.IsZero() {
						continue
					}

					// Sanity: if output > 1000x input, formula is likely wrong
					var limit uint256.Int
					limit.Mul(&entry.amount, uint256.NewInt(1000))
					if !limit.IsZero() && out.Gt(&limit) {
						continue
					}

					// Terminal: reached hub token, layer >= 2
					if edge.tokenOut == hub && layer >= 2 {
						path := backtrack(allEntries, eid, edge, startAmount)
						path.formulaOut = out
						candidates = append(candidates, path)
						continue
					}

					// Non-terminal: add to next layer frontier
					newID := int32(len(allEntries))
					allEntries = append(allEntries, bfsEntry{
						token:    edge.tokenOut,
						amount:   out,
						parentID: eid,
						pool:     edge.poolIdx,
						dir:      edge.dir,
					})

					// Top-3 per token
					existing := next[edge.tokenOut]
					if len(existing) < topPerToken {
						next[edge.tokenOut] = append(existing, newID)
					} else {
						// Find the worst
						worstSlot := 0
						for i := 1; i < len(existing); i++ {
							if allEntries[existing[i]].amount.Lt(&allEntries[existing[worstSlot]].amount) {
								worstSlot = i
							}
						}
						if out.Gt(&allEntries[existing[worstSlot]].amount) {
							existing[worstSlot] = newID
						}
					}
				}
			}
		}

		current = next
	}

	return candidates
}

// backtrack reconstructs the path from a terminal entry back to the root.
func backtrack(allEntries []bfsEntry, lastEntryID int32, finalEdge poolEdge, startAmount *uint256.Int) bfsPath {
	// Collect hops in reverse: final edge first, then walk parent chain
	var poolsList []uint16
	var dirsList []bool

	// The final hop (into hub)
	poolsList = append(poolsList, finalEdge.poolIdx)
	dirsList = append(dirsList, finalEdge.dir)

	// Walk backwards
	eid := lastEntryID
	for eid >= 0 {
		e := &allEntries[eid]
		if e.parentID < 0 {
			break // root — don't add, it's the starting point
		}
		poolsList = append(poolsList, e.pool)
		dirsList = append(dirsList, e.dir)
		eid = e.parentID
	}

	// Reverse to get forward order
	for i, j := 0, len(poolsList)-1; i < j; i, j = i+1, j-1 {
		poolsList[i], poolsList[j] = poolsList[j], poolsList[i]
		dirsList[i], dirsList[j] = dirsList[j], dirsList[i]
	}

	return bfsPath{
		pools:    poolsList,
		dirs:     dirsList,
		amountIn: new(uint256.Int).Set(startAmount),
	}
}

// ── EVM verification ──

type evmResult struct {
	path      bfsPath
	amountIn  *uint256.Int
	amountOut *uint256.Int
	gasUsed   uint64
	netProfit float64
}

// evmVerifyPath runs a full swap() call for a given path.
func evmVerifyPath(
	path *bfsPath,
	pools []pf.Pool,
	amountIn *uint256.Int,
	state *statedb.StateDB,
	evmCtx *statedb.CachedContext,
	caller common.Address,
	routerAddr common.Address,
) *evmResult {
	n := len(path.pools)
	poolAddrs := make([]common.Address, n)
	poolTypes := make([]int, n)
	extraDatas := make([]string, n)
	tokenPairs := make([]common.Address, n*2)

	for h := 0; h < n; h++ {
		p := &pools[path.pools[h]]
		poolAddrs[h] = p.Address
		poolTypes[h] = p.PoolType
		extraDatas[h] = p.ExtraData
		if path.dirs[h] {
			tokenPairs[h*2] = p.Tokens[0]
			tokenPairs[h*2+1] = p.Tokens[1]
		} else {
			tokenPairs[h*2] = p.Tokens[1]
			tokenPairs[h*2+1] = p.Tokens[0]
		}
	}

	calldata := pf.EncodeSwapMulti(poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, uint256.NewInt(0))
	cs := statedb.NewCallState(state)
	ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, caller, routerAddr, calldata)

	if err != nil || cs.Err() != nil || len(ret) < 32 {
		return nil
	}

	amountOut := new(uint256.Int).SetBytes(ret[:32])
	return &evmResult{
		path:      *path,
		amountIn:  new(uint256.Int).Set(amountIn),
		amountOut: amountOut,
		gasUsed:   gasUsed,
	}
}

// binarySearchSize finds the optimal input amount for a given path.
// Starts at baseAmount, searches between baseAmount/2 and baseAmount*2.
func binarySearchSize(
	path *bfsPath,
	pools []pf.Pool,
	baseAmount *uint256.Int,
	state *statedb.StateDB,
	evmCtx *statedb.CachedContext,
	caller common.Address,
	routerAddr common.Address,
	baseFee uint64,
) *evmResult {
	lo := new(uint256.Int).Rsh(baseAmount, 1) // baseAmount / 2
	hi := new(uint256.Int).Lsh(baseAmount, 1) // baseAmount * 2

	// Evaluate at lo, mid, hi
	evalAt := func(amt *uint256.Int) (netProfit float64, result *evmResult) {
		r := evmVerifyPath(path, pools, amt, state, evmCtx, caller, routerAddr)
		if r == nil {
			return -1e18, nil
		}
		// swap() returns gross profit directly for cyclic arbs
		gross := r.amountOut
		if gross.IsZero() {
			return -1e18, r
		}
		gasCost := new(uint256.Int).Mul(uint256.NewInt(r.gasUsed), uint256.NewInt(baseFee))
		if gross.Lt(gasCost) {
			r.netProfit = -(new(uint256.Int).Sub(gasCost, gross)).Float64()
			return r.netProfit, r
		}
		net := new(uint256.Int).Sub(gross, gasCost)
		r.netProfit = net.Float64()
		return r.netProfit, r
	}

	var best *evmResult
	bestProfit := -1e18

	// 5 rounds of binary search
	for round := 0; round < 5; round++ {
		mid := new(uint256.Int).Add(lo, hi)
		mid.Rsh(mid, 1) // (lo + hi) / 2

		profitLo, rLo := evalAt(lo)
		profitHi, rHi := evalAt(hi)
		profitMid, rMid := evalAt(mid)

		// Track best
		for _, pair := range []struct {
			p float64
			r *evmResult
		}{{profitLo, rLo}, {profitMid, rMid}, {profitHi, rHi}} {
			if pair.p > bestProfit && pair.r != nil {
				bestProfit = pair.p
				best = pair.r
			}
		}

		// Narrow: keep the better half
		if profitLo > profitHi {
			hi.Set(mid)
		} else {
			lo.Set(mid)
		}
	}

	return best
}

// ── Collect unique pools from top candidates ──

func collectPoolSet(candidates []bfsPath, topN int) map[uint16]bool {
	poolSet := make(map[uint16]bool)
	limit := topN
	if limit > len(candidates) {
		limit = len(candidates)
	}
	for i := 0; i < limit; i++ {
		for _, p := range candidates[i].pools {
			poolSet[p] = true
		}
	}
	return poolSet
}

// ── RPC helpers (from arb2) ──

func rpcCallJSON(body interface{}) ([]byte, error) {
	b, _ := json.Marshal(body)
	resp, err := http.Post(RPC, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func rpcAllowance(token, owner, spender common.Address) *uint256.Int {
	data := "0xdd62ed3e" +
		"000000000000000000000000" + hex.EncodeToString(owner[:]) +
		"000000000000000000000000" + hex.EncodeToString(spender[:])
	resp, err := rpcCallJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []interface{}{map[string]string{"to": token.Hex(), "data": data}, "latest"},
	})
	if err != nil {
		return uint256.NewInt(0)
	}
	var rpcResp struct{ Result string }
	json.Unmarshal(resp, &rpcResp)
	b, _ := hex.DecodeString(strings.TrimPrefix(rpcResp.Result, "0x"))
	if len(b) < 32 {
		return uint256.NewInt(0)
	}
	return new(uint256.Int).SetBytes(b[:32])
}

func rpcNonce(addr common.Address) uint64 {
	resp, err := rpcCallJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "eth_getTransactionCount",
		"params": []interface{}{addr.Hex(), "latest"},
	})
	if err != nil {
		return 0
	}
	var rpcResp struct{ Result string }
	json.Unmarshal(resp, &rpcResp)
	n := new(big.Int)
	n.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return n.Uint64()
}

func rpcBaseFee() uint64 {
	resp, err := rpcCallJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "eth_getBlockByNumber",
		"params": []interface{}{"latest", false},
	})
	if err != nil {
		return 25_000_000_000
	}
	var rpcResp struct {
		Result struct {
			BaseFeePerGas string `json:"baseFeePerGas"`
		}
	}
	json.Unmarshal(resp, &rpcResp)
	bf := new(big.Int)
	bf.SetString(strings.TrimPrefix(rpcResp.Result.BaseFeePerGas, "0x"), 16)
	return bf.Uint64()
}

func ensureApprovals(key *ecdsa.PrivateKey, caller common.Address, hubs []hubConfig) {
	routerAddr := router.DeployedRouter
	signer := types.NewLondonSigner(chainID)
	nonce := rpcNonce(caller)
	baseFee := rpcBaseFee()

	for _, hub := range hubs {
		allowance := rpcAllowance(hub.token, caller, routerAddr)
		needed := new(uint256.Int).Mul(hub.maxBalance, uint256.NewInt(1000))
		if allowance.Gt(needed) || allowance.Eq(needed) {
			fmt.Fprintf(os.Stderr, "[arb3] %s allowance OK\n", hub.label)
			continue
		}
		fmt.Fprintf(os.Stderr, "[arb3] %s approving...\n", hub.label)

		data := make([]byte, 68)
		data[0], data[1], data[2], data[3] = 0x09, 0x5e, 0xa7, 0xb3
		copy(data[4+12:4+32], routerAddr[:])
		neededBytes := needed.Bytes32()
		copy(data[36:68], neededBytes[:])

		maxFee := new(big.Int).SetUint64(baseFee*2 + 1_000_000_000)
		tokenAddr := hub.token
		tx := types.NewTx(&types.DynamicFeeTx{
			ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(0),
			GasFeeCap: maxFee, Gas: 60_000, To: &tokenAddr, Data: data,
		})
		signedTx, err := types.SignTx(tx, signer, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb3] sign approve %s: %v\n", hub.label, err)
			continue
		}
		rawTx, _ := signedTx.MarshalBinary()
		rawHex := "0x" + hex.EncodeToString(rawTx)
		resp, err := rpcCallJSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1, "method": "eth_sendRawTransaction",
			"params": []string{rawHex},
		})
		if err != nil {
			continue
		}
		var rpcResp struct {
			Error *struct{ Message string }
		}
		json.Unmarshal(resp, &rpcResp)
		if rpcResp.Error != nil {
			fmt.Fprintf(os.Stderr, "[arb3] approve %s failed: %s\n", hub.label, rpcResp.Error.Message)
			continue
		}
		fmt.Fprintf(os.Stderr, "[arb3] %s approved, nonce=%d\n", hub.label, nonce)
		nonce++
	}
}

func readBalance(state *statedb.StateDB, evmCtx *statedb.CachedContext, owner, token common.Address) *uint256.Int {
	var calldata [36]byte
	calldata[0], calldata[1], calldata[2], calldata[3] = 0x70, 0xa0, 0x82, 0x31
	copy(calldata[16:36], owner[:])
	cs := statedb.NewCallState(state)
	ret, _, err := evmCtx.ExecuteWithCallState(cs, owner, token, calldata[:])
	if err == nil && len(ret) >= 32 {
		return new(uint256.Int).SetBytes(ret[:32])
	}
	return uint256.NewInt(0)
}

func loadEnv() {
	for _, path := range []string{".env", "../.env", "../../.env"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq > 0 {
				os.Setenv(line[:eq], line[eq+1:])
			}
		}
		f.Close()
		return
	}
}

// ── Main ──

func main() {
	stateServerURL := "ws://localhost:7449/live"
	poolLimit := 4000
	singleBlock := false
	var filterPools []common.Address // --pools 0xabc,0xdef to restrict BFS to specific pools
	debugHops := false               // --debug-hops to print per-hop formula vs EVM comparison

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--pool-limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
		if arg == "--block" && i+1 < len(os.Args) {
			stateServerURL = fmt.Sprintf("ws://localhost:7449/debug/%s", os.Args[i+1])
			singleBlock = true
		}
		if arg == "--pools" && i+1 < len(os.Args) {
			for _, s := range strings.Split(os.Args[i+1], ",") {
				filterPools = append(filterPools, common.HexToAddress(strings.TrimSpace(s)))
			}
		}
		if arg == "--debug-hops" {
			debugHops = true
		}
	}

	// Load private key
	loadEnv()
	privKeyHex := strings.TrimPrefix(os.Getenv("ARB_PRIVATE_KEY"), "0x")
	var caller common.Address
	var privKey *ecdsa.PrivateKey
	if privKeyHex != "" {
		key, err := crypto.HexToECDSA(privKeyHex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb3] bad private key: %v\n", err)
			os.Exit(1)
		}
		privKey = key
		caller = crypto.PubkeyToAddress(key.PublicKey)
		fmt.Fprintf(os.Stderr, "[arb3] caller: %s\n", caller.Hex())
	}

	// Load registry + pools
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)
	fmt.Fprintf(os.Stderr, "[arb3] loaded %d pools\n", len(pools))

	// Connect to state server
	ls, err := statedb.Connect(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb3] connect failed: %v\n", err)
		os.Exit(1)
	}
	state := ls.State()

	// Build PoolManager
	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)
	for _, p := range pools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	pm.SetBlockTimestamp(ls.Timestamp())
	pm.SetEVMCaller(func(to common.Address, data []byte) ([]byte, bool) {
		cs := statedb.NewCallState(state)
		cfg := statedb.EVMConfig{BlockNumber: ls.Block(), Timestamp: ls.Timestamp(), ChainID: 43114, BaseFee: ls.BaseFee(), GasLimit: ls.GasLimit()}
		ctx := statedb.GetCachedContext(cfg)
		ret, _, err := ctx.ExecuteWithCallState(cs, common.Address{}, to, data)
		return ret, err == nil
	})

	// Build pool filter set (if --pools specified)
	var poolFilter map[common.Address]bool
	if len(filterPools) > 0 {
		poolFilter = make(map[common.Address]bool, len(filterPools))
		for _, addr := range filterPools {
			poolFilter[addr] = true
		}
		fmt.Fprintf(os.Stderr, "[arb3] pool filter: %d pools\n", len(poolFilter))
	}

	// Build adjacency: token → []poolEdge (with uint16 pool indices)
	adj := make(map[common.Address][]poolEdge)
	poolCount := 0
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		_, known := registry.GetFormulaID(p.Address)
		if !known {
			continue
		}
		if poolFilter != nil && !poolFilter[p.Address] {
			continue
		}
		idx := uint16(i)
		adj[p.Tokens[0]] = append(adj[p.Tokens[0]], poolEdge{idx, p.Tokens[1], true})
		adj[p.Tokens[1]] = append(adj[p.Tokens[1]], poolEdge{idx, p.Tokens[0], false})
		poolCount++
	}
	fmt.Fprintf(os.Stderr, "[arb3] adjacency: %d tokens, %d pools\n", len(adj), poolCount)

	// Warmup: build all pool quoters
	tw := time.Now()
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[arb3] warmup: %v\n", time.Since(tw).Round(time.Millisecond))

	// Hub config
	hubTokens := []struct {
		addr  common.Address
		label string
		sizes []*uint256.Int
	}{
		{WAVAX, "WAVAX", []*uint256.Int{
			uint256.NewInt(1_000_000_000_000_000_000),  // 1 AVAX
			uint256.NewInt(100_000_000_000_000_000),    // 0.1
			uint256.NewInt(10_000_000_000_000_000),     // 0.01
			uint256.NewInt(1_000_000_000_000_000),      // 0.001
			uint256.NewInt(100_000_000_000_000),        // 0.0001
		}},
		{USDC, "USDC", []*uint256.Int{
			uint256.NewInt(10_000_000), // $10
			uint256.NewInt(1_000_000),  // $1
			uint256.NewInt(100_000),    // $0.10
			uint256.NewInt(10_000),     // $0.01
			uint256.NewInt(1_000),      // $0.001
		}},
	}

	// Read balances + build hubs
	var hubs []hubConfig
	ls.RLock()
	initCfg := statedb.EVMConfig{
		BlockNumber: ls.Block(), Timestamp: ls.Timestamp(),
		ChainID: 43114, BaseFee: ls.BaseFee(), GasLimit: ls.GasLimit(),
	}
	evmCtx := statedb.GetCachedContext(initCfg)
	for _, ht := range hubTokens {
		bal := readBalance(state, evmCtx, caller, ht.addr)
		fmt.Fprintf(os.Stderr, "[arb3] hub %s: balance=%s\n", ht.label, bal.Dec())
		hubs = append(hubs, hubConfig{
			token: ht.addr, label: ht.label,
			sizes: ht.sizes, maxBalance: bal,
		})
	}
	ls.RUnlock()

	// Ensure approvals
	if privKey != nil {
		ensureApprovals(privKey, caller, hubs)
	}

	routerAddr := router.DeployedRouter

	// debugHopByHop prints per-hop formula vs EVM comparison for a path.
	debugPath := func(path *bfsPath, pools []pf.Pool, amountIn *uint256.Int,
		pm *formulas.PoolManager, state *statedb.StateDB, evmCtx *statedb.CachedContext,
		routerAddr common.Address) {

		// Formula hop-by-hop
		currentFormula := new(uint256.Int).Set(amountIn)
		fmt.Fprintf(os.Stderr, "        formula hop-by-hop:\n")
		for h := 0; h < len(path.pools); h++ {
			p := &pools[path.pools[h]]
			out := pm.Quote(p.Address, currentFormula, path.dirs[h])
			fmt.Fprintf(os.Stderr, "          hop%d: %s (%s) dir=%v in=%s out=%s\n",
				h+1, p.Address.Hex()[:12], p.Dex, path.dirs[h], currentFormula.Dec(), out.Dec())
			currentFormula.Set(&out)
		}

		// EVM hop-by-hop (single pool each, with token overrides)
		overrides := router.BuildTokenOverrides(routerAddr, pools)
		baseWithOverrides := pf.ApplyOverridesFlat(state, overrides)
		currentEVM := new(uint256.Int).Set(amountIn)
		fmt.Fprintf(os.Stderr, "        evm hop-by-hop (executeSwap):\n")
		for h := 0; h < len(path.pools); h++ {
			p := &pools[path.pools[h]]
			var tokenIn, tokenOut common.Address
			if path.dirs[h] {
				tokenIn, tokenOut = p.Tokens[0], p.Tokens[1]
			} else {
				tokenIn, tokenOut = p.Tokens[1], p.Tokens[0]
			}
			calldata := pf.EncodeExecuteSwapMulti(
				[]common.Address{p.Address},
				[]int{p.PoolType},
				[]common.Address{tokenIn, tokenOut},
				currentEVM,
				[]string{p.ExtraData},
			)
			cs := statedb.NewCallState(baseWithOverrides)
			ret, gas, err := evmCtx.ExecuteWithCallState(cs, common.Address{}, routerAddr, calldata)
			if err != nil || cs.Err() != nil || len(ret) < 32 {
				fmt.Fprintf(os.Stderr, "          hop%d: %s REVERT\n", h+1, p.Address.Hex()[:12])
				break
			}
			evmOut := new(uint256.Int).SetBytes(ret[:32])
			match := "MATCH"
			fOut := pm.Quote(p.Address, currentEVM, path.dirs[h])
			if !evmOut.Eq(&fOut) {
				if evmOut.IsZero() {
					match = "MISMATCH formula=" + fOut.Dec() + " evm=0"
				} else {
					var diff uint256.Int
					if fOut.Gt(evmOut) {
						diff.Sub(&fOut, evmOut)
					} else {
						diff.Sub(evmOut, &fOut)
					}
					pct := diff.Float64() / evmOut.Float64() * 100
					match = fmt.Sprintf("MISMATCH (%.2f%%) formula=%s", pct, fOut.Dec())
				}
			}
			fmt.Fprintf(os.Stderr, "          hop%d: %s (%s) in=%s evm_out=%s gas=%d %s\n",
				h+1, p.Address.Hex()[:12], p.Dex, currentEVM.Dec(), evmOut.Dec(), gas, match)
			currentEVM.Set(evmOut)
		}
		fmt.Fprintf(os.Stderr, "        formula total: %s → %s | evm total: %s → %s\n",
			amountIn.Dec(), currentFormula.Dec(), amountIn.Dec(), currentEVM.Dec())
	}

	// ── Run one block ──
	runBlock := func(block, timestamp, baseFee, gasLimit uint64) {
		cfg := statedb.EVMConfig{
			BlockNumber: block, Timestamp: timestamp,
			ChainID: 43114, BaseFee: baseFee, GasLimit: gasLimit,
		}
		evmCtx := statedb.GetCachedContext(cfg)

		for _, hub := range hubs {
			t0 := time.Now()

			// Pass 1: Formula BFS at all sizes
			var allCandidates []bfsPath
			for _, size := range hub.sizes {
				if hub.maxBalance != nil && size.Gt(hub.maxBalance) {
					continue
				}
				candidates := formulaBFS(pm, adj, pools, hub.token, size)
				allCandidates = append(allCandidates, candidates...)
			}

			// Sort by absolute gross profit descending (output - input)
			sort.Slice(allCandidates, func(i, j int) bool {
				pi := new(uint256.Int)
				pj := new(uint256.Int)
				if allCandidates[i].formulaOut.Gt(allCandidates[i].amountIn) {
					pi.Sub(&allCandidates[i].formulaOut, allCandidates[i].amountIn)
				}
				if allCandidates[j].formulaOut.Gt(allCandidates[j].amountIn) {
					pj.Sub(&allCandidates[j].formulaOut, allCandidates[j].amountIn)
				}
				return pi.Gt(pj)
			})

			// Collect unique pools from top 30 paths
			poolSet := collectPoolSet(allCandidates, 30)

			formulaTime := time.Since(t0)

			// Count profitable
			profitable := 0
			for _, c := range allCandidates {
				if c.formulaOut.Gt(c.amountIn) {
					profitable++
				}
			}

			fmt.Fprintf(os.Stderr, "[arb3] %s formula: %d candidates (%d profitable), %d pools, baseFee=%d, %v\n",
				hub.label, len(allCandidates), profitable, len(poolSet), baseFee, formulaTime.Round(time.Microsecond))

			// Log top 10 candidates
			for i := 0; i < len(allCandidates) && i < 10; i++ {
				c := &allCandidates[i]
				var gross uint256.Int
				if c.formulaOut.Gt(c.amountIn) {
					gross.Sub(&c.formulaOut, c.amountIn)
				}
				poolStrs := make([]string, len(c.pools))
				for j, pidx := range c.pools {
					poolStrs[j] = pools[pidx].Address.Hex()[:10]
				}
				fmt.Fprintf(os.Stderr, "[arb3]   #%d: %d hops, in=%s, gross=%s, pools=%v\n",
					i+1, len(c.pools), c.amountIn.Dec(), gross.Dec(), poolStrs)

				if debugHops && i < 5 {
					debugPath(c, pools, c.amountIn, pm, state, evmCtx, routerAddr)
				}
			}

			if len(allCandidates) == 0 || allCandidates[0].amountIn.Gt(&allCandidates[0].formulaOut) {
				// No profitable candidates
				fmt.Fprintf(os.Stderr, "[arb3] %s: no profitable candidates\n", hub.label)
			}
			if len(allCandidates) == 0 {
				continue
			}

			// Pass 2: EVM verify top 30 paths as full swap() calls
			t1 := time.Now()
			var best *evmResult
			evmCalls := 0

			// Dedup paths (same pool sequence)
			type pathKey struct {
				hops int
				p0, p1, p2, p3 uint16
			}
			seen := make(map[pathKey]bool)

			for i := 0; i < len(allCandidates) && i < 30; i++ {
				c := &allCandidates[i]
				key := pathKey{hops: len(c.pools)}
				if len(c.pools) > 0 { key.p0 = c.pools[0] }
				if len(c.pools) > 1 { key.p1 = c.pools[1] }
				if len(c.pools) > 2 { key.p2 = c.pools[2] }
				if len(c.pools) > 3 { key.p3 = c.pools[3] }
				if seen[key] {
					continue
				}
				seen[key] = true

				r := evmVerifyPath(c, pools, c.amountIn, state, evmCtx, caller, routerAddr)
				evmCalls++
				if r == nil {
					if debugHops && evmCalls <= 10 {
						poolStrs := make([]string, len(c.pools))
						for j, pidx := range c.pools { poolStrs[j] = pools[pidx].Address.Hex()[:10] }
						fmt.Fprintf(os.Stderr, "[arb3]   evm REVERT: in=%s pools=%v\n", c.amountIn.Dec(), poolStrs)
					}
					continue
				}

				// swap() returns the caller's balance delta for cyclic arbs (tokenIn==tokenOut).
				// amountOut IS the gross profit, not amountIn + profit.
				gross := r.amountOut
				gasCost := new(uint256.Int).Mul(uint256.NewInt(r.gasUsed), uint256.NewInt(baseFee))
				if debugHops && evmCalls <= 10 {
					fmt.Fprintf(os.Stderr, "[arb3]   evm: in=%s gross=%s gasCost=%s gas=%d\n",
						r.amountIn.Dec(), gross.Dec(), gasCost.Dec(), r.gasUsed)
				}
				if gross.Gt(gasCost) {
					r.netProfit = new(uint256.Int).Sub(gross, gasCost).Float64()
					if best == nil || r.netProfit > best.netProfit {
						best = r
					}
				}
			}

			evmTime := time.Since(t1)

			if best != nil {
				// Binary search for optimal size
				t2 := time.Now()
				sized := binarySearchSize(
					&best.path, pools, best.amountIn,
					state, evmCtx, caller, routerAddr, baseFee,
				)
				sizeTime := time.Since(t2)

				if sized != nil && sized.netProfit > best.netProfit {
					best = sized
				}

				pnlBps := int64(best.amountOut.Float64() / best.amountIn.Float64() * 10000)

				fmt.Fprintf(os.Stderr, "[arb3] %s PROFIT: in=%s gross=%s pnl=%+dbps gas=%d net=%.0f evm=%d calls sizing=%v\n",
					hub.label, best.amountIn.Dec(), best.amountOut.Dec(), pnlBps,
					best.gasUsed, best.netProfit, evmCalls, sizeTime.Round(time.Microsecond))
			} else {
				fmt.Fprintf(os.Stderr, "[arb3] %s evm: no profit, %d calls, %v\n",
					hub.label, evmCalls, evmTime.Round(time.Microsecond))
			}
		}
	}

	// Initial run
	ls.RLock()
	runBlock(ls.Block(), ls.Timestamp(), ls.BaseFee(), ls.GasLimit())
	ls.RUnlock()

	if singleBlock {
		return
	}

	fmt.Fprintf(os.Stderr, "[arb3] ready. Waiting for blocks...\n")

	// Block loop
	type blockEvent struct {
		block, timestamp, baseFee, gasLimit uint64
		entries                             [][2]string
	}
	blockCh := make(chan blockEvent, 4)

	ls.SetOnBlock(func(ls *statedb.LiveState, entries [][2]string) {
		select {
		case blockCh <- blockEvent{ls.Block(), ls.Timestamp(), ls.BaseFee(), ls.GasLimit(), entries}:
		default:
			fmt.Fprintf(os.Stderr, "[arb3] WARNING: dropped block %d\n", ls.Block())
		}
	})

	for bi := range blockCh {
		pm.SetBlockTimestamp(bi.timestamp)

		// Invalidate dirty pools
		dirtyCount := 0
		for _, entry := range bi.entries {
			key := entry[0]
			if strings.HasPrefix(key, "s:") {
				parts := strings.SplitN(key, ":", 3)
				if len(parts) == 3 {
					addr := common.HexToAddress(parts[1])
					slot := common.HexToHash(parts[2])
					poolAddr := pm.InvalidateBySlot(addr, slot)
					if poolAddr != (common.Address{}) {
						dirtyCount++
					}
				}
			}
		}

		fmt.Fprintf(os.Stderr, "[arb3] block=%d dirty=%d\n", bi.block, dirtyCount)

		ls.RLock()
		runBlock(bi.block, bi.timestamp, bi.baseFee, bi.gasLimit)
		ls.RUnlock()
	}
}
