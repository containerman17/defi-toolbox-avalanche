package formulas

// wombat.go — PoolQuoter for Wombat DynamicPoolV2 (stableswap with yield-bearing tokens).
//
// Pools: sAVAX/WAVAX, ggAVAX/WAVAX on Avalanche.
// Math: stableswap invariant with amplification factor.
//   D = Ax + Ay - A*(Lx²/Ax + Ly²/Ay)
//   rx_ = (Ax+Dx)/Lx
//   b = Lx*(rx_ - A/rx_)/Ly - D/Ly
//   ry_ = solveQuad(b, A)
//   idealOut = Ay - Ly*ry_
//
// Fees: flat haircut + high coverage ratio quadratic penalty.
// DynamicPoolV2 twist: quoteFactor scales from-side by relative price ratio.

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

var wad = big.NewInt(1e18)
var wadHalf = new(big.Int).Div(wad, big.NewInt(2))
var mask120 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 120), big.NewInt(1))

// Wombat storage slots (on pool proxy contract)
var (
	wombatSlotAmpFactor   = common.BigToHash(big.NewInt(202)) // uint256
	wombatSlotHaircutRate = common.BigToHash(big.NewInt(203)) // uint256
	wombatSlotCovRatio    = common.BigToHash(big.NewInt(264)) // packed: startCovRatio (low128) | endCovRatio (high128)
	wombatSlotAssetValues = big.NewInt(212)                   // mapping(address => IAsset) _assets.values
)

// Asset storage slot 11: packed cash (uint120) | liability (uint120)
var wombatAssetSlot11 = common.BigToHash(big.NewInt(11))

// Asset storage slot 13: exchangeRateOracle address
var wombatAssetSlot13 = common.BigToHash(big.NewInt(13))

// getPooledAvaxByShares(uint256) selector
var getPooledAvaxBySharesSelector = [4]byte{0xce, 0x7c, 0x2a, 0xc2}

// WombatPool is the PoolQuoter for Wombat DynamicPoolV2 pools.
type WombatPool struct {
	addr          common.Address
	tokens        []common.Address // sorted by address
	assetAddrs    map[common.Address]common.Address // token -> asset contract
	ampFactor     *big.Int
	haircutRate   *big.Int
	startCovRatio *big.Int // 18-decimal FP, e.g. 1.5e18
	endCovRatio   *big.Int // 18-decimal FP, e.g. 1.8e18
	reader        StorageReader
	caller        EVMCaller
}

func newWombatPool(addr common.Address, reader StorageReader, tokens []common.Address, caller EVMCaller) *WombatPool {
	// Read ampFactor (slot 202)
	ampRaw := reader(addr, wombatSlotAmpFactor)
	ampFactor := new(big.Int).SetBytes(ampRaw[:])
	if ampFactor.Sign() <= 0 {
		return nil
	}

	// Read haircutRate (slot 203)
	hairRaw := reader(addr, wombatSlotHaircutRate)
	haircutRate := new(big.Int).SetBytes(hairRaw[:])

	// Read startCovRatio/endCovRatio (slot 264, packed)
	covRaw := reader(addr, wombatSlotCovRatio)
	covVal := new(big.Int).SetBytes(covRaw[:])
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	startCovRatio := new(big.Int).And(covVal, mask128)
	endCovRatio := new(big.Int).Rsh(covVal, 128)
	if startCovRatio.Sign() <= 0 || endCovRatio.Sign() <= 0 {
		return nil
	}

	// Resolve asset addresses from hardcoded map (EnumerableMap storage is complex)
	poolAssets, ok := wombatAssetMap[addr]
	if !ok {
		return nil
	}
	assetAddrs := make(map[common.Address]common.Address, len(tokens))
	for _, tok := range tokens {
		asset, ok := poolAssets[tok]
		if !ok {
			return nil
		}
		assetAddrs[tok] = asset
	}

	return &WombatPool{
		addr:          addr,
		tokens:        tokens,
		assetAddrs:    assetAddrs,
		ampFactor:     ampFactor,
		haircutRate:   haircutRate,
		startCovRatio: startCovRatio,
		endCovRatio:   endCovRatio,
		reader:        reader,
		caller:        caller,
	}
}

