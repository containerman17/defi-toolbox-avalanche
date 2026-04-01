package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	router "defi-toolbox/contracts"
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"
	poolcollector "defi-toolbox/tools/pool-collector"

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
	tokenIn  common.Address
}

// ── Prescreen: rated edges for f64 path enumeration ──

type rateProbe struct {
	amountIn float64
	rate     float64 // amountOut / amountIn
}

type ratedEdge struct {
	poolIdx  uint16
	tokenOut common.Address
	tokenIn  common.Address
	probes   [3]rateProbe
	probeLen int
}

// rateAt returns interpolated exchange rate for a given input amount.
func (e *ratedEdge) rateAt(amountIn float64) float64 {
	n := e.probeLen
	if n == 0 {
		return 0
	}
	if n == 1 {
		return e.probes[0].rate
	}
	if amountIn <= e.probes[0].amountIn {
		return e.probes[0].rate
	}
	if amountIn >= e.probes[n-1].amountIn {
		return e.probes[n-1].rate
	}
	for i := 1; i < n; i++ {
		if amountIn <= e.probes[i].amountIn {
			a0, r0 := e.probes[i-1].amountIn, e.probes[i-1].rate
			a1, r1 := e.probes[i].amountIn, e.probes[i].rate
			t := (amountIn - a0) / (a1 - a0)
			return r0 + t*(r1-r0)
		}
	}
	return e.probes[n-1].rate
}

type prescreenData struct {
	tokenPrices map[common.Address]float64     // raw tokens per 1 AVAX wei
	ratedAdj    map[common.Address][]ratedEdge // forward adjacency with rates
	reverseAdj  map[common.Address][]common.Address
}

// buildTokenPrices computes how many raw tokens of each token you get for 1 AVAX.
func buildTokenPrices(pm *formulas.PoolManager, adj map[common.Address][]poolEdge, pools []pf.Pool) map[common.Address]float64 {
	prices := map[common.Address]float64{WAVAX: 1e18} // 1 AVAX = 1e18 wei of WAVAX
	oneAVAX := uint256.NewInt(1_000_000_000_000_000_000)

	// Wave 1: WAVAX → direct neighbors
	for _, edge := range adj[WAVAX] {
		if _, priced := prices[edge.tokenOut]; priced {
			continue
		}
		out := pm.Quote(pools[edge.poolIdx].Address, oneAVAX, edge.tokenIn, edge.tokenOut)
		if !out.IsZero() {
			f := out.Float64()
			if existing, ok := prices[edge.tokenOut]; !ok || f > existing {
				prices[edge.tokenOut] = f
			}
		}
	}

	// Wave 2: priced tokens → their unpriced neighbors
	wave1Tokens := make([]common.Address, 0, len(prices))
	for tok := range prices {
		if tok != WAVAX {
			wave1Tokens = append(wave1Tokens, tok)
		}
	}
	for _, tok := range wave1Tokens {
		tokPrice := prices[tok] // raw tokens per 1 AVAX
		if tokPrice == 0 {
			continue
		}
		// Quote ~1 AVAX worth of this token into its neighbors
		amt := new(uint256.Int)
		amt.SetFromBig(new(big.Int).SetUint64(uint64(tokPrice)))
		if amt.IsZero() {
			continue
		}
		for _, edge := range adj[tok] {
			if _, priced := prices[edge.tokenOut]; priced {
				continue
			}
			out := pm.Quote(pools[edge.poolIdx].Address, amt, edge.tokenIn, edge.tokenOut)
			if !out.IsZero() {
				// out = how many tokens of edge.tokenOut you get for tokPrice units of tok
				// tokPrice units of tok ≈ 1 AVAX, so out ≈ price of edge.tokenOut per AVAX
				f := out.Float64()
				if existing, ok := prices[edge.tokenOut]; !ok || f > existing {
					prices[edge.tokenOut] = f
				}
			}
		}
	}

	return prices
}

// buildPrescreenData builds the rate table and rated adjacency for f64 enumeration.
func buildPrescreenData(pm *formulas.PoolManager, adj map[common.Address][]poolEdge, pools []pf.Pool) *prescreenData {
	tokenPrices := buildTokenPrices(pm, adj, pools)

	ratedAdj := make(map[common.Address][]ratedEdge, len(adj))
	reverseAdj := make(map[common.Address][]common.Address, len(adj))

	// For each directed edge, quote at 3 amounts and store rates
	probeScales := [3]float64{0.001, 0.1, 10.0}

	for token, edges := range adj {
		for _, edge := range edges {
			// Get AVAX-equivalent amount for this input token
			tokenPrice := tokenPrices[token]
			if tokenPrice == 0 {
				tokenPrice = 1e18 // fallback: assume 1:1 with AVAX
			}

			re := ratedEdge{
				poolIdx:  edge.poolIdx,
				tokenOut: edge.tokenOut,
				tokenIn:  edge.tokenIn,
			}

			for pi, scale := range probeScales {
				rawAmount := tokenPrice * scale
				if rawAmount < 1 {
					rawAmount = 1
				}
				amt := new(uint256.Int)
				amt.SetFromBig(new(big.Int).SetUint64(uint64(rawAmount)))
				if amt.IsZero() {
					continue
				}

				out := pm.Quote(pools[edge.poolIdx].Address, amt, edge.tokenIn, edge.tokenOut)
				if out.IsZero() {
					continue
				}

				rate := out.Float64() / amt.Float64()
				re.probes[pi] = rateProbe{amountIn: amt.Float64(), rate: rate}
				re.probeLen = pi + 1
			}

			if re.probeLen > 0 {
				ratedAdj[token] = append(ratedAdj[token], re)
			}

			// Build reverse adjacency (token-level)
			reverseAdj[edge.tokenOut] = append(reverseAdj[edge.tokenOut], token)
		}
	}

	return &prescreenData{
		tokenPrices: tokenPrices,
		ratedAdj:    ratedAdj,
		reverseAdj:  reverseAdj,
	}
}

