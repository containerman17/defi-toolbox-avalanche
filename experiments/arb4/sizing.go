package main

import (
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// EVMSizing runs ternary search on the winning cycle's input amount using
// pure EVM calls. Each evaluation is one full-path swap() — exact output and gas.
//
// ~15 iterations × ~0.1ms = ~1.5ms total.
func EVMSizing(
	steps []pf.RouteStep,
	stateWithOverrides *statedb.StateDB,
	cfg statedb.EVMConfig,
	routerAddr, sender common.Address,
	hub common.Address,
	gasPrice uint64,
) *CycleResult {
	evmCtx := statedb.GetCachedContext(cfg)

	// Evaluate profit at a given input amount via EVM.
	evalEVM := func(amountIn *uint256.Int) *CycleResult {
		calldata := buildCalldata(steps, amountIn)
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
		totalCost := new(uint256.Int).Add(amountIn, gasCostU)
		if !evmOut.Gt(totalCost) {
			return nil
		}
		profit := new(uint256.Int).Sub(&evmOut, totalCost)
		return &CycleResult{
			Hub:       hub,
			Steps:     steps,
			AmountIn:  *amountIn,
			AmountOut: evmOut,
			Profit:    *profit,
			GasUsed:   gasUsed,
			Calldata:  calldata,
		}
	}

	// Search range based on hub token
	lo := toWei(1, 15) // 0.001 AVAX
	hi := toWei(1000, 18) // 1000 AVAX
	switch hub {
	case USDC, USDT:
		lo = toWei(1, 4)      // 0.01 USDC
		hi = toWei(100000, 6) // 100k USDC
	case WETHe:
		lo = toWei(1, 13)   // 0.00001 ETH
		hi = toWei(100, 18) // 100 ETH
	}

	// Ternary search: 15 iterations on concave profit curve
	for i := 0; i < 15; i++ {
		diff := new(uint256.Int).Sub(hi, lo)
		third := new(uint256.Int).Div(diff, uint256.NewInt(3))
		if third.IsZero() {
			break
		}

		m1 := new(uint256.Int).Add(lo, third)
		m2 := new(uint256.Int).Sub(hi, third)

		r1 := evalEVM(m1)
		r2 := evalEVM(m2)

		p1, p2 := int64(-1), int64(-1)
		if r1 != nil {
			p1 = int64(r1.Profit.Uint64())
		}
		if r2 != nil {
			p2 = int64(r2.Profit.Uint64())
		}

		if p1 < p2 {
			lo = m1
		} else {
			hi = m2
		}
	}

	// Evaluate at midpoint of final range
	bestAmount := new(uint256.Int).Add(lo, hi)
	bestAmount.Div(bestAmount, uint256.NewInt(2))
	return evalEVM(bestAmount)
}
