package formulas

import (
	"math/big"


	
)

// StateReader reads a 32-byte storage slot from a contract. All formula quote
// functions take this as a callback — they never own or cache state. The caller
// decides where data comes from (fresh node read, in-memory cache, snapshot, etc.).
// Returns [32]byte (stack-allocated, fixed size) — storage slots are always 32 bytes.
type StateReader func(contractAddr string, slot *big.Int) ([32]byte, error)

// NewRPCStateReader returns a StateReader that reads from the node via eth_getStorageAt.

// ReadStorageSlot reads a single storage slot via eth_getStorageAt (cost 0.01).

// Shared math utilities used by multiple formula files (algebra.go, v3.go, v4.go).

var (
	algebraMinTick  int32 = -887272
	algebraMaxTick  int32 = 887272
	algebraMinSqrtRatio   = big.NewInt(4295128739)
	algebraMaxSqrtRatio   = func() *big.Int {
		v, _ := new(big.Int).SetString("1461446703485210103287273052203988822378723970342", 10)
		return v
	}()
	maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func getSqrtRatioAtTick(tick int32) *big.Int {
	absTick := tick
	if absTick < 0 {
		absTick = -absTick
	}
	var ratio *big.Int
	if absTick&0x1 != 0 {
		ratio, _ = new(big.Int).SetString("fffcb933bd6fad37aa2d162d1a594001", 16)
	} else {
		ratio = new(big.Int).Lsh(big.NewInt(1), 128)
	}
	type mp struct{ bit int32; hex string }
	magics := []mp{
		{0x2, "fff97272373d413259a46990580e213a"}, {0x4, "fff2e50f5f656932ef12357cf3c7fdcc"},
		{0x8, "ffe5caca7e10e4e61c3624eaa0941cd0"}, {0x10, "ffcb9843d60f6159c9db58835c926644"},
		{0x20, "ff973b41fa98c081472e6896dfb254c0"}, {0x40, "ff2ea16466c96a3843ec78b326b52861"},
		{0x80, "fe5dee046a99a2a811c461f1969c3053"}, {0x100, "fcbe86c7900a88aedcffc83b479aa3a4"},
		{0x200, "f987a7253ac413176f2b074cf7815e54"}, {0x400, "f3392b0822b70005940c7a398e4b70f3"},
		{0x800, "e7159475a2c29b7443b29c7fa6e889d9"}, {0x1000, "d097f3bdfd2022b8845ad8f792aa5825"},
		{0x2000, "a9f746462d870fdf8a65dc1f90e061e5"}, {0x4000, "70d869a156d2a1b890bb3df62baf32f7"},
		{0x8000, "31be135f97d08fd981231505542fcfa6"}, {0x10000, "9aa508b5b7a84e1c677de54f3e99bc9"},
		{0x20000, "5d6af8dedb81196699c329225ee604"}, {0x40000, "2216e584f5fa1ea926041bedfe98"},
		{0x80000, "48a170391f7dc42444e8fa2"},
	}
	for _, m := range magics {
		if absTick&m.bit != 0 {
			v, _ := new(big.Int).SetString(m.hex, 16)
			ratio.Mul(ratio, v)
			ratio.Rsh(ratio, 128)
		}
	}
	if tick > 0 {
		ratio.Div(maxUint256, ratio)
	}
	rem := new(big.Int).Mod(ratio, new(big.Int).Lsh(big.NewInt(1), 32))
	result := new(big.Int).Rsh(ratio, 32)
	if rem.Sign() != 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}

func mulDiv(a, b, denominator *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(a, b), denominator)
}

func mulDivRoundingUp(a, b, denominator *big.Int) *big.Int {
	product := new(big.Int).Mul(a, b)
	result := new(big.Int).Div(product, denominator)
	if new(big.Int).Mod(product, denominator).Sign() > 0 { result.Add(result, big.NewInt(1)) }
	return result
}

func unsafeDivRoundingUp(a, b *big.Int) *big.Int {
	result := new(big.Int).Div(a, b)
	if new(big.Int).Mod(a, b).Sign() != 0 { result.Add(result, big.NewInt(1)) }
	return result
}

func sGetNextSqrtPriceFromInput(sqrtPX96, liquidity, amountIn *big.Int, zeroForOne bool) *big.Int {
	if zeroForOne {
		if amountIn.Sign() == 0 { return new(big.Int).Set(sqrtPX96) }
		n1 := new(big.Int).Lsh(liquidity, 96)
		prod := new(big.Int).Mul(amountIn, sqrtPX96)
		denom := new(big.Int).Add(n1, prod)
		return mulDivRoundingUp(n1, sqrtPX96, denom)
	}
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	quotient := mulDiv(amountIn, q96, liquidity)
	return new(big.Int).Add(sqrtPX96, quotient)
}

func sGetAmount0Delta(sqrtRatioAX96, sqrtRatioBX96, liquidity *big.Int, roundUp bool) *big.Int {
	a, b := sqrtRatioAX96, sqrtRatioBX96
	if a.Cmp(b) > 0 { a, b = b, a }
	n1 := new(big.Int).Lsh(liquidity, 96)
	n2 := new(big.Int).Sub(b, a)
	if roundUp { return unsafeDivRoundingUp(mulDivRoundingUp(n1, n2, b), a) }
	return new(big.Int).Div(mulDiv(n1, n2, b), a)
}

func sGetAmount1Delta(sqrtRatioAX96, sqrtRatioBX96, liquidity *big.Int, roundUp bool) *big.Int {
	a, b := sqrtRatioAX96, sqrtRatioBX96
	if a.Cmp(b) > 0 { a, b = b, a }
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	diff := new(big.Int).Sub(b, a)
	if roundUp { return mulDivRoundingUp(liquidity, diff, q96) }
	return mulDiv(liquidity, diff, q96)
}

