package pathfinder

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Test pools and tokens are defined in encode_test.go:
// poolA, poolB, poolC, poolD
// tokenUSDC, tokenWAVAX, tokenWETH, tokenUSDT

// Additional pools for merge tests
var (
	poolE = common.HexToAddress("0xEE")
	poolF = common.HexToAddress("0xFF")
)

func TestMerge_SingleRoute(t *testing.T) {
	routes := []SquishRoute{
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(1000),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 1, len(steps))
	assertEq(t, "pool", poolA, steps[0].Pool)
	assertEq(t, "amount", uint64(1000), amounts[0].Uint64())
}

func TestMerge_DisjointPaths(t *testing.T) {
	// Two completely different routes — no merge possible
	routes := []SquishRoute{
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(500),
		},
		{
			Steps:  []RouteStep{{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(300),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 2, len(steps))
	assertEq(t, "amount[0]", uint64(500), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(300), amounts[1].Uint64())
}

func TestMerge_IdenticalSingleHop(t *testing.T) {
	// Two legs with same single-hop path → merge into one, sum volumes
	routes := []SquishRoute{
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(400),
		},
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(600),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 1, len(steps))
	assertEq(t, "pool", poolA, steps[0].Pool)
	assertEq(t, "amount", uint64(1000), amounts[0].Uint64())
}

func TestMerge_IdenticalMultiHop(t *testing.T) {
	// Two legs with same 2-hop path → merge into one, sum volumes
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(700),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 2, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "amount[0]", uint64(1000), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
}

func TestMerge_SharedLastHop(t *testing.T) {
	// Two 2-hop legs ending at the same pool with same tokens:
	// Leg 1: poolA(USDC→WETH) → poolC(WETH→WAVAX)  vol=400
	// Leg 2: poolB(USDC→WETH) → poolC(WETH→WAVAX)  vol=600
	//
	// Merged: poolA(400), poolB(600), poolC(0)  — 3 steps instead of 4
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(400),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(600),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 3, len(steps))
	// Feeders first, shared last
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolC, steps[2].Pool)
	assertEq(t, "amount[0]", uint64(400), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(600), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())
}

func TestMerge_SharedSuffixTwoSteps(t *testing.T) {
	// Two 3-hop legs sharing last 2 hops:
	// Leg 1: poolA(USDC→WETH) → poolD(WETH→USDT) → poolE(USDT→WAVAX)  vol=300
	// Leg 2: poolB(USDC→WETH) → poolD(WETH→USDT) → poolE(USDT→WAVAX)  vol=700
	//
	// Merged: poolA(300), poolB(700), poolD(0), poolE(0)  — 4 steps instead of 6
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolD, PoolType: 4, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolD, PoolType: 4, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(700),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 4, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolD, steps[2].Pool)
	assertEq(t, "pool[3]", poolE, steps[3].Pool)
	assertEq(t, "amount[0]", uint64(300), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(700), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())
}

func TestMerge_SharedFirstHop_NoMerge(t *testing.T) {
	// Two legs with same FIRST hop but different second → NOT merged.
	// Shared first hops can't be merged because we can't split the output.
	//
	// Leg 1: poolA(USDC→WETH) → poolB(WETH→WAVAX)  vol=500
	// Leg 2: poolA(USDC→WETH) → poolC(WETH→USDT)   vol=500
	//
	// The trie sees reversed paths:
	//   B(WETH→WAVAX) → A(USDC→WETH)
	//   C(WETH→USDT)  → A(USDC→WETH)
	// Different first nodes (B vs C) → A appears in two branches → NOT merged.
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(500),
		},
	}
	steps, amounts := MergeRoutes(routes)
	// 4 steps — no merging occurred
	assertEq(t, "steps", 4, len(steps))
	// Each branch emitted independently: A(500), B(0), A(500), C(0)
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolA, steps[2].Pool)
	assertEq(t, "pool[3]", poolC, steps[3].Pool)
	assertEq(t, "amount[0]", uint64(500), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(500), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())
}