// prescreenPools uses f64 path enumeration to find the top pools for a given hub.
// Returns a set of pool indices that appear in promising cyclic paths.
func prescreenPools(data *prescreenData, hub common.Address, maxHops int, topN int) map[uint16]bool {
	poolScores := make(map[uint16]float64)
	hubPrice := data.tokenPrices[hub]
	if hubPrice == 0 {
		hubPrice = 1e18
	}
	inputAmount := hubPrice // ~1 AVAX worth of hub token

	edges1 := data.ratedAdj[hub]

	// For 4-hop meet-in-the-middle: collect forward 2-hop arrivals
	type fwd2Entry struct {
		amount float64
		pool1  uint16
		pool2  uint16
	}
	fwd2 := make(map[common.Address]fwd2Entry)

	for _, e1 := range edges1 {
		rate1 := e1.rateAt(inputAmount)
		if rate1 <= 0 {
			continue
		}
		amt1 := inputAmount * rate1

		// 1-hop: back to hub
		if e1.tokenOut == hub {
			if amt1 > poolScores[e1.poolIdx] {
				poolScores[e1.poolIdx] = amt1
			}
			continue
		}

		edges2 := data.ratedAdj[e1.tokenOut]
		for _, e2 := range edges2 {
			if e2.tokenOut == hub && maxHops < 2 {
				continue
			}
			rate2 := e2.rateAt(amt1)
			if rate2 <= 0 {
				continue
			}
			amt2 := amt1 * rate2

			// 2-hop: back to hub
			if e2.tokenOut == hub {
				for _, pi := range []uint16{e1.poolIdx, e2.poolIdx} {
					if amt2 > poolScores[pi] {
						poolScores[pi] = amt2
					}
				}
				continue
			}

			// Record forward 2-hop for meet-in-the-middle
			if entry, ok := fwd2[e2.tokenOut]; !ok || amt2 > entry.amount {
				fwd2[e2.tokenOut] = fwd2Entry{amount: amt2, pool1: e1.poolIdx, pool2: e2.poolIdx}
			}

			// 3-hop
			if maxHops >= 3 {
				edges3 := data.ratedAdj[e2.tokenOut]
				for _, e3 := range edges3 {
					if e3.tokenOut == e1.tokenOut {
						continue
					}
					rate3 := e3.rateAt(amt2)
					if rate3 <= 0 {
						continue
					}
					amt3 := amt2 * rate3

					if e3.tokenOut == hub {
						for _, pi := range []uint16{e1.poolIdx, e2.poolIdx, e3.poolIdx} {
							if amt3 > poolScores[pi] {
								poolScores[pi] = amt3
							}
						}
					}
				}
			}
		}
	}

	// 4-hop meet-in-the-middle
	if maxHops >= 4 && len(fwd2) > 0 {
		// Penultimate tokens: tokens with a direct edge back to hub
		penSet := make(map[common.Address]bool)
		for _, src := range data.reverseAdj[hub] {
			penSet[src] = true
		}

		for midToken, fwdEntry := range fwd2 {
			edges3 := data.ratedAdj[midToken]
			for _, e3 := range edges3 {
				if e3.tokenOut == hub || !penSet[e3.tokenOut] {
					continue
				}
				rate3 := e3.rateAt(fwdEntry.amount)
				if rate3 <= 0 {
					continue
				}
				amt3 := fwdEntry.amount * rate3

				edges4 := data.ratedAdj[e3.tokenOut]
				for _, e4 := range edges4 {
					if e4.tokenOut != hub {
						continue
					}
					rate4 := e4.rateAt(amt3)
					if rate4 <= 0 {
						continue
					}
					amt4 := amt3 * rate4

					for _, pi := range []uint16{fwdEntry.pool1, fwdEntry.pool2, e3.poolIdx, e4.poolIdx} {
						if amt4 > poolScores[pi] {
							poolScores[pi] = amt4
						}
					}
				}
			}
		}
	}

	// Sort by score, take top N
	type scored struct {
		poolIdx uint16
		score   float64
	}
	sortedPools := make([]scored, 0, len(poolScores))
	for pi, s := range poolScores {
		sortedPools = append(sortedPools, scored{pi, s})
	}
	sort.Slice(sortedPools, func(i, j int) bool {
		return sortedPools[i].score > sortedPools[j].score
	})

	result := make(map[uint16]bool, topN)
	for i, sp := range sortedPools {
		if i >= topN {
			break
		}
		result[sp.poolIdx] = true
	}

	// Also include top 20 pools directly adjacent to hub
	for i, edge := range data.ratedAdj[hub] {
		if i >= 20 {
			break
		}
		result[edge.poolIdx] = true
	}

	return result
}

