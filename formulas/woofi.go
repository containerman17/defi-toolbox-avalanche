package formulas

// woofi.go — PoolQuoter for WooFi V2 (WooPPV2) oracle-based PMM.
//
// Single pool with multiple base tokens, all quoted against USDC.
// Swaps: base→quote, quote→base, base→base (routed through quote).
//
// Math: quoteAmount = baseAmount * price * (1 - gamma - spread) / baseDec
//       where gamma = baseAmount * price * coeff / baseDec (price impact)

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

var (
	wooPriceDec  = big.NewInt(1e8)  // oracle price decimals
	wooFeeBase   = big.NewInt(1e5)  // feeRate denominator
	woo1e18      = new(big.Int).SetUint64(1e18)
)

// WooFi pool and oracle storage slots
var (
	wooPoolSlotTokenInfos = big.NewInt(3)  // mapping(address => TokenInfo)
	wooPoolSlotQuoteToken = common.BigToHash(big.NewInt(4))
	wooPoolSlotOracle     = common.BigToHash(big.NewInt(5))

	wooOracleSlotInfos    = big.NewInt(1)  // mapping(address => packed price|coeff|spread)
)

// Masks for unpacking
var (
	wooMask128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	wooMask64  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	wooMask192 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 192), big.NewInt(1))
	wooMask16  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 16), big.NewInt(1))
)

// Token decimals for WooFi pool tokens (hardcoded — these never change)
var wooTokenDecimals = map[common.Address]int{
	common.HexToAddress("0x152b9d0fdc40c096757f570a51e494bd4b943e50"): 8,  // BTC.b
	common.HexToAddress("0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be"): 18, // sAVAX
	common.HexToAddress("0x2f6f07cdcf3588944bf4c42ac74ff24bf56e7590"): 18, // tsAVAX
	common.HexToAddress("0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab"): 18, // WETH.e
	common.HexToAddress("0x6e84a6216ea6dacc71ee8e6b0a5b7322eebc0fdd"): 18, // JOE
	common.HexToAddress("0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"): 6,  // USDt
	common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"): 18, // WAVAX
	common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"): 6,  // USDC
	common.HexToAddress("0xd24c2ad096400b6fbcd2ad8b24e7acbc21a1da64"): 6,  // USDbC
}

// WooFiPool is the PoolQuoter for WooFi V2 pools.
type WooFiPool struct {
	addr       common.Address
	poolAddr   common.Address // actual WooPPV2 contract (not the router)
	oracleAddr common.Address
	quoteToken common.Address
	quoteDec   *big.Int
	reader     StorageReader
}

// WooFi pool address (WooPPV2, the actual pool behind the router)
var wooPoolAddr = common.HexToAddress("0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4")

// wooActiveTokens lists tokens with actual reserves and router support.
var wooActiveTokens = map[common.Address]bool{
	common.HexToAddress("0x152b9d0fdc40c096757f570a51e494bd4b943e50"): true, // BTC.b
	common.HexToAddress("0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab"): true, // WETH.e
	common.HexToAddress("0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"): true, // USDt
	common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"): true, // WAVAX
	common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"): true, // USDC
}

func newWooFiPool(addr common.Address, reader StorageReader) *WooFiPool {
	pool := wooPoolAddr

	// Read oracle address from pool slot 5
	oracleRaw := reader(pool, wooPoolSlotOracle)
	oracle := common.BytesToAddress(oracleRaw[12:32])
	if oracle == (common.Address{}) {
		return nil
	}

	// Read quote token from pool slot 4
	quoteRaw := reader(pool, wooPoolSlotQuoteToken)
	quoteToken := common.BytesToAddress(quoteRaw[12:32])
	if quoteToken == (common.Address{}) {
		return nil
	}

	quoteDec, ok := wooTokenDecimals[quoteToken]
	if !ok {
		return nil
	}

	return &WooFiPool{
		addr:       addr,
		poolAddr:   pool,
		oracleAddr: oracle,
		quoteToken: quoteToken,
		quoteDec:   new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(quoteDec)), nil),
		reader:     reader,
	}
}

func (p *WooFiPool) Address() common.Address { return p.addr }