func TestMerge_NestedSuffix(t *testing.T) {
	// Three legs with nested shared suffixes:
	// Leg 1: poolA(USDC→WETH)  → poolD(WETH→USDT) → poolE(USDT→WAVAX) vol=300
	// Leg 2: poolB(USDC→WETH)  → poolD(WETH→USDT) → poolE(USDT→WAVAX) vol=700
	// Leg 3: poolC(USDC→USDT)  → poolE(USDT→WAVAX)                    vol=500
	//
	// Legs 1+2 share suffix [poolD, poolE]. Leg 3 shares suffix [poolE] with both.
	// Merged: poolA(300), poolB(700), poolD(0), poolC(500), poolE(0)
	// = 5 steps instead of 8
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolD, PoolType: 4, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolD, PoolType: 4, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(700),
		},
		{
			Steps: []RouteStep{
				{Pool: poolC, PoolType: 3, TokenIn: tokenUSDC, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 5, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolD, steps[2].Pool)
	assertEq(t, "pool[3]", poolC, steps[3].Pool)
	assertEq(t, "pool[4]", poolE, steps[4].Pool)
	assertEq(t, "amount[0]", uint64(300), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(700), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(500), amounts[3].Uint64())
	assertEq(t, "amount[4]", uint64(0), amounts[4].Uint64())
}

func TestMerge_ThreeIdentical(t *testing.T) {
	// Three identical 2-hop paths → 1 merged leg with summed volume
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(200),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 2, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "amount[0]", uint64(1000), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
}

func TestMerge_MixedIdenticalAndShared(t *testing.T) {
	// Leg 1: poolA→poolC  vol=200  (identical to leg 2)
	// Leg 2: poolA→poolC  vol=300  (identical to leg 1)
	// Leg 3: poolB→poolC  vol=500  (shares last hop with legs 1+2)
	//
	// After trie construction:
	//   root → poolC → poolA [vol=200,300]
	//                → poolB [vol=500]
	//
	// Merged: poolA(500), poolB(500), poolC(0) — 3 steps instead of 6 naive
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(200),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 3, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolC, steps[2].Pool)
	assertEq(t, "amount[0]", uint64(500), amounts[0].Uint64()) // 200+300
	assertEq(t, "amount[1]", uint64(500), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())
}

func TestMerge_SharedLastHopDifferentTokens_NoMerge(t *testing.T) {
	// Same pool at the end but different tokenIn → NOT merged
	// Leg 1: poolA(USDC→WETH)  → poolD(WETH→WAVAX)  vol=400
	// Leg 2: poolB(USDC→USDT)  → poolD(USDT→WAVAX)  vol=600
	// poolD appears in both but with WETH vs USDT as input — different step keys
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolD, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(400),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenUSDT},
				{Pool: poolD, PoolType: 3, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(600),
		},
	}
	steps, amounts := MergeRoutes(routes)
	// 4 steps — no merge because poolD has different tokenIn
	assertEq(t, "steps", 4, len(steps))
	assertEq(t, "amount[0]", uint64(400), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(600), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())
}

func TestMerge_Empty(t *testing.T) {
	steps, amounts := MergeRoutes(nil)
	assertEq(t, "steps", 0, len(steps))
	assertEq(t, "amounts", 0, len(amounts))
}

// ── First-hop merging tests (require quoter) ────────────────────────

// fakeQuoter returns 2x the input amount as the intermediate output.
// This simulates a pool that doubles value (unrealistic but easy to verify).
func fakeQuoter(step RouteStep, amountIn *uint256.Int) uint256.Int {
	out := new(uint256.Int).Mul(amountIn, uint256.NewInt(2))
	return *out
}

