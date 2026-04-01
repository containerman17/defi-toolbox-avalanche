package formulas

// platypus.go — PoolQuoter for Platypus Exchange stableswap pools.
//
// Math: price slippage curve model with coverage ratios.
//   idealOut = fromAmount * 10^toDecimals / 10^fromDecimals
//   si = slippage_from(fromCash, fromLiab, fromAmount)
//   sj = slippage_to(toCash, toLiab, idealOut)
//   toAmount = idealOut * (1 + si - sj)
//   actualOut = toAmount - toAmount * haircutRate
//
// Slippage function g(x):
//   if x < xThreshold: g(x) = c1 - x
//   else:              g(x) = k / x^n   (computed via rpow in RAY=10^27)

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// Platypus storage slots (on pool proxy)
var (
	platSlotK          = common.BigToHash(big.NewInt(265)) // _slippageParamK uint256 WAD
	platSlotN          = common.BigToHash(big.NewInt(266)) // _slippageParamN uint256 (raw integer)
	platSlotC1         = common.BigToHash(big.NewInt(267)) // _c1 uint256 WAD
	platSlotXThreshold = common.BigToHash(big.NewInt(268)) // _xThreshold uint256 WAD
	platSlotHaircutRate = common.BigToHash(big.NewInt(269)) // _haircutRate uint256 WAD
	platSlotAssetValues = big.NewInt(275)                  // mapping(address => Asset) _assets.values
)

// Asset storage slots
var (
	platAssetSlotCash      = common.BigToHash(big.NewInt(153)) // _cash uint256
	platAssetSlotLiability = common.BigToHash(big.NewInt(154)) // _liability uint256
)

var ray = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)     // 1e27
var rayHalf = new(big.Int).Div(ray, big.NewInt(2))

// PlatypusPool is the PoolQuoter for Platypus stableswap pools.
type PlatypusPool struct {
	addr        common.Address
	tokens      []common.Address
	assetAddrs  map[common.Address]common.Address // token -> asset contract
	decimals    map[common.Address]int            // token -> decimals
	k           *big.Int // slippage param K (WAD)
	n           *big.Int // slippage param N (raw integer)
	c1          *big.Int // WAD
	xThreshold  *big.Int // WAD
	haircutRate *big.Int // WAD
	reader      StorageReader
}

func newPlatypusPool(addr common.Address, reader StorageReader, tokens []common.Address, caller EVMCaller) *PlatypusPool {
	// Read params from pool storage
	kRaw := reader(addr, platSlotK)
	k := new(big.Int).SetBytes(kRaw[:])
	nRaw := reader(addr, platSlotN)
	n := new(big.Int).SetBytes(nRaw[:])
	c1Raw := reader(addr, platSlotC1)
	c1 := new(big.Int).SetBytes(c1Raw[:])
	xthRaw := reader(addr, platSlotXThreshold)
	xth := new(big.Int).SetBytes(xthRaw[:])
	hairRaw := reader(addr, platSlotHaircutRate)
	hair := new(big.Int).SetBytes(hairRaw[:])

	if k.Sign() <= 0 || n.Sign() <= 0 || c1.Sign() <= 0 || xth.Sign() <= 0 {
		return nil
	}

	// Resolve asset addresses from hardcoded map
	poolAssets, ok := platypusAssetMap[addr]
	if !ok {
		return nil
	}
	assetAddrs := make(map[common.Address]common.Address, len(tokens))
	decimals := make(map[common.Address]int, len(tokens))
	for _, tok := range tokens {
		asset, ok := poolAssets[tok]
		if !ok {
			return nil
		}
		assetAddrs[tok] = asset.addr
		decimals[tok] = asset.decimals
	}

	return &PlatypusPool{
		addr:        addr,
		tokens:      tokens,
		assetAddrs:  assetAddrs,
		decimals:    decimals,
		k:           k,
		n:           n,
		c1:          c1,
		xThreshold:  xth,
		haircutRate: hair,
		reader:      reader,
	}
}

func (p *PlatypusPool) Address() common.Address { return p.addr }

