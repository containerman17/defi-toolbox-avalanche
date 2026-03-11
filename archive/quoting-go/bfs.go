package main

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
)

type BfsResult struct {
	AmountOut    *big.Int
	Route        []RouteStep
	GasUsed      uint64
	EvmCalls     int
	RequoteCalls int
}

type bestCandidate struct {
	amountOut *big.Int
	gasTotal  uint64
	route     []RouteStep
}

type backtrackEntry struct {
	pool      common.Address
	poolType  uint8
	fromToken common.Address
	amountIn  *big.Int
	amountOut *big.Int
	extraData ExtraData
}

func bfsRoute(
	evm *LocalEVM,
	quoterCode []byte,
	tokenOverrides map[common.Address]*TokenOverride,
	poolsByToken map[common.Address][]PoolEdge,
	tokenIn, tokenOut common.Address,
	inputAmount *big.Int,
	inputAvax *big.Int,
	maxHops int,
	blockInfo *BlockInfo,
	gasLimit uint64,
	gasAware bool,
	requote bool,
) *BfsResult {
	gasAware = gasAware && inputAvax.Sign() > 0
	basefee := blockInfo.Basefee

	// frontier: token -> (amount, cumulative gas)
	type frontierEntry struct {
		amount *big.Int
		gas    uint64
	}
	frontier := map[common.Address]frontierEntry{
		tokenIn: {amount: new(big.Int).Set(inputAmount), gas: 0},
	}

	totalEvmCalls := 0
	totalRequoteCalls := 0

	backtrack := make([]map[common.Address]*backtrackEntry, 0, maxHops)
	bestAtLayer := make([]*bestCandidate, maxHops)

	reachable := buildReachability(poolsByToken, tokenOut, maxHops)

	for layer := 0; layer < maxHops; layer++ {
		isLastLayer := layer == maxHops-1
		remaining := maxHops - layer - 1

		// Build jobs
		var jobs []QuoteJob
		for token, fe := range frontier {
			edges, ok := poolsByToken[token]
			if !ok {
				continue
			}
			for _, edge := range edges {
				// Same-pool reversal pruning
				if layer > 0 {
					if bt, ok := backtrack[layer-1][token]; ok {
						if edge.TokenOut == bt.fromToken && edge.Pool == bt.pool {
							continue
						}
					}
				}
				if edge.TokenOut == tokenIn && tokenIn != tokenOut {
					continue
				}
				if isLastLayer && edge.TokenOut != tokenOut {
					continue
				}
				if !isLastLayer && remaining > 0 {
					if remaining-1 < len(reachable) {
						if _, ok := reachable[remaining-1][edge.TokenOut]; !ok {
							continue
						}
					}
				}
				jobs = append(jobs, QuoteJob{
					Pool:      edge.Pool,
					PoolType:  edge.PoolType,
					TokenIn:   edge.TokenIn,
					TokenOut:  edge.TokenOut,
					Amount:    fe.amount,
					ExtraData: edge.ExtraData,
				})
			}
		}

		if len(jobs) == 0 {
			backtrack = append(backtrack, make(map[common.Address]*backtrackEntry))
			continue
		}

		nextFrontier := make(map[common.Address]frontierEntry)
		layerBacktrack := make(map[common.Address]*backtrackEntry)

		type terminalCandidate struct {
			estimatedAmount *big.Int
			estGas          uint64
			pool            common.Address
			poolType        uint8
			fromToken       common.Address
			extraData       ExtraData
		}
		var tokenOutCandidates []terminalCandidate

		// Quote all edges
		results, numCalls := quoteAll(evm, quoterCode, tokenOverrides, jobs, blockInfo, gasLimit)
		totalEvmCalls += numCalls

		for _, result := range results {
			if result.AmountOut.Sign() == 0 {
				continue
			}
			fromGas := uint64(0)
			if fe, ok := frontier[result.TokenIn]; ok {
				fromGas = fe.gas
			}
			pathGas := fromGas + result.GasUsed

			if result.TokenOut == tokenOut {
				if layer == 0 {
					// Layer 0: single-hop quotes are exact, score directly
					if isBetter(bestAtLayer[layer], result.AmountOut, pathGas, gasAware, inputAvax, basefee) {
						bestAtLayer[layer] = &bestCandidate{
							amountOut: result.AmountOut, gasTotal: pathGas,
							route: []RouteStep{{
								Pool: result.Pool, PoolType: result.PoolType,
								TokenIn: result.TokenIn, TokenOut: tokenOut,
								AmountIn: result.Amount, AmountOut: result.AmountOut,
								ExtraData: result.ExtraData,
							}},
						}
					}
				} else {
					// Layer >= 1: queue for full-path atomic quoting below
					tokenOutCandidates = append(tokenOutCandidates, terminalCandidate{
						estimatedAmount: result.AmountOut, estGas: pathGas,
						pool: result.Pool, poolType: result.PoolType,
						fromToken: result.TokenIn, extraData: result.ExtraData,
					})
				}
			} else {
				existing, exists := nextFrontier[result.TokenOut]
				better := !exists
				if exists {
					if gasAware {
						better = isBetterNet(result.AmountOut, pathGas, existing.amount, existing.gas, inputAvax, basefee)
					} else {
						better = result.AmountOut.Cmp(existing.amount) > 0
					}
				}
				if better {
					nextFrontier[result.TokenOut] = frontierEntry{amount: result.AmountOut, gas: pathGas}
					layerBacktrack[result.TokenOut] = &backtrackEntry{
						pool: result.Pool, poolType: result.PoolType,
						fromToken: result.TokenIn, amountIn: result.Amount,
						amountOut: result.AmountOut, extraData: result.ExtraData,
					}
				}
			}
		}

		backtrack = append(backtrack, layerBacktrack)

		// FULL-PATH ATOMIC QUOTING (layer >= 1)
		//
		// For multi-hop routes, we quote the entire accumulated path atomically
		// via executeRoute() instead of relying on single-hop estimates.
		// Single-hop swap above is used only as a FILTER to identify
		// which edges are worth exploring. The actual amounts come from here.
		//
		// Why: executing hops atomically means hop1 mutates pool state that
		// hop2 reads. Single-hop estimates ignore this, causing ~0.01% drift.
		//
		// TODO: This is a correctness-first approach. The proper fix is to
		// propagate state diffs through the route during BFS expansion,
		// avoiding the need to re-execute the full prefix for every new edge.
		// Current approach roughly doubles execution time.
		if layer > 0 {
			// Build all route jobs: both frontier and terminal candidates
			// are executed in a single parallel batch (matching Rust's approach).
			var allJobs []RouteJob
			var frontierTokens []common.Address
			var terminalInfos []terminalCandidate

			// Frontier routes
			for token := range nextFrontier {
				route := reconstructPath(backtrack, token, tokenIn, inputAmount)
				if route == nil {
					continue
				}
				frontierTokens = append(frontierTokens, token)
				allJobs = append(allJobs, RouteJob{Route: route})
			}

			terminalStart := len(allJobs)

			// Terminal routes
			for _, cand := range tokenOutCandidates {
				var prefix []RouteStep
				if cand.fromToken == tokenIn {
					prefix = nil
				} else {
					prefix = reconstructPath(backtrack, cand.fromToken, tokenIn, inputAmount)
					if prefix == nil {
						continue
					}
				}
				route := append(prefix, RouteStep{
					Pool: cand.pool, PoolType: cand.poolType,
					TokenIn: cand.fromToken, TokenOut: tokenOut,
					AmountIn: big.NewInt(0), AmountOut: big.NewInt(0),
					ExtraData: cand.extraData,
				})
				if len(route) > 0 {
					route[0].AmountIn = inputAmount
				}
				terminalInfos = append(terminalInfos, cand)
				allJobs = append(allJobs, RouteJob{Route: route})
			}

			if len(allJobs) > 0 {
				// Execute all routes in parallel
				results := executeRoutesParallel(evm, quoterCode, tokenOverrides, allJobs, inputAmount, blockInfo, gasLimit)
				totalRequoteCalls += len(results)

				// Apply frontier results
				for i := 0; i < terminalStart; i++ {
					token := frontierTokens[i]
					if results[i].AmountOut.Sign() > 0 {
						nextFrontier[token] = frontierEntry{amount: results[i].AmountOut, gas: results[i].GasUsed}
					} else {
						delete(nextFrontier, token)
					}
				}

				// Apply terminal results
				for i := terminalStart; i < len(results); i++ {
					if results[i].AmountOut.Sign() == 0 {
						continue
					}
					if isBetter(bestAtLayer[layer], results[i].AmountOut, results[i].GasUsed, gasAware, inputAvax, basefee) {
						bestAtLayer[layer] = &bestCandidate{
							amountOut: results[i].AmountOut, gasTotal: results[i].GasUsed,
							route: allJobs[i].Route,
						}
					}
				}
			}
		}

		// Update frontier
		frontier = make(map[common.Address]frontierEntry)
		for token, fe := range nextFrontier {
			if fe.amount.Sign() > 0 {
				frontier[token] = fe
			}
		}
	}

	// Pick best across all layers
	var bestLayer int = -1
	bestAmount := big.NewInt(0)
	var bestGas uint64

	for layer, cand := range bestAtLayer {
		if cand == nil {
			continue
		}
		better := bestLayer == -1
		if !better {
			if gasAware {
				better = isBetterNet(cand.amountOut, cand.gasTotal, bestAmount, bestGas, inputAvax, basefee)
			} else {
				better = cand.amountOut.Cmp(bestAmount) > 0
			}
		}
		if better {
			bestLayer = layer
			bestAmount = cand.amountOut
			bestGas = cand.gasTotal
		}
	}

	// Use the stored route directly — no reconstruction needed
	var route []RouteStep
	if bestLayer >= 0 {
		route = bestAtLayer[bestLayer].route
	}

	return &BfsResult{
		AmountOut:    bestAmount,
		Route:        route,
		GasUsed:      bestGas,
		EvmCalls:     totalEvmCalls,
		RequoteCalls: totalRequoteCalls,
	}
}

