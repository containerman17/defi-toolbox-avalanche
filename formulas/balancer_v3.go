package formulas

// balancer_v3.go — Balancer V3 swap math (Weighted + Stable pools).
// Port of Solidity contracts: FixedPoint.sol, LogExpMath.sol, WeightedMath.sol, StableMath.sol.
// All values are 18-decimal fixed point (1e18 = 1.0).

import (
	"math/big"
)

// ── FixedPoint constants ──

var (
	fpONE  = big.NewInt(1e18)
	fpTWO  = new(big.Int).Mul(big.NewInt(2), fpONE)
	fpFOUR = new(big.Int).Mul(big.NewInt(4), fpONE)

	fpMaxPowRelativeError = big.NewInt(10000) // 10^(-14)

	// LogExpMath constants (18 decimal)
	leONE18 = big.NewInt(1e18)
	leONE20 = new(big.Int).Mul(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
	leONE36 = new(big.Int).Exp(big.NewInt(10), big.NewInt(36), nil)

	leMaxNatExp = new(big.Int).Mul(big.NewInt(130), leONE18)
	leMinNatExp = new(big.Int).Mul(big.NewInt(-41), leONE18)

	leLn36LowerBound = new(big.Int).Sub(leONE18, new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil))
	leLn36UpperBound = new(big.Int).Add(leONE18, new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil))

	leMildExpBound = new(big.Int).Div(
		new(big.Int).Exp(big.NewInt(2), big.NewInt(254), nil),
		leONE20,
	)

	// Precomputed e^(2^n) constants
	leX0  = new(big.Int).Mul(big.NewInt(128), leONE18)
	leA0, _ = new(big.Int).SetString("38877084059945950922200000000000000000000000000000000000", 10)
	leX1  = new(big.Int).Mul(big.NewInt(64), leONE18)
	leA1, _ = new(big.Int).SetString("6235149080811616882910000000", 10)

	// 20-decimal constants
	leX2  = new(big.Int).Mul(big.NewInt(32), leONE20)
	leA2, _ = new(big.Int).SetString("7896296018268069516100000000000000", 10)
	leX3  = new(big.Int).Mul(big.NewInt(16), leONE20)
	leA3, _ = new(big.Int).SetString("888611052050787263676000000", 10)
	leX4  = new(big.Int).Mul(big.NewInt(8), leONE20)
	leA4, _ = new(big.Int).SetString("298095798704172827474000", 10)
	leX5  = new(big.Int).Mul(big.NewInt(4), leONE20)
	leA5, _ = new(big.Int).SetString("5459815003314423907810", 10)
	leX6  = new(big.Int).Mul(big.NewInt(2), leONE20)
	leA6, _ = new(big.Int).SetString("738905609893065022723", 10)
	leX7  = new(big.Int).Set(leONE20)
	leA7, _ = new(big.Int).SetString("271828182845904523536", 10)
	leX8  = new(big.Int).Div(leONE20, big.NewInt(2))
	leA8, _ = new(big.Int).SetString("164872127070012814685", 10)
	leX9  = new(big.Int).Div(leONE20, big.NewInt(4))
	leA9, _ = new(big.Int).SetString("128402541668774148407", 10)
	leX10 = new(big.Int).Div(leONE20, big.NewInt(8))
	leA10, _ = new(big.Int).SetString("113314845306682631683", 10)
	leX11 = new(big.Int).Div(leONE20, big.NewInt(16))
	leA11, _ = new(big.Int).SetString("106449445891785942956", 10)

	// StableMath constants
	stableAmpPrecision = big.NewInt(1000)
)

// ── FixedPoint math ──

func fpMulDown(a, b *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(a, b), fpONE)
}

func fpMulUp(a, b *big.Int) *big.Int {
	product := new(big.Int).Mul(a, b)
	if product.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Add(
		new(big.Int).Div(new(big.Int).Sub(product, big.NewInt(1)), fpONE),
		big.NewInt(1),
	)
}

