package pathfinder

import (
	"encoding/hex"
	"fmt"
	"time"

	"defi-toolbox/formulas"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// RouteStep represents one hop in a route.
type RouteStep struct {
	Pool     common.Address `json:"pool"`
	PoolType int            `json:"poolType"`
	TokenIn  common.Address `json:"tokenIn"`
	TokenOut common.Address `json:"tokenOut"`
}

// Route is the result of a pathfinding search.
type Route struct {
	Steps     []RouteStep `json:"steps"`
	AmountOut *uint256.Int `json:"amountOut"`
	Stats     RouteStats  `json:"stats"`
}

// RouteStats tracks quoting statistics.
type RouteStats struct {
	FormulaQuotes int     `json:"formulaQuotes"`
	EVMQuotes     int     `json:"evmQuotes"`
	TotalQuotes   int     `json:"totalQuotes"`
	FormulaMs     float64 `json:"formulaMs"`
	EVMMs         float64 `json:"evmMs"`
	OverheadMs    float64 `json:"overheadMs"`
}

// layerNode is a surviving candidate at an intermediate token.
type layerNode struct {
	steps   []RouteStep
	token   common.Address
	amount  *uint256.Int
	visited map[common.Address]bool
}

// hopQuote is a pending quote in the BFS.
type hopQuote struct {
	nodeIdx    int
	step       RouteStep
	targetPool *Pool
	amountIn   *uint256.Int
}

// DUMMY_SENDER is the from address for EVM calls.
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

// FindBestRoute finds the best swap route using BFS with layer-by-layer quoting.
// Quotes are computed in-process: formula first, EVM fallback.
func FindBestRoute(
	state *statedb.StateDB,
	cfg statedb.EVMConfig,
	registry *formulas.Registry,
	overrides []ParsedOverride,
	routerAddr common.Address,
	graph *Graph,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	maxHops int,
) *Route {
	if tokenIn == tokenOut {
		return nil
	}
	if maxHops <= 0 {
		maxHops = 4
	}

	var stats RouteStats

	// Create the overridden state once — flat clone, no overlay indirection
	baseWithOverrides := ApplyOverridesFlat(state, overrides)
	// CallState: thin overlay with journal snapshots, JUMPDEST sharing
	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(baseWithOverrides)

	nodes := []layerNode{{
		steps:   nil,
		token:   tokenIn,
		amount:  amountIn,
		visited: map[common.Address]bool{tokenIn: true},
	}}

	var bestRoute []RouteStep
	var bestAmountOut *uint256.Int

	for layer := 0; layer < maxHops; layer++ {
		var hops []hopQuote

		for ni := range nodes {
			node := &nodes[ni]
			edges := graph.Edges[node.token]

			seen := make(map[[24]byte]bool) // pool:tokenOut dedup
			for _, edge := range edges {
				if node.visited[edge.TokenOut] && edge.TokenOut != tokenOut {
					continue
				}
				var key [24]byte
				copy(key[:20], edge.Pool.Address[:])
				copy(key[20:], edge.TokenOut[:4])
				if seen[key] {
					continue
				}
				seen[key] = true

				hops = append(hops, hopQuote{
					nodeIdx: ni,
					step: RouteStep{
						Pool:     edge.Pool.Address,
						PoolType: edge.Pool.PoolType,
						TokenIn:  node.token,
						TokenOut: edge.TokenOut,
					},
					targetPool: edge.Pool,
					amountIn:   node.amount,
				})
			}
		}

		if len(hops) == 0 {
			break
		}

		// Quote all hops: formula first, EVM fallback
		bestPerToken := make(map[common.Address]*layerNode)

		for i := range hops {
			hop := &hops[i]
			stats.TotalQuotes++

			var amountOut *uint256.Int

			// Try formula
			ft0 := time.Now()
			reader := func(addr common.Address, key common.Hash) common.Hash {
				return state.GetState(addr, key)
			}
			calldata := EncodeSwapSingle(hop.step.Pool, hop.step.PoolType, hop.step.TokenIn, hop.step.TokenOut, hop.amountIn)
			if ret, ok := registry.TryQuote(reader, calldata); ok {
				stats.FormulaQuotes++
				stats.FormulaMs += float64(time.Since(ft0).Microseconds()) / 1000.0
				var out uint256.Int
				out.SetBytes(ret)
				if !out.IsZero() {
					amountOut = &out
				}
			}

			// EVM fallback
			if amountOut == nil {
				et0 := time.Now()
				stats.EVMQuotes++
				cs.Reset()
				ret, _, evmErr := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, routerAddr, calldata)
				stats.EVMMs += float64(time.Since(et0).Microseconds()) / 1000.0
				if evmErr == nil && len(ret) >= 32 {
					var out uint256.Int
					out.SetBytes(ret[:32])
					if !out.IsZero() {
						amountOut = &out
					}
				}
			}

			if amountOut == nil {
				continue
			}

			parentNode := &nodes[hop.nodeIdx]
			fullRoute := make([]RouteStep, len(parentNode.steps)+1)
			copy(fullRoute, parentNode.steps)
			fullRoute[len(parentNode.steps)] = hop.step

			if hop.step.TokenOut == tokenOut {
				if bestAmountOut == nil || amountOut.Gt(bestAmountOut) {
					bestRoute = fullRoute
					bestAmountOut = new(uint256.Int).Set(amountOut)
				}
				continue
			}

			existing, ok := bestPerToken[hop.step.TokenOut]
			if !ok || amountOut.Gt(existing.amount) {
				newVisited := make(map[common.Address]bool, len(parentNode.visited)+1)
				for k, v := range parentNode.visited {
					newVisited[k] = v
				}
				newVisited[hop.step.TokenOut] = true
				bestPerToken[hop.step.TokenOut] = &layerNode{
					steps:   fullRoute,
					token:   hop.step.TokenOut,
					amount:  new(uint256.Int).Set(amountOut),
					visited: newVisited,
				}
			}
		}

		nodes = nodes[:0]
		for _, n := range bestPerToken {
			nodes = append(nodes, *n)
		}
		if len(nodes) == 0 {
			break
		}
	}

	if bestAmountOut == nil {
		return nil
	}

	return &Route{
		Steps:     bestRoute,
		AmountOut: bestAmountOut,
		Stats:     stats,
	}
}