// filterAdjacency builds a subset adjacency containing only the selected pools.
func filterAdjacency(fullAdj map[common.Address][]poolEdge, poolSet map[uint16]bool) map[common.Address][]poolEdge {
	filtered := make(map[common.Address][]poolEdge, len(fullAdj))
	for token, edges := range fullAdj {
		var kept []poolEdge
		for _, e := range edges {
			if poolSet[e.poolIdx] {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			filtered[token] = kept
		}
	}
	return filtered
}

// ── Hub config ──

type hubConfig struct {
	token      common.Address
	label      string
	sizes      []*uint256.Int // starting amounts for formula BFS
	maxBalance *uint256.Int
	price      *uint256.Int // price of 1 AVAX in hub token units (for gas cost conversion)
}

// ── BFS data structures ──

// bfsEntry is one frontier node: an amount of some token reached via a specific pool.
type bfsEntry struct {
	token    common.Address
	amount   uint256.Int
	parentID int32  // index into flat entries array (-1 for root)
	pool     uint16 // pool index that produced this
	tokenIn  common.Address
}

// bfsPath is a reconstructed path from backtracking.
type bfsPath struct {
	pools     []uint16
	tokenIns  []common.Address
	tokenOuts []common.Address
	amountIn  *uint256.Int
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
) ([]bfsPath, int) {
	// ── Backward reachability pruning ──
	// Build reverse adjacency (token-level only): for each edge A→B, record A in revAdj[B].
	revAdj := make(map[common.Address][]common.Address)
	for token, edges := range adj {
		for _, edge := range edges {
			revAdj[edge.tokenOut] = append(revAdj[edge.tokenOut], token)
		}
	}

	// reachable[r] = tokens that can reach hub in at most r+1 hops.
	// reachable[0] = {hub} ∪ {tokens with a direct edge to hub}
	reachable := make([]map[common.Address]struct{}, maxLayers-1)
	reachable[0] = make(map[common.Address]struct{}, 512)
	reachable[0][hub] = struct{}{}
	for _, src := range revAdj[hub] {
		reachable[0][src] = struct{}{}
	}
	for r := 1; r < maxLayers-1; r++ {
		reachable[r] = make(map[common.Address]struct{}, len(reachable[r-1]))
		for tok := range reachable[r-1] {
			reachable[r][tok] = struct{}{}
		}
		for tok := range reachable[r-1] {
			for _, src := range revAdj[tok] {
				reachable[r][src] = struct{}{}
			}
		}
	}

	// Flat array of all entries for backtracking
	allEntries := []bfsEntry{{token: hub, amount: *startAmount, parentID: -1}}

	// Frontier: token → indices into allEntries (max topPerToken per token)
	type frontier = map[common.Address][]int32

	current := frontier{hub: {0}} // index 0 = the root entry
	var candidates []bfsPath
	quoteCount := 0

	for layer := 1; layer <= maxLayers; layer++ {
		next := make(frontier)
		remaining := maxLayers - layer // hops left after this expansion

		for token, entryIDs := range current {
			edges := adj[token]
			for _, eid := range entryIDs {
				entry := &allEntries[eid]
				if entry.amount.IsZero() {
					continue
				}
				for _, edge := range edges {
					// Backward reachability check
					if edge.tokenOut == hub {
						// Always allow edges back to hub (terminal candidates)
					} else if remaining == 0 {
						// Last layer: only hub-bound edges allowed
						continue
					} else if _, ok := reachable[remaining-1][edge.tokenOut]; !ok {
						// Token can't reach hub in remaining hops
						continue
					}

					quoteCount++
					out := pm.Quote(pools[edge.poolIdx].Address, &entry.amount, edge.tokenIn, edge.tokenOut)
					if out.IsZero() {
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
						tokenIn:  edge.tokenIn,
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

	return candidates, quoteCount
}

// backtrack reconstructs the path from a terminal entry back to the root.
func backtrack(allEntries []bfsEntry, lastEntryID int32, finalEdge poolEdge, startAmount *uint256.Int) bfsPath {
	// Collect hops in reverse: final edge first, then walk parent chain
	var poolsList []uint16
	var tokenInsList []common.Address
	var tokenOutsList []common.Address

	// The final hop (into hub)
	poolsList = append(poolsList, finalEdge.poolIdx)
	tokenInsList = append(tokenInsList, finalEdge.tokenIn)
	tokenOutsList = append(tokenOutsList, finalEdge.tokenOut)

	// Walk backwards
	eid := lastEntryID
	for eid >= 0 {
		e := &allEntries[eid]
		if e.parentID < 0 {
			break // root — don't add, it's the starting point
		}
		poolsList = append(poolsList, e.pool)
		tokenInsList = append(tokenInsList, e.tokenIn)
		tokenOutsList = append(tokenOutsList, e.token)
		eid = e.parentID
	}

	// Reverse to get forward order
	for i, j := 0, len(poolsList)-1; i < j; i, j = i+1, j-1 {
		poolsList[i], poolsList[j] = poolsList[j], poolsList[i]
		tokenInsList[i], tokenInsList[j] = tokenInsList[j], tokenInsList[i]
		tokenOutsList[i], tokenOutsList[j] = tokenOutsList[j], tokenOutsList[i]
	}

	return bfsPath{
		pools:     poolsList,
		tokenIns:  tokenInsList,
		tokenOuts: tokenOutsList,
		amountIn:  new(uint256.Int).Set(startAmount),
	}
}

// ── EVM verification ──

type evmResult struct {
	path      bfsPath
	amountIn  *uint256.Int
	amountOut *uint256.Int
	gasUsed   uint64
	netProfit float64
	calldata  []byte // swap() calldata ready for on-chain submission
}

// submitArb signs and broadcasts a swap transaction. Returns tx hash or error.
func submitArb(key *ecdsa.PrivateKey, nonce uint64, routerAddr common.Address,
	calldata []byte, gasLimit uint64, baseFee uint64) (string, error) {

	maxFee := new(big.Int).SetUint64(baseFee*2 + 1_000_000_000)
	to := routerAddr
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(0),
		GasFeeCap: maxFee,
		Gas:       gasLimit * 12 / 10, // 20% headroom
		To:        &to,
		Data:      calldata,
	})
	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	rawTx, _ := signedTx.MarshalBinary()
	rawHex := "0x" + hex.EncodeToString(rawTx)
	resp, err := rpcCallJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "eth_sendRawTransaction",
		"params": []string{rawHex},
	})
	if err != nil {
		return "", err
	}
	var rpcResp struct {
		Result string
		Error  *struct{ Message string }
	}
	json.Unmarshal(resp, &rpcResp)
	if rpcResp.Error != nil {
		return "", fmt.Errorf("rpc: %s", rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// verifyOnNode does an eth_call against the real node and compares with local EVM result.
// Returns true if amounts match. Exits the process if the node reverts or amounts differ.
func verifyOnNode(caller, routerAddr common.Address, calldata []byte, localAmountOut *uint256.Int, label string, block uint64) bool {
	blockHex := fmt.Sprintf("0x%x", block)
	resp, err := rpcCallJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []interface{}{
			map[string]string{
				"from": caller.Hex(),
				"to":   routerAddr.Hex(),
				"data": "0x" + hex.EncodeToString(calldata),
			},
			blockHex,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb4] ⚠️ %s NODE VERIFY @%s: rpc error: %v\n", label, blockHex, err)
		return false
	}
	var rpcResp struct {
		Result string
		Error  *struct{ Message string }
	}
	json.Unmarshal(resp, &rpcResp)
	if rpcResp.Error != nil {
		fmt.Fprintf(os.Stderr, "[arb4] ⚠️ %s NODE VERIFY @%s: node reverted: %s\n", label, blockHex, rpcResp.Error.Message)
		return false
	}
	b, _ := hex.DecodeString(strings.TrimPrefix(rpcResp.Result, "0x"))
	if len(b) < 32 {
		fmt.Fprintf(os.Stderr, "[arb4] ⚠️ %s NODE VERIFY @%s: short return (%d bytes)\n", label, blockHex, len(b))
		return false
	}
	nodeAmountOut := new(uint256.Int).SetBytes(b[:32])
	if !nodeAmountOut.Eq(localAmountOut) {
		fmt.Fprintf(os.Stderr, "[arb4] ⚠️ %s NODE VERIFY @%s: mismatch local=%s node=%s\n",
			label, blockHex, localAmountOut.Dec(), nodeAmountOut.Dec())
		return false
	}
	fmt.Fprintf(os.Stderr, "[arb4] %s NODE VERIFY OK @%s: %s\n", label, blockHex, nodeAmountOut.Dec())
	return true
}

// evmVerifyPath runs a full swap() call for a given path.
// stateWithOverrides should have sender balance + allowance overrides applied.
func evmVerifyPath(
	path *bfsPath,
	pools []pf.Pool,
	amountIn *uint256.Int,
	stateWithOverrides *statedb.StateDB,
	evmCtx *statedb.CachedContext,
	sender common.Address,
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
		tokenPairs[h*2] = path.tokenIns[h]
		tokenPairs[h*2+1] = path.tokenOuts[h]
	}

	calldata := pf.EncodeSwapMulti(poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, uint256.NewInt(1))
	cs := statedb.NewCallState(stateWithOverrides)
	ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, sender, routerAddr, calldata)

	if err != nil || cs.Err() != nil || len(ret) < 32 {
		return nil
	}

	amountOut := new(uint256.Int).SetBytes(ret[:32])
	return &evmResult{
		path:      *path,
		amountIn:  new(uint256.Int).Set(amountIn),
		amountOut: amountOut,
		gasUsed:   gasUsed,
		calldata:  calldata,
	}
}

// quoteAVAXPrice finds the price of 1 AVAX in hub token units by quoting through
// the best pool connecting WAVAX to the hub token.
func quoteAVAXPrice(pools []pf.Pool, pm *formulas.PoolManager, wavax, hubToken common.Address, oneAVAX *uint256.Int) *uint256.Int {
	var bestPrice uint256.Int
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		hasWavax := false
		hasHub := false
		for _, t := range p.Tokens {
			if t == wavax {
				hasWavax = true
			}
			if t == hubToken {
				hasHub = true
			}
		}
		if !hasWavax || !hasHub {
			continue
		}
		out := pm.Quote(p.Address, oneAVAX, wavax, hubToken)
		if out.Gt(&bestPrice) {
			bestPrice = out
		}
	}
	if bestPrice.IsZero() {
		return nil
	}
	return new(uint256.Int).Set(&bestPrice)
}

// binarySearchSize finds the optimal input amount for a given path.
// Starts at baseAmount, searches between baseAmount/2 and baseAmount*2.
func binarySearchSize(
	path *bfsPath,
	pools []pf.Pool,
	baseAmount *uint256.Int,
	stateWithOverrides *statedb.StateDB,
	evmCtx *statedb.CachedContext,
	caller common.Address,
	routerAddr common.Address,
	baseFee uint64,
	hubPrice *uint256.Int, // price of 1 AVAX in hub token units
) *evmResult {
	lo := new(uint256.Int).Div(baseAmount, uint256.NewInt(5)) // baseAmount / 5
	hi := new(uint256.Int).Mul(baseAmount, uint256.NewInt(5)) // baseAmount * 5

	// Evaluate at lo, mid, hi
	evalAt := func(amt *uint256.Int) (netProfit float64, result *evmResult) {
		r := evmVerifyPath(path, pools, amt, stateWithOverrides, evmCtx, caller, routerAddr)
		if r == nil {
			return -1e18, nil
		}
		// swap() returns gross profit directly for cyclic arbs
		gross := r.amountOut
		if gross.IsZero() {
			return -1e18, r
		}
		// gasCostInToken = gasUsed * baseFee * hubPrice / 1e18
		gasCostAVAX := new(uint256.Int).Mul(uint256.NewInt(r.gasUsed), uint256.NewInt(baseFee))
		gasCost := new(uint256.Int).Mul(gasCostAVAX, hubPrice)
		gasCost.Div(gasCost, uint256.NewInt(1_000_000_000_000_000_000))
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

func ensureApprovals(key *ecdsa.PrivateKey, caller common.Address, hubs []hubConfig) uint64 {
	routerAddr := router.DeployedRouter
	signer := types.NewLondonSigner(chainID)
	nonce := rpcNonce(caller)
	baseFee := rpcBaseFee()

	for _, hub := range hubs {
		allowance := rpcAllowance(hub.token, caller, routerAddr)
		needed := new(uint256.Int).Mul(hub.maxBalance, uint256.NewInt(1000))
		if allowance.Gt(needed) || allowance.Eq(needed) {
			fmt.Fprintf(os.Stderr, "[arb4] %s allowance OK\n", hub.label)
			continue
		}
		fmt.Fprintf(os.Stderr, "[arb4] %s approving...\n", hub.label)

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
			fmt.Fprintf(os.Stderr, "[arb4] sign approve %s: %v\n", hub.label, err)
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
			fmt.Fprintf(os.Stderr, "[arb4] approve %s failed: %s\n", hub.label, rpcResp.Error.Message)
			continue
		}
		fmt.Fprintf(os.Stderr, "[arb4] %s approved, nonce=%d\n", hub.label, nonce)
		nonce++
	}
	return nonce
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
	stateServerFlag := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimitFlag := flag.Int("pool-limit", 4000, "max pools to load")
	blockFlag := flag.String("block", "", "run on a specific block number (uses debug endpoint)")
	poolsFlag := flag.String("pools", "", "comma-separated pool addresses to restrict BFS")
	debugHopsFlag := flag.Bool("debug-hops", false, "print per-hop formula vs EVM comparison")
	exitOnTxFlag := flag.Bool("exit-on-tx", false, "exit after first submitted transaction")
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unknown argument: %s\n", flag.Arg(0))
		flag.Usage()
		os.Exit(1)
	}

	stateServerURL := *stateServerFlag
	poolLimit := *poolLimitFlag
	singleBlock := false
	debugHops := *debugHopsFlag
	exitOnTx := *exitOnTxFlag

	if *blockFlag != "" {
		stateServerURL = fmt.Sprintf("ws://localhost:7449/debug/%s", *blockFlag)
		singleBlock = true
	}

	var filterPools []common.Address
	if *poolsFlag != "" {
		for _, s := range strings.Split(*poolsFlag, ",") {
			filterPools = append(filterPools, common.HexToAddress(strings.TrimSpace(s)))
		}
	}

	// Load private key (required)
	loadEnv()
	privKeyHex := strings.TrimPrefix(os.Getenv("ARB_PRIVATE_KEY"), "0x")
	if privKeyHex == "" {
		fmt.Fprintf(os.Stderr, "[arb4] ARB_PRIVATE_KEY not set\n")
		os.Exit(1)
	}
	privKey, err := crypto.HexToECDSA(privKeyHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb4] bad private key: %v\n", err)
		os.Exit(1)
	}
	caller := crypto.PubkeyToAddress(privKey.PublicKey)
	fmt.Fprintf(os.Stderr, "[arb4] caller: %s\n", caller.Hex())

	// Load registry + pools
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)
	fmt.Fprintf(os.Stderr, "[arb4] loaded %d pools\n", len(pools))

	// Connect to state server
	ls, err := statedb.Connect(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb4] connect failed: %v\n", err)
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
			pm.SetPoolTokens(p.Address, p.Tokens...)
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
		fmt.Fprintf(os.Stderr, "[arb4] pool filter: %d pools\n", len(poolFilter))
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
		for ti := range p.Tokens {
			for tj := range p.Tokens {
				if ti != tj {
					adj[p.Tokens[ti]] = append(adj[p.Tokens[ti]], poolEdge{idx, p.Tokens[tj], p.Tokens[ti]})
				}
			}
		}
		poolCount++
	}
	fmt.Fprintf(os.Stderr, "[arb4] adjacency: %d tokens, %d pools\n", len(adj), poolCount)

	// Warmup: build all pool quoters
	tw := time.Now()
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[arb4] warmup: %v\n", time.Since(tw).Round(time.Millisecond))

	// Hub config
	hubTokens := []struct {
		addr  common.Address
		label string
		sizes []*uint256.Int
	}{
		{WAVAX, "WAVAX", []*uint256.Int{
			uint256.NewInt(1_000_000_000_000_000_000), // 1 AVAX
			uint256.NewInt(100_000_000_000_000_000),   // 0.1
			uint256.NewInt(10_000_000_000_000_000),    // 0.01
			uint256.NewInt(1_000_000_000_000_000),     // 0.001
			uint256.NewInt(100_000_000_000_000),       // 0.0001
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
		fmt.Fprintf(os.Stderr, "[arb4] hub %s: balance=%s\n", ht.label, bal.Dec())
		// Price of 1 AVAX in hub token units (for gas cost conversion)
		var price *uint256.Int
		if ht.addr == WAVAX {
			price = uint256.NewInt(1_000_000_000_000_000_000) // 1:1
		} else {
			// Quote 1 AVAX → hub token through any connecting pool
			oneAVAX := uint256.NewInt(1_000_000_000_000_000_000)
			price = quoteAVAXPrice(pools, pm, WAVAX, ht.addr, oneAVAX)
			if price == nil || price.IsZero() {
				price = uint256.NewInt(1) // fallback: prevent div-by-zero
				fmt.Fprintf(os.Stderr, "[arb4] WARNING: no AVAX price for %s\n", ht.label)
			}
		}
		if ht.addr != WAVAX {
			fmt.Fprintf(os.Stderr, "[arb4] hub %s: AVAX price=%s\n", ht.label, price.Dec())
		}
		hubs = append(hubs, hubConfig{
			token: ht.addr, label: ht.label,
			sizes: ht.sizes, maxBalance: bal, price: price,
		})
	}
	ls.RUnlock()

	// Ensure approvals
	approvalNonce := ensureApprovals(privKey, caller, hubs)

	routerAddr := router.DeployedRouter

	// Build persistent overlay with sender overrides for EVM verification.
	// swap() uses transferFrom(sender→router), so the sender needs balance + allowance.
	senderOverrides := router.BuildSenderOverrides(caller, routerAddr, pools)
	stateWithOverrides := pf.ApplyOverridesFlat(state, senderOverrides)

	// debugHopByHop prints per-hop formula vs EVM comparison for a path.
	debugPath := func(path *bfsPath, pools []pf.Pool, amountIn *uint256.Int,
		pm *formulas.PoolManager, state *statedb.StateDB, evmCtx *statedb.CachedContext,
		routerAddr common.Address) {

		// Formula hop-by-hop
		currentFormula := new(uint256.Int).Set(amountIn)
		fmt.Fprintf(os.Stderr, "        formula hop-by-hop:\n")
		for h := 0; h < len(path.pools); h++ {
			p := &pools[path.pools[h]]
			out := pm.Quote(p.Address, currentFormula, path.tokenIns[h], path.tokenOuts[h])
			fmt.Fprintf(os.Stderr, "          hop%d: %s (%s) %s→%s in=%s out=%s\n",
				h+1, p.Address.Hex()[:12], p.Dex, path.tokenIns[h].Hex()[:8], path.tokenOuts[h].Hex()[:8], currentFormula.Dec(), out.Dec())
			currentFormula.Set(&out)
		}

		// EVM hop-by-hop (single pool each, with token overrides)
		overrides := router.BuildTokenOverrides(routerAddr, pools)
		baseWithOverrides := pf.ApplyOverridesFlat(state, overrides)
		currentEVM := new(uint256.Int).Set(amountIn)
		fmt.Fprintf(os.Stderr, "        evm hop-by-hop (debugSwapSingle):\n")
		for h := 0; h < len(path.pools); h++ {
			p := &pools[path.pools[h]]
			tokenIn, tokenOut := path.tokenIns[h], path.tokenOuts[h]
			calldata := pf.EncodeSwapSingleWithExtra(p.Address, p.PoolType, tokenIn, tokenOut, currentEVM, p.ExtraData)
			cs := statedb.NewCallState(baseWithOverrides)
			ret, gas, err := evmCtx.ExecuteWithCallState(cs, common.Address{}, routerAddr, calldata)
			if err != nil || cs.Err() != nil || len(ret) < 32 {
				fmt.Fprintf(os.Stderr, "          hop%d: %s REVERT\n", h+1, p.Address.Hex()[:12])
				break
			}
			evmOut := new(uint256.Int).SetBytes(ret[:32])
			match := "MATCH"
			fOut := pm.Quote(p.Address, currentEVM, tokenIn, tokenOut)
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

	nextNonce := approvalNonce

	// ── Run one block ──
	runBlock := func(block, timestamp, baseFee, gasLimit uint64) {
		blockT0 := time.Now()
		cfg := statedb.EVMConfig{
			BlockNumber: block, Timestamp: timestamp,
			ChainID: 43114, BaseFee: baseFee, GasLimit: gasLimit,
		}
		evmCtx := statedb.GetCachedContext(cfg)

		// Update AVAX prices for non-WAVAX hubs
		oneAVAX := uint256.NewInt(1_000_000_000_000_000_000)
		for i := range hubs {
			if hubs[i].token != WAVAX {
				if p := quoteAVAXPrice(pools, pm, WAVAX, hubs[i].token, oneAVAX); p != nil && !p.IsZero() {
					hubs[i].price = p
				}
			}
		}

		// Phase 0+1: Build prescreen data (rate table + rated adjacency) — once per block.
		// Retry until token count stabilizes (early runs have cold cache, quotes fail
		// but backfill state for the next attempt).
		psT0 := time.Now()
		var psData *prescreenData
		prevTokens := 0
		for attempt := 0; attempt < 5; attempt++ {
			psData = buildPrescreenData(pm, adj, pools)
			if len(psData.tokenPrices) == prevTokens {
				break // stabilized
			}
			prevTokens = len(psData.tokenPrices)
		}
		psTime := time.Since(psT0)
		totalEdges := 0
		for _, edges := range psData.ratedAdj {
			totalEdges += len(edges)
		}
		fmt.Fprintf(os.Stderr, "[arb4] prescreen: %d tokens priced, %d rated edges, %v\n",
			len(psData.tokenPrices), totalEdges, psTime.Round(time.Millisecond))

		// Per-hub result collected from parallel search
		type hubResult struct {
			hub      hubConfig
			best     *evmResult
			evmCalls int
		}

		hubResults := make([]hubResult, len(hubs))
		var hubWg sync.WaitGroup
		for hi, hub := range hubs {
			hubWg.Add(1)
			go func(hi int, hub hubConfig) {
				defer hubWg.Done()
				t0 := time.Now()

				// Phase 2: f64 path enumeration to select relevant pools
				prescreenSet := prescreenPools(psData, hub.token, maxLayers, 500)
				filteredAdj := filterAdjacency(adj, prescreenSet)
				prescreenTime := time.Since(t0)

				// Phase 3: Formula BFS at all sizes on filtered pool set
				var allCandidates []bfsPath
				totalQuotes := 0
				for _, size := range hub.sizes {
					if hub.maxBalance != nil && size.Gt(hub.maxBalance) {
						continue
					}
					candidates, quotes := formulaBFS(pm, filteredAdj, pools, hub.token, size)
					allCandidates = append(allCandidates, candidates...)
					totalQuotes += quotes
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

				formulaTime := time.Since(t0)

				// Count profitable
				profitable := 0
				for _, c := range allCandidates {
					if c.formulaOut.Gt(c.amountIn) {
						profitable++
					}
				}

				fmt.Fprintf(os.Stderr, "[arb4] %s prescreen=%d pools, formula: %d candidates (%d profitable), %d quotes, %v+%v\n",
					hub.label, len(prescreenSet), len(allCandidates), profitable, totalQuotes, prescreenTime.Round(time.Microsecond), formulaTime.Round(time.Microsecond))

				// Log top 3 candidates
				for i := 0; i < len(allCandidates) && i < 3; i++ {
					c := &allCandidates[i]
					var gross uint256.Int
					if c.formulaOut.Gt(c.amountIn) {
						gross.Sub(&c.formulaOut, c.amountIn)
					}
					poolStrs := make([]string, len(c.pools))
					for j, pidx := range c.pools {
						poolStrs[j] = pools[pidx].Address.Hex()[:10]
					}
					fmt.Fprintf(os.Stderr, "[arb4]   #%d: %d hops, in=%s, gross=%s, pools=%v\n",
						i+1, len(c.pools), c.amountIn.Dec(), gross.Dec(), poolStrs)

					if debugHops && i < 5 {
						debugPath(c, pools, c.amountIn, pm, state, evmCtx, routerAddr)
					}
				}

				if len(allCandidates) == 0 || allCandidates[0].amountIn.Gt(&allCandidates[0].formulaOut) {
					fmt.Fprintf(os.Stderr, "[arb4] %s: no profitable candidates\n", hub.label)
				}
				if len(allCandidates) == 0 {
					return
				}

				// Pass 2: EVM verify top 30 paths as full swap() calls
				t1 := time.Now()
				fetchBefore := statedb.FetchCount.Load()
				evmCalls := 0

				// Dedup paths (same pool sequence)
				type pathKey struct {
					hops           int
					p0, p1, p2, p3 uint16
				}
				seen := make(map[pathKey]bool)

				// Collect top 5 profitable candidates for binary search
				var topCandidates []*evmResult
				const maxSizingCandidates = 5

				localEvmCtx := statedb.GetCachedContext(cfg)
				for i := 0; i < len(allCandidates) && i < 30; i++ {
					c := &allCandidates[i]
					key := pathKey{hops: len(c.pools)}
					if len(c.pools) > 0 {
						key.p0 = c.pools[0]
					}
					if len(c.pools) > 1 {
						key.p1 = c.pools[1]
					}
					if len(c.pools) > 2 {
						key.p2 = c.pools[2]
					}
					if len(c.pools) > 3 {
						key.p3 = c.pools[3]
					}
					if seen[key] {
						continue
					}
					seen[key] = true

					r := evmVerifyPath(c, pools, c.amountIn, stateWithOverrides, localEvmCtx, caller, routerAddr)
					evmCalls++
					if r == nil {
						if debugHops && evmCalls <= 10 {
							poolStrs := make([]string, len(c.pools))
							for j, pidx := range c.pools {
								poolStrs[j] = pools[pidx].Address.Hex()[:10]
							}
							fmt.Fprintf(os.Stderr, "[arb4]   evm REVERT: in=%s pools=%v\n", c.amountIn.Dec(), poolStrs)
						}
						continue
					}

					// swap() returns the caller's balance delta for cyclic arbs (tokenIn==tokenOut).
					// amountOut IS the gross profit, not amountIn + profit.
					gross := r.amountOut
					// gasCostInToken = gasUsed * baseFee * hubPrice / 1e18
					gasCostAVAX := new(uint256.Int).Mul(uint256.NewInt(r.gasUsed), uint256.NewInt(baseFee))
					gasCost := new(uint256.Int).Mul(gasCostAVAX, hub.price)
					gasCost.Div(gasCost, uint256.NewInt(1_000_000_000_000_000_000))
					if debugHops && evmCalls <= 10 {
						fmt.Fprintf(os.Stderr, "[arb4]   evm: in=%s gross=%s gasCost=%s gas=%d\n",
							r.amountIn.Dec(), gross.Dec(), gasCost.Dec(), r.gasUsed)
					}
					if gross.Gt(gasCost) {
						r.netProfit = new(uint256.Int).Sub(gross, gasCost).Float64()
						if len(topCandidates) < maxSizingCandidates {
							topCandidates = append(topCandidates, r)
						} else {
							// Replace the worst if this one is better
							worstIdx := 0
							for j := 1; j < len(topCandidates); j++ {
								if topCandidates[j].netProfit < topCandidates[worstIdx].netProfit {
									worstIdx = j
								}
							}
							if r.netProfit > topCandidates[worstIdx].netProfit {
								topCandidates[worstIdx] = r
							}
						}
					}
				}

				evmTime := time.Since(t1)

				if len(topCandidates) > 0 {
					// Binary search for optimal size on top 5 candidates (parallel)
					t2 := time.Now()
					results := make([]*evmResult, len(topCandidates))
					var wg sync.WaitGroup
					for ci, cand := range topCandidates {
						wg.Add(1)
						go func(idx int, c *evmResult) {
							defer wg.Done()
							sizeCtx := statedb.GetCachedContext(cfg)
							sized := binarySearchSize(
								&c.path, pools, c.amountIn,
								stateWithOverrides, sizeCtx, caller, routerAddr, baseFee, hub.price,
							)
							if sized != nil && sized.netProfit > c.netProfit {
								results[idx] = sized
							} else {
								results[idx] = c
							}
						}(ci, cand)
					}
					wg.Wait()
					var best *evmResult
					for _, r := range results {
						if r != nil && (best == nil || r.netProfit > best.netProfit) {
							best = r
						}
					}
					sizeTime := time.Since(t2)

					div := 1e18
					if hub.token == USDC {
						div = 1e6
					}
					fetchAfter := statedb.FetchCount.Load()
					fmt.Fprintf(os.Stderr, "[arb4] %s PROFIT: in=%.4f gross=%.6f gas=%d net=%.6f evm=%d sizing=%v fetches=%d\n",
						hub.label, best.amountIn.Float64()/div, best.amountOut.Float64()/div,
						best.gasUsed, best.netProfit/div, evmCalls, sizeTime.Round(time.Microsecond), fetchAfter-fetchBefore)

					hubResults[hi] = hubResult{hub: hub, best: best, evmCalls: evmCalls}
				} else {
					fmt.Fprintf(os.Stderr, "[arb4] %s evm: no profit, %d calls, %v\n",
						hub.label, evmCalls, evmTime.Round(time.Microsecond))
				}
			}(hi, hub)
		}
		hubWg.Wait()

		// Submit best result across all hubs (one tx per block max)
		sentThisBlock := false
		for _, hr := range hubResults {
			if hr.best == nil {
				continue
			}
			nodeOK := hr.best.calldata != nil && !sentThisBlock && verifyOnNode(caller, routerAddr, hr.best.calldata, hr.best.amountOut, hr.hub.label, block)
			if nodeOK {
				txHash, err := submitArb(privKey, nextNonce, routerAddr, hr.best.calldata, hr.best.gasUsed, baseFee)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[arb4] %s TX FAILED: %v\n", hr.hub.label, err)
				} else {
					fmt.Fprintf(os.Stderr, "[arb4] %s TX SENT: %s nonce=%d\n", hr.hub.label, txHash, nextNonce)
					nextNonce++
					sentThisBlock = true
					if exitOnTx {
						os.Exit(0)
					}
				}
			}
		}
		fmt.Fprintf(os.Stderr, "[arb4] block=%d total=%v\n", block, time.Since(blockT0).Round(time.Millisecond))
	}

	// Initial run
	ls.RLock()
	runBlock(ls.Block(), ls.Timestamp(), ls.BaseFee(), ls.GasLimit())
	ls.RUnlock()

	if singleBlock {
		return
	}

	fmt.Fprintf(os.Stderr, "[arb4] ready. Waiting for blocks...\n")

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
			fmt.Fprintf(os.Stderr, "[arb4] WARNING: dropped block %d\n", ls.Block())
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

		fmt.Fprintf(os.Stderr, "[arb4] block=%d dirty=%d\n", bi.block, dirtyCount)

		ls.RLock()
		runBlock(bi.block, bi.timestamp, bi.baseFee, bi.gasLimit)
		ls.RUnlock()
	}
}