func fpDivDown(a, b *big.Int) *big.Int {
	aInflated := new(big.Int).Mul(a, fpONE)
	return new(big.Int).Div(aInflated, b)
}

func fpDivUp(a, b *big.Int) *big.Int {
	return fpMulDivUp(a, fpONE, b)
}

func fpMulDivUp(a, b, c *big.Int) *big.Int {
	product := new(big.Int).Mul(a, b)
	if product.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Add(
		new(big.Int).Div(new(big.Int).Sub(product, big.NewInt(1)), c),
		big.NewInt(1),
	)
}

func fpDivUpRaw(a, b *big.Int) *big.Int {
	if a.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Add(
		big.NewInt(1),
		new(big.Int).Div(new(big.Int).Sub(a, big.NewInt(1)), b),
	)
}

func fpComplement(x *big.Int) *big.Int {
	if x.Cmp(fpONE) < 0 {
		return new(big.Int).Sub(fpONE, x)
	}
	return big.NewInt(0)
}

// ── LogExpMath ──

func lePow(x, y *big.Int) *big.Int {
	if y.Sign() == 0 {
		return new(big.Int).Set(leONE18)
	}
	if x.Sign() == 0 {
		return big.NewInt(0)
	}

	xInt := new(big.Int).Set(x)
	yInt := new(big.Int).Set(y)

	var logxTimesY *big.Int

	if xInt.Cmp(leLn36LowerBound) > 0 && xInt.Cmp(leLn36UpperBound) < 0 {
		ln36x := leLn36(xInt)
		// logx_times_y = (ln36x / ONE_18) * y + ((ln36x % ONE_18) * y) / ONE_18
		part1 := new(big.Int).Mul(new(big.Int).Div(ln36x, leONE18), yInt)
		part2 := new(big.Int).Div(new(big.Int).Mul(new(big.Int).Mod(ln36x, leONE18), yInt), leONE18)
		logxTimesY = new(big.Int).Add(part1, part2)
	} else {
		logxTimesY = new(big.Int).Mul(leLn(xInt), yInt)
	}
	logxTimesY.Div(logxTimesY, leONE18)

	return new(big.Int).SetBytes(leExp(logxTimesY).Bytes()) // ensure unsigned
}

func leExp(x *big.Int) *big.Int {
	negativeExponent := false
	xVal := new(big.Int).Set(x)

	if xVal.Sign() < 0 {
		xVal.Neg(xVal)
		negativeExponent = true
	}

	var firstAN *big.Int

	if xVal.Cmp(leX0) >= 0 {
		xVal.Sub(xVal, leX0)
		firstAN = new(big.Int).Set(leA0)
	} else if xVal.Cmp(leX1) >= 0 {
		xVal.Sub(xVal, leX1)
		firstAN = new(big.Int).Set(leA1)
	} else {
		firstAN = big.NewInt(1)
	}

	xVal.Mul(xVal, big.NewInt(100))

	product := new(big.Int).Set(leONE20)

	type pair struct{ x, a *big.Int }
	pairs := []pair{
		{leX2, leA2}, {leX3, leA3}, {leX4, leA4}, {leX5, leA5},
		{leX6, leA6}, {leX7, leA7}, {leX8, leA8}, {leX9, leA9},
	}
	for _, p := range pairs {
		if xVal.Cmp(p.x) >= 0 {
			xVal.Sub(xVal, p.x)
			product.Div(new(big.Int).Mul(product, p.a), leONE20)
		}
	}

	// Taylor series for exp(x) with 12 terms
	seriesSum := new(big.Int).Set(leONE20)
	term := new(big.Int).Set(xVal)
	seriesSum.Add(seriesSum, term)

	for n := int64(2); n <= 12; n++ {
		term = new(big.Int).Div(new(big.Int).Mul(term, xVal), leONE20)
		term.Div(term, big.NewInt(n))
		seriesSum.Add(seriesSum, term)
	}

	result := new(big.Int).Div(new(big.Int).Mul(product, seriesSum), leONE20)
	result.Mul(result, firstAN)
	result.Div(result, big.NewInt(100))

	if negativeExponent {
		one18sq := new(big.Int).Mul(leONE18, leONE18)
		result.Div(one18sq, result)
	}

	return result
}