func (p *WooFiPool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if amountIn.IsZero() {
		return uint256.Int{}
	}

	// Map native AVAX sentinel to WAVAX
	wavax := common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7")
	nativeSentinel := common.HexToAddress("0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if tokenIn == nativeSentinel {
		tokenIn = wavax
	}
	if tokenOut == nativeSentinel {
		tokenOut = wavax
	}

	// Same token after mapping = no swap
	if tokenIn == tokenOut {
		return uint256.Int{}
	}

	// Only tokens with active reserves can be swapped
	if !wooActiveTokens[tokenIn] || !wooActiveTokens[tokenOut] {
		return uint256.Int{}
	}

	amt := amountIn.ToBig()

	if tokenIn == p.quoteToken {
		// Quote → Base (sellQuote)
		return p.sellQuote(tokenOut, amt)
	} else if tokenOut == p.quoteToken {
		// Base → Quote (sellBase)
		return p.sellBase(tokenIn, amt)
	} else {
		// Base → Base (through quote)
		return p.baseToBase(tokenIn, tokenOut, amt)
	}
}

// sellBase: base → quote
func (p *WooFiPool) sellBase(baseToken common.Address, baseAmount *big.Int) uint256.Int {
	price, coeff, spread := p.readOracleState(baseToken)
	if price.Sign() <= 0 {
		return uint256.Int{}
	}

	feeRate, reserve, maxGamma, maxNotional := p.readTokenInfo(baseToken)
	if reserve.Sign() <= 0 {
		return uint256.Int{}
	}

	baseDec := p.getBaseDec(baseToken)
	if baseDec == nil {
		return uint256.Int{}
	}

	quoteAmount, gamma := wooCalcQuoteAmountSellBase(baseAmount, price, coeff, spread, baseDec, p.quoteDec)
	if quoteAmount == nil || quoteAmount.Sign() <= 0 {
		return uint256.Int{}
	}

	// Check gamma <= maxGamma
	if maxGamma.Sign() > 0 && gamma.Cmp(maxGamma) > 0 {
		return uint256.Int{}
	}

	// Check notional <= maxNotionalSwap
	if maxNotional.Sign() > 0 && quoteAmount.Cmp(maxNotional) > 0 {
		return uint256.Int{}
	}

	// Apply fee
	fee := new(big.Int).Mul(quoteAmount, feeRate)
	fee.Div(fee, wooFeeBase)
	quoteAmount.Sub(quoteAmount, fee)

	// Check reserve (quote token reserve)
	_, quoteReserve, _, _ := p.readTokenInfo(p.quoteToken)
	if quoteReserve.Sign() > 0 && quoteAmount.Cmp(quoteReserve) > 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(quoteAmount)
	if overflow || quoteAmount.Sign() <= 0 {
		return uint256.Int{}
	}
	return *result
}

// sellQuote: quote → base
func (p *WooFiPool) sellQuote(baseToken common.Address, quoteAmount *big.Int) uint256.Int {
	feeRate, reserve, maxGamma, maxNotional := p.readTokenInfo(baseToken)
	if reserve.Sign() <= 0 {
		return uint256.Int{}
	}

	// Apply fee first (before computing base amount)
	fee := new(big.Int).Mul(quoteAmount, feeRate)
	fee.Div(fee, wooFeeBase)
	quoteAfterFee := new(big.Int).Sub(quoteAmount, fee)
	if quoteAfterFee.Sign() <= 0 {
		return uint256.Int{}
	}

	price, coeff, spread := p.readOracleState(baseToken)
	if price.Sign() <= 0 {
		return uint256.Int{}
	}

	baseDec := p.getBaseDec(baseToken)
	if baseDec == nil {
		return uint256.Int{}
	}

	baseAmount, gamma := wooCalcBaseAmountSellQuote(quoteAfterFee, price, coeff, spread, baseDec, p.quoteDec)
	if baseAmount == nil || baseAmount.Sign() <= 0 {
		return uint256.Int{}
	}

	// Check gamma
	if maxGamma.Sign() > 0 && gamma.Cmp(maxGamma) > 0 {
		return uint256.Int{}
	}

	// Check notional
	if maxNotional.Sign() > 0 && quoteAfterFee.Cmp(maxNotional) > 0 {
		return uint256.Int{}
	}

	// Check reserve
	if reserve.Sign() > 0 && baseAmount.Cmp(reserve) > 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(baseAmount)
	if overflow || baseAmount.Sign() <= 0 {
		return uint256.Int{}
	}
	return *result
}

