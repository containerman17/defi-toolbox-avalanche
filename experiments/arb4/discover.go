package main

import (
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// DiscoverPools runs exclusion BFS to find a diverse set of pool indices
// that participate in profitable cycles from the given hub token.
//
// Algorithm:
// 1. Find best cycle via formula BFS (cyclic mode).
// 2. For each pool in the winning cycle, exclude it and re-run BFS.
// 3. Collect all new pools from exclusion rounds.
// 4. Run one more round excluding the newly found pools.
// 5. Stop after 2 rounds or when cycles are unprofitable.
func DiscoverPools(
	pm *formulas.PoolManager,
	adj map[common.Address][]pf.PoolEdge,
	pools []pf.Pool,
	stateWithOverrides *statedb.StateDB,
	cfg statedb.EVMConfig,
	routerAddr, sender common.Address,
	hub common.Address,
	probeAmount *uint256.Int,
	maxHops int,
) []uint16 {
	discovered := make(map[uint16]bool)

	// Round 1: find best cycle, then exclude each pool
	route := pf.FindBestRoute(pm, adj, pools, stateWithOverrides, cfg, routerAddr, sender,
		hub, hub, probeAmount, maxHops)
	if route == nil {
		return nil
	}

	// Check profitability: output > input
	if !route.AmountOut.Gt(probeAmount) {
		return nil
	}

	// Collect pools from winning route
	var roundPools []uint16
	for _, step := range route.Steps {
		idx := poolIndex(pools, step.Pool)
		if idx >= 0 && !discovered[uint16(idx)] {
			discovered[uint16(idx)] = true
			roundPools = append(roundPools, uint16(idx))
		}
	}

	// Exclusion rounds
	for round := 0; round < 2; round++ {
		var newPools []uint16
		for _, excludeIdx := range roundPools {
			filteredAdj := filterAdjacency(adj, excludeIdx)
			exRoute := pf.FindBestRoute(pm, filteredAdj, pools, stateWithOverrides, cfg, routerAddr, sender,
				hub, hub, probeAmount, maxHops)
			if exRoute == nil {
				continue
			}
			// Only accept profitable cycles
			if !exRoute.AmountOut.Gt(probeAmount) {
				continue
			}
			for _, step := range exRoute.Steps {
				idx := poolIndex(pools, step.Pool)
				if idx >= 0 && !discovered[uint16(idx)] {
					discovered[uint16(idx)] = true
					newPools = append(newPools, uint16(idx))
				}
			}
		}
		if len(newPools) == 0 {
			break
		}
		roundPools = newPools
	}

	result := make([]uint16, 0, len(discovered))
	for idx := range discovered {
		result = append(result, idx)
	}
	return result
}

// filterAdjacency returns a copy of adj with all edges for the given pool index removed.
func filterAdjacency(adj map[common.Address][]pf.PoolEdge, excludePoolIdx uint16) map[common.Address][]pf.PoolEdge {
	filtered := make(map[common.Address][]pf.PoolEdge, len(adj))
	for token, edges := range adj {
		var kept []pf.PoolEdge
		for _, e := range edges {
			if e.PoolIdx != excludePoolIdx {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			filtered[token] = kept
		}
	}
	return filtered
}

// poolIndex finds the index of a pool by address. Returns -1 if not found.
func poolIndex(pools []pf.Pool, addr common.Address) int {
	for i := range pools {
		if pools[i].Address == addr {
			return i
		}
	}
	return -1
}

// probeAmountForHub returns a standard 1-unit probe amount for a given hub.
func probeAmountForHub(hub common.Address) *uint256.Int {
	switch hub {
	case WAVAX:
		return toWei(1, 18) // 1 AVAX
	case USDC, USDT:
		return toWei(20, 6) // 20 USDC/USDT
	case WETHe:
		return toWei(1, 16) // 0.01 ETH
	}
	return uint256.NewInt(0)
}