func leLn(a *big.Int) *big.Int {
	negativeExponent := false
	aVal := new(big.Int).Set(a)

	if aVal.Cmp(leONE18) < 0 {
		one18sq := new(big.Int).Mul(leONE18, leONE18)
		aVal.Div(one18sq, aVal)
		negativeExponent = true
	}

	sum := big.NewInt(0)

	a0x1e18 := new(big.Int).Mul(leA0, leONE18)
	if aVal.Cmp(a0x1e18) >= 0 {
		aVal.Div(aVal, leA0)
		sum.Add(sum, leX0)
	}

	a1x1e18 := new(big.Int).Mul(leA1, leONE18)
	if aVal.Cmp(a1x1e18) >= 0 {
		aVal.Div(aVal, leA1)
		sum.Add(sum, leX1)
	}

	sum.Mul(sum, big.NewInt(100))
	aVal.Mul(aVal, big.NewInt(100))

	type pair struct{ x, a *big.Int }
	pairs := []pair{
		{leX2, leA2}, {leX3, leA3}, {leX4, leA4}, {leX5, leA5},
		{leX6, leA6}, {leX7, leA7}, {leX8, leA8}, {leX9, leA9},
		{leX10, leA10}, {leX11, leA11},
	}
	for _, p := range pairs {
		if aVal.Cmp(p.a) >= 0 {
			aVal.Div(new(big.Int).Mul(aVal, leONE20), p.a)
			sum.Add(sum, p.x)
		}
	}

	// Taylor series: ln(a) ≈ 2 * (z + z^3/3 + z^5/5 + ...)  where z = (a-1)/(a+1)
	z := new(big.Int).Div(
		new(big.Int).Mul(new(big.Int).Sub(aVal, leONE20), leONE20),
		new(big.Int).Add(aVal, leONE20),
	)
	zSquared := new(big.Int).Div(new(big.Int).Mul(z, z), leONE20)

	num := new(big.Int).Set(z)
	seriesSum := new(big.Int).Set(num)

	for _, denom := range []int64{3, 5, 7, 9, 11} {
		num = new(big.Int).Div(new(big.Int).Mul(num, zSquared), leONE20)
		seriesSum.Add(seriesSum, new(big.Int).Div(num, big.NewInt(denom)))
	}

	seriesSum.Mul(seriesSum, big.NewInt(2))

	result := new(big.Int).Div(new(big.Int).Add(sum, seriesSum), big.NewInt(100))

	if negativeExponent {
		result.Neg(result)
	}
	return result
}

func leLn36(x *big.Int) *big.Int {
	xVal := new(big.Int).Mul(x, leONE18)

	z := new(big.Int).Div(
		new(big.Int).Mul(new(big.Int).Sub(xVal, leONE36), leONE36),
		new(big.Int).Add(xVal, leONE36),
	)
	zSquared := new(big.Int).Div(new(big.Int).Mul(z, z), leONE36)

	num := new(big.Int).Set(z)
	seriesSum := new(big.Int).Set(num)

	for _, denom := range []int64{3, 5, 7, 9, 11, 13, 15} {
		num = new(big.Int).Div(new(big.Int).Mul(num, zSquared), leONE36)
		seriesSum.Add(seriesSum, new(big.Int).Div(num, big.NewInt(denom)))
	}

	return new(big.Int).Mul(seriesSum, big.NewInt(2))
}

// ── FixedPoint power functions ──

