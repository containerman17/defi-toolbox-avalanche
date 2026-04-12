// formula-accuracy — Compares formula quotes against EVM ground truth.
//
// For each pool in the registry, quotes via formula and via EVM (router
// debugSwapSingle), reports match/mismatch/overquote stats per pool type.
//
// Usage:
//
//	go run ./benchmarks/formula-accuracy/ [--rpc ws://...] [--limit 4000]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	router "defi-toolbox/contracts"
	"defi-toolbox/formulas"
	lc "defi-toolbox/lightclient"
	"defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
)

var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
var debugEVM = true

const defaultPoolLimit = 4000

var typeNames = map[int]string{
	0:  "uniswap_v3",
	1:  "algebra",
	2:  "lfj_v1",
	3:  "lfj_v2",
	4:  "dodo",
	5:  "woofi_v2",
	6:  "balancer_v3",
	7:  "pharaoh_v1",
	8:  "v2",
	9:  "uniswap_v4",
	10: "erc4626",
	11: "balancer_v3_buffered",
	12: "wombat",
	13: "platypus",
	14: "woopp_v2",
	15: "transfer_from",
	16: "balancer_v2",
	17: "cavalre",
	18: "kyber_dmm",
	19: "synapse",
	20: "trident",
}

type typeStats struct {
	Quotes     int
	Match      int
	Overquote  int // formula > EVM — never acceptable
	Underquote int // formula < EVM — safe, missed opportunity
	Zero       int // both returned zero
}

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	poolLimit := flag.Int("limit", defaultPoolLimit, "pool limit (default 4000, 0 = all)")
	flag.Parse()

	pool, err := lc.NewRPCPool(*rpcURL, *concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	fetcher := lc.NewBlockFetcher(pool)
	vs := lc.NewVersionedState()

	headBlock, headTimestamp, baseFee, err := fetchHead(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "head: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[bench] head block: %d\n", headBlock)

	chainCfg, err := lc.FetchChainConfig(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain config: %v\n", err)
		os.Exit(1)
	}

	pools := poolcollector.EmbeddedPools(*poolLimit)
	registry := formulas.LoadEmbeddedRegistry()
	tokenAmounts := formulas.LoadEmbeddedTokenAmounts()

	// Register V4 pools.
	for _, p := range pools {
		if p.PoolType != 9 || p.ExtraData == "" {
			continue
		}
		var poolIdHex string
		var fee uint32
		var tickSpacing int32
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
			}
		}
		if poolIdHex != "" && tickSpacing != 0 {
			var poolId [32]byte
			copy(poolId[:], common.FromHex(poolIdHex))
			formulas.RegisterV4Pool(strings.ToLower(p.Address.Hex()), poolId, tickSpacing, fee, 0, common.Address{})
		}
	}

	miss := fetcher.MissCallbacks(vs)
	sv := lc.NewStateView(vs, headBlock, miss)

	// Apply token overrides so EVM swap calls work (balance + allowance for sender).
	allTokens := make([]common.Address, 0)
	tokenSeen := make(map[common.Address]bool)
	for _, p := range pools {
		for _, t := range p.Tokens {
			if !tokenSeen[t] {
				tokenSeen[t] = true
				allTokens = append(allTokens, t)
			}
		}
	}
	router.ApplyTokenOverrides(sv, DUMMY_SENDER, router.DeployedRouter, allTokens)
	fmt.Fprintf(os.Stderr, "[bench] applied token overrides for %d tokens\n", len(allTokens))

	// Build PoolManager.
	reader := func(addr common.Address, slot common.Hash) common.Hash {
		return sv.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, reader)
	pm.SetBlockTimestamp(headTimestamp)
	for _, p := range pools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens...)
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}

	// Filter to registered pools with known token amounts.
	type quoteJob struct {
		pool     pathfinder.Pool
		tokenIn  common.Address
		tokenOut common.Address
		amount   *uint256.Int
	}
	var jobs []quoteJob
	jobPools := make(map[common.Address]struct{})
	for _, p := range pools {
		if _, known := registry.GetFormulaID(p.Address); !known {
			continue
		}
		if len(p.Tokens) < 2 {
			continue
		}
		addedJob := false
		for ti, tok := range p.Tokens {
			if amt, ok := tokenAmounts[tok]; ok {
				for tj := range p.Tokens {
					if ti != tj {
						jobs = append(jobs, quoteJob{p, tok, p.Tokens[tj], amt})
						addedJob = true
					}
				}
				break // one direction per pool is enough
			}
		}
		if addedJob {
			jobPools[p.Address] = struct{}{}
		}
	}
	fmt.Fprintf(os.Stderr, "[bench] %d quote jobs across %d eligible pools (%d input pools)\n",
		len(jobs), len(jobPools), len(pools))

	// Run quotes.
	byType := make(map[int]*typeStats)
	t0 := time.Now()
	overquotes := 0

	for i, job := range jobs {
		// EVM quote.
		evmOut := evmQuote(sv, headTimestamp, baseFee, chainCfg,
			job.pool.Address, job.pool.PoolType, job.tokenIn, job.tokenOut, job.amount, job.pool.ExtraData)

		// Formula quote.
		fOut := pm.Quote(job.pool.Address, job.amount, job.tokenIn, job.tokenOut)

		st := byType[job.pool.PoolType]
		if st == nil {
			st = &typeStats{}
			byType[job.pool.PoolType] = st
		}
		st.Quotes++

		evmIsZero := evmOut == nil || evmOut.IsZero()
		fIsZero := fOut.IsZero()

		if evmIsZero && fIsZero {
			st.Zero++
			st.Match++
		} else if evmIsZero || fIsZero {
			if !fIsZero {
				st.Overquote++
				overquotes++
			} else {
				st.Underquote++
			}
		} else if fOut.Eq(evmOut) {
			st.Match++
		} else if fOut.Gt(evmOut) {
			st.Overquote++
			overquotes++
			fmt.Printf("OVERQUOTE %s type=%d %s→%s evm=%s formula=%s\n",
				job.pool.Address.Hex()[:10], job.pool.PoolType,
				job.tokenIn.Hex()[:10], job.tokenOut.Hex()[:10],
				evmOut.Dec(), fOut.Dec())
		} else {
			st.Underquote++
		}

		if (i+1)%500 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d (overquotes=%d)\n", i+1, len(jobs), overquotes)
		}
	}

	elapsed := time.Since(t0)
	fmt.Fprintf(os.Stderr, "\n[bench] done in %v\n\n", elapsed.Round(time.Millisecond))

	// Print results.
	totalQuotes, totalMatch, totalOver, totalUnder, totalZero := 0, 0, 0, 0, 0
	fmt.Printf("%-15s %6s %6s %6s %6s %6s\n", "type", "quotes", "match", "over", "under", "zero")
	fmt.Printf("%-15s %6s %6s %6s %6s %6s\n", "----", "------", "-----", "----", "-----", "----")
	poolTypes := make([]int, 0, len(byType))
	for pt := range byType {
		poolTypes = append(poolTypes, pt)
	}
	sort.Ints(poolTypes)
	for _, pt := range poolTypes {
		st := byType[pt]
		if st == nil {
			continue
		}
		name := typeNames[pt]
		if name == "" {
			name = fmt.Sprintf("type_%d", pt)
		}
		fmt.Printf("%-15s %6d %6d %6d %6d %6d\n", name, st.Quotes, st.Match, st.Overquote, st.Underquote, st.Zero)
		totalQuotes += st.Quotes
		totalMatch += st.Match
		totalOver += st.Overquote
		totalUnder += st.Underquote
		totalZero += st.Zero
	}
	fmt.Printf("%-15s %6d %6d %6d %6d %6d\n", "TOTAL", totalQuotes, totalMatch, totalOver, totalUnder, totalZero)
	fmt.Printf("\ninput_pools=%d eligible_pools=%d quote_jobs=%d\n", len(pools), len(jobPools), totalQuotes)
	fmt.Printf("exact=%.2f%% over=%.2f%% under=%.2f%% zero=%.2f%% non_zero=%.2f%%\n",
		pct(totalMatch, totalQuotes), pct(totalOver, totalQuotes), pct(totalUnder, totalQuotes),
		pct(totalZero, totalQuotes), pct(totalQuotes-totalZero, totalQuotes))

	if totalQuotes != len(jobs) {
		fmt.Fprintf(os.Stderr, "\nWARNING: summarized %d jobs, but ran %d jobs\n", totalQuotes, len(jobs))
	}

	if totalOver > 0 {
		fmt.Fprintf(os.Stderr, "\nWARNING: %d overquotes detected!\n", totalOver)
	}
}

