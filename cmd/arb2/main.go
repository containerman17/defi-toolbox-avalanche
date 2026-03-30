package main

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")

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

	// Initial stage 1
	ls.RLock()
	tokenPrice, _ := stage1(pm, adj)
	ls.RUnlock()
	printPrices(tokenPrice)

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

		ls.RLock()
		tokenPrice, totalQuotes := stage1(pm, adj)
		ls.RUnlock()

		fmt.Fprintf(os.Stderr, "[arb2] block=%d dirty=%d quotes=%d  ", bi.block, len(dirtySet), totalQuotes)
		printPrices(tokenPrice)
	}
}
