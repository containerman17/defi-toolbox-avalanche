package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"defi-toolbox/arb"
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// WAVAX is the hub token for cyclic arb on Avalanche C-Chain.
var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")

// ─── Main ──────────────────────────────────────────────────────────

func main() {
	stateServerURL := "ws://localhost:7449/live"
	rpcURL := ""
	maxHops := 4
	poolLimit := 1500
	dryRun := true

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--rpc" && i+1 < len(os.Args) {
			rpcURL = os.Args[i+1]
		}
		if arg == "--max-hops" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &maxHops)
		}
		if arg == "--pool-limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
		if arg == "--execute" {
			dryRun = false
		}
	}

	fmt.Fprintf(os.Stderr, "[arb] WAVAX cyclic arb scanner starting...\n")
	fmt.Fprintf(os.Stderr, "[arb] state server: %s, max hops: %d, pool limit: %d\n",
		stateServerURL, maxHops, poolLimit)
	if dryRun {
		fmt.Fprintf(os.Stderr, "[arb] mode: DRY RUN (pass --execute --rpc <url> and set ARB_PRIVATE_KEY to go live)\n")
	} else {
		fmt.Fprintf(os.Stderr, "[arb] mode: LIVE EXECUTION\n")
	}

	// Load formula registry
	registry := formulas.LoadEmbeddedRegistry()
	validated, invalid := registry.RegistryStats()
	fmt.Fprintf(os.Stderr, "[arb] formula registry: %d validated, %d invalid\n", validated, invalid)

	// Load pools and build graph
	embeddedPools := poolcollector.EmbeddedPools(poolLimit)
	graph := pf.BuildGraph(embeddedPools)
	fmt.Fprintf(os.Stderr, "[arb] loaded %d pools\n", len(embeddedPools))

	fmt.Fprintf(os.Stderr, "[arb] router: %s\n", router.DeployedRouter.Hex())

	// Connect to state server
	ls, err := statedb.Connect(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb] failed to connect: %v\n", err)
		os.Exit(1)
	}
	state := ls.State()

	// Create PoolManager
	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)
	for _, p := range embeddedPools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	pm.SetBlockTimestamp(ls.Timestamp())

	// Build pool table and enumerate cycles
	pt := arb.NewPoolTable(embeddedPools)
	t0 := time.Now()
	cycles := arb.EnumerateCycles(graph, embeddedPools, WAVAX, maxHops, registry, pt)
	enumTime := time.Since(t0)
	fmt.Fprintf(os.Stderr, "[arb] enumerated %d cycles in %v\n", len(cycles), enumTime.Round(time.Millisecond))

	// Create scanner
	scanner := arb.NewScanner(cycles, pm, pt, WAVAX)

	// Register pool token0 for direction resolution in rate table
	for _, p := range embeddedPools {
		if len(p.Tokens) >= 2 {
			scanner.RateTable().SetPoolToken0(p.Address, p.Tokens[0])
		}
	}

	// Set up executor if live mode
	var executor *arb.Executor
	var caller common.Address
	if !dryRun {
		privKey := os.Getenv("ARB_PRIVATE_KEY")
		if privKey == "" {
			fmt.Fprintf(os.Stderr, "[arb] ERROR: ARB_PRIVATE_KEY env var required for --execute\n")
			os.Exit(1)
		}
		if rpcURL == "" {
			fmt.Fprintf(os.Stderr, "[arb] ERROR: --rpc <url> required for --execute\n")
			os.Exit(1)
		}
		var err error
		executor, err = arb.NewExecutor(privKey, rpcURL, router.DeployedRouter, pt, WAVAX)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] ERROR: %v\n", err)
			os.Exit(1)
		}
		caller = executor.Address()
		nonce, err := executor.FetchNonce()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] ERROR fetching nonce: %v\n", err)
			os.Exit(1)
		}
		executor.SetNonce(nonce)

		// Query WAVAX (ERC-20) balance — this is what we trade.
		wavaxBal, err := executor.FetchERC20Balance(WAVAX)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] WARNING: could not fetch WAVAX balance: %v\n", err)
		} else {
			balF := new(big.Float).Quo(new(big.Float).SetInt(wavaxBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb] WAVAX balance: %s\n", balF.Text('f', 6))

		}
		// Also show native AVAX (for gas)
		nativeBal, err := executor.FetchBalance()
		if err == nil {
			balF := new(big.Float).Quo(new(big.Float).SetInt(nativeBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb] native AVAX (gas): %s\n", balF.Text('f', 6))
		}
		// Check WAVAX approval for router, approve if needed
		allowance, err := executor.CheckAllowance(WAVAX)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] WARNING: could not check allowance: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[arb] WAVAX allowance: %s\n", allowance.String())

			// Need at least 1000 WAVAX allowance to be useful
			minAllowance := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
			if allowance.Cmp(minAllowance) < 0 {
				fmt.Fprintf(os.Stderr, "[arb] WAVAX allowance too low (%s), approving router...\n", allowance.String())
				txHash, err := executor.Approve(WAVAX, ls.BaseFee())
				if err != nil {
					fmt.Fprintf(os.Stderr, "[arb] ERROR: approve failed: %v\n", err)
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "[arb] approve tx: %s (waiting 3s for confirmation)\n", txHash.Hex())
				time.Sleep(3 * time.Second)
				// Re-fetch nonce after approval
				nonce, _ = executor.FetchNonce()
				executor.SetNonce(nonce)
			} else {
				fmt.Fprintf(os.Stderr, "[arb] WAVAX allowance OK\n")
			}
		}

		fmt.Fprintf(os.Stderr, "[arb] executor ready, nonce=%d\n", nonce)
	}

	// Cap trade size to WAVAX balance (the token we trade, not native AVAX)
	if executor != nil {
		wavaxBal, err := executor.FetchERC20Balance(WAVAX)
		if err == nil && wavaxBal.Sign() > 0 {
			u, _ := uint256.FromBig(wavaxBal)
			scanner.MaxSize = u
			balF := new(big.Float).Quo(new(big.Float).SetInt(wavaxBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb] max trade size: %s WAVAX\n", balF.Text('f', 6))
		}
	}

	// Initial rate sweep under read lock
	ls.RLock()
	fmt.Fprintf(os.Stderr, "[arb] router code: %d bytes, WAVAX code: %d bytes\n",
		state.GetCodeSize(router.DeployedRouter), state.GetCodeSize(WAVAX))
	fmt.Fprintf(os.Stderr, "[arb] running initial rate sweep...\n")
	scanner.InitRates()
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[arb] ready. Waiting for blocks...\n")

	// Block processing via SetOnBlock callback
	type blockEvent struct {
		block, timestamp, baseFee, gasLimit uint64
		entries                             [][2]string
	}
	blockCh := make(chan blockEvent, 4)

	ls.SetOnBlock(func(ls *statedb.LiveState, entries [][2]string) {
		// Called with write lock held — state is already updated
		select {
		case blockCh <- blockEvent{ls.Block(), ls.Timestamp(), ls.BaseFee(), ls.GasLimit(), entries}:
		default:
			fmt.Fprintf(os.Stderr, "[arb] WARNING: dropped block %d\n", ls.Block())
		}
	})

	for bi := range blockCh {
		pm.SetBlockTimestamp(bi.timestamp)

		// Invalidate dirty pools from the diff entries
		dirtySet := make(map[common.Address]bool)
		var dp []common.Address
		for _, entry := range bi.entries {
			key := entry[0]
			if strings.HasPrefix(key, "s:") {
				parts := strings.SplitN(key, ":", 3)
				if len(parts) == 3 {
					addr := common.HexToAddress(parts[1])
					slot := common.HexToHash(parts[2])
					poolAddr := pm.InvalidateBySlot(addr, slot)
					if poolAddr != (common.Address{}) && !dirtySet[poolAddr] {
						dirtySet[poolAddr] = true
						dp = append(dp, poolAddr)
					}
				}
			}
		}

		if len(dp) == 0 {
			continue
		}

		cfg := statedb.EVMConfig{
			BlockNumber: bi.block,
			Timestamp:   bi.timestamp,
			ChainID:     43114,
			BaseFee:     bi.baseFee,
			GasLimit:    bi.gasLimit,
		}

		ls.RLock()
		verifier := arb.NewVerifier(state, cfg, router.DeployedRouter, caller, pt, WAVAX)
		opp, evmResults := scanner.OnBlock(dp, verifier, bi.baseFee)
		ls.RUnlock()

		fmt.Fprintf(os.Stderr, "[arb] block=%d dirty=%d | %s\n",
			bi.block, len(dp), arb.FormatOpportunity(opp, pt))

		// ── Stage 3b: RPC cross-check ALL local EVM results ──
		if executor != nil && len(evmResults) > 0 {
			matched, mismatched, rpcErrors := 0, 0, 0
			for _, er := range evmResults {
				rpc := executor.EthCallAtBlock(er.Calldata, er.Block)

				if er.Reverted && rpc.Reverted {
					matched++ // both reverted — OK
					continue
				}
				if er.Reverted != rpc.Reverted {
					mismatched++
					fmt.Fprintf(os.Stderr, "[arb] *** MISMATCH *** block=%d local_revert=%v rpc_revert=%v local_err=%s rpc_err=%s\n",
						er.Block, er.Reverted, rpc.Reverted, er.ErrMsg, rpc.ErrMsg)
					continue
				}
				if rpc.ErrMsg != "" && rpc.RetData == nil {
					rpcErrors++
					continue
				}
				// Both succeeded — compare return data byte-for-byte
				if !bytes.Equal(er.RetData, rpc.RetData) {
					mismatched++
					// Extract amountOut from both for readable log
					var localOut, rpcOut string
					if len(er.RetData) >= 32 {
						localOut = fmt.Sprintf("%x", er.RetData[len(er.RetData)-32:])
					}
					if len(rpc.RetData) >= 32 {
						rpcOut = fmt.Sprintf("%x", rpc.RetData[len(rpc.RetData)-32:])
					}
					fmt.Fprintf(os.Stderr, "[arb] *** OUTPUT MISMATCH *** block=%d local_out=%s rpc_out=%s local_gas=%d local_ret_len=%d rpc_ret_len=%d\n",
						er.Block, localOut, rpcOut, er.GasUsed, len(er.RetData), len(rpc.RetData))
				} else {
					matched++
				}
			}
			fmt.Fprintf(os.Stderr, "[arb] stage3 cross-check: %d/%d matched, %d mismatched, %d rpc-errors\n",
				matched, len(evmResults), mismatched, rpcErrors)
		}

		// ── Stage 4: Execute if profitable and verified ──
		if opp != nil && opp.EVMVerified && opp.EVMProfit > 0 && executor != nil {
			txHash, err := executor.Execute(opp, bi.baseFee)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[arb] exec error: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "\n[arb] *** TRADE EXECUTED ***\n")
				fmt.Fprintf(os.Stderr, "[arb] tx: %s\n", txHash.Hex())
				fmt.Fprintf(os.Stderr, "[arb] snowtrace: https://snowtrace.io/tx/%s\n\n", txHash.Hex())
				execOut, _ := json.Marshal(map[string]interface{}{
					"type":   "tx_sent",
					"block":  bi.block,
					"txHash": txHash.Hex(),
				})
				fmt.Println(string(execOut))
			}
		}
	}
}
