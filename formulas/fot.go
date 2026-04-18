package formulas

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
)

// Fee-on-transfer (FoT) token transfer tax rates on Avalanche C-Chain.
//
// Each token has an exact fee calculator that replicates the Solidity integer
// math from its transfer() function. This avoids rounding mismatches that
// occur when approximating with a single bps rate.

// IsFotExemptPool returns true if the pool is exempt from FoT adjustments.
func IsFotExemptPool(pool string) bool {
	return FotExemptPools[pool]
}

// IsFotExemptInputPool returns true if the pool is exempt from FoT on the input side only.
// This covers tokens where fee is skipped when the pool is the recipient (to == pool).
func IsFotExemptInputPool(pool string) bool {
	return FotExemptInputPools[pool]
}

// IsFotExemptOutputPool returns true if the pool is exempt from FoT on the output side only.
// This covers tokens where fee is skipped when the pool is the sender (from == pool),
// e.g. tokens using noTaxable[pool]=true which exempts the pool as a sender.
func IsFotExemptOutputPool(pool string) bool {
	return FotExemptOutputPools[pool]
}

// fotCalc holds a fee calculator and an optional received-amount calculator.
// When calcReceived is set, it computes the post-fee amount directly using the
// complement form (e.g., amount * (10000 - fee) / 10000), which matches
// Solidity's integer division rounding exactly. Without it, the model falls
// back to amount - calcFee(amount), which can be 1 wei MORE than Solidity
// when amount is not divisible by the fee denominator.
type fotCalc struct {
	calcFee      func(*big.Int) *big.Int
	calcReceived func(*big.Int) *big.Int // optional; used by fotTokenModel.adjust
}

// Helper: fee = amount * rate / 10000, received = amount * (10000 - rate) / 10000
func fotBps(rate int64) fotCalc {
	complement := 10000 - rate
	return fotCalc{
		calcFee: func(amount *big.Int) *big.Int {
			fee := new(big.Int).Mul(amount, big.NewInt(rate))
			fee.Div(fee, big.NewInt(10000))
			return fee
		},
		calcReceived: func(amount *big.Int) *big.Int {
			result := new(big.Int).Mul(amount, big.NewInt(complement))
			result.Div(result, big.NewInt(10000))
			return result
		},
	}
}

// Helper: fee = amount * rate / 100, received = amount * (100 - rate) / 100
func fotPct(rate int64) fotCalc {
	complement := 100 - rate
	return fotCalc{
		calcFee: func(amount *big.Int) *big.Int {
			fee := new(big.Int).Mul(amount, big.NewInt(rate))
			fee.Div(fee, big.NewInt(100))
			return fee
		},
		calcReceived: func(amount *big.Int) *big.Int {
			result := new(big.Int).Mul(amount, big.NewInt(complement))
			result.Div(result, big.NewInt(100))
			return result
		},
	}
}

// fotCustom wraps a custom fee calculator without a complement form.
// The model will use amount - calcFee(amount), which is correct for
// tokens with multi-step fee deductions matching Solidity's subtraction.
func fotCustom(calcFee func(*big.Int) *big.Int) fotCalc {
	return fotCalc{calcFee: calcFee}
}