func (p *WombatPool) Address() common.Address { return p.addr }

func (p *WombatPool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if amountIn.IsZero() {
		return uint256.Int{}
	}

	fromAsset, ok1 := p.assetAddrs[tokenIn]
	toAsset, ok2 := p.assetAddrs[tokenOut]
	if !ok1 || !ok2 {
		return uint256.Int{}
	}

	// Read cash/liability from asset contracts (slot 11, packed uint120)
	fromCash, fromLiab := p.readAssetCashLiability(fromAsset)
	toCash, toLiab := p.readAssetCashLiability(toAsset)
	if fromCash.Sign() <= 0 || fromLiab.Sign() <= 0 || toCash.Sign() <= 0 || toLiab.Sign() <= 0 {
		return uint256.Int{}
	}

	fromAmount := amountIn.ToBig()

	// DynamicPoolV2 quoteFactor: scale from-side by relative price ratio
	scaleFactor := p.quoteFactor(fromAsset, toAsset)

	// Scale from-side values
	scaledFromCash := fromCash
	scaledFromLiab := fromLiab
	scaledFromAmt := fromAmount
	if scaleFactor != nil && scaleFactor.Cmp(wad) != 0 {
		scaledFromCash = wMul(fromCash, scaleFactor)
		scaledFromLiab = wMul(fromLiab, scaleFactor)
		scaledFromAmt = wMul(fromAmount, scaleFactor)
	}

	// _swapQuoteFunc
	idealToAmount := wombatSwapQuoteFunc(scaledFromCash, toCash, scaledFromLiab, toLiab, scaledFromAmt, p.ampFactor)
	if idealToAmount == nil || idealToAmount.Sign() <= 0 {
		return uint256.Int{}
	}

	// Check enough cash
	if toCash.Cmp(idealToAmount) < 0 {
		return uint256.Int{}
	}

	// Flat haircut
	haircut := wMul(idealToAmount, p.haircutRate)
	actualTo := new(big.Int).Sub(idealToAmount, haircut)
	if actualTo.Sign() <= 0 {
		return uint256.Int{}
	}

	// High coverage ratio fee (uses UNSCALED from-side)
	// Solidity: (cash * 1e18) / liability — truncating integer division.
	finalFromCash := new(big.Int).Add(fromCash, fromAmount)
	finalCovRatio := rawWadDiv(finalFromCash, fromLiab)
	if finalCovRatio.Cmp(p.endCovRatio) > 0 {
		return uint256.Int{} // would revert on-chain
	}
	if finalCovRatio.Cmp(p.startCovRatio) > 0 {
		initCovRatio := rawWadDiv(fromCash, fromLiab)
		highFee := wombatHighCovRatioFee(initCovRatio, finalCovRatio, p.startCovRatio, p.endCovRatio)
		if highFee != nil && highFee.Sign() > 0 {
			penalty := wMul(highFee, actualTo)
			actualTo.Sub(actualTo, penalty)
		}
	}

	if actualTo.Sign() <= 0 {
		return uint256.Int{}
	}
	result, overflow := uint256.FromBig(actualTo)
	if overflow {
		return uint256.Int{}
	}
	return *result
}

// readAssetCashLiability reads cash and liability from asset storage slot 11.
func (p *WombatPool) readAssetCashLiability(asset common.Address) (*big.Int, *big.Int) {
	raw := p.reader(asset, wombatAssetSlot11)
	val := new(big.Int).SetBytes(raw[:])
	cash := new(big.Int).And(val, mask120)
	liability := new(big.Int).And(new(big.Int).Rsh(val, 120), mask120)
	return cash, liability
}