// ParsedOverride holds pre-parsed override data.
type ParsedOverride struct {
	Addr    common.Address
	Balance *uint256.Int
	Nonce   uint64
	Code    []byte
	Slots   []struct {
		Slot  common.Hash
		Value common.Hash
	}
}

// ApplyOverrides creates a fresh overlay with overrides applied.
func ApplyOverrides(base *statedb.StateDB, overrides []ParsedOverride) *statedb.StateDB {
	if len(overrides) == 0 {
		return base
	}
	overlay := base.NewOverlay()
	for _, po := range overrides {
		if po.Code != nil {
			overlay.SetAccount(po.Addr, po.Balance, po.Nonce, po.Code)
		}
		for _, s := range po.Slots {
			overlay.SetStorageSlot(po.Addr, s.Slot, s.Value)
		}
	}
	return overlay
}

// ApplyOverridesFlat creates a flat clone of base with overrides baked in.
// No overlay indirection — CallState reads directly from one layer.
// Uses COW for storage maps so the original base state is not modified.
func ApplyOverridesFlat(base *statedb.StateDB, overrides []ParsedOverride) *statedb.StateDB {
	if len(overrides) == 0 {
		return base
	}
	flat := base.CloneFlat()

	// Track which accounts are shared with original to do COW
	shared := make(map[common.Address]bool, len(overrides))
	for _, po := range overrides {
		shared[po.Addr] = true
	}

	for _, po := range overrides {
		if po.Code != nil {
			flat.SetAccount(po.Addr, po.Balance, po.Nonce, po.Code)
			delete(shared, po.Addr) // SetAccount creates fresh account, no longer shared
		}
		for _, s := range po.Slots {
			flat.SetStorageSlotCOW(po.Addr, s.Slot, s.Value, shared)
		}
	}
	return flat
}

// helper for debug
var _ = fmt.Sprintf
var _ = hex.EncodeToString
