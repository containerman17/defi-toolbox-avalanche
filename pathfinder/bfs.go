package pathfinder

import (
	"bytes"
	"sort"
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

// DUMMY_SENDER is the from address for EVM calls.
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

// quotePool quotes a single pool swap: PoolManager first (cached structs), EVM fallback.
// Returns nil if the pool returns zero or errors.
func quotePool(
	pool *Pool,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	pm *formulas.PoolManager,
	cs *statedb.CallState,
	evmCtx *statedb.CachedContext,
	routerAddr common.Address,
	stats *RouteStats,
) *uint256.Int {
	stats.TotalQuotes++

	// Try PoolManager (pool cache + quote cache)
	ft0 := time.Now()
	zeroForOne := bytes.Compare(tokenIn[:], tokenOut[:]) < 0
	out, ok := pm.Quote(pool.Address, amountIn, zeroForOne)
	if ok || pm.Get(pool.Address) != nil {
		// Formula exists (cached hit or pool struct present)
		stats.FormulaQuotes++
		stats.FormulaMs += float64(time.Since(ft0).Microseconds()) / 1000.0
		if ok && out != nil && !out.IsZero() {
			return out
		}
		return nil
	}

	// EVM fallback (pool not in registry or construction failed)
	calldata := EncodeSwapSingleWithExtra(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn, pool.ExtraData)
	et0 := time.Now()
	stats.EVMQuotes++
	cs.Reset()
	ret, _, err := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, routerAddr, calldata)
	stats.EVMMs += float64(time.Since(et0).Microseconds()) / 1000.0
	if err == nil && len(ret) >= 32 {
		var out uint256.Int
		out.SetBytes(ret[:32])
		if !out.IsZero() {
			return &out
		}
	}
	return nil
}

// evmQuotePool quotes a single pool swap using EVM only (no formula).
func evmQuotePool(
	pool *Pool,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	cs *statedb.CallState,
	evmCtx *statedb.CachedContext,
	routerAddr common.Address,
) *uint256.Int {
	calldata := EncodeSwapSingleWithExtra(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn, pool.ExtraData)
	cs.Reset()
	ret, _, err := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, routerAddr, calldata)
	if err == nil && len(ret) >= 32 {
		var out uint256.Int
		out.SetBytes(ret[:32])
		if !out.IsZero() {
			return &out
		}
	}
	return nil
}

// FindBestRoute finds the best swap route using a simple two-phase approach:
//   - Phase 1: quote all direct pools (1 hop)
//   - Phase 2: find intermediate tokens adjacent to both tokenIn and tokenOut,
//     quote hop 1 (keep best per intermediate), then quote hop 2
//
// Maximum 2 hops, no path splitting.
func FindBestRoute(
	state *statedb.StateDB,
	cfg statedb.EVMConfig,
	pm *formulas.PoolManager,
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

	var stats RouteStats

	baseWithOverrides := ApplyOverridesFlat(state, overrides)
	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(baseWithOverrides)

	type routeCandidate struct {
		steps     []RouteStep
		pools     []*Pool
		amountOut *uint256.Int
	}
	var candidates []routeCandidate

	// ── Phase 1: Direct pools (1 hop) ──────────────────────────────────
	for _, edge := range graph.Edges[tokenIn] {
		if edge.TokenOut != tokenOut {
			continue
		}
		out := quotePool(edge.Pool, tokenIn, tokenOut, amountIn,
			pm, cs, evmCtx, routerAddr, &stats)
		if out != nil {
			candidates = append(candidates, routeCandidate{
				steps: []RouteStep{{
					Pool: edge.Pool.Address, PoolType: edge.Pool.PoolType,
					TokenIn: tokenIn, TokenOut: tokenOut,
				}},
				pools:     []*Pool{edge.Pool},
				amountOut: new(uint256.Int).Set(out),
			})
		}
	}

	// ── Phase 2: Two-hop via intermediate tokens ───────────────────────
	type hop1Result struct {
		step   RouteStep
		pool   *Pool
		amount *uint256.Int
	}
	bestPerIntermediate := make(map[common.Address]*hop1Result)

	seen := make(map[[24]byte]bool) // pool:tokenOut dedup
	for _, edge := range graph.Edges[tokenIn] {
		mid := edge.TokenOut
		if mid == tokenIn || mid == tokenOut {
			continue
		}

		// Dedup: same pool + same output token
		var key [24]byte
		copy(key[:20], edge.Pool.Address[:])
		copy(key[20:], mid[:4])
		if seen[key] {
			continue
		}
		seen[key] = true

		// Pre-check: does this intermediate connect to tokenOut?
		if _, known := bestPerIntermediate[mid]; !known {
			hasPath := false
			for _, e2 := range graph.Edges[mid] {
				if e2.TokenOut == tokenOut {
					hasPath = true
					break
				}
			}
			if !hasPath {
				continue
			}
		}

		out := quotePool(edge.Pool, tokenIn, mid, amountIn,
			pm, cs, evmCtx, routerAddr, &stats)
		if out == nil {
			continue
		}

		existing := bestPerIntermediate[mid]
		if existing == nil || out.Gt(existing.amount) {
			bestPerIntermediate[mid] = &hop1Result{
				step: RouteStep{
					Pool: edge.Pool.Address, PoolType: edge.Pool.PoolType,
					TokenIn: tokenIn, TokenOut: mid,
				},
				pool:   edge.Pool,
				amount: new(uint256.Int).Set(out),
			}
		}
	}

	// Hop 2: for each surviving intermediate, quote all pools to tokenOut
	for mid, hop1 := range bestPerIntermediate {
		seenPool := make(map[common.Address]bool)
		for _, edge := range graph.Edges[mid] {
			if edge.TokenOut != tokenOut {
				continue
			}
			if seenPool[edge.Pool.Address] {
				continue
			}
			seenPool[edge.Pool.Address] = true

			out := quotePool(edge.Pool, mid, tokenOut, hop1.amount,
				pm, cs, evmCtx, routerAddr, &stats)
			if out != nil {
				candidates = append(candidates, routeCandidate{
					steps: []RouteStep{
						hop1.step,
						{
							Pool: edge.Pool.Address, PoolType: edge.Pool.PoolType,
							TokenIn: mid, TokenOut: tokenOut,
						},
					},
					pools:     []*Pool{hop1.pool, edge.Pool},
					amountOut: new(uint256.Int).Set(out),
				})
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// ── EVM verification ───────────────────────────────────────────────
	// Take top 10 by formula amountOut, EVM-verify all, return the best.
	// Formulas can lie (e.g. wrong storage slots), so EVM is ground truth.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].amountOut.Gt(candidates[j].amountOut)
	})
	top := 10
	if top > len(candidates) {
		top = len(candidates)
	}

	var bestRoute []RouteStep
	var bestEVMOut *uint256.Int
	for _, cand := range candidates[:top] {
		evmAmount := new(uint256.Int).Set(amountIn)
		valid := true
		for i, pool := range cand.pools {
			step := cand.steps[i]
			out := evmQuotePool(pool, step.TokenIn, step.TokenOut, evmAmount, cs, evmCtx, routerAddr)
			if out == nil {
				valid = false
				break
			}
			evmAmount = out
		}
		if !valid {
			continue
		}
		stats.EVMQuotes += len(cand.pools)
		if bestEVMOut == nil || evmAmount.Gt(bestEVMOut) {
			bestEVMOut = new(uint256.Int).Set(evmAmount)
			bestRoute = cand.steps
		}
	}

	if bestEVMOut == nil {
		return nil
	}
	return &Route{
		Steps:     bestRoute,
		AmountOut: bestEVMOut,
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