func pct(numer, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return 100 * float64(numer) / float64(denom)
}

func evmQuote(sv *lc.StateView, timestamp uint64, baseFee *big.Int,
	chainCfg *params.ChainConfig, poolAddr common.Address,
	poolType int, tokenIn, tokenOut common.Address, amount *uint256.Int, extraData string,
) *uint256.Int {
	calldata := pathfinder.EncodeSwapSingleWithExtra(poolAddr, poolType, tokenIn, tokenOut, amount, extraData)
	ret, _, err := lc.EVMCallOn(sv, timestamp, baseFee, chainCfg,
		DUMMY_SENDER, router.DeployedRouter, calldata)
	if err != nil {
		if debugEVM {
			fmt.Fprintf(os.Stderr, "  EVM err: %v (pool=%s)\n", err, poolAddr.Hex()[:10])
		}
		return uint256.NewInt(0)
	}
	if len(ret) < 32 {
		return uint256.NewInt(0)
	}
	var out uint256.Int
	out.SetBytes(ret[:32])
	if out.Bytes32()[0]&0x80 != 0 {
		return uint256.NewInt(0)
	}
	return &out
}

func fetchHead(pool *lc.RPCPool) (block uint64, timestamp uint64, baseFee *big.Int, err error) {
	raw, err := pool.Call("eth_getBlockByNumber", []interface{}{"latest", false})
	if err != nil {
		return 0, 0, nil, err
	}
	var hdr struct {
		Number    string `json:"number"`
		Timestamp string `json:"timestamp"`
		BaseFee   string `json:"baseFeePerGas"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return 0, 0, nil, err
	}
	fmt.Sscanf(hdr.Number, "0x%x", &block)
	fmt.Sscanf(hdr.Timestamp, "0x%x", &timestamp)
	baseFee = new(big.Int)
	baseFee.SetString(strings.TrimPrefix(hdr.BaseFee, "0x"), 16)
	return
}