var fotCalculators = map[string]fotCalc{
	// =====================================================================
	// Tokens with exact Solidity math from source code analysis
	// =====================================================================

	// Good Bridging (GB): moved to reflectionTokenConfigs for exact RFI math.
	// Was: 1% pure reflection, ~3.7 PPM residual with static fee.

	// SLED: moved to reflectionTokenConfigs for exact RFI math (2% pure reflection).
	// Was: fotPct(2), ~22 PPM residual from reflection redistribution drift.

	// JUNIOR (0x214d): fee = amount * 1 / 100 (1% tax, subtract form, mutable up to 9%)
	// Pools: 0xd100bb9a (lfj_v1, JUNIOR/WAVAX), 0xcf25fba7 (lfj_v1, JUNIOR/HOWDY),
	//        0x02f51540 (lfj_v1), 0xd3e6527c (uniswap_v2, JUNIOR/WAVAX).
	"0x214dd1b5cbe543d4189ab39832f1bc1eedebb1d3": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(1))
		fee.Div(fee, big.NewInt(100))
		return fee
	}),

	// GIVE TR YOUR COQ (0xa12d): fee = amount * 6 / 100 (6% tax, subtract form)
	// Pool: 0xd65328f9 (lfj_v1, token0/WAVAX)
	"0xa12dd2e5bcd0611a9245518902effa73e788b142": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(6))
		fee.Div(fee, big.NewInt(100))
		return fee
	}),

	// Tortuga: fee = amount*3/100 + amount*3/100 + amount*1/100 (THREE separate divisions)
	"0xab2712b217f0015b602c06e4fb66b8cf8b04f894": fotCustom(func(amount *big.Int) *big.Int {
		f1 := new(big.Int).Mul(amount, big.NewInt(3))
		f1.Div(f1, big.NewInt(100))
		f2 := new(big.Int).Mul(amount, big.NewInt(3))
		f2.Div(f2, big.NewInt(100))
		f3 := new(big.Int).Mul(amount, big.NewInt(1))
		f3.Div(f3, big.NewInt(100))
		return f1.Add(f1, f2).Add(f1, f3)
	}),

	// GoodToken (GOOD): fee = (amount * 2) / 100 (2% tax, only when sender==lp || recipient==lp)
	// Pool: 0x21013fe86ad9646c41cd1f9d57e69e933524dcd5 (lfj_v1, GOOD/0x420f)
	// setLiquidity(pool) makes the pool the registered LP; fee applies both directions.
	"0x169e8f8773072ce4b87fb7e7a47eed31b481a31f": fotPct(2),

	// L-Swing: moved to reflectionTokenConfigs for exact RFI math (20% pure reflection).
	// Was: fotPct(20), ~53 ppb residual from missing reflection redistribution bonus.

	// WorldOfDogs: fee = amount*6/100 + amount*5/100 (TWO separate divisions)
	"0xadcfb771e88fd804e0fb04eef6492a0daf389c51": fotCustom(func(amount *big.Int) *big.Int {
		f1 := new(big.Int).Mul(amount, big.NewInt(6))
		f1.Div(f1, big.NewInt(100))
		f2 := new(big.Int).Mul(amount, big.NewInt(5))
		f2.Div(f2, big.NewInt(100))
		return f1.Add(f1, f2)
	}),

	// SPORE: moved to reflectionTokenConfigs for exact RFI math.
	// Was: 6% pure reflection (div-then-mul), ~0.575 PPM residual with static fee.

	// Safemoon fork: fee = amount * 500 / 10000
	"0x3960716779870ef8757aeb43f3c4f0c30cb2d557": fotBps(500),

	// BillMaher.Finance (BMF, 0x9e78): bytecode unverified on routescan, empirical
	// 599 bps tax on pool→router transfer. Suspected 6% tax with reflection-style
	// rounding drift; using 600 underquotes safely by ~10B wei at 239e21 scale.
	// Used by pools 0x04a47b49, 0x9cFDa80B, 0x9de728D3 (all pangolin_v2).
	"0x9e7890b2e2de38d764a29b0de1e2b9b37c90ddb9": fotBps(600),

	// Tjoma (0x8d4a): RFI reflection, _taxFee applied via _getValues with rate.
	// Empirical 599 bps on bench-amount transfers. Using fotBps(600) underquotes
	// safely. Precise RFI math would need reflectionTokenConfigs entry.
	// Pools 0xAfAd3c7C, 0xc4877C4a (pangolin_v2).
	"0x8d4a8522f2fb040d4755757c05d99976c0d29fce": fotBps(600),

	// THORSMEAD (0x245c): RFI reflection, current _taxFee=5 (charityFee,
	// liquidityFee are 0). transfer() applies 5% reflection fee per
	// calculateTaxFee. Pool 0xb97F23A9 (lfj_v1).
	"0x245c2591403e182e41d7a851eab53b01854844ce": fotBps(500),

	// SnowyYields (0xcd0d): _finalSellTax=4, _finalBuyTax=4, applied as
	// amount.mul(4).div(100) when from==pair or to==pair (bench path is
	// pool→router, hitting the buy branch since router is not excluded).
	// Pool 0x57Ba9107 (lfj_v1).
	"0xcd0dcc37376a96078014561312e906cbabb2045c": fotBps(400),

	// DejàVu: fee = amount * 51 / 10000
	"0x78aed06eb93351aae6886d9c012888f87b64c918": fotBps(51),

	// DICK (CoinToken, 0xaaec): moved to reflectionTokenConfigs for exact RFI+burn math.
	// Was: TAX=1% reflect + BURN=1% + CHARITY=2% = 4% total, ~0.13 PPM residual with static fee.

	// 0x4fc8: same double-division pattern, total 500 bps
	// DEX pair exemption possible (_isExcluded[recipient] or FeeAddress)
	"0x4fc8aab93a6e4e6928fd7e9ba979a715ccf55a6a": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(500))
		fee.Div(fee, big.NewInt(100))
		fee.Div(fee, big.NewInt(100))
		return fee
	}),

	// Bonfire: fee = 3 * (amount * 300 / 100 / 100) — three separate fees of 300 each
	"0xa0a924dcb97a597351a5c3787234b706845e7510": fotCustom(func(amount *big.Int) *big.Int {
		// Each sub-fee: amount * 300 / 100 / 100
		oneFee := func() *big.Int {
			f := new(big.Int).Mul(amount, big.NewInt(300))
			f.Div(f, big.NewInt(100))
			f.Div(f, big.NewInt(100))
			return f
		}
		total := oneFee()
		total.Add(total, oneFee())
		total.Add(total, oneFee())
		return total
	}),

	// Green Token (GREEN): moved to reflectionTokenConfigs for exact RFI math (0 ppb).
	// Was: 1% reflection + 3% team = 4% total, ~2.93 PPM residual with static fee.

	// AvaFOX (AFM): moved to reflectionTokenConfigs for exact RFI math (0 ppb).
	// Was: 1% reflection + 3% team = 4% total, ~2.35 PPM residual with static fee.

	// BYAS: fee = amount * 30 / 1000
	"0x26b13e7673cd4d47783c863c2ec7b20ac74fbe60": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(30))
		fee.Div(fee, big.NewInt(1000))
		return fee
	}),

	// BigRed: fee = amount * 3 / 100 (300 bps, only on pair trades after buyCount>100)
	"0x87bbfc9dcb66caa8ce7582a3f17b60a25cd8a248": fotPct(3),

	// fBomb: fee = amount - amount * 99 / 100 (100 bps burn)
	// DEX pair exemption possible (taxExempt list)
	"0x5c09a9ce08c4b332ef1cc5f7cadb1158c32767ce": fotCustom(func(amount *big.Int) *big.Int {
		kept := new(big.Int).Mul(amount, big.NewInt(99))
		kept.Div(kept, big.NewInt(100))
		return new(big.Int).Sub(amount, kept)
	}),

	// SABTIWE2.0 (Stars Arena Bailout Edition): fee = amountToTake1(value) = ceil(value,50)*50/100 ≈ 50%
	// hyperSonic=true && liqBugFixed=true (on-chain confirmed via storage slot 15).
	// transfer() path: totalLoss = ceil(amount,50)*50/100; tokensToTransfer = amount - totalLoss.
	// For large amounts (multiples of 50), ceil(v,50)=v so fee = v*50/100 = v/2 exactly.
	// Output token in pool 0xdf56a97e (lfj_v1), dir=1 WAVAX→SABTIWE2.0.
	"0x791ae3e4ade59a63fd2a1c1da9218c1e4da4db16": fotPct(50),

	// Waifu: taxAmount is dynamic, reads 0 at pinned block 0x4cea4a8 — no fee active.
	// Removed: "0xff24003428fb2e969c39edee4e9f464b0b78313d": fotBps(50),

	// BulletCollection: fee = amount * totalFeePercentage / 10000 (totalFeePercentage=50 from bulletConfig).
	// FoT only applies on transfers involving registered AMM pairs (isAutomatedMarketMakerPair).
	// Pool 0x70201236 (lfj_v1) IS registered → fee applies.
	// Pool 0x3c4beea7 (lfj_v1) is NOT registered → exempt (see FotExemptPools).
	"0xf84be5e3f534e6d4b60d104b299e33ecb03ce7fd": fotBps(50),

	// HERESY (BulletCollection): fee = amount * 50 / 10000 (totalFeePercentage=50 from shared bulletConfig).
	// FoT only applies on transfers involving registered AMM pairs (isAutomatedMarketMakerPair).
	// The lfj_v1 pair (0x17885bb0) IS registered → fee applies.
	// Pharaoh/V3 pairs are NOT registered → exempt (see FotExemptPools).
	"0x432d38f83a50ec77c409d086e97448794cf76dcf": fotBps(50),

	// Pollen: fee = amount * totalFees / 100 (totalFees=3, confirmed via storage slot 23)
	// Fee gate: _isExcludedFromFees[from||to] — neither known pool is excluded.
	// automatedMarketMakerPairs is NOT a fee gate (only governs sell-tx-limit).
	// Applies to all Pollen pools:
	//   pool 0xdf4eb13a7dd25d0086be88a0c99a8b772ddd0db3 (lfj_v1, USDC.e/Pollen)
	//   pool 0x2742e6d7bf96154cacca20a6d83c37af615e5d98 (lfj_v1, WAVAX/Pollen)
	// ~3.09% observed mismatch matches 1/(1-0.03) ratio exactly.
	"0xc118d77baf86a93ec41d867675c48c98b19953fd": fotPct(3),

	// Hamster (HAM): fee = tAmount.div(100).mul(2) (div-then-mul, 2% reflection tax)
	// _getTValues: tFee = tAmount.div(100).mul(2); hardcoded, no exemptions.
	// Residual ~26 ppm from reflection rate drift — within tolerance.
	// Pool: 0x1e41a42bd47ea44c09b01e06d498164159e3d0f3 (lfj_v1, WAVAX/HAM)
	"0xcbcc61f7a0b39512a6f986ddf174caf7232a0808": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Div(amount, big.NewInt(100))
		fee.Mul(fee, big.NewInt(2))
		return fee
	}),

	// AtlantisUniverse (AUA): fee = (amount / 100) * 2 (integer div first, then mul — 2% reflection tax)
	// _getTValues: tFee = tAmount.div(100).mul(2) — unconditional, no DEX pair exemption.
	// Pool: 0xd755a2083b8a85048705b72e6d176ea25a71dad8 (lfj_v1, WAVAX/AUA), dir=0.
	"0xb8edc9145e21a7c3345b848ca73300fa35150b0f": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Div(amount, big.NewInt(100))
		fee.Mul(fee, big.NewInt(2))
		return fee
	}),

	// ALAQ: fee = (amount / 100) * 5 (integer div first, then mul — 5% tax)
	// noTaxable[pool] is set per-pool by owner. Uniswap_v2 pool 0x661368c5bd has noTaxable=true
	// (pool is sender → output side exempt); sushiswap pools 0x4e29f0aa and 0xb6eda80d are NOT exempt.
	// Fee gate: !noTaxable[sender] — applies on input side (user sends to pool) for all pools,
	// but NOT on output side for the uniswap_v2 pool (see FotExemptOutputPools).
	"0xca3130f29e296f1966e5999889d0824a9032ee97": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Div(amount, big.NewInt(100))
		fee.Mul(fee, big.NewInt(5))
		return fee
	}),

	// 0xfb8a (WAVAX/0xfb8a lfj_v1 pool 0x16f1): fee = amount * 501 / 10000 (5.01% tax, subtract form)
	// Pool: 0x16f139fe (lfj_v1 pool#2570). Complement form (9499/10000) is 1 wei off.
	"0xfb8a29e67eff2f8ec633771b572b08b7e69c57b4": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(501))
		fee.Div(fee, big.NewInt(10000))
		return fee
	}),

	// 0xc970 (WAVAX/0xc970 lfj_v1 pool 0x117ef430): fee = amount * 2 / 100 (2% tax, subtract form)
	// Both directions affected: output tax (dir=0) and input tax (dir=1).
	// Pools: 0x117ef430 (lfj_v1), 0x52495ce8 (lfj_v1). Pharaoh pool 0x13e4a7f1 exempt (not liquidityPool).
	"0xc970d70234895dd6033f984fd00909623c666e66": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(2))
		fee.Div(fee, big.NewInt(100))
		return fee
	}),

	// HEFE: fee = amount * 10 / 1000 (1% tax on buys/sells for registered LPs)
	"0x18e3605b13f10016901eac609b9e188cf7c18973": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(10))
		fee.Div(fee, big.NewInt(1000))
		return fee
	}),

	// Vaccine: fee = amount * 19 / 10000 (0.19% = covidnineteenFee=19 bps),
	// with subtract-form rounding.
	"0x89d4c4dbcd477345f8fbb083d1194faeafba1522": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(19))
		fee.Div(fee, big.NewInt(10000))
		return fee
	}),

	// HOWDY: fee = floor(amount / 14)
	"0x7b640a60daa4ee5fbc2ce81797c11d174daa3b4f": fotCustom(func(amount *big.Int) *big.Int {
		return new(big.Int).Div(amount, big.NewInt(14))
	}),

	// EverRise: fee = amount * liquidityFee / 100 (liquidityFee=5, mutable up to 10)
	"0xc17c30e98541188614df99239cabd40280810ca3": fotPct(5),

	// GOD CHEEMS (GCH): fee = amount * _taxFee / 100 (_taxFee=5, slot[12], reflection token).
	// Pool 0xa457717dad87d72de49d3f10677c5a3846037595 (lfj_v1, GCH/WAVAX), dir=1.
	// Confirmed: formula*(1-0.05) ~= evm to within 0.0073% (residual = rFee reflection redistribution).
	"0xa755c4aa57315933ee7b8de0b41b1f02911f1f5a": fotPct(5),

	// Red Pepe (RPEPE): fee = value * taxRate / 10000 (taxRate=69, mutable)
	"0xb36faf341c7817d681f23bcedbd3d85467e5ad9f": fotBps(69),

	// BABYTOKEN: fee = amount * totalFees / 100 (totalFees=6, mutable up to 25)
	"0x50ad50fce988bcbf0d11f0c633e34c3510efbe54": fotPct(6),

	// Avalanche Subnets Memes (DODO factory): fee = amount*660/10000 + amount*330/10000 (9.9%)
	"0xf80fc26d5d20cca25c1c987abf7932942f9e57eb": fotCustom(func(amount *big.Int) *big.Int {
		burn := new(big.Int).Mul(amount, big.NewInt(660))
		burn.Div(burn, big.NewInt(10000))
		team := new(big.Int).Mul(amount, big.NewInt(330))
		team.Div(team, big.NewInt(10000))
		return burn.Add(burn, team)
	}),

	// Miller (20lab.app): fee = amount * 1074 / 10000 (10.74%)
	"0x3c859470c9b6220036fa4461f516ad8049671176": fotBps(1074),

	// MetaFloki: fee = tAmount * taxFee / 100 (taxFee=10, reflection only).
	// _teamFee=10 is also deducted from sender but credited to contract via _takeTeam,
	// NOT subtracted from recipient's rOwned — recipient-visible FoT is taxFee=10% only.
	// Pool 0x235bd272c84acb448db66fd0c47727f8eb582594 (pangolin_v2), dir=1.
	// Confirmed: formula*(1-0.10) = evm to within 1 wei.
	"0x9b413747801cb9def889bc865fe43c2a65585fb1": fotPct(10),

	// SHIBX (SHIBAVAX): moved to reflectionTokenConfigs for exact RFI math (0 ppb).
	// Was: 10% reflection tax, ~23.7 PPM residual with static fee.
	// Pools: 0x82ab53e405 (pangolin_v2), 0x3f7e7ca004 (partyswap).

	// Raini Studios Token (RST): fee = (amount * transferFeeBasisPoints) / 10000
	// transferFeeBasisPoints=100 (1%, confirmed on-chain; MAX_FEE=200, mutable).
	// Fee applies only when `to` has FEE_TO_ROLE; pool 0x648c2151d7 has FEE_TO_ROLE confirmed.
	// `from` must not have NO_FEE_FROM_ROLE (pool is recipient, sender is router — not exempt).
	"0x23675ba5d0a8075da5ba18756554e7633cea2c85": fotBps(100),

	// Mistel Finance (reflection): fee = amount*3/100 + amount*8/100 (~11%)
	"0xf3f8772f92028bfb6d641c28bbcf1dbded424767": fotCustom(func(amount *big.Int) *big.Int {
		tax := new(big.Int).Mul(amount, big.NewInt(3))
		tax.Div(tax, big.NewInt(100))
		team := new(big.Int).Mul(amount, big.NewInt(8))
		team.Div(team, big.NewInt(100))
		return tax.Add(tax, team)
	}),

	// ARENA BURN (Gladiator): fee = (value * fee) / denominator = value * 20000 / 1000000 (2%)
	// Source: _transfer() sets _fee = (value*fee)/denominator when _trade (from==pool || to==pool).
	// fee var was changed from 30000 (3%) to 20000 (2%) via setTax().
	// denominator=1000000, fee=20000 → 2% = fotBps(200) (integer-identical: x*20000/1000000 == x*200/10000)
	// Applies on both buy (from==pool) and sell (to==pool) sides.
	"0x8a398a53dbd7181d3131eafa1c8e73760889a9ad": fotBps(200),

	// =====================================================================
	// Tokens not yet source-analyzed — using standard bps approximation.
	// These are tokens that showed consistent bps rates across multiple pools.
	// If they cause mismatches, source-analyze their exact Solidity math.
	// =====================================================================

	// 0x41df729c — 3% tax (300 bps), seen in sushiswap_v2, uniswap_v2, uniswap_v3
	"0x41df729c20fc8792ed3687420a8666255a092e9a": fotBps(300),

	// 0x0592af54 — 1% tax (100 bps), seen in lfj_v1, pangolin_v2
	"0x0592af5414f2f8d90a5ae3c25e937804d3965c87": fotBps(100),

	// 0xc9ac17de — 5% tax (500 bps), seen in sushiswap_v2, lfj_v1
	"0xc9ac17de0c47129efb09224af75fcaff07608b7a": fotBps(500),

	// 0xf9a075c9 — 5% tax (500 bps), seen in lfj_v1, partyswap.
	// Current live pools match subtract-form rounding.
	"0xf9a075c9647e91410bf6c402bdf166e1540f67f0": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(500))
		fee.Div(fee, big.NewInt(10000))
		return fee
	}),

	// 0xcc0cbc7a — 1% tax (100 bps), seen in pangolin_v2, lfj_v2
	"0xcc0cbc7aad6e89ffbe5028dea24dd80ddeb8455b": fotBps(100),

	// 0x039d2e8f (LINDA) — 1% tax (100 bps) on registered AMM pairs (lfj_v1 at slot 15).
	// Unregistered pairs (e.g., pharaoh_v1) get the transfer fee of 50 bps.
	// The token subtracts fee from the transfer amount, so use subtract-form
	// rounding instead of the complement form.
	// See FotPoolTokenOverrides for per-pool adjustments.
	"0x039d2e8f097331278bd6c1415d839310e0d5ece4": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(100))
		fee.Div(fee, big.NewInt(10000))
		return fee
	}),

	// 0x0512384c — 1% tax (100 bps)
	"0x0512384c595ef182f3dacd8414e951c2fa7f6ee9": fotBps(100),

	// 0x0fec6d8a — 1% tax (100 bps)
	"0x0fec6d8a84a85b79a1ffe0e28c1902e08b653efe": fotBps(100),

	// 0x8901cb2e — 1% tax (100 bps)
	"0x8901cb2e82cc95c01e42206f8d1f417fe53e7af0": fotBps(100),

	// 0xa2cac35f — 1% tax (100 bps)
	"0xa2cac35f93c4a8005983bcc6748a6bc477827168": fotBps(100),

	// 0x030afbbf — 5% tax (500 bps)
	"0x030afbbf8b85ccfdc860b014439fd89ebc3e71a4": fotBps(500),

	// 0x0fec28d1 — 5% tax (500 bps)
	"0x0fec28d1694914cf3cbf4eda5eebeaf9e7b8e643": fotBps(500),

	// 0x7f64a65c — 5% tax (500 bps)
	"0x7f64a65c0d38d4150d73178d869663dcce4c0141": fotBps(500),

	// 0x8cb66252 — 5% tax (500 bps)
	"0x8cb66252e8c03791de080df5fb3d979e46f1cc27": fotBps(500),

	// 0x8908ea96 — 5% tax (500 bps)
	"0x8908ea968d2f79d078d893c0bcecd63eacdd9322": fotBps(500),

	// 0x4ba16daf — 10% tax (1000 bps)
	"0x4ba16daf8ed418ded920c66e45cc3eaffde53ac7": fotBps(1000),

	// 0x8a610bf3 — 10% tax (1000 bps), subtract-form rounding.
	"0x8a610bf3b64099a2bd9ef293838ba35986a3dfbb": fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(1000))
		fee.Div(fee, big.NewInt(10000))
		return fee
	}),

	// 0x894aa2d0 — 10% tax (1000 bps)
	"0x894aa2d0d3e63471c5ffbd22a8a95c8476826cf9": fotBps(1000),

	// 0x2ab6e759 — 10% tax (1000 bps)
	"0x2ab6e7599372ebaedc0329379a9d97402a7bcadd": fotBps(1000),

	// 0x9d11bb9b — 2% tax (200 bps)
	// DEX pair exemption possible (_isExcludedFromFee)
	"0x9d11bb9b6b6134182477859937c4c3921f5bf441": fotBps(200),

	// BabyCoq (0x22897cf0): moved to reflectionTokenConfigs for exact RFI math.
	// Was: fotBps(103), ~25 PPM residual from pool-side reflection bonus on existing balance.

	// KIOO (Reflectx): moved to reflectionTokenConfigs for exact RFI+burn math.

	// MMTH (Mammoth): recipient gets amount * (10000 - taxfee) / 10000, taxfee=100 → 1%
	// taxenabled=true, pool is not tax-exempt. Fee = amount * taxfee / 10000.
	"0x09ef821c35b4577f856ca416377bd2dddbd3d0c9": fotBps(100),

	// Build Token (BUILD): fee = amount * transferFee / 10000 (transferFee=100, 1%)
	// feeTo=0x000...dEaD (burn); MAX_TRANSFER_FEE=500 (mutable, currently 100).
	// isExcluded[pool]=false → fee applies. No exemption for AMM pairs.
	// Pool: 0x87305ece9f6dbf58522a5507afe09a4f8a9e7cb0 (radioshack, BUILD/RADIO)
	"0x5f018e73c185ab23647c82bd039e762813877f0e": fotBps(100),

	// Majinoors: reflectionFee=500 + tradingRoyalty=50 = 5.5% nominal, 3.5% effective
	"0xb528f3171bcdd1f5ee578fa7a2aaea75ce945094": fotBps(350),

	// NOTE: 0xe668f8030bf17f3931a3069f31f4fa56efe9dd54 (WSPP) — confirmed NOT FoT, removed.
}

