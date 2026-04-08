package main

import (
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// OptimalSize binary-searches for the input amount that maximizes profit
// for a given cycle (path of steps from hub back to hub).
//
// The profit curve is concave: profit increases with amount until pool
// depletion causes output to drop. Binary search finds the peak.
//
// Uses formula quotes for fast iteration, then one final EVM verification.
func OptimalSize(
	pm *formulas.PoolManager,
	steps []pf.RouteStep,
	stateWithOverrides *statedb.StateDB,
	cfg statedb.EVMConfig,
	routerAddr, sender common.Address,
	hub common.Address,
	estimatedGas uint64,
	gasPrice uint64,
	prices map[common.Address]float64,
) *CycleResult {
	gasCostWei := estimatedGas * gasPrice

	// Evaluate profit at a given input amount using formula quotes.
	formulaProfit := func(amountIn *uint256.Int) int64 {
		out := pf.QuotePath(pm, steps, amountIn)
		if out.IsZero() {
			return -(1 << 62) // very negative
		}
		// profit = out - in - gasCost
		gasCostU := new(uint256.Int).SetUint64(gasCostWei)
		totalCost := new(uint256.Int).Add(amountIn, gasCostU)
		if out.Gt(totalCost) {
			diff := new(uint256.Int).Sub(&out, totalCost)
			if diff.IsUint64() {
				return int64(diff.Uint64())
			}
			return 1 << 62 // very positive, capped
		}
		diff := new(uint256.Int).Sub(totalCost, &out)
		if diff.IsUint64() {
			return -int64(diff.Uint64())
		}
		return -(1 << 62)
	}

	// Binary search: find the amount that maximizes profit.
	// Start with a range from 0.001 AVAX-equivalent to 1000 AVAX-equivalent.
	lo := toWei(1, 15) // 0.001 AVAX
	hi := toWei(1000, 18) // 1000 AVAX

	// Adjust for non-WAVAX hubs
	switch hub {
	case USDC, USDT:
		lo = toWei(1, 4) // 0.01 USDC
		hi = toWei(100000, 6) // 100k USDC
	case WETHe:
		lo = toWei(1, 13) // 0.00001 ETH
		hi = toWei(100, 18) // 100 ETH
	}

	// Ternary search on the concave profit curve (15 iterations)
	for i := 0; i < 15; i++ {
		diff := new(uint256.Int).Sub(hi, lo)
		third := new(uint256.Int).Div(diff, uint256.NewInt(3))
		if third.IsZero() {
			break
		}

		m1 := new(uint256.Int).Add(lo, third)
		m2 := new(uint256.Int).Sub(hi, third)

		p1 := formulaProfit(m1)
		p2 := formulaProfit(m2)

		if p1 < p2 {
			lo = m1
		} else {
			hi = m2
		}
	}

	// Best amount is midpoint of final range
	bestAmount := new(uint256.Int).Add(lo, hi)
	bestAmount.Div(bestAmount, uint256.NewInt(2))

	// Check formula profitability at best amount
	if formulaProfit(bestAmount) <= 0 {
		return nil
	}

	// Final EVM verification at the optimal amount
	calldata := buildCalldata(steps, bestAmount)
	evmCtx := statedb.GetCachedContext(cfg)
	cs := statedb.NewCallState(stateWithOverrides)
	ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, sender, routerAddr, calldata)
	if err != nil || len(ret) < 32 {
		return nil
	}

	var evmOut uint256.Int
	evmOut.SetBytes(ret[:32])
	if evmOut.Bytes32()[0]&0x80 != 0 || evmOut.IsZero() {
		return nil
	}

	gasCostU := new(uint256.Int).SetUint64(gasUsed * gasPrice)
	totalCost := new(uint256.Int).Add(bestAmount, gasCostU)
	if !evmOut.Gt(totalCost) {
		return nil
	}

	profit := new(uint256.Int).Sub(&evmOut, totalCost)
	return &CycleResult{
		Hub:       hub,
		Steps:     steps,
		AmountIn:  *bestAmount,
		AmountOut: evmOut,
		Profit:    *profit,
		GasUsed:   gasUsed,
		Calldata:  calldata,
	}
}
