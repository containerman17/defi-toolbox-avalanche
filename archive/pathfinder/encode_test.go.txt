package pathfinder

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

var (
	poolA = common.HexToAddress("0xAA")
	poolB = common.HexToAddress("0xBB")
	poolC = common.HexToAddress("0xCC")
	poolD = common.HexToAddress("0xDD")

	tokenUSDC  = common.HexToAddress("0x01")
	tokenWAVAX = common.HexToAddress("0x02")
	tokenWETH  = common.HexToAddress("0x03")
	tokenUSDT  = common.HexToAddress("0x04")
)

// decodeSquished extracts the flat arrays from swap() calldata for verification.
// Returns pools, poolTypes, tokenPairs, amounts (as uint64 for easy comparison).
func decodeSquished(calldata []byte) (pools []common.Address, poolTypes []int, tokens []common.Address, amounts []uint64) {
	if len(calldata) < 4 {
		return
	}
	buf := calldata[4:] // skip selector

	readOffset := func(pos int) int {
		return int(new(uint256.Int).SetBytes(buf[pos : pos+32]).Uint64())
	}
	readLen := func(off int) int {
		return int(new(uint256.Int).SetBytes(buf[off : off+32]).Uint64())
	}
	readAddr := func(off int) common.Address {
		return common.BytesToAddress(buf[off+12 : off+32])
	}
	readU64 := func(off int) uint64 {
		return new(uint256.Int).SetBytes(buf[off : off+32]).Uint64()
	}

	poolsOff := readOffset(0)
	typesOff := readOffset(32)
	tokensOff := readOffset(64)
	amountsOff := readOffset(96)

	n := readLen(poolsOff)
	for i := 0; i < n; i++ {
		pools = append(pools, readAddr(poolsOff+32+i*32))
	}

	n = readLen(typesOff)
	for i := 0; i < n; i++ {
		poolTypes = append(poolTypes, int(readU64(typesOff+32+i*32)))
	}

	n = readLen(tokensOff)
	for i := 0; i < n; i++ {
		tokens = append(tokens, readAddr(tokensOff+32+i*32))
	}

	n = readLen(amountsOff)
	for i := 0; i < n; i++ {
		amounts = append(amounts, readU64(amountsOff+32+i*32))
	}
	return
}

func TestEncodeSquished_TwoSingleHop(t *testing.T) {
	// Two single-hop legs: 400 USDC→WAVAX via poolA, 600 USDC→WAVAX via poolB
	routes := []SquishRoute{
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(400),
		},
		{
			Steps:  []RouteStep{{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(600),
		},
	}

	calldata := EncodeSquished(routes, uint256.NewInt(0))
	pools, poolTypes, tokens, amounts := decodeSquished(calldata)

	assertEq(t, "pools count", 2, len(pools))
	assertEq(t, "pool[0]", poolA, pools[0])
	assertEq(t, "pool[1]", poolB, pools[1])
	assertEq(t, "type[0]", 2, poolTypes[0])
	assertEq(t, "type[1]", 0, poolTypes[1])
	assertEq(t, "tokens count", 4, len(tokens))
	assertEq(t, "tokens[0]", tokenUSDC, tokens[0])
	assertEq(t, "tokens[1]", tokenWAVAX, tokens[1])
	assertEq(t, "tokens[2]", tokenUSDC, tokens[2])
	assertEq(t, "tokens[3]", tokenWAVAX, tokens[3])
	assertEq(t, "amount[0]", uint64(400), amounts[0])
	assertEq(t, "amount[1]", uint64(600), amounts[1])
}

func TestEncodeSquished_SingleHopPlusMultiHop(t *testing.T) {
	// Leg 1: 300 USDC→WAVAX via poolA (1 hop)
	// Leg 2: 700 USDC→WETH→WAVAX via poolB→poolC (2 hops)
	routes := []SquishRoute{
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(300),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(700),
		},
	}

	calldata := EncodeSquished(routes, uint256.NewInt(0))
	pools, poolTypes, tokens, amounts := decodeSquished(calldata)

	assertEq(t, "pools count", 3, len(pools))
	assertEq(t, "pool[0]", poolA, pools[0])
	assertEq(t, "pool[1]", poolB, pools[1])
	assertEq(t, "pool[2]", poolC, pools[2])
	assertEq(t, "type[0]", 2, poolTypes[0])
	assertEq(t, "type[1]", 0, poolTypes[1])
	assertEq(t, "type[2]", 3, poolTypes[2])
	// Tokens: leg1=[USDC,WAVAX], leg2=[USDC,WETH, WETH,WAVAX]
	assertEq(t, "tokens count", 6, len(tokens))
	assertEq(t, "tokens[0]", tokenUSDC, tokens[0])
	assertEq(t, "tokens[1]", tokenWAVAX, tokens[1])
	assertEq(t, "tokens[2]", tokenUSDC, tokens[2])
	assertEq(t, "tokens[3]", tokenWETH, tokens[3])
	assertEq(t, "tokens[4]", tokenWETH, tokens[4])
	assertEq(t, "tokens[5]", tokenWAVAX, tokens[5])
	// Amounts: leg1 first step=300, leg2 first step=700, leg2 second step=0
	assertEq(t, "amount[0]", uint64(300), amounts[0])
	assertEq(t, "amount[1]", uint64(700), amounts[1])
	assertEq(t, "amount[2]", uint64(0), amounts[2])
}