// quoteFactor computes fromRelativePrice * 1e18 / toRelativePrice.
func (p *WombatPool) quoteFactor(fromAsset, toAsset common.Address) *big.Int {
	fromPrice := p.getRelativePrice(fromAsset)
	toPrice := p.getRelativePrice(toAsset)
	if fromPrice == nil || toPrice == nil || toPrice.Sign() <= 0 {
		return wad // fallback to 1:1
	}
	// Solidity DynamicPoolV2._quoteFactor: (fromAssetRate * WAD) / toAssetRate — truncating.
	return rawWadDiv(fromPrice, toPrice)
}

// sAVAX rate storage slots: totalPooledAvax at slot 201, totalShares at slot 202.
var (
	stakingSlotTotalPooledAvax = common.BigToHash(big.NewInt(201))
	stakingSlotTotalShares     = common.BigToHash(big.NewInt(202))
)

// ERC-4626 convertToAssets(uint256) selector = 0x07a2d13a
var convertToAssetsSelector = [4]byte{0x07, 0xa2, 0xd1, 0x3a}

// getRelativePrice reads the asset's relative price.
// For WAVAX assets, returns 1e18. For yield-bearing assets (sAVAX, ggAVAX),
// tries storage-based rate first (sAVAX: slots 201/202), then EVM call fallback.
func (p *WombatPool) getRelativePrice(asset common.Address) *big.Int {
	// Read oracle address from asset slot 13
	oracleRaw := p.reader(asset, wombatAssetSlot13)
	oracle := common.BytesToAddress(oracleRaw[12:32])
	if oracle == (common.Address{}) {
		return new(big.Int).Set(wad)
	}

	// Try sAVAX pattern: totalPooledAvax / totalShares (slots 201/202)
	totalPooledRaw := p.reader(oracle, stakingSlotTotalPooledAvax)
	totalSharesRaw := p.reader(oracle, stakingSlotTotalShares)
	totalPooled := new(big.Int).SetBytes(totalPooledRaw[:])
	totalShares := new(big.Int).SetBytes(totalSharesRaw[:])
	if totalPooled.Sign() > 0 && totalShares.Sign() > 0 {
		// Match sAVAX.getPooledAvaxByShares(WAD) = totalPooledAvax * WAD / totalShares (truncating).
		return rawWadDiv(totalPooled, totalShares)
	}

	// Fallback: ERC-4626 convertToAssets(1e18) via EVM call (ggAVAX)
	if p.caller != nil {
		calldata := make([]byte, 36)
		copy(calldata[:4], convertToAssetsSelector[:])
		wad.FillBytes(calldata[4:36])
		result, ok := p.caller(oracle, calldata)
		if ok && len(result) >= 32 {
			price := new(big.Int).SetBytes(result[:32])
			if price.Sign() > 0 {
				return price
			}
		}
	}

	return new(big.Int).Set(wad)
}

// wombatSwapQuoteFunc implements CoreV2._swapQuoteFunc.
// All values are 18-decimal fixed-point (WAD).
func wombatSwapQuoteFunc(Ax, Ay, Lx, Ly, Dx, A *big.Int) *big.Int {
	if Lx.Sign() <= 0 || Ly.Sign() <= 0 || Ax.Sign() <= 0 || Ay.Sign() <= 0 {
		return nil
	}

	// D = Ax + Ay - A * (Lx²/Ax + Ly²/Ay)
	lxSq := new(big.Int).Mul(Lx, Lx)
	lxSqDivAx := new(big.Int).Div(lxSq, Ax)
	lySq := new(big.Int).Mul(Ly, Ly)
	lySqDivAy := new(big.Int).Div(lySq, Ay)
	sum := new(big.Int).Add(lxSqDivAx, lySqDivAy)
	D := new(big.Int).Add(Ax, Ay)
	D.Sub(D, wMul(A, sum))

	// rx_ = (Ax + Dx) / Lx  (WAD division)
	axPlusDx := new(big.Int).Add(Ax, Dx)
	rx_ := wDiv(axPlusDx, Lx)
	if rx_.Sign() <= 0 {
		return nil
	}

	// b = Lx * (rx_ - A/rx_) / Ly - D/Ly
	aDivRx := wDiv(A, rx_)
	rxMinusADivRx := new(big.Int).Sub(rx_, aDivRx)
	lxTimesRx := new(big.Int).Mul(Lx, rxMinusADivRx)
	lxTimesRxDivLy := new(big.Int).Div(lxTimesRx, Ly)
	dDivLy := wDiv(D, Ly)
	b := new(big.Int).Sub(lxTimesRxDivLy, dDivLy)

	// Solve: ry_² + b*ry_ - A = 0
	ry_ := wombatSolveQuad(b, A)
	if ry_ == nil || ry_.Sign() <= 0 {
		return nil
	}

	// Dy = Ly * ry_ - Ay
	Dy := new(big.Int).Sub(wMul(Ly, ry_), Ay)

	// Output is -Dy (positive means tokens going out)
	if Dy.Sign() >= 0 {
		return nil // no output
	}
	return new(big.Int).Neg(Dy)
}

