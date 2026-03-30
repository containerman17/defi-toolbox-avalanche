package main

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"bufio"
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
var USDC = common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E")
var ROUTER = router.DeployedRouter

// hubConfig holds per-hub cycle data, balance cap, and size buckets.
type hubConfig struct {
	token      common.Address
	label      string
	cycles     []Cycle
	maxBalance *uint256.Int
	sizes      []*uint256.Int
	price      *uint256.Int // price of 1 AVAX in hub token units (updated per block)
}

// Minimum quote: 1 AVAX in wei
var minQuote = uint256.NewInt(1_000_000_000_000_000_000) // 1e18

// Well-known tokens for sanity check output
var debugTokens = map[common.Address]struct {
	symbol   string
	decimals int
}{
	common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E"): {"USDC", 6},
	common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7"): {"USDT", 6},
	common.HexToAddress("0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB"): {"WETH.e", 18},
	common.HexToAddress("0x152b9d0FdC40C096757F570A51E494bd4b943E50"): {"BTC.b", 8},
	common.HexToAddress("0x2b2C81e08f1Af8835a78Bb2A90AE924ACE0eA4bE"): {"sAVAX", 18},
	common.HexToAddress("0x5c49b268c9841AFF1Cc3B0a418ff5c3442eE3F3b"): {"MAI", 18},
	common.HexToAddress("0xd586E7F844cEa2F87f50152665BCbc2C279D8d70"): {"DAI.e", 18},
	common.HexToAddress("0xA7D7079b0FEaD91F3e65f86E8915Cb59c1a4C664"): {"USDC.e", 6},
	common.HexToAddress("0x6e84a6216eA6dACC71eE8E6b0a5B7322EEbC0fDd"): {"JOE", 18},
	common.HexToAddress("0x60781C2586D68229fde47564546784ab3fACA982"): {"PNG", 18},
}

type poolEdge struct {
	pool     *pf.Pool
	tokenOut common.Address
	dir      bool // zeroForOne
}

// stage1 runs price discovery: two waves of quoting to price all tokens in WAVAX terms.
// Returns tokenPrice map and total quote count.
const numWaves = 4

func stage1(pm *formulas.PoolManager, adj map[common.Address][]poolEdge) (map[common.Address]*uint256.Int, int) {
	t0 := time.Now()

	tokenPrice := make(map[common.Address]*uint256.Int)
	tokenPrice[WAVAX] = new(uint256.Int).Set(minQuote)

	totalQuotes := 0
	for wave := 0; wave < numWaves; wave++ {
		waveQuotes := 0
		pricedBefore := len(tokenPrice)
		for token, edges := range adj {
			if _, priced := tokenPrice[token]; priced {
				continue
			}
			for _, e := range edges {
				knownPrice, priced := tokenPrice[e.tokenOut]
				if !priced {
					continue
				}
				out := pm.Quote(e.pool.Address, knownPrice, !e.dir)
				waveQuotes++
				if out.IsZero() {
					continue
				}
				existing, exists := tokenPrice[token]
				if !exists || out.Gt(existing) {
					o := new(uint256.Int).Set(&out)
					tokenPrice[token] = o
				}
			}
		}
		totalQuotes += waveQuotes
		newTokens := len(tokenPrice) - pricedBefore
		fmt.Fprintf(os.Stderr, "[arb2]   wave %d: %d quotes, +%d tokens (%d total)\n",
			wave+1, waveQuotes, newTokens, len(tokenPrice))
		if newTokens == 0 {
			break // no new tokens discovered, done
		}
	}

	fmt.Fprintf(os.Stderr, "[arb2] stage1: %d quotes, %d tokens, %v\n",
		totalQuotes, len(tokenPrice), time.Since(t0).Round(time.Microsecond))

	return tokenPrice, totalQuotes
}

// AVAX size multipliers relative to minQuote (1 AVAX).
// 0.001, 0.01, 0.1, 1, 10 AVAX → multiplied by tokenPrice to get input amount.
var sizeMultipliers = [5]struct {
	num uint64
	den uint64
}{
	{1, 1000}, // 0.001 AVAX
	{1, 100},  // 0.01 AVAX
	{1, 10},   // 0.1 AVAX
	{1, 1},    // 1 AVAX
	{10, 1},   // 10 AVAX
}