func TestMergeWithQuoter_SharedFirstHop(t *testing.T) {
	// Two 2-hop legs with same first hop but different second hop:
	// Leg 1: poolA(USDC→WETH) → poolB(WETH→WAVAX)  vol=500
	// Leg 2: poolA(USDC→WETH) → poolC(WETH→USDT)   vol=300
	//
	// Without quoter: 4 steps (no merge)
	// With quoter: 3 steps
	//   poolA(800, USDC→WETH)   — merged, total volume
	//   poolB(1000, WETH→WAVAX) — explicit intermediate (fakeQuoter: 500*2=1000)
	//   poolC(0, WETH→USDT)     — balance (leftovers)
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(300),
		},
	}

	// Without quoter — no first-hop merge
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "no-quoter steps", 4, len(steps))

	// With quoter — first hop merged
	steps, amounts = MergeRoutesWithQuoter(routes, fakeQuoter)
	assertEq(t, "steps", 3, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)    // merged first hop
	assertEq(t, "pool[1]", poolB, steps[1].Pool)    // first consumer (explicit)
	assertEq(t, "pool[2]", poolC, steps[2].Pool)    // last consumer (balance)
	assertEq(t, "amount[0]", uint64(800), amounts[0].Uint64())  // 500+300
	assertEq(t, "amount[1]", uint64(1000), amounts[1].Uint64()) // fakeQuoter(500) = 1000
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())    // balance
}

func TestMergeWithQuoter_ThreeWaySharedFirstHop(t *testing.T) {
	// Three legs all starting with poolA, diverging to B, C, D:
	// Leg 1: poolA(USDC→WETH) → poolB(WETH→WAVAX) vol=100
	// Leg 2: poolA(USDC→WETH) → poolC(WETH→USDT)  vol=200
	// Leg 3: poolA(USDC→WETH) → poolD(WETH→WAVAX) vol=300
	//
	// Merged: poolA(600), poolB(200), poolC(400), poolD(0)
	// = 4 steps instead of 6
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(100),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(200),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolD, PoolType: 4, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
	}

	steps, amounts := MergeRoutesWithQuoter(routes, fakeQuoter)
	assertEq(t, "steps", 4, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolC, steps[2].Pool)
	assertEq(t, "pool[3]", poolD, steps[3].Pool)
	assertEq(t, "amount[0]", uint64(600), amounts[0].Uint64())  // total
	assertEq(t, "amount[1]", uint64(200), amounts[1].Uint64())  // fakeQuoter(100)
	assertEq(t, "amount[2]", uint64(400), amounts[2].Uint64())  // fakeQuoter(200)
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())    // balance
}

func TestMergeWithQuoter_SharedFirstAndLastHop(t *testing.T) {
	// Legs share BOTH first and last hop, different middle:
	// Leg 1: poolA(USDC→WETH) → poolB(WETH→USDT) → poolE(USDT→WAVAX) vol=400
	// Leg 2: poolA(USDC→WETH) → poolC(WETH→USDT) → poolE(USDT→WAVAX) vol=600
	//
	// Phase 1 (suffix): poolE is shared → poolA(400),poolB(0), poolA(600),poolC(0), poolE(0)
	// Phase 2 (first-hop): two branches start with poolA → merge
	//   poolA(1000), poolB(800 = fakeQuoter(400)), poolC(0), poolE(0)
	// = 4 steps instead of 6 naive
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 3, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(400),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 3, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(600),
		},
	}

	steps, amounts := MergeRoutesWithQuoter(routes, fakeQuoter)
	assertEq(t, "steps", 4, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)    // merged first hop
	assertEq(t, "pool[1]", poolB, steps[1].Pool)    // first tail, explicit intermediate
	assertEq(t, "pool[2]", poolC, steps[2].Pool)    // last tail, balance
	assertEq(t, "pool[3]", poolE, steps[3].Pool)    // shared suffix
	assertEq(t, "amount[0]", uint64(1000), amounts[0].Uint64()) // 400+600
	assertEq(t, "amount[1]", uint64(800), amounts[1].Uint64())  // fakeQuoter(400)
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())    // balance
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())    // shared suffix
}