func fpPowDown(x, y *big.Int) *big.Int {
	if y.Cmp(fpONE) == 0 {
		return new(big.Int).Set(x)
	}
	if y.Cmp(fpTWO) == 0 {
		return fpMulDown(x, x)
	}
	if y.Cmp(fpFOUR) == 0 {
		sq := fpMulDown(x, x)
		return fpMulDown(sq, sq)
	}

	raw := lePow(x, y)
	maxError := new(big.Int).Add(fpMulUp(raw, fpMaxPowRelativeError), big.NewInt(1))
	if raw.Cmp(maxError) < 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Sub(raw, maxError)
}

func fpPowUp(x, y *big.Int) *big.Int {
	if y.Cmp(fpONE) == 0 {
		return new(big.Int).Set(x)
	}
	if y.Cmp(fpTWO) == 0 {
		return fpMulUp(x, x)
	}
	if y.Cmp(fpFOUR) == 0 {
		sq := fpMulUp(x, x)
		return fpMulUp(sq, sq)
	}

	raw := lePow(x, y)
	maxError := new(big.Int).Add(fpMulUp(raw, fpMaxPowRelativeError), big.NewInt(1))
	return new(big.Int).Add(raw, maxError)
}

// ── WeightedMath ──

// WeightedComputeOutGivenExactIn computes amountOut for a weighted pool swap.
// All values are 18-decimal fixed point. Fee has already been deducted from amountIn.
//   amountOut = balanceOut * (1 - (balanceIn / (balanceIn + amountIn)) ^ (weightIn / weightOut))
func WeightedComputeOutGivenExactIn(balanceIn, weightIn, balanceOut, weightOut, amountIn *big.Int) *big.Int {
	denominator := new(big.Int).Add(balanceIn, amountIn)
	base := fpDivUp(balanceIn, denominator)
	exponent := fpDivDown(weightIn, weightOut)
	power := fpPowUp(base, exponent)
	return fpMulDown(balanceOut, fpComplement(power))
}

// ── StableMath ──

// StableComputeInvariant computes the StableSwap invariant D.
// amp is amplificationParameter (already multiplied by AMP_PRECISION = 1000).
func StableComputeInvariant(amp *big.Int, balances []*big.Int) *big.Int {
	numTokens := big.NewInt(int64(len(balances)))
	sum := big.NewInt(0)
	for _, b := range balances {
		sum.Add(sum, b)
	}
	if sum.Sign() == 0 {
		return big.NewInt(0)
	}

	invariant := new(big.Int).Set(sum)
	ampTimesTotal := new(big.Int).Mul(amp, numTokens)

	for i := 0; i < 255; i++ {
		dP := new(big.Int).Set(invariant)
		for _, b := range balances {
			// D_P = D_P * invariant / (balance * numTokens)
			dP = new(big.Int).Div(
				new(big.Int).Mul(dP, invariant),
				new(big.Int).Mul(b, numTokens),
			)
		}

		prevInvariant := new(big.Int).Set(invariant)

		// numerator = (ampTimesTotal * sum / AMP_PRECISION + D_P * numTokens) * invariant
		numPart1 := new(big.Int).Div(new(big.Int).Mul(ampTimesTotal, sum), stableAmpPrecision)
		numPart2 := new(big.Int).Mul(dP, numTokens)
		numerator := new(big.Int).Mul(new(big.Int).Add(numPart1, numPart2), invariant)

		// denominator = (ampTimesTotal - AMP_PRECISION) * invariant / AMP_PRECISION + (numTokens + 1) * D_P
		denPart1 := new(big.Int).Div(
			new(big.Int).Mul(new(big.Int).Sub(ampTimesTotal, stableAmpPrecision), invariant),
			stableAmpPrecision,
		)
		denPart2 := new(big.Int).Mul(new(big.Int).Add(numTokens, big.NewInt(1)), dP)
		denominator := new(big.Int).Add(denPart1, denPart2)

		invariant.Div(numerator, denominator)

		// Check convergence
		diff := new(big.Int).Sub(invariant, prevInvariant)
		if diff.Sign() < 0 {
			diff.Neg(diff)
		}
		if diff.Cmp(big.NewInt(1)) <= 0 {
			return invariant
		}
	}

	return invariant // didn't converge, return best guess
}