// wombatSolveQuad solves x² + b*x - c = 0 for positive root.
// x = (-b + sqrt(b² + 4*c*WAD)) / 2
// Uses Babylonian sqrt with b as initial guess.
func wombatSolveQuad(b, c *big.Int) *big.Int {
	// discriminant = b² + 4*c*WAD
	bSq := new(big.Int).Mul(b, b)
	fourCWad := new(big.Int).Mul(c, big.NewInt(4))
	fourCWad.Mul(fourCWad, wad)
	disc := new(big.Int).Add(bSq, fourCWad)
	if disc.Sign() < 0 {
		return nil
	}

	sqrtDisc := wombatSqrt(disc, b)
	if sqrtDisc == nil {
		return nil
	}

	// (-b + sqrt) / 2
	result := new(big.Int).Sub(sqrtDisc, b)
	result.Div(result, big.NewInt(2))
	return result
}

// wombatSqrt computes integer sqrt using Babylonian method with guess.
func wombatSqrt(y, guess *big.Int) *big.Int {
	if y.Sign() <= 0 {
		return big.NewInt(0)
	}
	three := big.NewInt(3)
	if y.Cmp(three) <= 0 {
		if y.Sign() != 0 {
			return big.NewInt(1)
		}
		return big.NewInt(0)
	}

	// Pick initial z from guess
	z := new(big.Int)
	absGuess := new(big.Int).Abs(guess)
	if absGuess.Sign() > 0 && absGuess.Cmp(y) <= 0 {
		z.Set(absGuess)
	} else {
		z.Set(y)
	}

	x := new(big.Int)
	x.Div(y, z)
	x.Add(x, z)
	x.Div(x, big.NewInt(2))

	for x.Cmp(z) != 0 {
		z.Set(x)
		x.Div(y, x)
		x.Add(x, z)
		x.Div(x, big.NewInt(2))
	}
	return z
}

// wombatHighCovRatioFee computes the high coverage ratio fee.
// fee = ((b-a) / (finalCov-initCov) / 2) / (endCov-startCov)
// where a = max(0, (initCov-startCov)²), b = (finalCov-startCov)²
func wombatHighCovRatioFee(initCovRatio, finalCovRatio, startCovRatio, endCovRatio *big.Int) *big.Int {
	if finalCovRatio.Cmp(startCovRatio) <= 0 || finalCovRatio.Cmp(initCovRatio) <= 0 {
		return big.NewInt(0)
	}

	var a *big.Int
	if initCovRatio.Cmp(startCovRatio) <= 0 {
		a = big.NewInt(0)
	} else {
		diff := new(big.Int).Sub(initCovRatio, startCovRatio)
		a = wMul(diff, diff)
	}

	diffB := new(big.Int).Sub(finalCovRatio, startCovRatio)
	bVal := wMul(diffB, diffB)

	bMinusA := new(big.Int).Sub(bVal, a)
	covDiff := new(big.Int).Sub(finalCovRatio, initCovRatio)
	if covDiff.Sign() <= 0 {
		return big.NewInt(0)
	}

	// (b-a) / (finalCov-initCov)
	ratio := wDiv(bMinusA, covDiff)
	// / 2
	ratio.Div(ratio, big.NewInt(2))
	// / (endCov-startCov)
	endMinusStart := new(big.Int).Sub(endCovRatio, startCovRatio)
	if endMinusStart.Sign() <= 0 {
		return big.NewInt(0)
	}
	return wDiv(ratio, endMinusStart)
}