func TestMergeWithQuoter_RealisticSplitter(t *testing.T) {
	// Realistic: same as TestMerge_RealisticSplitterOutput but with quoter.
	// Path1 (algebra→lfj_v2) x3: vol=2500 each
	// Path2 (algebra→pharaoh) x1: vol=7500
	// Path3 (direct) x1: vol=5000
	//
	// Without quoter: 5 steps (identical merge + no first-hop merge)
	// With quoter: algebra is shared first hop between Path1 and Path2
	//   algebra(15000), lfjV2(fakeQuoter(7500)=15000), pharaoh(0), direct(5000)
	// = 4 steps instead of 5
	algebra := poolA
	lfjV2 := poolB
	pharaoh := poolC
	directPool := poolD

	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfjV2, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(2500),
		},
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfjV2, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(2500),
		},
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfjV2, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(2500),
		},
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: pharaoh, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(7500),
		},
		{
			Steps: []RouteStep{
				{Pool: directPool, PoolType: 4, TokenIn: tokenWAVAX, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(5000),
		},
	}

	steps, amounts := MergeRoutesWithQuoter(routes, fakeQuoter)
	assertEq(t, "steps", 4, len(steps))
	assertEq(t, "pool[0]", algebra, steps[0].Pool)
	assertEq(t, "pool[1]", lfjV2, steps[1].Pool)
	assertEq(t, "pool[2]", pharaoh, steps[2].Pool)
	assertEq(t, "pool[3]", directPool, steps[3].Pool)
	assertEq(t, "amount[0]", uint64(15000), amounts[0].Uint64()) // 7500+7500
	assertEq(t, "amount[1]", uint64(15000), amounts[1].Uint64()) // fakeQuoter(7500)
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())     // balance
	assertEq(t, "amount[3]", uint64(5000), amounts[3].Uint64())  // unrelated
}

func TestMerge_ManyIdenticalLegs(t *testing.T) {
	// Realistic: splitter produces 15 identical legs through the same path.
	// All should collapse to a single 2-step route with summed volume.
	var routes []SquishRoute
	for i := 0; i < 15; i++ {
		routes = append(routes, SquishRoute{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(100),
		})
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 2, len(steps))
	assertEq(t, "amount[0]", uint64(1500), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
}

func TestMerge_FourWaySharedSuffix(t *testing.T) {
	// Four different feeders all converging on the same last pool:
	// Leg 1: poolA(USDC→WETH) → poolE(WETH→WAVAX)  vol=100
	// Leg 2: poolB(USDC→WETH) → poolE(WETH→WAVAX)  vol=200
	// Leg 3: poolC(USDC→WETH) → poolE(WETH→WAVAX)  vol=300
	// Leg 4: poolD(USDC→WETH) → poolE(WETH→WAVAX)  vol=400
	//
	// Merged: A(100), B(200), C(300), D(400), E(0) — 5 steps instead of 8
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolE, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(100),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolE, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(200),
		},
		{
			Steps: []RouteStep{
				{Pool: poolC, PoolType: 3, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolE, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolD, PoolType: 4, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolE, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(400),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 5, len(steps))
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	assertEq(t, "pool[2]", poolC, steps[2].Pool)
	assertEq(t, "pool[3]", poolD, steps[3].Pool)
	assertEq(t, "pool[4]", poolE, steps[4].Pool)
	assertEq(t, "amount[0]", uint64(100), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(200), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(300), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(400), amounts[3].Uint64())
	assertEq(t, "amount[4]", uint64(0), amounts[4].Uint64())
}

func TestMerge_RealisticSplitterOutput(t *testing.T) {
	// Realistic scenario: optimized splitter produces 5 legs across 3 distinct paths.
	// Path1 (algebra→lfj_v2): 3 legs with vol 2500 each
	// Path2 (algebra→pharaoh): 1 leg with vol 7500
	// Path3 (single-hop lfj_v2 direct): 1 leg with vol 5000
	//
	// Path1 legs are identical → merge to 1 leg (vol=7500)
	// Path1 and Path2 share first hop (algebra) → NOT merged (different suffix)
	// Net: 3 distinct paths, 5 total steps (was 10)
	algebra := poolA
	lfjV2 := poolB
	pharaoh := poolC
	directPool := poolD

	routes := []SquishRoute{
		// Path1 x3
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfjV2, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(2500),
		},
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfjV2, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(2500),
		},
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfjV2, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(2500),
		},
		// Path2 x1
		{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: pharaoh, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(7500),
		},
		// Path3 x1
		{
			Steps: []RouteStep{
				{Pool: directPool, PoolType: 4, TokenIn: tokenWAVAX, TokenOut: tokenUSDT},
			},
			Volume: uint256.NewInt(5000),
		},
	}
	steps, amounts := MergeRoutes(routes)

	// Path1 (3 identical) → algebra(7500), lfjV2(0)  = 2 steps
	// Path2 → algebra(7500), pharaoh(0)              = 2 steps
	// Path3 → directPool(5000)                       = 1 step
	// Total: 5 steps (was 10 naive)
	//
	// BUT Path1 and Path2 share last step? No:
	//   Path1: algebra→lfjV2(WETH→USDT)
	//   Path2: algebra→pharaoh(WETH→USDT)
	// Different last pool. No shared suffix.
	//
	// Do they share first hop? Yes: algebra(WAVAX→WETH).
	// But reversed trie sees: lfjV2→algebra vs pharaoh→algebra.
	// lfjV2 ≠ pharaoh → different branches → algebra appears twice. Correct!
	assertEq(t, "steps", 5, len(steps))

	// Branch 1 (Path1 merged): algebra(7500), lfjV2(0)
	assertEq(t, "pool[0]", algebra, steps[0].Pool)
	assertEq(t, "pool[1]", lfjV2, steps[1].Pool)
	assertEq(t, "amount[0]", uint64(7500), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())

	// Branch 2 (Path2): algebra(7500), pharaoh(0)
	assertEq(t, "pool[2]", algebra, steps[2].Pool)
	assertEq(t, "pool[3]", pharaoh, steps[3].Pool)
	assertEq(t, "amount[2]", uint64(7500), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())

	// Branch 3 (Path3): directPool(5000)
	assertEq(t, "pool[4]", directPool, steps[4].Pool)
	assertEq(t, "amount[4]", uint64(5000), amounts[4].Uint64())
}

