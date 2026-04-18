// discover — Formula registry discovery using the light client.
//
// Probes every formula-eligible pool (including those currently marked -1 in
// registry.txt) against EVM ground truth with up to 10 amounts. Prints the
// discovered formula ID and the registry delta — pools that could be
// un-blacklisted, pools that should be blacklisted, and new pools.
//
// Read-only: makes no changes to registry.txt. Edit by hand based on output.
//
// Usage:
//   go run ./tools/discover/ [--rpc ws://...] [--limit 5000]
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
	poolLimit := flag.Int("limit", 4000, "pool limit (top N most recently active; 0 = all)")
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
		var hooks common.Address
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
				hooks = common.HexToAddress(parts[1])
			}
		}
		if poolIdHex != "" && tickSpacing != 0 {
			var poolId [32]byte
			copy(poolId[:], common.FromHex(poolIdHex))
			formulas.RegisterV4Pool(strings.ToLower(p.Address.Hex()), poolId, tickSpacing, fee, 0, hooks)
			v4Count++
		}
	}
	if v4Count > 0 {
		fmt.Fprintf(os.Stderr, "[discover] registered %d V4 pools\n", v4Count)
	}

	// Build miss callbacks for state fetching.
	miss := fetcher.MissCallbacks(vs, &lc.FetchStats{})
	sv := lc.NewStateView(vs, headBlock, miss)

	// Apply token overrides so EVM swap calls work.
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

	// Build PoolManager for formula quoting.
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

	// Every formula-eligible pool is a candidate. We deliberately re-probe
	// pools that are already in the registry — including those blacklisted
	// with -1 — so the output surfaces registry entries that disagree with
	// current on-chain behavior.
	type candidate struct {
		pool       pathfinder.Pool
		formulaID  int // candidate formula for this pool type
		existingID int // from registry.txt; math.MinInt if absent
	}
	const notInRegistry = -9999
	var candidates []candidate
	for _, p := range pools {
		fid, ok := formulaMap[p.PoolType]
		if !ok || len(p.Tokens) < 2 {
			continue
		}
		existing := notInRegistry
		if id, known := registry.GetFormulaID(p.Address); known {
			existing = id
		}
		candidates = append(candidates, candidate{pool: p, formulaID: fid, existingID: existing})
	}
	fmt.Fprintf(os.Stderr, "[discover] %d candidates\n", len(candidates))

	// Discovery pass.
	fmt.Fprintf(os.Stderr, "[discover] verifying (formula vs EVM, up to 10 amounts)...\n")
	t0 := time.Now()

	type probeResult struct {
		addr       common.Address
		existingID int // notInRegistry if absent
		probedID   int // -1 on mismatch
	}
	var results []probeResult

	for i, c := range candidates {
		p := c.pool
		probed := -1

		for _, dir := range [][2]int{{0, 1}, {1, 0}} {
			tokenIn := p.Tokens[dir[0]]
			tokenOut := p.Tokens[dir[1]]

			baseAmount, hasAmount := tokenAmounts[tokenIn]
			if !hasAmount {
				continue
			}

			evmOut := evmQuote(sv, headTimestamp, baseFee, chainCfg,
				p.Address, p.PoolType, tokenIn, tokenOut, baseAmount, p.ExtraData)
			pq := pm.BuildQuoterForFormulaID(p.Address, c.formulaID)
			fOut := formulaQuote(pq, baseAmount, tokenIn, tokenOut)
			if !amountsEqual(evmOut, fOut) {
				continue
			}

			allMatch := true
			for mult := uint64(1); mult <= 10; mult++ {
				testAmount := new(uint256.Int).Mul(baseAmount, uint256.NewInt(mult))
				evm := evmQuote(sv, headTimestamp, baseFee, chainCfg,
					p.Address, p.PoolType, tokenIn, tokenOut, testAmount, p.ExtraData)
				pq = pm.BuildQuoterForFormulaID(p.Address, c.formulaID)
				f := formulaQuote(pq, testAmount, tokenIn, tokenOut)
				if !amountsEqual(evm, f) {
					allMatch = false
					break
				}
			}

			if allMatch {
				probed = c.formulaID
				break
			}
		}

		results = append(results, probeResult{addr: p.Address, existingID: c.existingID, probedID: probed})

		if (i+1)%100 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d\n", i+1, len(candidates))
		}
	}

	fmt.Fprintf(os.Stderr, "[discover] done in %v\n\n", time.Since(t0).Round(time.Millisecond))

	// Bucket by change category. The actionable categories are the first two:
	// un-blacklists (safe wins) and pools that should be newly registered.
	var unblacklist, register, newlyBlacklist, agreement, stillBlacklist []probeResult
	for _, r := range results {
		switch {
		case r.existingID == -1 && r.probedID >= 0:
			unblacklist = append(unblacklist, r)
		case r.existingID == notInRegistry && r.probedID >= 0:
			register = append(register, r)
		case r.existingID >= 0 && r.probedID == -1:
			newlyBlacklist = append(newlyBlacklist, r)
		case r.existingID == r.probedID && r.probedID >= 0:
			agreement = append(agreement, r)
		case r.existingID == -1 && r.probedID == -1:
			stillBlacklist = append(stillBlacklist, r)
		}
	}

	fmt.Printf("=== un-blacklist candidates: %d ===\n", len(unblacklist))
	fmt.Printf("(pools currently :-1 whose formula matches EVM on 10 amounts)\n")
	for _, r := range unblacklist {
		fmt.Printf("%s:%d\n", strings.ToLower(r.addr.Hex()), r.probedID)
	}

	fmt.Printf("\n=== new registry entries: %d ===\n", len(register))
	fmt.Printf("(pools not yet in registry whose formula matches EVM)\n")
	for _, r := range register {
		fmt.Printf("%s:%d\n", strings.ToLower(r.addr.Hex()), r.probedID)
	}

	fmt.Printf("\n=== newly-broken (disagree with EVM): %d ===\n", len(newlyBlacklist))
	fmt.Printf("(pools with a valid formulaID today but re-probe now fails)\n")
	for _, r := range newlyBlacklist {
		fmt.Printf("%s:%d→-1\n", strings.ToLower(r.addr.Hex()), r.existingID)
	}

	fmt.Fprintf(os.Stderr, "\nSummary:\n")
	fmt.Fprintf(os.Stderr, "  un-blacklist:    %d\n", len(unblacklist))
	fmt.Fprintf(os.Stderr, "  new entries:     %d\n", len(register))
	fmt.Fprintf(os.Stderr, "  newly broken:    %d\n", len(newlyBlacklist))
	fmt.Fprintf(os.Stderr, "  still valid:     %d\n", len(agreement))
	fmt.Fprintf(os.Stderr, "  still -1:        %d\n", len(stillBlacklist))
}

func evmQuote(sv *lc.StateView, timestamp uint64, baseFee *big.Int,
	chainCfg *params.ChainConfig, poolAddr common.Address,
	poolType int, tokenIn, tokenOut common.Address, amount *uint256.Int, extraData string,
) *uint256.Int {
	calldata := pathfinder.EncodeSwapSingleWithExtra(poolAddr, poolType, tokenIn, tokenOut, amount, extraData)
	ret, _, err := lc.EVMCallOn(sv, timestamp, baseFee, chainCfg,
		DUMMY_SENDER, router.DeployedRouter, calldata)
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
