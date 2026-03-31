package main

import (
	"defi-toolbox/formulas"
	"math"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

const NumSizeBuckets = 5

// SizeBuckets are the 5 WAVAX-equivalent input amounts for rate screening.
// 0.001, 0.01, 0.1, 1, 10 AVAX (in wei).
var SizeBuckets = [NumSizeBuckets]*uint256.Int{
	new(uint256.Int).Mul(uint256.NewInt(1), uint256.NewInt(1_000_000_000_000_000)),       // 0.001 AVAX
	new(uint256.Int).Mul(uint256.NewInt(10), uint256.NewInt(1_000_000_000_000_000)),      // 0.01 AVAX
	new(uint256.Int).Mul(uint256.NewInt(100), uint256.NewInt(1_000_000_000_000_000)),     // 0.1 AVAX
	new(uint256.Int).Mul(uint256.NewInt(1000), uint256.NewInt(1_000_000_000_000_000)),    // 1 AVAX
	new(uint256.Int).Mul(uint256.NewInt(10000), uint256.NewInt(1_000_000_000_000_000)),   // 10 AVAX
}

// poolRate stores rates for one pool: [direction][sizeBucket].
type poolRate [2][NumSizeBuckets]float64

// RateTable holds float64 rate ratios (out/in) per pool per direction per size bucket.
type RateTable struct {
	rates     map[common.Address]*poolRate
	poolToken0 map[common.Address]common.Address // pool → token0
	deadPools  map[common.Address]bool            // pools that failed to build (skip until invalidated)
}

func NewRateTable() *RateTable {
	return &RateTable{
		rates:      make(map[common.Address]*poolRate),
		poolToken0: make(map[common.Address]common.Address),
		deadPools:  make(map[common.Address]bool),
	}
}

// ClearDead removes a pool from the dead set (call when pool is invalidated/dirty).
func (rt *RateTable) ClearDead(pool common.Address) {
	delete(rt.deadPools, pool)
}

// SetPoolToken0 registers a pool's token0 for direction resolution.
func (rt *RateTable) SetPoolToken0(pool, token0 common.Address) {
	rt.poolToken0[pool] = token0
}

// Update re-quotes a pool at all 5 size buckets × 2 directions using the PoolManager.
// Returns true if the pool was successfully quoted at any size.
func (rt *RateTable) Update(pool common.Address, pm *formulas.PoolManager) bool {
	if rt.deadPools[pool] {
		return false
	}

	r := &poolRate{}
	anyOk := false

	for dir := 0; dir < 2; dir++ {
		zeroForOne := dir == 0
		for s := 0; s < NumSizeBuckets; s++ {
			out := pm.Quote(pool, SizeBuckets[s], zeroForOne)
			if !out.IsZero() {
				// Rate = out / in as float64
				inF := float64FromU256(SizeBuckets[s])
				outF := float64FromU256(&out)
				if inF > 0 {
					r[dir][s] = outF / inF
					anyOk = true
				}
			}
		}
	}

	if !anyOk {
		rt.deadPools[pool] = true
		return false
	}
	rt.rates[pool] = r
	return true
}

// Get returns the rate for a pool, direction, and size bucket.
// direction: 0 = token0→token1 (zeroForOne=true), 1 = token1→token0.
func (rt *RateTable) Get(pool common.Address, direction int, size int) float64 {
	r, ok := rt.rates[pool]
	if !ok {
		return 0
	}
	return r[direction][size]
}

// ScreenCycle multiplies rates along a cycle for a given size bucket.
// Returns the product (> 1.0 means profitable before gas).
func (rt *RateTable) ScreenCycle(c *Cycle, pt *PoolTable, size int) float64 {
	product := 1.0
	for i := 0; i < int(c.Hops); i++ {
		dir := 0
		if !c.Dirs[i] {
			dir = 1
		}
		r := rt.Get(pt.Addr(c.Pools[i]), dir, size)
		if r <= 0 {
			return 0 // missing rate, can't screen
		}
		product *= r
	}
	return product
}

// float64FromU256 converts a uint256 to float64 (lossy but fine for rate screening).
func float64FromU256(v *uint256.Int) float64 {
	if v.IsUint64() {
		return float64(v.Uint64())
	}
	// For large values, use the top bits
	b := v.Bytes32()
	// Find leading non-zero byte
	shift := 0
	for i := 0; i < 32; i++ {
		if b[i] != 0 {
			shift = (32 - i) * 8
			break
		}
	}
	if shift <= 64 {
		return float64(v.Uint64())
	}
	// Take top 8 bytes as float and shift
	hi := new(uint256.Int).Rsh(v, uint(shift-64))
	return float64(hi.Uint64()) * math.Pow(2, float64(shift-64))
}