// PoolRate stores the output/input rate for a pool at a given direction and size.
type PoolRate struct {
	Rate [2][5]float64 // [dir][size] = output/input ratio
}

// stage2 quotes every pool in both directions at 5 AVAX-equivalent sizes.
// Rates are decimal-normalized: rate = (rawOut / 10^decOut) / (rawIn / 10^decIn).
// Returns a flat array indexed by pool index.
func stage2(pm *formulas.PoolManager, pools []pf.Pool, tokenPrice map[common.Address]*uint256.Int, registry *formulas.Registry, decimals map[common.Address]uint8) ([]PoolRate, int) {
	t0 := time.Now()
	rates := make([]PoolRate, len(pools))
	totalQuotes := 0

	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		_, known := registry.GetFormulaID(p.Address)
		if !known {
			continue
		}

		t0Price := tokenPrice[p.Tokens[0]]
		t1Price := tokenPrice[p.Tokens[1]]
		if t0Price == nil && t1Price == nil {
			continue
		}

		dec0 := decimalScale(decimals[p.Tokens[0]])
		dec1 := decimalScale(decimals[p.Tokens[1]])

		for s := 0; s < 5; s++ {
			if t0Price != nil {
				var amountIn uint256.Int
				amountIn.Mul(t0Price, uint256.NewInt(sizeMultipliers[s].num))
				if sizeMultipliers[s].den > 1 {
					amountIn.Div(&amountIn, uint256.NewInt(sizeMultipliers[s].den))
				}
				if !amountIn.IsZero() {
					out := pm.Quote(p.Address, &amountIn, true)
					totalQuotes++
					if !out.IsZero() {
						// dir=0: token0 in, token1 out
						// normalized rate = (rawOut/10^dec1) / (rawIn/10^dec0)
						inF := float64FromU256(&amountIn) / dec0
						outF := float64FromU256(&out) / dec1
						if inF > 0 {
							rates[i].Rate[0][s] = outF / inF
						}
					}
				}
			}
			if t1Price != nil {
				var amountIn uint256.Int
				amountIn.Mul(t1Price, uint256.NewInt(sizeMultipliers[s].num))
				if sizeMultipliers[s].den > 1 {
					amountIn.Div(&amountIn, uint256.NewInt(sizeMultipliers[s].den))
				}
				if !amountIn.IsZero() {
					out := pm.Quote(p.Address, &amountIn, false)
					totalQuotes++
					if !out.IsZero() {
						// dir=1: token1 in, token0 out
						// normalized rate = (rawOut/10^dec0) / (rawIn/10^dec1)
						inF := float64FromU256(&amountIn) / dec1
						outF := float64FromU256(&out) / dec0
						if inF > 0 {
							rates[i].Rate[1][s] = outF / inF
						}
					}
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "[arb2] stage2: %d quotes, %d pools, %v\n",
		totalQuotes, len(pools), time.Since(t0).Round(time.Microsecond))
	return rates, totalQuotes
}

// loadEnv reads key=value pairs from a .env file into os environment.
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

// float64FromU256 converts uint256 to float64, handling large values via bit shifting.
func float64FromU256(v *uint256.Int) float64 {
	if v.IsUint64() {
		return float64(v.Uint64())
	}
	b := v.ToBig()
	f, _ := new(big.Float).SetInt(b).Float64()
	return f
}

// poolIndex maps pool addresses to dense uint16 indices for array-based lookups.
type poolIndex struct {
	toIdx map[common.Address]uint16
	toAddr []common.Address
}

func newPoolIndex(pools []pf.Pool) *poolIndex {
	pi := &poolIndex{
		toIdx:  make(map[common.Address]uint16, len(pools)),
		toAddr: make([]common.Address, len(pools)),
	}
	for i := range pools {
		pi.toIdx[pools[i].Address] = uint16(i)
		pi.toAddr[i] = pools[i].Address
	}
	return pi
}

// Cycle is a compact cached cycle: pool indices + directions.
// 12 bytes per cycle (4 uint16 + 4 bool + 1 hops).
type Cycle struct {
	Hops  int
	Pools [4]uint16 // indices into poolIndex
	Dirs  [4]bool   // true = zeroForOne
}

// enumerateCycles does a one-time DFS from hub to find all 2-4 hop cycles.
func enumerateCycles(adj map[common.Address][]poolEdge, hub common.Address, maxHops int, pi *poolIndex) []Cycle {
	t0 := time.Now()

	type frame struct {
		token common.Address
		depth int
		pools [4]uint16
		dirs  [4]bool
	}

	// Dedup by canonical key
	seen := make(map[uint64]bool)

	canonicalize := func(c *Cycle) uint64 {
		minIdx := 0
		for i := 1; i < c.Hops; i++ {
			if c.Pools[i] < c.Pools[minIdx] {
				minIdx = i
			}
		}
		var key uint64
		for i := 0; i < c.Hops; i++ {
			key |= uint64(c.Pools[(minIdx+i)%c.Hops]) << (i * 16)
		}
		return key
	}

	var cycles []Cycle
	stack := make([]frame, 0, 4096)

	for _, e := range adj[hub] {
		idx, ok := pi.toIdx[e.pool.Address]
		if !ok {
			continue
		}
		f := frame{token: e.tokenOut, depth: 1}
		f.pools[0] = idx
		f.dirs[0] = e.dir
		stack = append(stack, f)
	}

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for _, e := range adj[f.token] {
			idx, ok := pi.toIdx[e.pool.Address]
			if !ok {
				continue
			}
			dup := false
			for j := 0; j < f.depth; j++ {
				if f.pools[j] == idx {
					dup = true
					break
				}
			}
			if dup {
				continue
			}

			newDepth := f.depth + 1

			if e.tokenOut == hub && newDepth >= 2 {
				c := Cycle{Hops: newDepth}
				copy(c.Pools[:], f.pools[:])
				c.Pools[f.depth] = idx
				copy(c.Dirs[:], f.dirs[:])
				c.Dirs[f.depth] = e.dir
				key := canonicalize(&c)
				if !seen[key] {
					seen[key] = true
					cycles = append(cycles, c)
				}
			} else if newDepth < maxHops {
				nf := frame{token: e.tokenOut, depth: newDepth}
				copy(nf.pools[:], f.pools[:])
				nf.pools[f.depth] = idx
				copy(nf.dirs[:], f.dirs[:])
				nf.dirs[f.depth] = e.dir
				stack = append(stack, nf)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "[arb2] enumerated %d cycles in %v\n",
		len(cycles), time.Since(t0).Round(time.Millisecond))
	return cycles
}

// buildCyclesByPool builds a reverse index: pool index → cycle indices.
func buildCyclesByPool(cycles []Cycle) map[uint16][]int32 {
	m := make(map[uint16][]int32)
	for i := range cycles {
		var seen [4]uint16
		nSeen := 0
		for j := 0; j < cycles[i].Hops; j++ {
			idx := cycles[i].Pools[j]
			dup := false
			for k := 0; k < nSeen; k++ {
				if seen[k] == idx {
					dup = true
					break
				}
			}
			if !dup {
				seen[nSeen] = idx
				nSeen++
				m[idx] = append(m[idx], int32(i))
			}
		}
	}
	return m
}

// stage3Result holds one profitable cycle candidate.
type stage3Result struct {
	cycleIdx int
	size     int
	product  float64
}

// stage3 scores cached cycles against the rate table.
// Returns top N candidates sorted by product descending.
func stage3(cycles []Cycle, rates []PoolRate, topN int) []stage3Result {
	t0 := time.Now()

	results := make([]stage3Result, 0, topN+1)
	minProduct := 0.0
	minIdx := 0

	for ci := range cycles {
		c := &cycles[ci]
		for s := 0; s < 5; s++ {
			product := 1.0
			valid := true
			for h := 0; h < c.Hops; h++ {
				dir := 0
				if !c.Dirs[h] {
					dir = 1
				}
				r := rates[c.Pools[h]].Rate[dir][s]
				if r == 0 {
					valid = false
					break
				}
				product *= r
			}
			if !valid || product == 0 {
				continue
			}

			if len(results) < topN {
				results = append(results, stage3Result{ci, s, product})
				if len(results) == topN {
					minProduct = results[0].product
					minIdx = 0
					for i, r := range results {
						if r.product < minProduct {
							minProduct = r.product
							minIdx = i
						}
					}
				}
			} else if product > minProduct {
				results[minIdx] = stage3Result{ci, s, product}
				minProduct = results[0].product
				minIdx = 0
				for i, r := range results {
					if r.product < minProduct {
						minProduct = r.product
						minIdx = i
					}
				}
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].product > results[j].product
	})

	fmt.Fprintf(os.Stderr, "[arb2] stage3: %d cycles scored, %d candidates, %v\n",
		len(cycles), len(results), time.Since(t0).Round(time.Microsecond))
	return results
}

// expandCycle expands a compact cycle into arrays suitable for EncodeSwapMulti.
func expandCycle(c *Cycle, pools []pf.Pool, hub common.Address) (
	poolAddrs []common.Address, poolTypes []int, tokenPairs []common.Address, extraDatas []string,
) {
	n := c.Hops
	poolAddrs = make([]common.Address, n)
	poolTypes = make([]int, n)
	extraDatas = make([]string, n)
	tokenPairs = make([]common.Address, n*2)

	for h := 0; h < n; h++ {
		p := &pools[c.Pools[h]]
		poolAddrs[h] = p.Address
		poolTypes[h] = p.PoolType
		extraDatas[h] = p.ExtraData
		if c.Dirs[h] {
			tokenPairs[h*2] = p.Tokens[0]
			tokenPairs[h*2+1] = p.Tokens[1]
		} else {
			tokenPairs[h*2] = p.Tokens[1]
			tokenPairs[h*2+1] = p.Tokens[0]
		}
	}
	return
}

// Per-hub size buckets for EVM verification.
var wavaxSizes = []*uint256.Int{
	uint256.NewInt(1_000_000_000_000_000),    // 0.001 AVAX
	uint256.NewInt(10_000_000_000_000_000),   // 0.01 AVAX
	uint256.NewInt(100_000_000_000_000_000),  // 0.1 AVAX
	uint256.NewInt(1_000_000_000_000_000_000), // 1 AVAX
}

var usdcSizes = []*uint256.Int{
	uint256.NewInt(10_000),      // $0.01
	uint256.NewInt(100_000),     // $0.10
	uint256.NewInt(1_000_000),   // $1
	uint256.NewInt(10_000_000),  // $10
}

// stage4Result holds the best EVM-verified opportunity.
type stage4Result struct {
	cycleIdx  int
	size      int
	amountIn  *uint256.Int
	amountOut *uint256.Int
	gasUsed   uint64
	netProfit float64 // in WAVAX wei as float64
}

// stage4 runs top candidates through local EVM via the router's swap().
// hubPrice is the price of 1 AVAX in hub token units (from stage1 tokenPrice).
// For WAVAX hub, pass 1e18. For USDC hub, pass e.g. 8_900_000 (=$8.90).
func stage4(
	candidates []stage3Result,
	cycles []Cycle,
	pools []pf.Pool,
	state *statedb.StateDB,
	cfg statedb.EVMConfig,
	hub common.Address,
	baseFee uint64,
	caller common.Address,
	maxBalance *uint256.Int,
	sizes []*uint256.Int,
	hubPrice *uint256.Int,
) *stage4Result {
	t0 := time.Now()
	totalVerified := 0

	routerAddr := router.DeployedRouter
	evmCtx := statedb.GetCachedContext(cfg)
	var best *stage4Result
	reverted, succeeded, unprofitable := 0, 0, 0

	triedCycles := make(map[int]bool)

	for _, cand := range candidates {
		if triedCycles[cand.cycleIdx] {
			continue
		}
		triedCycles[cand.cycleIdx] = true

		c := &cycles[cand.cycleIdx]

		poolAddrs, poolTypes, tokenPairs, extraDatas := expandCycle(c, pools, hub)

		for s := 0; s < len(sizes); s++ {
			amountIn := sizes[s]
			if maxBalance != nil && amountIn.Gt(maxBalance) {
				continue
			}
			calldata := pf.EncodeSwapMulti(poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, uint256.NewInt(0))

			cs := statedb.NewCallState(state)
			ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, caller, routerAddr, calldata)
			totalVerified++

			if err != nil || cs.Err() != nil || len(ret) < 32 {
				reverted++
				continue
			}

			amountOut := new(uint256.Int).SetBytes(ret[len(ret)-32:])
			if amountOut.IsZero() {
				unprofitable++
				continue
			}

			// Log every successful EVM execution
			pnlBps := int64(0)
			if !amountIn.IsZero() {
				diff := new(uint256.Int)
				if amountOut.Gt(amountIn) {
					diff.Sub(amountOut, amountIn)
					pnlBps = int64(diff.Float64() / amountIn.Float64() * 10000)
				} else {
					diff.Sub(amountIn, amountOut)
					pnlBps = -int64(diff.Float64() / amountIn.Float64() * 10000)
				}
			}
			if succeeded < 10 {
				fmt.Fprintf(os.Stderr, "[arb2]   evm ok: hops=%d size=%d in=%s out=%s pnl=%+dbps gas=%d\n",
					c.Hops, s, amountIn.Dec(), amountOut.Dec(), pnlBps, gasUsed)
			}

			if !amountOut.Gt(amountIn) {
				unprofitable++
				succeeded++
				continue
			}

			grossProfit := new(uint256.Int).Sub(amountOut, amountIn)
			// Convert gas cost (AVAX wei) to hub token units:
			// gasCostInToken = gasUsed * baseFee * hubPrice / 1e18
			gasCostAVAX := new(uint256.Int).Mul(uint256.NewInt(gasUsed), uint256.NewInt(baseFee))
			gasCost := new(uint256.Int).Mul(gasCostAVAX, hubPrice)
			gasCost.Div(gasCost, uint256.NewInt(1_000_000_000_000_000_000))
			if !grossProfit.Gt(gasCost) {
				unprofitable++
				continue
			}
			succeeded++

			netProfit := new(uint256.Int).Sub(grossProfit, gasCost)
			netF := float64FromU256(netProfit)

			if best == nil || netF > best.netProfit {
				best = &stage4Result{
					cycleIdx:  cand.cycleIdx,
					size:      s,
					amountIn:  new(uint256.Int).Set(amountIn),
					amountOut: new(uint256.Int).Set(amountOut),
					gasUsed:   gasUsed,
					netProfit: netF,
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "[arb2] stage4: %d verified (ok=%d unprof=%d revert=%d), fetches=%d, %v\n",
		totalVerified, succeeded, unprofitable, reverted, statedb.FetchCount.Load(), time.Since(t0).Round(time.Microsecond))
	return best
}

func printPrices(tokenPrice map[common.Address]*uint256.Int) {
	usdcAddr := common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E")
	usdcPrice, hasUSDC := tokenPrice[usdcAddr]
	if hasUSDC {
		avaxInUSDC := float64(usdcPrice.Uint64()) / 1e6
		fmt.Fprintf(os.Stderr, "[arb2] 1 AVAX ≈ $%.2f  |  ", avaxInUSDC)
	}

	for addr, info := range debugTokens {
		price, ok := tokenPrice[addr]
		if !ok {
			continue
		}
		if !hasUSDC {
			continue
		}
		bf := new(big.Float).SetInt(price.ToBig())
		tokensPerAvax, _ := bf.Quo(bf, new(big.Float).SetFloat64(math.Pow(10, float64(info.decimals)))).Float64()
		if tokensPerAvax <= 0 {
			continue
		}
		avaxInUSDCf := float64(usdcPrice.Uint64()) / 1e6
		usd := avaxInUSDCf / tokensPerAvax
		fmt.Fprintf(os.Stderr, "%s=$%.2f ", info.symbol, usd)
	}
	fmt.Fprintf(os.Stderr, "\n")
}

const RPC = "http://localhost:9650/ext/bc/C/rpc"

var chainID = big.NewInt(43114)

// rpcCall sends a JSON-RPC request and returns the raw response body.
func rpcCall(body interface{}) ([]byte, error) {
	b, _ := json.Marshal(body)
	resp, err := http.Post(RPC, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// rpcAllowance checks ERC-20 allowance(owner, spender) via eth_call.
func rpcAllowance(token, owner, spender common.Address) *uint256.Int {
	// allowance(address,address) = 0xdd62ed3e
	data := "0xdd62ed3e" +
		"000000000000000000000000" + hex.EncodeToString(owner[:]) +
		"000000000000000000000000" + hex.EncodeToString(spender[:])
	resp, err := rpcCall(map[string]interface{}{
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

// rpcNonce fetches the current nonce for an address.
func rpcNonce(addr common.Address) uint64 {
	resp, err := rpcCall(map[string]interface{}{
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

// rpcBaseFee fetches the current base fee from the latest block.
func rpcBaseFee() uint64 {
	resp, err := rpcCall(map[string]interface{}{
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

// ensureApprovals checks and issues approve() txs for each hub token if needed.
// Approves for 1000x the current balance.
func ensureApprovals(key *ecdsa.PrivateKey, caller common.Address, hubs []hubConfig) {
	routerAddr := router.DeployedRouter
	signer := types.NewLondonSigner(chainID)
	nonce := rpcNonce(caller)
	baseFee := rpcBaseFee()

	for _, hub := range hubs {
		allowance := rpcAllowance(hub.token, caller, routerAddr)
		needed := new(uint256.Int).Mul(hub.maxBalance, uint256.NewInt(1000))

		if allowance.Gt(needed) || allowance.Eq(needed) {
			fmt.Fprintf(os.Stderr, "[arb2] %s allowance OK (%s)\n", hub.label, allowance.Dec())
			continue
		}

		fmt.Fprintf(os.Stderr, "[arb2] %s allowance %s < %s, approving...\n",
			hub.label, allowance.Dec(), needed.Dec())

		// approve(address,uint256) = 0x095ea7b3
		data := make([]byte, 68)
		data[0], data[1], data[2], data[3] = 0x09, 0x5e, 0xa7, 0xb3
		copy(data[4+12:4+32], routerAddr[:])
		neededBytes := needed.Bytes32()
		copy(data[36:68], neededBytes[:])

		maxFee := new(big.Int).SetUint64(baseFee*2 + 1_000_000_000)
		tokenAddr := hub.token
		tx := types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: big.NewInt(0),
			GasFeeCap: maxFee,
			Gas:       60_000,
			To:        &tokenAddr,
			Value:     big.NewInt(0),
			Data:      data,
		})
		signedTx, err := types.SignTx(tx, signer, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb2] sign approve %s: %v\n", hub.label, err)
			continue
		}
		rawTx, _ := signedTx.MarshalBinary()
		rawHex := "0x" + hex.EncodeToString(rawTx)
		resp, err := rpcCall(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1, "method": "eth_sendRawTransaction",
			"params": []string{rawHex},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb2] send approve %s: %v\n", hub.label, err)
			continue
		}
		var rpcResp struct {
			Result string
			Error  *struct{ Message string }
		}
		json.Unmarshal(resp, &rpcResp)
		if rpcResp.Error != nil {
			fmt.Fprintf(os.Stderr, "[arb2] approve %s failed: %s\n", hub.label, rpcResp.Error.Message)
			continue
		}
		fmt.Fprintf(os.Stderr, "[arb2] %s approved, tx=%s nonce=%d\n",
			hub.label, signedTx.Hash().Hex()[:14], nonce)
		nonce++
	}
}

// readDecimals calls decimals() on a token via local EVM. Returns 18 as default.
func readDecimals(state *statedb.StateDB, evmCtx *statedb.CachedContext, token common.Address) uint8 {
	// decimals() selector = 0x313ce567
	calldata := []byte{0x31, 0x3c, 0xe5, 0x67}
	cs := statedb.NewCallState(state)
	ret, _, err := evmCtx.ExecuteWithCallState(cs, common.Address{}, token, calldata)
	if err == nil && len(ret) >= 32 {
		d := ret[31]
		if d <= 77 { // sanity check
			return d
		}
	}
	return 18
}

// buildDecimalsMap reads decimals for all tokens in the adjacency map.
func buildDecimalsMap(state *statedb.StateDB, evmCtx *statedb.CachedContext, adj map[common.Address][]poolEdge) map[common.Address]uint8 {
	t0 := time.Now()
	decimals := make(map[common.Address]uint8, len(adj))
	for token := range adj {
		decimals[token] = readDecimals(state, evmCtx, token)
	}
	fmt.Fprintf(os.Stderr, "[arb2] decimals: %d tokens, %v\n",
		len(decimals), time.Since(t0).Round(time.Millisecond))
	return decimals
}

// decimalScale returns 10^decimals as float64.
func decimalScale(d uint8) float64 {
	return math.Pow(10, float64(d))
}

// readBalance calls balanceOf(owner) on token via local EVM.
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

func main() {
	stateServerURL := "ws://localhost:7449/live"
	poolLimit := 4000
	singleBlock := false

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
	}

	// Load private key for EVM caller
	loadEnv()
	privKeyHex := strings.TrimPrefix(os.Getenv("ARB_PRIVATE_KEY"), "0x")
	if privKeyHex == "" {
		fmt.Fprintf(os.Stderr, "[arb2] WARNING: ARB_PRIVATE_KEY not set, stage4 will fail\n")
	}
	var caller common.Address
	var privKey *ecdsa.PrivateKey
	if privKeyHex != "" {
		key, err := crypto.HexToECDSA(privKeyHex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb2] bad private key: %v\n", err)
			os.Exit(1)
		}
		privKey = key
		caller = crypto.PubkeyToAddress(key.PublicKey)
		fmt.Fprintf(os.Stderr, "[arb2] caller: %s\n", caller.Hex())
	}

	// Load registry + pools
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)
	fmt.Fprintf(os.Stderr, "[arb2] loaded %d pools\n", len(pools))

	// Connect to state server
	ls, err := statedb.Connect(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb2] connect failed: %v\n", err)
		os.Exit(1)
	}
	state := ls.State()

	// Hub tokens for arb cycles
	hubTokens := []struct {
		addr     common.Address
		label    string
		decimals int
		sizes    []*uint256.Int
	}{
		{WAVAX, "WAVAX", 18, wavaxSizes},
		{USDC, "USDC", 6, usdcSizes},
	}

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

	// Build adjacency: token → []pool edges
	adj := make(map[common.Address][]poolEdge)
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		_, known := registry.GetFormulaID(p.Address)
		if !known {
			continue
		}
		t0, t1 := p.Tokens[0], p.Tokens[1]
		adj[t0] = append(adj[t0], poolEdge{p, t1, true})
		adj[t1] = append(adj[t1], poolEdge{p, t0, false})
	}
	fmt.Fprintf(os.Stderr, "[arb2] adjacency: %d tokens\n", len(adj))

	// Warmup: build all pool quoters + read token decimals
	tw := time.Now()
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	warmupCfg := statedb.EVMConfig{
		BlockNumber: ls.Block(),
		Timestamp:   ls.Timestamp(),
		ChainID:     43114,
		BaseFee:     ls.BaseFee(),
		GasLimit:    ls.GasLimit(),
	}
	warmupCtx := statedb.GetCachedContext(warmupCfg)
	tokenDecimals := buildDecimalsMap(state, warmupCtx, adj)
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[arb2] warmup: %v\n", time.Since(tw).Round(time.Millisecond))

	maxHops := 4
	topN := 100

	// One-time cycle enumeration + balance read per hub
	pi := newPoolIndex(pools)
	var hubs []hubConfig

	ls.RLock()
	initCfg := statedb.EVMConfig{
		BlockNumber: ls.Block(),
		Timestamp:   ls.Timestamp(),
		ChainID:     43114,
		BaseFee:     ls.BaseFee(),
		GasLimit:    ls.GasLimit(),
	}
	evmCtx := statedb.GetCachedContext(initCfg)
	for _, ht := range hubTokens {
		cycles := enumerateCycles(adj, ht.addr, maxHops, pi)
		if len(cycles) == 0 {
			continue
		}
		bal := readBalance(state, evmCtx, caller, ht.addr)
		divisor := math.Pow(10, float64(ht.decimals))
		fmt.Fprintf(os.Stderr, "[arb2] hub %s: %d cycles, balance=%.4f\n",
			ht.label, len(cycles), bal.Float64()/divisor)
		hubs = append(hubs, hubConfig{
			token:      ht.addr,
			label:      ht.label,
			cycles:     cycles,
			maxBalance: bal,
			sizes:      ht.sizes,
		})
	}
	ls.RUnlock()

	// Check and issue approvals if needed
	if privKey != nil {
		ensureApprovals(privKey, caller, hubs)
	}

	// Initial stage 1 + 2 + 3 + 4
	ls.RLock()
	tokenPrice, _ := stage1(pm, adj)
	rateTable, _ := stage2(pm, pools, tokenPrice, registry, tokenDecimals)
	cfg := statedb.EVMConfig{
		BlockNumber: ls.Block(),
		Timestamp:   ls.Timestamp(),
		ChainID:     43114,
		BaseFee:     ls.BaseFee(),
		GasLimit:    ls.GasLimit(),
	}
	for i := range hubs {
		if p, ok := tokenPrice[hubs[i].token]; ok {
			hubs[i].price = p
		} else {
			hubs[i].price = uint256.NewInt(1_000_000_000_000_000_000) // fallback: 1:1
		}
		s3results := stage3(hubs[i].cycles, rateTable, topN)
		best := stage4(s3results, hubs[i].cycles, pools, state, cfg, hubs[i].token, ls.BaseFee(), caller, hubs[i].maxBalance, hubs[i].sizes, hubs[i].price)
		if best != nil {
			fmt.Fprintf(os.Stderr, "[arb2] %s PROFIT: net=%.0f, in=%s out=%s gas=%d\n",
				hubs[i].label, best.netProfit, best.amountIn.Dec(), best.amountOut.Dec(), best.gasUsed)
		}
	}
	ls.RUnlock()
	printPrices(tokenPrice)

	if singleBlock {
		return
	}
	fmt.Fprintf(os.Stderr, "[arb2] ready. Waiting for blocks...\n")

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
			fmt.Fprintf(os.Stderr, "[arb2] WARNING: dropped block %d\n", ls.Block())
		}
	})

	for bi := range blockCh {
		pm.SetBlockTimestamp(bi.timestamp)

		// Invalidate dirty pools
		dirtySet := make(map[common.Address]bool)
		for _, entry := range bi.entries {
			key := entry[0]
			if strings.HasPrefix(key, "s:") {
				parts := strings.SplitN(key, ":", 3)
				if len(parts) == 3 {
					addr := common.HexToAddress(parts[1])
					slot := common.HexToHash(parts[2])
					poolAddr := pm.InvalidateBySlot(addr, slot)
					if poolAddr != (common.Address{}) {
						dirtySet[poolAddr] = true
					}
				}
			}
		}

		cfg := statedb.EVMConfig{
			BlockNumber: bi.block,
			Timestamp:   bi.timestamp,
			ChainID:     43114,
			BaseFee:     bi.baseFee,
			GasLimit:    bi.gasLimit,
		}

		ls.RLock()
		tokenPrice, s1q := stage1(pm, adj)
		rateTable, s2q := stage2(pm, pools, tokenPrice, registry, tokenDecimals)
		for i := range hubs {
			if p, ok := tokenPrice[hubs[i].token]; ok {
				hubs[i].price = p
			}
			s3results := stage3(hubs[i].cycles, rateTable, topN)
			best := stage4(s3results, hubs[i].cycles, pools, state, cfg, hubs[i].token, bi.baseFee, caller, hubs[i].maxBalance, hubs[i].sizes, hubs[i].price)
			if best != nil {
				fmt.Fprintf(os.Stderr, "[arb2] %s PROFIT: net=%.0f, in=%s out=%s gas=%d\n",
					hubs[i].label, best.netProfit, best.amountIn.Dec(), best.amountOut.Dec(), best.gasUsed)
			}
		}
		ls.RUnlock()

		fmt.Fprintf(os.Stderr, "[arb2] block=%d dirty=%d s1=%d s2=%d  ", bi.block, len(dirtySet), s1q, s2q)
		printPrices(tokenPrice)
	}
}