// ---- Helpers ----

func isBetter(current *bestCandidate, newAmount *big.Int, newGas uint64, gasAware bool, inputAvax *big.Int, basefee uint64) bool {
	if current == nil {
		return true
	}
	if gasAware {
		return isBetterNet(newAmount, newGas, current.amountOut, current.gasTotal, inputAvax, basefee)
	}
	return newAmount.Cmp(current.amountOut) > 0
}

func buildReachability(poolsByToken map[common.Address][]PoolEdge, tokenOut common.Address, maxHops int) []map[common.Address]bool {
	// Build reverse map: tokenOut -> set of tokenIn
	reverse := make(map[common.Address]map[common.Address]bool)
	for _, edges := range poolsByToken {
		for _, e := range edges {
			if reverse[e.TokenOut] == nil {
				reverse[e.TokenOut] = make(map[common.Address]bool)
			}
			reverse[e.TokenOut][e.TokenIn] = true
		}
	}

	layers := make([]map[common.Address]bool, 0, maxHops)
	currentSet := map[common.Address]bool{tokenOut: true}
	if sources, ok := reverse[tokenOut]; ok {
		for t := range sources {
			currentSet[t] = true
		}
	}
	layers = append(layers, copyBoolSet(currentSet))

	for i := 1; i < maxHops; i++ {
		nextSet := copyBoolSet(currentSet)
		for t := range currentSet {
			if sources, ok := reverse[t]; ok {
				for s := range sources {
					nextSet[s] = true
				}
			}
		}
		currentSet = nextSet
		layers = append(layers, copyBoolSet(currentSet))
	}

	return layers
}