// baseToBase: base1 → base2 (routed through quote).
// The WooPP pool applies a merged spread and merged fee for base-to-base swaps.
// spread = max(spread1, spread2) / 2, fee = max(feeRate1, feeRate2)
func (p *WooFiPool) baseToBase(base1, base2 common.Address, base1Amount *big.Int) uint256.Int {
	price1, coeff1, spread1 := p.readOracleState(base1)
	price2, coeff2, spread2 := p.readOracleState(base2)
	if price1.Sign() <= 0 || price2.Sign() <= 0 {
		return uint256.Int{}
	}

	feeRate1, reserve1, maxGamma1, maxNotional1 := p.readTokenInfo(base1)
	feeRate2, reserve2, maxGamma2, maxNotional2 := p.readTokenInfo(base2)

	// No reserves = no swap possible
	if reserve1.Sign() <= 0 || reserve2.Sign() <= 0 {
		return uint256.Int{}
	}

	// Spread = max(spread1, spread2) / 2
	spreadMerged := spread1
	if spread2.Cmp(spread1) > 0 {
		spreadMerged = spread2
	}
	spreadMerged = new(big.Int).Div(spreadMerged, big.NewInt(2))

	// Fee = max(feeRate1, feeRate2)
	feeRate := feeRate1
	if feeRate2.Cmp(feeRate1) > 0 {
		feeRate = feeRate2
	}

	base1Dec := p.getBaseDec(base1)
	base2Dec := p.getBaseDec(base2)
	if base1Dec == nil || base2Dec == nil {
		return uint256.Int{}
	}

	// Step 1: base1 → quote
	quoteAmount, gamma1 := wooCalcQuoteAmountSellBase(base1Amount, price1, coeff1, spreadMerged, base1Dec, p.quoteDec)
	if quoteAmount == nil || quoteAmount.Sign() <= 0 {
		return uint256.Int{}
	}
	if maxGamma1.Sign() > 0 && gamma1.Cmp(maxGamma1) > 0 {
		return uint256.Int{}
	}
	if maxNotional1.Sign() > 0 && quoteAmount.Cmp(maxNotional1) > 0 {
		return uint256.Int{}
	}

	// Apply fee to intermediate quote amount
	fee := new(big.Int).Mul(quoteAmount, feeRate)
	fee.Div(fee, wooFeeBase)
	quoteAmount.Sub(quoteAmount, fee)
	if quoteAmount.Sign() <= 0 {
		return uint256.Int{}
	}

	// Step 2: quote → base2
	base2Amount, gamma2 := wooCalcBaseAmountSellQuote(quoteAmount, price2, coeff2, spreadMerged, base2Dec, p.quoteDec)
	if base2Amount == nil || base2Amount.Sign() <= 0 {
		return uint256.Int{}
	}
	if maxGamma2.Sign() > 0 && gamma2.Cmp(maxGamma2) > 0 {
		return uint256.Int{}
	}
	if maxNotional2.Sign() > 0 && quoteAmount.Cmp(maxNotional2) > 0 {
		return uint256.Int{}
	}

	// Check reserve
	if reserve2.Sign() > 0 && base2Amount.Cmp(reserve2) > 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(base2Amount)
	if overflow || base2Amount.Sign() <= 0 {
		return uint256.Int{}
	}
	return *result
}

// wooCalcQuoteAmountSellBase: base → quote math
// Matches deployed WooPPV2 Solidity:
//   coef = 1e18 - (coeff * baseAmount * price / baseDec / priceDec) - spread
//   quoteAmount = baseAmount * quoteDec * price / priceDec * coef / 1e18 / baseDec
func wooCalcQuoteAmountSellBase(baseAmount, price, coeff, spread, baseDec, quoteDec *big.Int) (*big.Int, *big.Int) {
	// gamma = coeff * baseAmount * price / baseDec / priceDec
	gamma := new(big.Int).Mul(coeff, baseAmount)
	gamma.Mul(gamma, price)
	gamma.Div(gamma, baseDec)
	gamma.Div(gamma, wooPriceDec)

	// coef = 1e18 - gamma - spread
	coef := new(big.Int).Sub(woo1e18, gamma)
	coef.Sub(coef, spread)
	if coef.Sign() <= 0 {
		return nil, gamma
	}

	// quoteAmount = baseAmount * quoteDec * price / priceDec * coef / 1e18 / baseDec
	out := new(big.Int).Mul(baseAmount, quoteDec)
	out.Mul(out, price)
	out.Div(out, wooPriceDec)
	out.Mul(out, coef)
	out.Div(out, woo1e18)
	out.Div(out, baseDec)

	return out, gamma
}