// FotExemptInputPools lists pool addresses where FoT should NOT be applied on the
// INPUT side only. This occurs when a token's transfer() skips fees when `to` is a
// registered LP address (i.e., when selling INTO the pool, no fee is charged), but
// still charges fees when transferring OUT of the pool to a buyer.
//
// Example: bCASH (0x4ba16daf) checks lp[to]==1; the pool is registered, so
// transferring bCASH to the pool (dir=0) is fee-free, but transferring bCASH out
// to a buyer (dir=1) still incurs the 10% fee.
var FotExemptInputPools = map[string]bool{
	// bCASH (0x4ba16daf): lp[pool]=1 → fee-free when pool is `to` (dir=0, input side).
	// Fee still applies when pool is `from` and buyer is `to` (dir=1, output side).
	"0x07280f32830e3a1ca7b535b603b09890e692eaf6": true, // bCASH/WAVAX pangolin_v2
}

// FotExemptOutputPools lists pool addresses where FoT should NOT be applied on the
// OUTPUT side only. This occurs when a token's transfer() skips fees when `from` is
// the pool (i.e., noTaxable[pool]=true), so buying OUT of the pool is fee-free, but
// selling INTO the pool (user is sender) still incurs the fee on the input side.
var FotExemptOutputPools = map[string]bool{
	// ALAQ (0xca31...): noTaxable[pool]=true — pool is sender on output → no output fee.
	// Fee still applies when user sends ALAQ into the pool (dir=1, input side).
	// sushiswap pools 0x4e29f0aa and 0xb6eda80d are NOT in noTaxable → not exempt.
	"0x661368c5bdecd87475aae157b9ea718c0450125f": true, // ALAQ/WAVAX uniswap_v2

	// RST (RainiStudiosToken, 0x23675ba5): fee = amount * transferFeeBasisPoints / 10000.
	// Fee gate: hasRole(FEE_TO_ROLE, to) — fee only when recipient has FEE_TO_ROLE.
	// Pool has FEE_TO_ROLE (input fee works), but swap buyer doesn't → no output fee.
	"0x648c2151d7e6f43849c4d3abace0b12474814dc5": true, // RST/WAVAX lfj_v1
}