// StableComputeBalance computes a token balance given invariant and other balances.
// Uses Newton-Raphson. Rounds up.
func StableComputeBalance(amp *big.Int, balances []*big.Int, invariant *big.Int, tokenIndex int) *big.Int {
	numTokens := big.NewInt(int64(len(balances)))
	ampTimesTotal := new(big.Int).Mul(amp, numTokens)

	sum := big.NewInt(0)
	pD := new(big.Int).Mul(balances[0], numTokens)

	for j := 1; j < len(balances); j++ {
		pD = new(big.Int).Div(
			new(big.Int).Mul(new(big.Int).Mul(pD, balances[j]), numTokens),
			invariant,
		)
		sum.Add(sum, balances[j])
	}
	// Add balances[0] to sum (we skipped it in the loop since we started pD with it)
	sum.Add(sum, balances[0])
	// Remove balances[tokenIndex] from sum
	sum.Sub(sum, balances[tokenIndex])

	inv2 := new(big.Int).Mul(invariant, invariant)

	// c = (inv2 * AMP_PRECISION) / (ampTimesTotal * P_D) * balances[tokenIndex]
	c := fpDivUpRaw(new(big.Int).Mul(inv2, stableAmpPrecision), new(big.Int).Mul(ampTimesTotal, pD))
	c.Mul(c, balances[tokenIndex])

	// b = sum + invariant * AMP_PRECISION / ampTimesTotal
	b := new(big.Int).Add(sum, new(big.Int).Div(new(big.Int).Mul(invariant, stableAmpPrecision), ampTimesTotal))

	// Initial approximation
	tokenBalance := fpDivUpRaw(new(big.Int).Add(inv2, c), new(big.Int).Add(invariant, b))

	for i := 0; i < 255; i++ {
		prevTokenBalance := new(big.Int).Set(tokenBalance)

		// tokenBalance = (tokenBalance^2 + c) / (2*tokenBalance + b - invariant)
		tokenBalance = fpDivUpRaw(
			new(big.Int).Add(new(big.Int).Mul(tokenBalance, tokenBalance), c),
			new(big.Int).Sub(
				new(big.Int).Add(new(big.Int).Mul(tokenBalance, big.NewInt(2)), b),
				invariant,
			),
		)

		diff := new(big.Int).Sub(tokenBalance, prevTokenBalance)
		if diff.Sign() < 0 {
			diff.Neg(diff)
		}
		if diff.Cmp(big.NewInt(1)) <= 0 {
			return tokenBalance
		}
	}

	return tokenBalance
}

// StableComputeOutGivenExactIn computes amountOut for a stable pool swap (ExactIn).
// amp is amplificationParameter (already * AMP_PRECISION).
// Balances and amountIn are all scaled18.
func StableComputeOutGivenExactIn(amp *big.Int, balances []*big.Int, indexIn, indexOut int, amountIn, invariant *big.Int) *big.Int {
	// Make a copy of balances and add amountIn
	bals := make([]*big.Int, len(balances))
	for i, b := range balances {
		bals[i] = new(big.Int).Set(b)
	}
	bals[indexIn].Add(bals[indexIn], amountIn)

	finalBalanceOut := StableComputeBalance(amp, bals, invariant, indexOut)

	// Restore the original balance (not strictly needed since we copied)
	// amountOut = balances[indexOut] - finalBalanceOut - 1 (round down)
	result := new(big.Int).Sub(balances[indexOut], finalBalanceOut)
	result.Sub(result, big.NewInt(1))
	if result.Sign() < 0 {
		return big.NewInt(0)
	}
	return result
}
