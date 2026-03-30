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

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
var ROUTER = router.DeployedRouter

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
// Returns rate table indexed by pool address.
// stage2 quotes every pool in both directions at 5 AVAX-equivalent sizes.
// Returns a flat array indexed by pool index.
func stage2(pm *formulas.PoolManager, pools []pf.Pool, tokenPrice map[common.Address]*uint256.Int, registry *formulas.Registry) ([]PoolRate, int) {
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
						inF := float64FromU256(&amountIn)
						outF := float64FromU256(&out)
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
						inF := float64FromU256(&amountIn)
						outF := float64FromU256(&out)
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

// sizeBuckets are the 5 WAVAX input amounts for EVM verification.
var sizeBuckets = [5]*uint256.Int{
	uint256.NewInt(1_000_000_000_000_000),                                              // 0.001 AVAX
	uint256.NewInt(10_000_000_000_000_000),                                             // 0.01 AVAX
	uint256.NewInt(100_000_000_000_000_000),                                            // 0.1 AVAX
	uint256.NewInt(1_000_000_000_000_000_000),                                          // 1 AVAX
	new(uint256.Int).Mul(uint256.NewInt(10), uint256.NewInt(1_000_000_000_000_000_000)), // 10 AVAX
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
func stage4(
	candidates []stage3Result,
	cycles []Cycle,
	pools []pf.Pool,
	state *statedb.StateDB,
	cfg statedb.EVMConfig,
	hub common.Address,
	baseFee uint64,
	caller common.Address,
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

		for s := 0; s < 5; s++ {
			amountIn := sizeBuckets[s]
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
			gasCost := new(uint256.Int).Mul(uint256.NewInt(gasUsed), uint256.NewInt(baseFee))
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

func main() {
	stateServerURL := "ws://localhost:7449/live"
	poolLimit := 4000

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--pool-limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
	}

	// Load private key for EVM caller
	loadEnv()
	privKeyHex := strings.TrimPrefix(os.Getenv("ARB_PRIVATE_KEY"), "0x")
	if privKeyHex == "" {
		fmt.Fprintf(os.Stderr, "[arb2] WARNING: ARB_PRIVATE_KEY not set, stage4 will fail\n")
	}
	var caller common.Address
	if privKeyHex != "" {
		key, err := crypto.HexToECDSA(privKeyHex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb2] bad private key: %v\n", err)
			os.Exit(1)
		}
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

	// Warmup: build all pool quoters
	tw := time.Now()
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[arb2] warmup: %v\n", time.Since(tw).Round(time.Millisecond))

	maxHops := 4
	topN := 100

	// One-time cycle enumeration
	pi := newPoolIndex(pools)
	cycles := enumerateCycles(adj, WAVAX, maxHops, pi)
	_ = buildCyclesByPool(cycles) // will use later for dirty-pool filtering

	// Initial stage 1 + 2 + 3 + 4
	ls.RLock()
	tokenPrice, _ := stage1(pm, adj)
	rateTable, _ := stage2(pm, pools, tokenPrice, registry)
	s3results := stage3(cycles, rateTable, topN)
	cfg := statedb.EVMConfig{
		BlockNumber: ls.Block(),
		Timestamp:   ls.Timestamp(),
		ChainID:     43114,
		BaseFee:     ls.BaseFee(),
		GasLimit:    ls.GasLimit(),
	}
	best := stage4(s3results, cycles, pools, state, cfg, WAVAX, ls.BaseFee(), caller)
	ls.RUnlock()
	printPrices(tokenPrice)
	if best != nil {
		fmt.Fprintf(os.Stderr, "[arb2] PROFIT: net=%.0f wei, in=%s out=%s gas=%d\n",
			best.netProfit, best.amountIn.Dec(), best.amountOut.Dec(), best.gasUsed)
	} else if len(s3results) > 0 {
		c := &cycles[s3results[0].cycleIdx]
		fmt.Fprintf(os.Stderr, "[arb2] no profit, top candidate: product=%.6f hops=%d size=%d\n",
			s3results[0].product, c.Hops, s3results[0].size)
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
		rateTable, s2q := stage2(pm, pools, tokenPrice, registry)
		s3results := stage3(cycles, rateTable, topN)
		best := stage4(s3results, cycles, pools, state, cfg, WAVAX, bi.baseFee, caller)
		ls.RUnlock()

		fmt.Fprintf(os.Stderr, "[arb2] block=%d dirty=%d s1=%d s2=%d  ", bi.block, len(dirtySet), s1q, s2q)
		printPrices(tokenPrice)
		if best != nil {
			fmt.Fprintf(os.Stderr, "  PROFIT: net=%.0f wei, in=%s out=%s gas=%d\n",
				best.netProfit, best.amountIn.Dec(), best.amountOut.Dec(), best.gasUsed)
		}
	}
}