// FotExemptPools lists pool addresses where FoT should NOT be applied even though
// one of their tokens is in the FoT list. This happens when the token's transfer
// function checks for specific DEX pair addresses (e.g., isAutomatedMarketMakerPair)
// and the pool is not registered.
var FotExemptPools = map[string]bool{
	// BigRed (0x87bb...): only charges fee on its JoeV2Pair (0x7ef8e0af, lfj_v1).
	// Any other pool is not JoeV2Pair, so no fee is charged.
	"0x97fe71c72037d307d69694e41f20d97868c848ec": true, // BigRed/USDC uniswap_v3
	"0x9514c20a3c020ba4bc21f565e92aa2aa2875e6be": true, // BigRed/WAVAX uniswap_v2
	"0xab043e1b1cb3ac3a97485b71b77e586b6d4422aa": true, // BigRed/AMI lfj_v1
	"0x95375153743540a3a443b6cddece480e99576c32": true, // BigRed/COOP lfj_v1
	"0xb562931b866369770e8d2ae72782f9186e9f561f": true, // BigRed/NICK lfj_v1
	"0x8f2b16e2386000caefb9211c70fc631dcd2327bb": true, // KIMBO/BigRed lfj_v1
	"0x65659f44053eaf634ef924edb6427014b6f00b60": true, // BigRed/WAVAX lfj_v2

	// HERESY (BulletCollection, 0x432d...): only charges fee on registered AMM pairs.
	// Pharaoh V1/V3 pairs and lfj_v1 pair 0x2be1dcc are not registered in the swapManager.
	// Only 0x17885bb0 (lfj_v1 HERESY/WAVAX) is registered.
	"0x04a954bc8af9a1fdc2ce5f3192bdca369a4512cc": true, // HERESY/WAVAX pharaoh_v1
	"0x2bcbf5c38a0e11985779f507c5b98ad1fdd7b196": true, // HERESY/WAVAX pharaoh_v1
	"0x08ca0e8905beb997a6ade2a9a89a5a31eb1698ff": true, // HERESY/WAVAX pharaoh_v3
	"0x2be1dccb84a06235a611cfae1d137388e710854a": true, // HERESY/MeowCat lfj_v1 — not registered in swapManager

	// YFX (0x8901...): pool has NOT_TAXED_TO and NOT_TAXED_FROM roles — fully exempt.
	"0x640f87fef16c1e767ed80bae124e067b49d3e6a7": true, // YFX/USDC.e pharaoh_v1

	// BulletCollection (0xf84b...): only charges fee on registered AMM pairs.
	// Pool 0x3c4beea7 (lfj_v1) is NOT registered as an AMM pair.
	"0x3c4beea709e9a46f869ef5c1e9b18fd2195bd87f": true, // BulletCollection/USDC lfj_v1

	// ARENA BURN / Gladiator (0x8a39...): fee only applies when _trade flag is set.
	// Only pool 0xc7087eb4 triggers _trade; other pools are exempt.
	"0xdf9db5a5f3a00e0e27def12af95b4528ec23cf86": true, // Gladiator/ArenaToken lfj_v1
	"0x592ac0969b67457842af49555633b1f5ff730cb5": true, // Gladiator/WAVAX lfj_v1
	"0x4c79e30bc8eb6d83620b6166e49a27615eeed221": true, // Gladiator/USDC lfj_v1
	"0x11af559cacbcec2b0941257da071a53363ee984b": true, // 0x147929/Gladiator lfj_v1

	// HEFE (0x18e3...): pharaoh pools are not registered LPs — no fee applied.
	"0xc4fa66b4839af7379a4fcbe5dd048b18fe99a2ac": true, // HEFE/USDC pharaoh_v1
	"0x73f7212838692e560f5b26fdb05cf0aa6cf56f33": true, // HEFE/WAVAX pharaoh_v1
	"0x2064f67ba4362422eaae6ba7689c0cb0fa82c961": true, // HEFE/WAVAX pharaoh_v1
	"0x7a02148e4af381735faca391cd2d781b8f1ed272": true, // HEFE/WAVAX pharaoh_v3
	"0x9b214d9c2872b5cd33f548aadb9c5396fa7e8546": true, // HEFE/USDC pharaoh_v3

	// HEFE (0x18e3...): lfj_v1 HEFE/CANS pool not registered in isLiquidityPool — no fee applied.
	// Only one LP (0xe11e871d) is registered; isLiquidityPool(this pool) = false confirmed on-chain.
	"0xb9509de4034e1c7d23c07f5da785472eb4ef53e4": true, // HEFE/CANS lfj_v1

	// HEFE (0x18e3...): lfj_v1 HEFE/0x7a84 pool not registered in isLiquidityPool — no fee.
	"0x357233526bb85746829e67b076490462e49bdaa6": true, // HEFE/0x7a84 lfj_v1
	"0xe795273c9938b98554dc31831b4b4360ad869259": true, // HEFE/Always lfj_v1
	"0x1e9477cfb06b58cba28a99b5a1a035c9f4045529": true, // HEFE/0x234b lfj_v1
	"0x3fb14bc69c84f3030cb8e7fec7e5b45b1bfd1afc": true, // HEFE/0xc139 lfj_v1

	// Quasi (0xc970...): fee only applies when from/to == liquidityPool (slot 6 = 0x117ef430, lfj_v1).
	// Pharaoh_v1 pool is NOT the registered liquidityPool — no fee.
	"0x13e4a7f12c72f0a791b4e6831b2e3a6aa2680b9a": true, // Quasi/WAVAX pharaoh_v1

	// GoodToken (GOOD, 0x169e8f): fee only applies for the ONE registered lp address
	// (set via setLiquidity). Only pool 0x21013fe86a is the registered lp.
	// All other GOOD pools are exempt.
	"0x24208ef8e891db2b327a20eaefccf22206783e9a": true, // GOOD/WAVAX lfj_v1
	"0x4d30d49735dc3cf20c39eb97ddcfa2b3258134ea": true, // GOOD/0x234b lfj_v1
	"0x874d7fe773b3a73d6b26032ec543cf79ece89701": true, // GOOD/WAVAX lfj_v2

	// SnowyYields (0xcd0d): _transfer gates taxAmount by `from|to == uniswapV2Pair`.
	// Slot 11 confirms uniswapV2Pair = 0x57ba9107 (registered via openTrading() with
	// factory.createPair(this, WAVAX)). Only that pool triggers the 4% tax.
	// Other SnowyYields pools pair with non-WAVAX tokens and don't match the
	// registered pair — they transfer fee-free.
	"0x72de7ea2934e35949734302e29bb9d4281fad9f2": true, // 0x8729438e/SnowyYields lfj_v1
	"0xb56beab415b4ef185da7a3937782087480e8db29": true, // 0x420fca01/SnowyYields lfj_v1
	"0x0df8ec2e8b2b8076dee5038a950e193bb9f540c9": true, // 0x65378b69/SnowyYields lfj_v1
}

