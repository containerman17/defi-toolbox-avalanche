// discover — Formula registry discovery using the light client.
//
// For each pool not in registry.txt, probes formula vs EVM with up to 10
// amounts. If all amounts match exactly → assigns the formula ID.
// If any disagree → assigns -1 (broken formula).
// Existing registry entries are never overwritten (append-only).
//
// Usage:
//   go run ./tools/discover/ [--write] [--rpc ws://...] [--limit 5000]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"runtime"
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

var formulaMap = map[int]int{
	0: 2, // uniswap_v3, pharaoh_v3 → V3
	1: 4, // algebra → Algebra
	2: 0, // lfj_v1 → V2 constant product
	3: 3, // lfj_v2 → LFJ V2
	4: 5, // dodo → DODO
	7: 1, // pharaoh_v1 → Pharaoh V1
	8: 0, // v2 family → V2 constant product
	9: 6, // uniswap_v4 → V4 (singleton PoolManager)
	6: 7, // balancer_v3 → Balancer V3
}

var formulaNames = map[int]string{
	0: "V2 constant product", 1: "Pharaoh V1", 2: "V3 tick-walking",
	3: "LFJ V2 Liquidity Book", 4: "Algebra V1 Integral", 5: "DODO PMM",
	6: "V4 PoolManager", 7: "Balancer V3", -1: "invalid (formula mismatch)",
}

var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	poolLimit := flag.Int("limit", 0, "pool limit (0 = all)")
	doWrite := flag.Bool("write", false, "write results to registry.txt")
	flag.Parse()

	pool, err := lc.NewRPCPool(*rpcURL, *concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	fetcher := lc.NewBlockFetcher(pool)
	vs := lc.NewVersionedState()

	// Get current head block for state reads.
	headBlock, headTimestamp, baseFee, err := fetchHead(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "head: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[discover] head block: %d\n", headBlock)

	chainCfg, err := lc.FetchChainConfig(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain config: %v\n", err)
		os.Exit(1)
	}

	pools := poolcollector.EmbeddedPools(*poolLimit)
	registry := formulas.LoadEmbeddedRegistry()
	tokenAmounts := formulas.LoadEmbeddedTokenAmounts()
	fmt.Fprintf(os.Stderr, "[discover] %d pools, %d token amounts\n", len(pools), len(tokenAmounts))

	// Register V4 pools from ExtraData.
	v4Count := 0
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
			v4Count++
		}
	}
	if v4Count > 0 {
		fmt.Fprintf(os.Stderr, "[discover] registered %d V4 pools\n", v4Count)
	}

	// Build miss callbacks for state fetching.
	miss := fetcher.MissCallbacks(vs)

	// Build PoolManager for formula quoting.
	sv := lc.NewStateView(vs, headBlock, miss)
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

	// Filter to formula-eligible pools not already in registry.
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
	fmt.Fprintf(os.Stderr, "[discover] %d candidates (%d skipped, in registry)\n", len(candidates), skipped)

	// Discovery pass.
	fmt.Fprintf(os.Stderr, "[discover] verifying (formula vs EVM, up to 10 amounts)...\n")
	t0 := time.Now()

	var results []fillResult
	matched, mismatched := 0, 0

	for i, c := range candidates {
		p := c.pool
		bestFormulaID := -1

		for _, dir := range [][2]int{{0, 1}, {1, 0}} {
			tokenIn := p.Tokens[dir[0]]
			tokenOut := p.Tokens[dir[1]]

			baseAmount, hasAmount := tokenAmounts[tokenIn]
			if !hasAmount {
				continue
			}

			// EVM quote via router.
			evmOut := evmQuote(vs, headBlock, headTimestamp, baseFee, miss, chainCfg,
				p.Address, p.PoolType, tokenIn, tokenOut, baseAmount, p.ExtraData)

			// Formula quote.
			pq := pm.BuildQuoterForFormulaID(p.Address, c.formulaID)
			fOut := formulaQuote(pq, baseAmount, tokenIn, tokenOut)

			if !amountsEqual(evmOut, fOut) {
				continue
			}

			// Multi-amount verification.
			allMatch := true
			for mult := uint64(1); mult <= 10; mult++ {
				testAmount := new(uint256.Int).Mul(baseAmount, uint256.NewInt(mult))
				evm := evmQuote(vs, headBlock, headTimestamp, baseFee, miss, chainCfg,
					p.Address, p.PoolType, tokenIn, tokenOut, testAmount, p.ExtraData)
				pq = pm.BuildQuoterForFormulaID(p.Address, c.formulaID)
				f := formulaQuote(pq, testAmount, tokenIn, tokenOut)
				if !amountsEqual(evm, f) {
					allMatch = false
					break
				}
			}

			if allMatch {
				bestFormulaID = c.formulaID
				break
			}
		}

		results = append(results, fillResult{addr: p.Address, formulaID: bestFormulaID})
		if bestFormulaID >= 0 {
			matched++
		} else {
			mismatched++
		}

		if (i+1)%100 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d (matched=%d, failed=%d)\n", i+1, len(candidates), matched, mismatched)
		}
	}

	fmt.Fprintf(os.Stderr, "[discover] done in %v\n\n", time.Since(t0).Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "Stats:\n")
	fmt.Fprintf(os.Stderr, "  Formula match: %d\n", matched)
	fmt.Fprintf(os.Stderr, "  Formula fail:  %d (assigned -1)\n", mismatched)
	fmt.Fprintf(os.Stderr, "  Skipped:       %d (already in registry)\n", skipped)

	byFormula := make(map[int]int)
	for _, r := range results {
		byFormula[r.formulaID]++
	}
	fmt.Fprintf(os.Stderr, "\nBy formula:\n")
	for _, id := range []int{0, 1, 2, 3, 4, 5, 6, 7, -1} {
		if cnt := byFormula[id]; cnt > 0 {
			fmt.Fprintf(os.Stderr, "  %s: %d\n", formulaNames[id], cnt)
		}
	}

	if *doWrite {
		writeRegistry(results)
	} else {
		fmt.Fprintf(os.Stderr, "\nDry run — pass --write to append (%d new results)\n", len(results))
	}
}