func TestMerge_SharedFirstAndLastHop(t *testing.T) {
	// Both legs share the same FIRST hop AND the same LAST hop, but different middle:
	// Leg 1: poolA(USDC→WETH) → poolB(WETH→USDT) → poolE(USDT→WAVAX)  vol=400
	// Leg 2: poolA(USDC→WETH) → poolC(WETH→USDT) → poolE(USDT→WAVAX)  vol=600
	//
	// Shared last hop: poolE(USDT→WAVAX) — should be merged.
	// Shared first hop: poolA(USDC→WETH) — should NOT be merged.
	//
	// Reversed trie:
	//   poolE(USDT→WAVAX)
	//     ├── poolB(WETH→USDT) → poolA(USDC→WETH) [vol=400]
	//     └── poolC(WETH→USDT) → poolA(USDC→WETH) [vol=600]
	//
	// Merged: poolA(400), poolB(0), poolA(600), poolC(0), poolE(0)
	// = 5 steps instead of 6. Only last hop merged.
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 3, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(400),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: poolE, PoolType: 3, TokenIn: tokenUSDT, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(600),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 5, len(steps))
	// Branch 1: poolA(400), poolB(0)
	assertEq(t, "pool[0]", poolA, steps[0].Pool)
	assertEq(t, "pool[1]", poolB, steps[1].Pool)
	// Branch 2: poolA(600), poolC(0)
	assertEq(t, "pool[2]", poolA, steps[2].Pool)
	assertEq(t, "pool[3]", poolC, steps[3].Pool)
	// Shared: poolE(0)
	assertEq(t, "pool[4]", poolE, steps[4].Pool)
	assertEq(t, "amount[0]", uint64(400), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(600), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())
	assertEq(t, "amount[4]", uint64(0), amounts[4].Uint64())
}