// FotPoolTokenOverrides maps (pool, token) to a custom FoT calculator.
// Used when a token charges different fees for different pools (e.g., registered
// AMM pairs get buyTotalFees/sellTotalFees, while unregistered pools get a lower
// "transfer" fee rate).
type fotPoolTokenKey struct {
	Pool  string // lowercase hex pool address
	Token string // lowercase hex token address
}

var FotPoolTokenOverrides = map[fotPoolTokenKey]fotCalc{
	// LINDA (0x039d...): 100 bps on registered lfj_v1 pair, 50 bps on unregistered pools.
	// Pharaoh V1 pool 0xe4f2... is not in the token's AMM pair mapping (slot 11),
	// so the token charges the transfer fee (buyMarketingFee=50 bps, slot 21) instead
	// of buyTotalFees (100 bps).
	{Pool: "0xe4f24831b8e525b7330dffbdb725c16af62847e2", Token: "0x039d2e8f097331278bd6c1415d839310e0d5ece4"}: fotCustom(func(amount *big.Int) *big.Int {
		fee := new(big.Int).Mul(amount, big.NewInt(50))
		fee.Div(fee, big.NewInt(10000))
		return fee
	}),
}

// FotRebasingTokens lists tokens that gain value over time (negative "tax"),
// e.g. interest-bearing or rebasing upward tokens. Formula output < eth_call output.
// These cannot use a simple tax adjustment; they need to fall back to eth_call.
var FotRebasingTokens = map[string]bool{
	// 0xc891eb4cbdeff6e073e859e987815ed1505c2acd — TUSD (rebasing, -49.25 bps)
	"0xc891eb4cbdeff6e073e859e987815ed1505c2acd": true,

	// 0xffff003a6bad9b743d658048742935fffe2b6ed7 — rebasing token (-45.23 bps)
	"0xffff003a6bad9b743d658048742935fffe2b6ed7": true,
}