func evmQuote(vs *lc.VersionedState, block, timestamp uint64, baseFee *big.Int,
	miss lc.MissCallbacks, chainCfg *params.ChainConfig, poolAddr common.Address,
	poolType int, tokenIn, tokenOut common.Address, amount *uint256.Int, extraData string,
) *uint256.Int {
	calldata := pathfinder.EncodeSwapSingleWithExtra(poolAddr, poolType, tokenIn, tokenOut, amount, extraData)
	ret, _, err := lc.EVMCall(vs, block, timestamp, baseFee, miss, chainCfg, DUMMY_SENDER, router.DeployedRouter, calldata)
	if err != nil || len(ret) < 32 {
		return uint256.NewInt(0)
	}
	var out uint256.Int
	out.SetBytes(ret[:32])
	// Negative = revert indicator.
	if out.Bytes32()[0]&0x80 != 0 {
		return uint256.NewInt(0)
	}
	return &out
}

func formulaQuote(pq formulas.PoolQuoter, amount *uint256.Int, tokenIn, tokenOut common.Address) *uint256.Int {
	if pq == nil {
		return uint256.NewInt(0)
	}
	out := pq.Quote(amount, tokenIn, tokenOut)
	if out.IsZero() {
		return uint256.NewInt(0)
	}
	return &out
}

func amountsEqual(a, b *uint256.Int) bool {
	if a == nil {
		a = uint256.NewInt(0)
	}
	if b == nil {
		b = uint256.NewInt(0)
	}
	return a.Eq(b)
}

func writeRegistry(results []fillResult) {
	// This is a placeholder — full implementation reads existing registry,
	// merges, and writes. See archive/tools/discover/main.go.txt for the
	// complete version.
	fmt.Fprintf(os.Stderr, "\nTODO: implement --write (see archived version)\n")
}

type fillResult struct {
	addr      common.Address
	formulaID int
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
