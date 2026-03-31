package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/tools/pool-collector"
	router "defi-toolbox/contracts"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")

var ROUTER = router.DeployedRouter

// Fake caller for swap() simulation — needs WAVAX balance + allowance
var CALLER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

const RPC = "http://localhost:9650/ext/bc/C/rpc"

func main() {
	stateServerURL := "ws://localhost:7449/live"
	poolLimit := 10000

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--pool-limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
	}

	// Load pools
	pools := poolcollector.EmbeddedPools(poolLimit)
	registry := formulas.LoadEmbeddedRegistry()
	fmt.Fprintf(os.Stderr, "[watcher] loaded %d pools\n", len(pools))

	// Build pool address → index map
	poolIdx := make(map[common.Address]int, len(pools))
	for i := range pools {
		poolIdx[pools[i].Address] = i
	}

	// Build slot→pool mapping via PoolManager (to detect dirty pools from storage keys)
	ls, err := statedb.Connect(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[watcher] connect failed: %v\n", err)
		os.Exit(1)
	}
	state := ls.State()

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

	// Figure out token prices (1 AVAX worth) for realistic quote amounts
	// Quick 1-wave: just quote WAVAX into direct neighbors
	tokenPrice := make(map[common.Address]*uint256.Int)
	tokenPrice[WAVAX] = uint256.NewInt(1_000_000_000_000_000_000) // 1 AVAX

	type poolEdge struct {
		pool     *pf.Pool
		tokenOut common.Address
		dir      bool
	}
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
		adj[p.Tokens[0]] = append(adj[p.Tokens[0]], poolEdge{p, p.Tokens[1], true})
		adj[p.Tokens[1]] = append(adj[p.Tokens[1]], poolEdge{p, p.Tokens[0], false})
	}

	// Warmup pool quoters
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	// Wave 1 pricing
	for _, e := range adj[WAVAX] {
		if _, ok := tokenPrice[e.tokenOut]; ok {
			continue
		}
		out := pm.Quote(e.pool.Address, tokenPrice[WAVAX], e.dir)
		if !out.IsZero() {
			existing, exists := tokenPrice[e.tokenOut]
			if !exists || out.Gt(existing) {
				o := new(uint256.Int).Set(&out)
				tokenPrice[e.tokenOut] = o
			}
		}
	}
	// Wave 2
	for token, edges := range adj {
		if _, ok := tokenPrice[token]; ok {
			continue
		}
		for _, e := range edges {
			knownPrice, ok := tokenPrice[e.tokenOut]
			if !ok {
				continue
			}
			out := pm.Quote(e.pool.Address, knownPrice, !e.dir)
			if !out.IsZero() {
				o := new(uint256.Int).Set(&out)
				tokenPrice[token] = o
				break
			}
		}
	}
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[watcher] priced %d tokens\n", len(tokenPrice))

	// Build token overrides for the router (executeSwap style)
	overrides := router.BuildTokenOverrides(ROUTER, pools)

	// Add WAVAX balance override for CALLER (for swap() transferFrom)
	callerWavaxOverride := router.BuildSingleTokenOverride(CALLER, WAVAX, new(uint256.Int).Mul(uint256.NewInt(1_000_000), uint256.NewInt(1e18)))
	if callerWavaxOverride != nil {
		overrides = append(overrides, *callerWavaxOverride)
	}

	// Add WAVAX allowance override: allowance[CALLER][ROUTER] = maxUint256
	// WAVAX uses standard Solidity mapping: allowance at slot 4
	// keccak256(abi.encode(ROUTER, keccak256(abi.encode(CALLER, 4))))
	allowanceInnerSlot := crypto.Keccak256Hash(
		common.LeftPadBytes(CALLER.Bytes(), 32),
		common.LeftPadBytes([]byte{4}, 32),
	)
	allowanceSlot := crypto.Keccak256Hash(
		common.LeftPadBytes(ROUTER.Bytes(), 32),
		allowanceInnerSlot.Bytes(),
	)
	maxUint := new(uint256.Int).Sub(new(uint256.Int), uint256.NewInt(1))
	overrides = append(overrides, pf.ParsedOverride{
		Addr: WAVAX,
		Slots: []struct {
			Slot  common.Hash
			Value common.Hash
		}{{Slot: allowanceSlot, Value: common.Hash(maxUint.Bytes32())}},
	})

	baseWithOverrides := pf.ApplyOverridesFlat(state, overrides)

	// Build RPC state override object for eth_call
	rpcOverrides := buildRPCOverrides(overrides)

	fmt.Fprintf(os.Stderr, "[watcher] ready, watching for state changes...\n")

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
		}
	})

	for bi := range blockCh {
		pm.SetBlockTimestamp(bi.timestamp)

		// Find dirty pools from storage change entries
		dirtyPools := make(map[common.Address]bool)
		for _, entry := range bi.entries {
			key := entry[0]
			if strings.HasPrefix(key, "s:") {
				parts := strings.SplitN(key, ":", 3)
				if len(parts) == 3 {
					addr := common.HexToAddress(parts[1])
					slot := common.HexToHash(parts[2])
					poolAddr := pm.InvalidateBySlot(addr, slot)
					if poolAddr != (common.Address{}) {
						dirtyPools[poolAddr] = true
					}
				}
			}
		}

		if len(dirtyPools) == 0 {
			continue
		}

		cfg := statedb.EVMConfig{
			BlockNumber: bi.block,
			Timestamp:   bi.timestamp,
			ChainID:     43114,
			BaseFee:     bi.baseFee,
			GasLimit:    bi.gasLimit,
		}
		evmCtx := statedb.GetCachedContext(cfg)
		blockHex := fmt.Sprintf("0x%x", bi.block)

		matched, mismatched, skipped := 0, 0, 0

		ls.RLock()
		for poolAddr := range dirtyPools {
			idx, ok := poolIdx[poolAddr]
			if !ok {
				continue
			}
			p := &pools[idx]
			if len(p.Tokens) < 2 {
				continue
			}

			// Pick a direction where we have a priced input token
			for _, tokenIdxPair := range [][2]int{{0, 1}, {1, 0}} {
				tokenIn := p.Tokens[tokenIdxPair[0]]
				tokenOut := p.Tokens[tokenIdxPair[1]]

				// Only test WAVAX-input swaps (we have WAVAX balance + allowance override)
				if tokenIn != WAVAX {
					skipped++
					continue
				}

				amountIn := uint256.NewInt(1_000_000_000_000_000_000) // 1 AVAX

				// Use swap() — real tx format with transferFrom
				calldata := pf.EncodeSwapMulti(
					[]common.Address{p.Address},
					[]int{p.PoolType},
					[]common.Address{tokenIn, tokenOut},
					amountIn,
					[]string{p.ExtraData},
					uint256.NewInt(0), // minOutput = 0
				)

				// Local EVM with CALLER (has WAVAX balance + allowance)
				cs := statedb.NewCallState(baseWithOverrides)
				ret, _, evmErr := evmCtx.ExecuteWithCallState(cs, CALLER, ROUTER, calldata)
				var localOut uint256.Int
				localReverted := evmErr != nil || cs.Err() != nil || len(ret) < 32
				if !localReverted {
					localOut.SetBytes(ret[:32])
				}

				// RPC eth_call at same block with same overrides
				rpcOut, rpcReverted := rpcCall(calldata, blockHex, rpcOverrides)

				if localReverted && rpcReverted {
					matched++ // both reverted
				} else if localReverted != rpcReverted {
					mismatched++
					fmt.Fprintf(os.Stderr, "[watcher] MISMATCH block=%d pool=%s dir=%d local_revert=%v rpc_revert=%v local_out=%s rpc_out=%s\n",
						bi.block, p.Address.Hex()[:10], tokenIdxPair[0], localReverted, rpcReverted, localOut.Dec(), rpcOut.Dec())
				} else if !localOut.Eq(&rpcOut) {
					mismatched++
					fmt.Fprintf(os.Stderr, "[watcher] MISMATCH block=%d pool=%s dir=%d local=%s rpc=%s\n",
						bi.block, p.Address.Hex()[:10], tokenIdxPair[0], localOut.Dec(), rpcOut.Dec())
				} else {
					matched++
				}
			}
		}
		ls.RUnlock()

		fmt.Fprintf(os.Stderr, "[watcher] block=%d dirty=%d matched=%d mismatched=%d skipped=%d\n",
			bi.block, len(dirtyPools), matched, mismatched, skipped)
	}
}