// WAD math helpers (18 decimal fixed-point, signed)
func wMul(x, y *big.Int) *big.Int {
	product := new(big.Int).Mul(x, y)
	if product.Sign() >= 0 {
		product.Add(product, wadHalf)
	} else {
		product.Sub(product, wadHalf)
	}
	return product.Div(product, wad)
}

// rawWadDiv is truncating (x * WAD) / y, matching Solidity's raw integer division.
// Use where Solidity uses `*1e18/y` (not `.wdiv(y)`): exchange rates, cov-ratio,
// quoteFactor. wDiv rounds-to-nearest and produces off-by-one results that
// propagate through the swap curve as hundreds of wei of overquote.
func rawWadDiv(x, y *big.Int) *big.Int {
	if y.Sign() == 0 {
		return big.NewInt(0)
	}
	r := new(big.Int).Mul(x, wad)
	return r.Quo(r, y)
}

func wDiv(x, y *big.Int) *big.Int {
	if y.Sign() == 0 {
		return big.NewInt(0)
	}
	xWad := new(big.Int).Mul(x, wad)
	yHalf := new(big.Int).Div(y, big.NewInt(2))
	if xWad.Sign() >= 0 {
		xWad.Add(xWad, yHalf)
	} else {
		xWad.Sub(xWad, yHalf)
	}
	return xWad.Div(xWad, y)
}

// wombatMappingSlot computes keccak256(leftPad32(key) ++ leftPad32(slot)) for address key.
func wombatMappingSlot(key common.Address, baseSlot *big.Int) common.Hash {
	var data [64]byte
	copy(data[12:32], key.Bytes())
	baseSlot.FillBytes(data[32:64])
	return crypto.Keccak256Hash(data[:])
}

// wombatAssetMap maps (pool, token) -> asset contract address.
// EnumerableMap storage layout is complex; hardcode known mappings.
var wombatAssetMap = map[common.Address]map[common.Address]common.Address{
	common.HexToAddress("0xe3abc29b035874a9f6dcdb06f8f20d9975069d87"): { // sAVAX/WAVAX
		common.HexToAddress("0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be"): common.HexToAddress("0xc096ff2606152ed2a06dd12f15a3c0466aa5a9fa"), // sAVAX
		common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"): common.HexToAddress("0x29eeb257a2a6ecde2984acedf80a1b687f18ec91"), // WAVAX
	},
	common.HexToAddress("0xbba43749efc1bc29ea434d88ebaf8a97dc7aeb77"): { // ggAVAX/WAVAX
		common.HexToAddress("0xa25eaf2906fa1a3a13edac9b9657108af7b703e3"): common.HexToAddress("0x2ddfdd8e1bec473f07815fa3cfea3bba4d39f37e"), // ggAVAX
		common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"): common.HexToAddress("0x960c66dda302f4a496d936f693e083b1e9ace306"), // WAVAX
	},
	common.HexToAddress("0xc12c0ced34b115655234e8a4db87ebc8f6f362d0"): { // WAVAX/sAVAX/ggAVAX
		common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"): common.HexToAddress("0xd9ffeea3062e401ad9da1415f61c67acca48e576"), // WAVAX
		common.HexToAddress("0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be"): common.HexToAddress("0x41571e5d9551f120ae084afc3bcac2cf3231bc7b"), // sAVAX
		common.HexToAddress("0xa25eaf2906fa1a3a13edac9b9657108af7b703e3"): common.HexToAddress("0x616264fbd5732aa679921c0130a4ae605d981d06"), // ggAVAX
	},
}