func (p *PlatypusPool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if amountIn.IsZero() {
		return uint256.Int{}
	}

	fromAsset, ok1 := p.assetAddrs[tokenIn]
	toAsset, ok2 := p.assetAddrs[tokenOut]
	if !ok1 || !ok2 {
		return uint256.Int{}
	}
	fromDecimals := p.decimals[tokenIn]
	toDecimals := p.decimals[tokenOut]

	// Read cash/liability from asset contracts
	fromCash := p.readSlot(fromAsset, platAssetSlotCash)
	fromLiab := p.readSlot(fromAsset, platAssetSlotLiability)
	toCash := p.readSlot(toAsset, platAssetSlotCash)
	toLiab := p.readSlot(toAsset, platAssetSlotLiability)

	if fromCash.Sign() <= 0 || fromLiab.Sign() <= 0 || toCash.Sign() <= 0 || toLiab.Sign() <= 0 {
		return uint256.Int{}
	}

	fromAmount := amountIn.ToBig()

	// Step 1: idealToAmount (decimal conversion, 1:1 peg)
	idealToAmount := new(big.Int).Set(fromAmount)
	if toDecimals > fromDecimals {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(toDecimals-fromDecimals)), nil)
		idealToAmount.Mul(idealToAmount, scale)
	} else if fromDecimals > toDecimals {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(fromDecimals-toDecimals)), nil)
		idealToAmount.Div(idealToAmount, scale)
	}
	if idealToAmount.Sign() <= 0 {
		return uint256.Int{}
	}

	// Check enough cash
	if toCash.Cmp(idealToAmount) < 0 {
		return uint256.Int{}
	}

	// Step 2: slippage_from (adding fromAmount to from-side)
	covBefore := wDiv(fromCash, fromLiab)
	fromCashAfter := new(big.Int).Add(fromCash, fromAmount)
	covAfter := wDiv(fromCashAfter, fromLiab)
	gBefore := p.gFunc(covBefore)
	gAfter := p.gFunc(covAfter)
	// si = (g(covBefore) - g(covAfter)) / (covAfter - covBefore)
	si := wDiv(new(big.Int).Sub(gBefore, gAfter), new(big.Int).Sub(covAfter, covBefore))

	// Step 3: slippage_to (removing idealToAmount from to-side)
	covBeforeTo := wDiv(toCash, toLiab)
	toCashAfter := new(big.Int).Sub(toCash, idealToAmount)
	if toCashAfter.Sign() < 0 {
		return uint256.Int{}
	}
	covAfterTo := wDiv(toCashAfter, toLiab)
	gBeforeTo := p.gFunc(covAfterTo)
	gAfterTo := p.gFunc(covBeforeTo)
	// sj = (g(covAfterTo) - g(covBeforeTo)) / (covBeforeTo - covAfterTo)
	covDiffTo := new(big.Int).Sub(covBeforeTo, covAfterTo)
	if covDiffTo.Sign() <= 0 {
		return uint256.Int{}
	}
	sj := wDiv(new(big.Int).Sub(gBeforeTo, gAfterTo), covDiffTo)

	// Step 4: swappingSlippage = WAD + si - sj
	swapSlippage := new(big.Int).Add(wad, si)
	swapSlippage.Sub(swapSlippage, sj)

	// toAmount = idealToAmount * swappingSlippage / WAD
	toAmount := wMul(idealToAmount, swapSlippage)
	if toAmount.Sign() <= 0 {
		return uint256.Int{}
	}

	// Step 5: haircut
	haircut := wMul(toAmount, p.haircutRate)
	actualTo := new(big.Int).Sub(toAmount, haircut)
	if actualTo.Sign() <= 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(actualTo)
	if overflow {
		return uint256.Int{}
	}
	return *result
}

// gFunc computes the slippage function g(x):
//   if x < xThreshold: g(x) = c1 - x
//   else:              g(x) = k / x^n
func (p *PlatypusPool) gFunc(x *big.Int) *big.Int {
	if x.Cmp(p.xThreshold) < 0 {
		return new(big.Int).Sub(p.c1, x)
	}
	// g(x) = k * WAD / x^n
	// x^n computed via rpow (RAY precision)
	xPowN := rpow(x, p.n)
	if xPowN.Sign() <= 0 {
		return big.NewInt(0)
	}
	return wDiv(p.k, xPowN)
}

func (p *PlatypusPool) readSlot(contract common.Address, slot common.Hash) *big.Int {
	raw := p.reader(contract, slot)
	return new(big.Int).SetBytes(raw[:])
}