// FotFormulaIssueTokens lists tokens where the formula produces different output
// than eth_call but not due to transfer tax (e.g. TWAMM pools, LFJ V2 precision).
// These should remain as fallback pools.
var FotFormulaIssueTokens = map[string]bool{
	// 0xd24c2ad096400b6fbcd2ad8b24e7acbc21a1da64 — fraxswap TWAMM output token
	// (V2 formula gives 0, fraxswap uses different AMM math)
	"0xd24c2ad096400b6fbcd2ad8b24e7acbc21a1da64": true,

	// 0x130966628846bfd36ff31a822705796e8cb8c18d — AnyswapV5ERC20 (MIM bridge token).
	// Standard ERC20, NO transfer fee. Pharaoh V1 pool shows 0 bps mismatch
	// (18 wei diff only, not FoT — possible rounding in stable swap math).
	// Source: AnyswapV5ERC20.sol — transfer() does plain balanceOf[from]-=value, balanceOf[to]+=value.
	"0x130966628846bfd36ff31a822705796e8cb8c18d": true,

	// GB, DICK, SPORE: moved to reflectionTokenConfigs for exact RFI math.

	// LFJ V2 pools with 0% at size 0 but negative diff at size 2:
	// These are formula precision issues in LFJ V2, not transfer taxes.
	"0x73a2b117b397346fa8e45577f478a7621b6045df": true, // 0x17094895 pool
	"0x6c14c1898c843ff66ca51e87244690bbc28df215": true, // 0x2cf90b72 pool (ORNG — standard ERC20, no fee)
	"0x420fca0121dc28039145009570975747295f2329": true, // 0x3a2cbbd1 pool (standard ERC20, no fee)
	"0x407e0ce3ef9d370e00a972cba7344158ed60a6cd": true, // 0x78f81cf4 pool

	// NOTE: Psychic, AMI, Trinity, BridgeToken, SynapseERC20 etc. were previously
	// listed here but are standard ERC20 tokens with NO transfer fee. They were
	// incorrectly added due to a discover bug (pm.Get returning nil for unregistered
	// pools). The bug is fixed — these tokens work correctly with formulas.

	// 0xbc4f2cb9b351ee602be4e2bc966832a59784c412 — unverified source (11 pools).
	// Contract source not published on-chain.
	"0xbc4f2cb9b351ee602be4e2bc966832a59784c412": true,

}

// reflectionTokenConfig holds the parameters for an RFI/SafeMoon reflection token.
// These tokens have a reflection fee that reduces _rTotal on each transfer, causing
// the recipient to receive slightly more tokens than tTransferAmount.
// For tokens with burn (e.g. KIOO/Reflectx), set burnRate/burnDenom and tTotalSlot.
type reflectionTokenConfig struct {
	rTotalSlot   common.Hash            // storage slot for _rTotal (_reflectSupply)
	tTotalSlot   common.Hash            // storage slot for _tTotal (zero = use constant tTotal)
	tTotal       *big.Int               // constant total supply (used when tTotalSlot is zero)
	reflectRate  int64                  // reflection fee numerator (e.g. 1 for 1%)
	reflectDenom int64                  // reflection fee denominator (e.g. 100)
	burnRate     int64                  // burn fee numerator (0 = no burn)
	burnDenom    int64                  // burn fee denominator (0 = no burn)
	calcFee      func(*big.Int) *big.Int // total fee (reflection + team/burn), same math as fotCalculators

	// Excluded account support: set all three to enable _getCurrentSupply() exclusion math.
	excludedArraySlot common.Hash // storage slot for _excluded dynamic array (0 = disabled)
	rOwnedSlot        common.Hash // storage slot for _rOwned mapping
	tOwnedSlot        common.Hash // storage slot for _tOwned mapping
}