func TestEncodeSquished_SharedFirstHop(t *testing.T) {
	// Both legs start with DODO (poolA), diverge after:
	// Leg 1: USDC→(poolA:DODO)→WETH→(poolB:LFJ)→WAVAX  vol=500
	// Leg 2: USDC→(poolA:DODO)→WETH→(poolC:V3)→WAVAX   vol=500
	// Same first hop pool, different second hop
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 4, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolB, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 4, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 0, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
	}

	calldata := EncodeSquished(routes, uint256.NewInt(0))
	pools, _, tokens, amounts := decodeSquished(calldata)

	// 4 total steps: 2 per leg
	assertEq(t, "pools count", 4, len(pools))
	assertEq(t, "pool[0]", poolA, pools[0]) // leg1 hop1: DODO
	assertEq(t, "pool[1]", poolB, pools[1]) // leg1 hop2: LFJ
	assertEq(t, "pool[2]", poolA, pools[2]) // leg2 hop1: DODO (same pool!)
	assertEq(t, "pool[3]", poolC, pools[3]) // leg2 hop2: V3

	// Amounts: leg1=[500,0], leg2=[500,0]
	assertEq(t, "amount[0]", uint64(500), amounts[0])
	assertEq(t, "amount[1]", uint64(0), amounts[1])
	assertEq(t, "amount[2]", uint64(500), amounts[2])
	assertEq(t, "amount[3]", uint64(0), amounts[3])

	// Tokens: 8 entries (4 steps * 2)
	assertEq(t, "tokens count", 8, len(tokens))
}