// wooCalcBaseAmountSellQuote: quote → base math
// Matches deployed WooPPV2 Solidity:
//   coef = 1e18 - (quoteAmount * coeff / quoteDec) - spread
//   baseAmount = quoteAmount * baseDec * priceDec / price * coef / 1e18 / quoteDec
func wooCalcBaseAmountSellQuote(quoteAmount, price, coeff, spread, baseDec, quoteDec *big.Int) (*big.Int, *big.Int) {
	// gamma = quoteAmount * coeff / quoteDec
	gamma := new(big.Int).Mul(quoteAmount, coeff)
	gamma.Div(gamma, quoteDec)

	// coef = 1e18 - gamma - spread
	coef := new(big.Int).Sub(woo1e18, gamma)
	coef.Sub(coef, spread)
	if coef.Sign() <= 0 {
		return nil, gamma
	}

	// baseAmount = quoteAmount * baseDec * priceDec / price * coef / 1e18 / quoteDec
	out := new(big.Int).Mul(quoteAmount, baseDec)
	out.Mul(out, wooPriceDec)
	out.Div(out, price)
	out.Mul(out, coef)
	out.Div(out, woo1e18)
	out.Div(out, quoteDec)

	return out, gamma
}

// readOracleState reads price, coeff, spread from oracle's infos[token].
func (p *WooFiPool) readOracleState(token common.Address) (price, coeff, spread *big.Int) {
	slot := wooMappingSlot(token, wooOracleSlotInfos)
	raw := p.reader(p.oracleAddr, slot)
	val := new(big.Int).SetBytes(raw[:])

	// Packed: spread (64 bits, 192-255) | coeff (64 bits, 128-191) | price (128 bits, 0-127)
	price = new(big.Int).And(val, wooMask128)
	coeff = new(big.Int).And(new(big.Int).Rsh(val, 128), wooMask64)
	spread = new(big.Int).And(new(big.Int).Rsh(val, 192), wooMask64)
	return
}

// readTokenInfo reads feeRate, reserve, maxGamma, maxNotionalSwap from pool's tokenInfos[token].
func (p *WooFiPool) readTokenInfo(token common.Address) (feeRate, reserve, maxGamma, maxNotional *big.Int) {
	slot := wooMappingSlot(token, wooPoolSlotTokenInfos)
	// slot+0: feeRate (uint16, bits 192-207) | reserve (uint192, bits 0-191)
	raw0 := p.reader(p.poolAddr, slot)
	val0 := new(big.Int).SetBytes(raw0[:])
	reserve = new(big.Int).And(val0, wooMask192)
	feeRate = new(big.Int).And(new(big.Int).Rsh(val0, 192), wooMask16)

	// slot+1: maxNotionalSwap (uint128, bits 128-255) | maxGamma (uint128, bits 0-127)
	slot1 := new(big.Int).Add(slot.Big(), big.NewInt(1))
	raw1 := p.reader(p.poolAddr, common.BigToHash(slot1))
	val1 := new(big.Int).SetBytes(raw1[:])
	maxGamma = new(big.Int).And(val1, wooMask128)
	maxNotional = new(big.Int).Rsh(val1, 128)
	return
}

func (p *WooFiPool) getBaseDec(token common.Address) *big.Int {
	dec, ok := wooTokenDecimals[token]
	if !ok {
		return nil
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
}

// wooMappingSlot computes keccak256(leftPad32(key) ++ leftPad32(slot)).
func wooMappingSlot(key common.Address, baseSlot *big.Int) common.Hash {
	var data [64]byte
	copy(data[12:32], key.Bytes())
	baseSlot.FillBytes(data[32:64])
	return crypto.Keccak256Hash(data[:])
}