// reflectionTokenConfigs maps token addresses to their reflection config.
// These override any fotCalculators entry for the same address.
var reflectionTokenConfigs = map[string]reflectionTokenConfig{
	// Green Token (GREEN): 1% reflection + 3% team = 4% total fee.
	// _taxFee=1 is the reflection fee that reduces _rTotal via _reflectFee(rFee).
	// TeamFee=3 is taken via _takeTeam (adds to contract's rOwned, does NOT reduce _rTotal).
	// _rTotal at storage slot 6 (after Ownable's 2 slots + 4 mappings).
	// _tTotal = 1_000_000_000_000 * 1e9 = 1e21 (constant, not in storage).
	// Pool: 0x40029f0cd32423b04f101d44458858395ef6385e (lfj_v1, GREEN/WAVAX).
	// Verified: reflection formula matches EVM to 0 ppb.
	"0x4d6fc3925fcadca6ad952afbd649ec44e756b000": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		tTotal:       new(big.Int).Mul(big.NewInt(1000000000000), big.NewInt(1e9)),
		reflectRate:  1,
		reflectDenom: 100,
		calcFee: func(amount *big.Int) *big.Int {
			tFee := new(big.Int).Mul(amount, big.NewInt(1))
			tFee.Div(tFee, big.NewInt(100))
			tTeam := new(big.Int).Mul(amount, big.NewInt(3))
			tTeam.Div(tTeam, big.NewInt(100))
			return tFee.Add(tFee, tTeam)
		},
	},

	// AvaFOX (AFM): 1% reflection + 3% team = 4% total fee.
	// _taxFee=1 is the reflection fee that reduces _rTotal via _reflectFee(rFee).
	// TeamFee=3 is HARDCODED in _getValues (not from _teamFee state var which is 1).
	// _rTotal at storage slot 6 (same layout as GREEN: Ownable 2 slots + 4 mappings).
	// _tTotal = 1_000_000_000_000 * 1e9 = 1e21 (constant, not in storage).
	// Pool: 0x4ea4440e35ed4194c777f0cf26a33298c77bb3c5 (lfj_v1, AFM/WAVAX), dir=1.
	"0x03ae7c5c942547772e1e0f01c04699ebf1cc9761": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		tTotal:       new(big.Int).Mul(big.NewInt(1000000000000), big.NewInt(1e9)),
		reflectRate:  1,
		reflectDenom: 100,
		calcFee: func(amount *big.Int) *big.Int {
			tFee := new(big.Int).Mul(amount, big.NewInt(1))
			tFee.Div(tFee, big.NewInt(100))
			tTeam := new(big.Int).Mul(amount, big.NewInt(3))
			tTeam.Div(tTeam, big.NewInt(100))
			return tFee.Add(tFee, tTeam)
		},
	},

	// SHIBX (SHIBAVAX): 10% pure reflection tax (all goes to _reflectFee, no team fee).
	// _getTValues: tFee = tAmount.mul(10).div(100) — unconditional, no DEX pair exemption.
	// _rTotal at storage slot 6. _tTotal = 10_000_000_000e18 (constant).
	// Pool is NOT _isExcluded; tradeLimit=0 (no cap).
	// Pools: 0x82ab53e405 (pangolin_v2, SHIBX/WAVAX), 0x3f7e7ca004 (partyswap, SHIBX/WAVAX).
	// Verified: reflection formula matches EVM to 0 ppb (2026-03-25).
	"0x440abbf18c54b2782a4917b80a1746d3a2c2cce1": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		tTotal:       new(big.Int).Mul(big.NewInt(10_000_000_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
		reflectRate:  10,
		reflectDenom: 100,
		calcFee:      fotPct(10).calcFee,
	},

	// KIOO (Reflectx): 3% reflection fee + 1% burn = 4% total.
	// _reflectFeeBurn reduces _reflectSupply by (reflectFees + reflectBurn) and _totalSupply by burn.
	// Unlike GREEN/AFM, burn changes BOTH _reflectSupply and _totalSupply, so both must be
	// read from storage. FEES_PERCENT=3 (reflection), BURN_PERCENT=1 (burn).
	// Storage layout (Ownable + Reflectx): slot 0=_owner, slot 1=_reflectSupply, slot 2=_totalSupply.
	// MAX_SUPPLY = 72_000_000_000e18 (initial; _totalSupply decreases with each burn).
	// Pools: 0xf3f119ceb9 (lfj_v1, KIOO/WAVAX), 0x6ccf639b55 (lfj_v1, KIOO/USDT.e), etc.
	"0x45cdaf3fd17bd31d9830fa977159162dd2431683": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		tTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002"),
		reflectRate:  3,
		reflectDenom: 100,
		burnRate:     1,
		burnDenom:    100,
		calcFee: func(amount *big.Int) *big.Int {
			fees := new(big.Int).Mul(amount, big.NewInt(3))
			fees.Div(fees, big.NewInt(100))
			burn := new(big.Int).Mul(amount, big.NewInt(1))
			burn.Div(burn, big.NewInt(100))
			return fees.Add(fees, burn)
		},
	},

	// Good Bridging (GB): 1% pure reflection tax (classic reflect.finance fork).
	// _getTValues: tFee = tAmount.div(100) — unconditional, no team fee, no burn.
	// _reflectFee only reduces _rTotal by rFee. _tTotal is constant (no burn).
	// Storage layout (Ownable inherits Context — no slots; Ownable slot 0 = _owner):
	//   slot 0=_owner, slot 1=_rOwned(map), slot 2=_tOwned(map), slot 3=_allowances(map),
	//   slot 4=_isExcluded(map), slot 5=_excluded(array), slot 6=_rTotal, slot 7=_tFeeTotal.
	// _tTotal = 14327880 * 1e9 = 14327880000000000 (constant).
	// Pools: 0x77eb05e7 (partyswap, GB/WAVAX), 0xd1ef5be3 (lfj_v1, GB/USDT.e),
	//        0x0a1041fe (pangolin_v2, GB/WAVAX).
	"0x90842eb834cfd2a1db0b1512b254a18e4d396215": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		tTotal:       new(big.Int).Mul(big.NewInt(14327880), big.NewInt(1e9)),
		reflectRate:  1,
		reflectDenom: 100,
		calcFee: func(amount *big.Int) *big.Int {
			return new(big.Int).Div(amount, big.NewInt(100))
		},
	},

	// DICK (CoinToken, 0xaaec): TAX=1% reflect + BURN=1% + CHARITY=2% = 4% total.
	// CoinToken uses double-division: fee = (amount * FEE * GRANULARITY) / GRANULARITY / 100.
	// _TAX_FEE=100, _BURN_FEE=100, _CHARITY_FEE=200, _GRANULARITY=100.
	// tFee = amount*100/100/100 = 1%, tBurn = amount*100/100/100 = 1%, tCharity = amount*200/100/100 = 2%.
	// _reflectFee: _rTotal = _rTotal.sub(rFee).sub(rBurn) — both tax and burn reduce _rTotal.
	//              _tTotal = _tTotal.sub(tBurn) — only burn reduces _tTotal.
	// Charity is sent to FeeAddress via _sendToCharity (rOwned, does NOT reduce _rTotal).
	// Storage layout (Ownable has public _owner at slot 0):
	//   slot 0=_owner, slot 1=_rOwned(map), slot 2=_tOwned(map), slot 3=_allowances(map),
	//   slot 4=_isExcluded(map), slot 5=_excluded(array), slot 6=_NAME, slot 7=_SYMBOL,
	//   slot 8=_DECIMALS, slot 9=FeeAddress, slot 10=_MAX, slot 11=_DECIMALFACTOR,
	//   slot 12=_GRANULARITY, slot 13=_tTotal, slot 14=_rTotal.
	// Pool: 0x655082c9 (lfj_v1, MIM/DICK).
	"0xaaec4017381a1d1e564cb88600c001d05b21571d": {
		rTotalSlot:   common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000000e"),
		tTotalSlot:   common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000000d"),
		reflectRate:  1,
		reflectDenom: 100,
		burnRate:     1,
		burnDenom:    100,
		calcFee: func(amount *big.Int) *big.Int {
			// tFee = amount * 100 / 100 / 100 (1% reflect)
			tFee := new(big.Int).Mul(amount, big.NewInt(100))
			tFee.Div(tFee, big.NewInt(100))
			tFee.Div(tFee, big.NewInt(100))
			// tBurn = amount * 100 / 100 / 100 (1% burn)
			tBurn := new(big.Int).Mul(amount, big.NewInt(100))
			tBurn.Div(tBurn, big.NewInt(100))
			tBurn.Div(tBurn, big.NewInt(100))
			// tCharity = amount * 200 / 100 / 100 (2% charity)
			tCharity := new(big.Int).Mul(amount, big.NewInt(200))
			tCharity.Div(tCharity, big.NewInt(100))
			tCharity.Div(tCharity, big.NewInt(100))
			return tFee.Add(tFee, tBurn).Add(tFee, tCharity)
		},
	},

	// SPORE (Spore.Finance): 6% pure reflection tax (no burn, no team).
	// _getTValues: tFee = tAmount.div(100).mul(6) — div-then-mul, unconditional.
	// _reflectFee only reduces _rTotal by rFee. _tTotal is constant (no burn).
	// Storage layout (Ownable slot 0 = _owner):
	//   slot 0=_owner, slot 1=_rOwned(map), slot 2=_tOwned(map), slot 3=_allowances(map),
	//   slot 4=_isExcluded(map), slot 5=_excluded(array), slot 6=_rTotal.
	// _tTotal = 100000000000 * 10^6 * 10^9 = 10^26 (constant).
	// Pool: 0x0a63179a (pangolin_v2, SPORE/WAVAX).
	"0x6e7f5c0b9f4432716bdd0a77a3601291b9d9e985": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		tTotal:       new(big.Int).Exp(big.NewInt(10), big.NewInt(26), nil),
		reflectRate:  6,
		reflectDenom: 100,
		calcFee: func(amount *big.Int) *big.Int {
			// tFee = tAmount.div(100).mul(6) — div then mul
			fee := new(big.Int).Div(amount, big.NewInt(100))
			fee.Mul(fee, big.NewInt(6))
			return fee
		},
	},

	// L-Swing: 20% pure reflection tax (all goes to _reflectFee, no team fee, no burn).
	// _getTValues: tFee = tAmount.mul(20).div(100) — unconditional.
	// _reflectFee only reduces _rTotal by rFee. _tTotal stored at slot 6.
	// Storage layout (standard RFI fork):
	//   slot 0=_owner, slot 1=_rOwned(map), slot 2=_tOwned(map), slot 3=_allowances(map),
	//   slot 4=_isExcluded(map), slot 5=_excluded(array), slot 6=_tTotal, slot 7=_rTotal.
	// Pools: 0xaacfe51a (lfj_v1, tsAVAX/L-Swing, pool#2786),
	//        0xe2330bfa (lfj_v1, L-Swing/USDC, pool#2993).
	// Was fotPct(20) — mismatch of ~110 units (~52 ppb) due to excluded accounts in _getRate().
	// Fix: read _excluded array and subtract rOwned/tOwned from supply (mirrors _getCurrentSupply).
	"0x556b959d952085405e7c630bc45a34ace73854eb": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000007"),
		tTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		reflectRate:  20,
		reflectDenom: 100,
		calcFee:      fotPct(20).calcFee,
		// Excluded accounts: _getRate() uses _getCurrentSupply() which subtracts
		// excluded accounts' rOwned/tOwned. Without this, the rate is slightly off
		// causing ~110 unit mismatch.
		excludedArraySlot: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000005"),
		rOwnedSlot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		tOwnedSlot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002"),
	},

	// SLED (sledfinance): 2% pure reflection tax (all goes to _reflectFee, no team fee, no burn).
	// _getTValues: tFee = tAmount.mul(2).div(100) — unconditional.
	// _reflectFee only reduces _rTotal by rFee. _tTotal is constant (no burn).
	// Storage layout (standard RFI fork with Ownable):
	//   slot 0=_owner, slot 1=_rOwned(map), slot 2=_tOwned(map), slot 3=_allowances(map),
	//   slot 4=_isExcluded(map), slot 5=_excluded(array), slot 6=_rTotal, slot 7=_tFeeTotal.
	// _tTotal = 10 * 10**6 * 10**9 = 10_000_000_000_000_000 (constant, 9 decimals).
	// Pools: 0x7e449382 (elkdex, SLED/ELK, pool#3076), 0x18c8e134 (pangolin_v2, SLED/WAVAX).
	// Was fotPct(2) — ~22 PPM residual from reflection redistribution drift.
	"0x1f1fe1ef06ab30a791d6357fdf0a7361b39b1537": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		tTotal:       new(big.Int).Mul(big.NewInt(10_000_000), big.NewInt(1e9)),
		reflectRate:  2,
		reflectDenom: 100,
		calcFee:      fotPct(2).calcFee,
		// Excluded accounts: _getRate() uses _getCurrentSupply() which subtracts
		// excluded accounts' rOwned/tOwned from supply.
		excludedArraySlot: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000005"),
		rOwnedSlot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		tOwnedSlot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002"),
	},

	// BabyCoq (0x22897cf0): 0.02% reflection + 1% liquidity + 0.01% charity = 1.03% total.
	// _taxFee=2, _liquidityFee=100, _charityFee=1 — all divided by 10000 (fee denom = 10^(_feeDecimal+2)).
	// _taxFee (2/10000) is the reflection fee that reduces _rTotal via _reflectFee(rFee).
	// Liquidity and charity fees go to contract/wallet (do NOT reduce _rTotal).
	// Storage layout (Ownable slot 0 = _owner, renounced to 0x0):
	//   slot 0=_owner, slot 1=_rOwned(map), slot 2=_tOwned(map), slot 3=_allowances(map),
	//   slot 4=_isExcluded(map), slot 5=_isExcludedFromFee(map), slot 6=_excluded(array, length=2),
	//   slot 7=_tTotal(420690e18), slot 8=_rTotal, slot 9=_tFeeTotal.
	// Excluded accounts: 0xdaf604... (zero balance), 0x2a9613... (charity, nonzero rOwned/tOwned).
	// Pool: 0x02c7d2d1 (lfj_v1, BabyCoq/WAVAX).
	// Was fotBps(103) — ~25 PPM residual from pool-side reflection bonus on existing balance.
	// Needs RecipientAwareInputAdjuster to compute balanceOf(pool) after - before exactly.
	"0x22897cf0da31e1f118649d9f6ad1809cabd84948": {
		rTotalSlot:   common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000008"),
		tTotal:       new(big.Int).Mul(big.NewInt(420690), new(big.Int).Exp(big.NewInt(10), big.NewInt(15), nil)),
		reflectRate:  2,
		reflectDenom: 10000,
		calcFee:      fotBps(103).calcFee,
		excludedArraySlot: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006"),
		rOwnedSlot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		tOwnedSlot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002"),
	},
}