func TestEncodeSquished_SharedLastHop(t *testing.T) {
	// Both legs end at the same pool (poolD: LFJ_V2):
	// Leg 1: USDC→(poolA:DODO)→WETH→(poolD:LFJ)→WAVAX  vol=400
	// Leg 2: USDC→(poolB:V3)→USDT→(poolD:LFJ)→WAVAX    vol=600
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 4, TokenIn: tokenUSDC, TokenOut: tokenWETH},
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

	calldata := EncodeSquished(routes, uint256.NewInt(0))
	pools, _, tokens, amounts := decodeSquished(calldata)

	assertEq(t, "pools count", 4, len(pools))
	assertEq(t, "pool[0]", poolA, pools[0])
	assertEq(t, "pool[1]", poolD, pools[1]) // shared last hop
	assertEq(t, "pool[2]", poolB, pools[2])
	assertEq(t, "pool[3]", poolD, pools[3]) // shared last hop again

	assertEq(t, "amount[0]", uint64(400), amounts[0])
	assertEq(t, "amount[1]", uint64(0), amounts[1])
	assertEq(t, "amount[2]", uint64(600), amounts[2])
	assertEq(t, "amount[3]", uint64(0), amounts[3])

	// Tokens
	assertEq(t, "tokens count", 8, len(tokens))
	assertEq(t, "tokens[0]", tokenUSDC, tokens[0])   // leg1 hop1 in
	assertEq(t, "tokens[1]", tokenWETH, tokens[1])    // leg1 hop1 out
	assertEq(t, "tokens[2]", tokenWETH, tokens[2])    // leg1 hop2 in
	assertEq(t, "tokens[3]", tokenWAVAX, tokens[3])   // leg1 hop2 out
	assertEq(t, "tokens[4]", tokenUSDC, tokens[4])    // leg2 hop1 in
	assertEq(t, "tokens[5]", tokenUSDT, tokens[5])    // leg2 hop1 out
	assertEq(t, "tokens[6]", tokenUSDT, tokens[6])    // leg2 hop2 in
	assertEq(t, "tokens[7]", tokenWAVAX, tokens[7])   // leg2 hop2 out
}

func TestEncodeSquished_ThreeLegs(t *testing.T) {
	// 3 legs with mixed hop counts:
	// Leg 1: 1-hop  USDC→WAVAX via poolA  (200)
	// Leg 2: 2-hop  USDC→WETH→WAVAX via poolB→poolC  (500)
	// Leg 3: 1-hop  USDC→WAVAX via poolD  (300)
	routes := []SquishRoute{
		{
			Steps:  []RouteStep{{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(200),
		},
		{
			Steps: []RouteStep{
				{Pool: poolB, PoolType: 0, TokenIn: tokenUSDC, TokenOut: tokenWETH},
				{Pool: poolC, PoolType: 3, TokenIn: tokenWETH, TokenOut: tokenWAVAX},
			},
			Volume: uint256.NewInt(500),
		},
		{
			Steps:  []RouteStep{{Pool: poolD, PoolType: 4, TokenIn: tokenUSDC, TokenOut: tokenWAVAX}},
			Volume: uint256.NewInt(300),
		},
	}

	calldata := EncodeSquished(routes, uint256.NewInt(0))
	pools, _, tokens, amounts := decodeSquished(calldata)

	assertEq(t, "pools count", 4, len(pools))
	assertEq(t, "amounts count", 4, len(amounts))

	// leg1: amount=200
	// leg2: amount=500, then 0
	// leg3: amount=300
	assertEq(t, "amount[0]", uint64(200), amounts[0])
	assertEq(t, "amount[1]", uint64(500), amounts[1])
	assertEq(t, "amount[2]", uint64(0), amounts[2])
	assertEq(t, "amount[3]", uint64(300), amounts[3])

	// Tokens: 2 + 4 + 2 = 8
	assertEq(t, "tokens count", 8, len(tokens))
}

func TestEncodeSquished_ExtraData(t *testing.T) {
	// Verify extraData passes through correctly
	routes := []SquishRoute{
		{
			Steps: []RouteStep{
				{Pool: poolA, PoolType: 2, TokenIn: tokenUSDC, TokenOut: tokenWAVAX, ExtraData: "fee=25"},
			},
			Volume: uint256.NewInt(1000),
		},
	}

	// Should not panic — extraData is encoded
	calldata := EncodeSquished(routes, uint256.NewInt(0))
	if len(calldata) < 4 {
		t.Fatal("calldata too short")
	}
	// Verify selector
	if calldata[0] != 0xf3 || calldata[1] != 0xb1 || calldata[2] != 0xb2 || calldata[3] != 0x3a {
		t.Fatalf("wrong selector: %x", calldata[:4])
	}
}

func assertEq[T comparable](t *testing.T, name string, want, got T) {
	t.Helper()
	if want != got {
		t.Errorf("%s: want %v, got %v", name, want, got)
	}
}