func TestMerge_ExtraDataPreserved(t *testing.T) {
	// Verify ExtraData and PoolType survive the merge
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 9, TokenIn: tokenUSDC, TokenOut: tokenWETH, ExtraData: "fee=25,ts=1"},
				{Pool: poolB, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
		{
			Steps: []RouteStep{
				{Pool: poolC, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
	}
	steps, amounts := MergeRoutes(routes)
	assertEq(t, "steps", 3, len(steps))
	assertEq(t, "pool[0] type", 9, steps[0].PoolType)
	assertEq(t, "pool[0] extra", "fee=25,ts=1", steps[0].ExtraData)
	assertEq(t, "pool[1] type", 2, steps[1].PoolType)
	assertEq(t, "pool[2] type", 3, steps[2].PoolType)
	assertEq(t, "amount[0]", uint64(500), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(500), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(0), amounts[2].Uint64())
}

func TestMerge_StepCountReduction(t *testing.T) {
	// Verify the step reduction ratio for a realistic 20-chunk scenario.
	// 15 legs via path1 (algebra→lfj), 5 legs via path2 (algebra→pharaoh),
	// both ending at the same final pool for USDT→WAVAX.
	//
	// Path1: algebra(WAVAX→WETH) → lfj(WETH→USDT) → final(USDT→WAVAX)
	// Path2: pharaoh(WAVAX→USDT) → final(USDT→WAVAX)
	//
	// Naive: 15*3 + 5*2 = 55 steps
	// Merged: algebra(summed) → lfj(0) → final is shared with path2
	//   path1 legs identical → 1 branch: algebra(sum), lfj(0)
	//   path2 legs identical → 1 branch: pharaoh(sum)
	//   final(0) shared
	//   = 4 steps total
	algebra := poolA
	lfj := poolB
	pharaoh := poolC
	finalPool := poolD

	var routes []SquishRoute
	for i := 0; i < 15; i++ {
		routes = append(routes, SquishRoute{
			Steps: []RouteStep{
				{Pool: algebra, PoolType: 2, TokenIn: tokenWAVAX, TokenOut: tokenWETH},
				{Pool: lfj, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenUSDT},
				{Pool: finalPool, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenUSDC},
			},
			Volume: uint256.NewInt(2500),
		})
	}
	for i := 0; i < 5; i++ {
		routes = append(routes, SquishRoute{
			Steps: []RouteStep{
				{Pool: pharaoh, PoolType: 0, TokenIn: tokenWAVAX, TokenOut: tokenUSDT},
				{Pool: finalPool, PoolType: 0, TokenIn: tokenUSDT, TokenOut: tokenUSDC},
			},
			Volume: uint256.NewInt(5000),
		})
	}

	steps, amounts := MergeRoutes(routes)

	// Trie:
	//   root → finalPool(USDT→USDC)
	//            ├── lfj(WETH→USDT) → algebra(WAVAX→WETH) [vol=15*2500=37500]
	//            └── pharaoh(WAVAX→USDT) [vol=5*5000=25000]
	//
	// Post-order: algebra(37500), lfj(0), pharaoh(25000), finalPool(0)
	assertEq(t, "steps", 4, len(steps))
	assertEq(t, "pool[0]", algebra, steps[0].Pool)
	assertEq(t, "pool[1]", lfj, steps[1].Pool)
	assertEq(t, "pool[2]", pharaoh, steps[2].Pool)
	assertEq(t, "pool[3]", finalPool, steps[3].Pool)
	assertEq(t, "amount[0]", uint64(37500), amounts[0].Uint64())
	assertEq(t, "amount[1]", uint64(0), amounts[1].Uint64())
	assertEq(t, "amount[2]", uint64(25000), amounts[2].Uint64())
	assertEq(t, "amount[3]", uint64(0), amounts[3].Uint64())

	// 4 steps instead of 55 — 93% reduction
	naiveSteps := 15*3 + 5*2
	t.Logf("naive=%d merged=%d reduction=%.0f%%", naiveSteps, len(steps),
		float64(naiveSteps-len(steps))/float64(naiveSteps)*100)
}