// rpcCall runs eth_call at a specific block with state overrides.
func rpcCall(calldata []byte, blockHex string, overrides map[string]interface{}) (uint256.Int, bool) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []interface{}{
			map[string]string{
				"from": CALLER.Hex(),
				"to":   ROUTER.Hex(),
				"data": "0x" + hex.EncodeToString(calldata),
				"gas":  "0x1000000",
			},
			blockHex,
			overrides,
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(RPC, "application/json", bytes.NewReader(b))
	if err != nil {
		return uint256.Int{}, true
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var result struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &result)

	if result.Error != nil || result.Result == "" {
		return uint256.Int{}, true
	}

	outBytes, _ := hex.DecodeString(strings.TrimPrefix(result.Result, "0x"))
	if len(outBytes) < 32 {
		return uint256.Int{}, true
	}
	var out uint256.Int
	out.SetBytes(outBytes[:32])
	return out, false
}

// buildRPCOverrides converts ParsedOverrides into the eth_call state override format.
func buildRPCOverrides(overrides []pf.ParsedOverride) map[string]interface{} {
	result := make(map[string]interface{})
	for _, po := range overrides {
		addr := strings.ToLower(po.Addr.Hex())
		entry, ok := result[addr].(map[string]interface{})
		if !ok {
			entry = make(map[string]interface{})
			result[addr] = entry
		}

		if po.Code != nil {
			entry["code"] = "0x" + hex.EncodeToString(po.Code)
		}

		stateDiff, ok := entry["stateDiff"].(map[string]string)
		if !ok {
			stateDiff = make(map[string]string)
			entry["stateDiff"] = stateDiff
		}
		for _, s := range po.Slots {
			stateDiff[s.Slot.Hex()] = s.Value.Hex()
		}
	}
	return result
}