func copyBoolSet(m map[common.Address]bool) map[common.Address]bool {
	r := make(map[common.Address]bool, len(m))
	for k := range m {
		r[k] = true
	}
	return r
}

func reconstructPath(backtrack []map[common.Address]*backtrackEntry, toToken, tokenIn common.Address, inputAmount *big.Int) []RouteStep {
	if len(backtrack) == 0 {
		return nil
	}
	lastLayer := len(backtrack) - 1
	entry, ok := backtrack[lastLayer][toToken]
	if !ok {
		return nil
	}

	var steps []RouteStep
	steps = append(steps, RouteStep{
		Pool: entry.pool, PoolType: entry.poolType,
		TokenIn: entry.fromToken, TokenOut: toToken,
		AmountIn: entry.amountIn, AmountOut: entry.amountOut,
		ExtraData: entry.extraData,
	})

	current := entry.fromToken
	for l := lastLayer - 1; l >= 0; l-- {
		if current == tokenIn {
			break
		}
		e, ok := backtrack[l][current]
		if !ok {
			return nil
		}
		steps = append(steps, RouteStep{
			Pool: e.pool, PoolType: e.poolType,
			TokenIn: e.fromToken, TokenOut: current,
			AmountIn: e.amountIn, AmountOut: e.amountOut,
			ExtraData: e.extraData,
		})
		current = e.fromToken
	}

	if current != tokenIn {
		return nil
	}

	// Reverse
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	if len(steps) > 0 {
		steps[0].AmountIn = inputAmount
	}
	return steps
}