// rpow computes x^n in WAD precision using exponentiation by squaring.
// Converts to RAY (1e27) internally for precision, then back to WAD.
func rpow(x, n *big.Int) *big.Int {
	// Convert x from WAD to RAY: x_ray = x * 1e9
	xRay := new(big.Int).Mul(x, big.NewInt(1e9))

	// Exponentiation by squaring in RAY
	z := new(big.Int).Set(ray) // z = 1.0 in RAY
	base := new(big.Int).Set(xRay)
	exp := new(big.Int).Set(n)

	for exp.Sign() > 0 {
		if exp.Bit(0) == 1 {
			z.Mul(z, base)
			z.Add(z, rayHalf)
			z.Div(z, ray)
		}
		base.Mul(base, base)
		base.Add(base, rayHalf)
		base.Div(base, ray)
		exp.Rsh(exp, 1)
	}

	// Convert back from RAY to WAD: result = z / 1e9
	z.Div(z, big.NewInt(1e9))
	return z
}

// platypusMappingSlot computes keccak256(leftPad32(key) ++ leftPad32(slot)).
func platypusMappingSlot(key common.Address, baseSlot *big.Int) common.Hash {
	var data [64]byte
	copy(data[12:32], key.Bytes())
	baseSlot.FillBytes(data[32:64])
	return crypto.Keccak256Hash(data[:])
}

// platAssetEntry holds a Platypus asset's address and token decimals.
type platAssetEntry struct {
	addr     common.Address
	decimals int
}

// platypusAssetMap: pool -> token -> asset contract + decimals.
var platypusAssetMap = map[common.Address]map[common.Address]platAssetEntry{
	// Pool 0x5ee9: Main USD (USDT.e, USDC.e, DAI.e, USDC, USDt)
	common.HexToAddress("0x5ee9008e49b922cafef9dde21446934547e42ad6"): {
		common.HexToAddress("0xc7198437980c041c805a1edcba50c1ce5db95118"): {common.HexToAddress("0xba347682cc6175704bb78c55ac3687ff272409a8"), 6},  // USDT.e
		common.HexToAddress("0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664"): {common.HexToAddress("0x6c9137388c63351089315067927b4d7dca74460e"), 6},  // USDC.e
		common.HexToAddress("0xd586e7f844cea2f87f50152665bcbc2c279d8d70"): {common.HexToAddress("0x2d566402f05a980efa74b1815a99bcff200cd681"), 18}, // DAI.e
		common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"): {common.HexToAddress("0x2469eb1f646f57c1987b65a1bf1cdc47856d05fc"), 6},  // USDC
		common.HexToAddress("0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"): {common.HexToAddress("0xa6e3814743dffbcd3f561a26fecec6be25986b9a"), 6},  // USDt
	},
	// Pool 0x2779: USDC/USDbC
	common.HexToAddress("0x27792000fca68acdc2a08c1eed32e7a2a66ff3af"): {
		common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"): {common.HexToAddress("0xba056cdb411603bbd69c69f1422d37e46d66538c"), 6}, // USDC
		common.HexToAddress("0xd24c2ad096400b6fbcd2ad8b24e7acbc21a1da64"): {common.HexToAddress("0xd60bb9b48cbaad0f6e573f3c95632d8effa7d0d6"), 6}, // USDbC
	},
	// Pool 0x1332: YUSD/USDC
	common.HexToAddress("0x13320b3e1050b48776e6a019423effa7c5eba72e"): {
		common.HexToAddress("0x1c20e891bab6b1727d14da358fae2984ed9b59eb"): {common.HexToAddress("0xc75bd8e4a2e5f5c3daa63091b5dea0b3ead2d737"), 18}, // YUSD
		common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"): {common.HexToAddress("0xa55112f184feb9f60d0d80d68281fbc15e6fc308"), 6},  // USDC
	},
	// Pool 0xcee2: BTC.b/WBTC.e
	common.HexToAddress("0xcee236fdae6efba6a7e3c2a2c3a792fa27f3e263"): {
		common.HexToAddress("0x152b9d0fdc40c096757f570a51e494bd4b943e50"): {common.HexToAddress("0x00c0ff520db2fabd7abb6b4013bbc538c2e52360"), 8}, // BTC.b
		common.HexToAddress("0x50b7545627a5162f82a992c33b87adc75187b218"): {common.HexToAddress("0xe461378dc0b9b3b69ee24e689f8ef4ce47e92251"), 8}, // WBTC.e
	},
}
