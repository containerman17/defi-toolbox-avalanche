package formulas

// pool_balancer_v3.go — PoolQuoter implementation for Balancer V3 pools.
// Reads state from the Vault singleton contract's storage slots.

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// Balancer V3 Vault address on Avalanche C-Chain.
var balV3VaultAddr = common.HexToAddress("0xba1333333333a1ba1108e8412f11850a5c319ba9")

// Vault storage slot indices.
// C3 linearization: VaultStorage comes before ERC20MultiToken.
// VaultStorage state variables start at slot 0:
//   0: _poolConfigBits     mapping(address pool => PoolConfigBits)
//   1: _poolRoleAccounts   mapping(address pool => PoolRoleAccounts)
//   2: _hooksContracts     mapping(address pool => IHooks)
//   3: _poolTokens         mapping(address pool => IERC20[])
//   4: _poolTokenInfo      mapping(address pool => mapping(IERC20 => TokenInfo))
//   5: _poolTokenBalances  mapping(address pool => mapping(uint256 index => bytes32))
//   6: _aggregateFeeAmounts
//
// These slot numbers may need verification against the deployed contract.
// If results don't match, adjust the constants.
var (
	balV3SlotPoolConfigBits    = 0
	balV3SlotPoolTokenBalances = 5
)

// Fee extraction from PoolConfigBits:
// STATIC_SWAP_FEE_OFFSET = 18 (after 18 bool flags)
// FEE_BITLENGTH = 24
// FEE_SCALING_FACTOR = 1e11
// tokenDecimalDiffs: offset = 18 + 24 + 24 + 24 = 90, length = 40 bits
const (
	balV3StaticSwapFeeOffset = 18
	balV3FeeBitlength        = 24
	balV3FeeScalingFactor    = 100_000_000_000 // 1e11
	balV3DecimalDiffsOffset  = 90              // 18 + 24 + 24 + 24
	balV3DecimalDiffsBitlen  = 40
	balV3DecimalDiffPerToken = 5
)

// BalancerV3PoolType distinguishes the pool math to use.
type BalancerV3PoolType uint8

const (
	BalV3Weighted BalancerV3PoolType = iota
	BalV3Stable
)

// BalV3TokenType mirrors the Balancer V3 TokenType enum.
// STANDARD = 0, WITH_RATE = 1, ERC4626 = 2.
type BalV3TokenType uint8

const (
	balV3TokenStandard BalV3TokenType = 0
	balV3TokenWithRate BalV3TokenType = 1
	balV3TokenERC4626  BalV3TokenType = 2
)

// BalancerV3PoolInfo holds per-pool configuration registered at startup.
type BalancerV3PoolInfo struct {
	PoolType  BalancerV3PoolType
	NumTokens int
	// Token addresses in registration order (needed to map tokenIn/Out to indices)
	Tokens []common.Address

	// Weighted pool: normalized weights (18-decimal FP), one per token
	Weights []*big.Int

	// Stable pool: amplification parameter (already * AMP_PRECISION = 1000)
	Amp *big.Int

	// Per-token type and rate provider (read from vault _poolTokenInfo storage).
	// TokenTypes[i] == balV3TokenWithRate or balV3TokenERC4626 → rate provider applies.
	// Nil/empty slices mean all tokens are STANDARD (rate=1e18).
	TokenTypes    []BalV3TokenType
	RateProviders []common.Address
}

// balV3PoolInfos maps pool address (lowercase hex) to its info.
var balV3PoolInfos = map[string]*BalancerV3PoolInfo{}

// RegisterBalancerV3Pool registers a Balancer V3 pool for formula quoting.
func RegisterBalancerV3Pool(poolAddress string, info *BalancerV3PoolInfo) {
	balV3PoolInfos[poolAddress] = info
}

// BalancerV3Pool is the PoolQuoter for Balancer V3 pools.
type BalancerV3Pool struct {
	addr     common.Address
	info     *BalancerV3PoolInfo

	// Pre-loaded from vault storage at construction:
	swapFeePercentage *big.Int    // 18-decimal FP
	decimalScalingFactors []*big.Int // per token, e.g. 1e12 for 6-decimal tokens

	// Raw balances (in native token decimals), read from _poolTokenBalances
	balancesRaw []*big.Int
	// Live scaled18 balances = rawBalance * decimalScalingFactor * tokenRate / 1e18
	balancesLiveScaled18 []*big.Int

	// Token rates (18-decimal FP). 1e18 for STANDARD tokens, getRate() for WITH_RATE/ERC4626.
	// Used for amountIn scaling and amountOut unscaling in Quote().
	tokenRates []*big.Int
}

// balV3GetRateSelector is the 4-byte selector for getRate() → bytes4(keccak256("getRate()"))
var balV3GetRateSelector = [4]byte{0x67, 0x9a, 0xef, 0xce}

func newBalancerV3Pool(addr common.Address, reader StorageReader, caller EVMCaller) *BalancerV3Pool {
	poolHex := poolHex(addr)
	info, ok := balV3PoolInfos[poolHex]
	if !ok {
		return nil
	}

	vault := balV3VaultAddr

	// 1. Read poolConfigBits from _poolConfigBits[pool]
	configSlot := balV3MappingSlot(addr, balV3SlotPoolConfigBits)
	configBits := reader(vault, configSlot)
	configVal := new(big.Int).SetBytes(configBits[:])

	// Extract static swap fee percentage
	swapFeeRaw := extractBits(configVal, balV3StaticSwapFeeOffset, balV3FeeBitlength)
	swapFeePercentage := new(big.Int).Mul(swapFeeRaw, big.NewInt(balV3FeeScalingFactor))

	// Extract token decimal diffs
	decimalDiffsRaw := extractBits(configVal, balV3DecimalDiffsOffset, balV3DecimalDiffsBitlen)
	decimalScalingFactors := make([]*big.Int, info.NumTokens)
	for i := 0; i < info.NumTokens; i++ {
		diff := extractBits(decimalDiffsRaw, uint(i)*balV3DecimalDiffPerToken, balV3DecimalDiffPerToken)
		// scalingFactor = 10^diff
		decimalScalingFactors[i] = new(big.Int).Exp(big.NewInt(10), diff, nil)
	}

	// 2. Fetch token rates. STANDARD tokens have rate = 1e18.
	//    WITH_RATE and ERC4626 tokens have a rate provider whose getRate() must be called.
	//    If no caller is available, we fall back to rate=1e18 (less accurate for yield tokens).
	tokenRates := make([]*big.Int, info.NumTokens)
	for i := 0; i < info.NumTokens; i++ {
		tokenRates[i] = new(big.Int).Set(fpONE) // default: 1e18

		if len(info.TokenTypes) > i && len(info.RateProviders) > i {
			tt := info.TokenTypes[i]
			rp := info.RateProviders[i]
			needsRate := (tt == balV3TokenWithRate || tt == balV3TokenERC4626) &&
				rp != (common.Address{}) && caller != nil
			if needsRate {
				result, ok := caller(rp, balV3GetRateSelector[:])
				if ok && len(result) >= 32 {
					rate := new(big.Int).SetBytes(result[:32])
					if rate.Sign() > 0 {
						tokenRates[i] = rate
					}
				}
			}
		}
	}

	// 3. Read raw balances from _poolTokenBalances[pool][tokenIndex]
	//    Each entry is PackedTokenBalance: lower 128 bits = rawBalance, upper 128 bits = derivedBalance
	balancesRaw := make([]*big.Int, info.NumTokens)
	balancesLiveScaled18 := make([]*big.Int, info.NumTokens)
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

	// First compute the base slot for this pool's token balances:
	// keccak256(leftPad32(pool) + leftPad32(5))
	poolBalBaseSlot := balV3MappingSlot(addr, balV3SlotPoolTokenBalances)

	for i := 0; i < info.NumTokens; i++ {
		// Nested mapping: keccak256(leftPad32(tokenIndex) + poolBalBaseSlot)
		tokenIdxHash := crypto.Keccak256Hash(
			common.BigToHash(big.NewInt(int64(i))).Bytes(),
			poolBalBaseSlot.Bytes(),
		)
		packedBalance := reader(vault, tokenIdxHash)
		packedVal := new(big.Int).SetBytes(packedBalance[:])

		rawBal := new(big.Int).And(packedVal, mask128)
		balancesRaw[i] = rawBal

		if rawBal.Sign() == 0 {
			return nil // pool has zero balance, skip
		}

		// Live balance = raw * scalingFactor * tokenRate / 1e18
		liveBalance := new(big.Int).Mul(rawBal, decimalScalingFactors[i])
		if tokenRates[i].Cmp(fpONE) != 0 {
			liveBalance = new(big.Int).Div(new(big.Int).Mul(liveBalance, tokenRates[i]), fpONE)
		}
		balancesLiveScaled18[i] = liveBalance
	}

	return &BalancerV3Pool{
		addr:                  addr,
		info:                  info,
		swapFeePercentage:     swapFeePercentage,
		decimalScalingFactors: decimalScalingFactors,
		balancesRaw:           balancesRaw,
		balancesLiveScaled18:  balancesLiveScaled18,
		tokenRates:            tokenRates,
	}
}

func (p *BalancerV3Pool) Address() common.Address {
	return p.addr
}

func (p *BalancerV3Pool) Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	if amountIn.IsZero() {
		return uint256.Int{}
	}

	// Determine token indices from zeroForOne.
	// zeroForOne means tokenIn < tokenOut (by address ordering).
	// But Balancer V3 uses registration order, not address order.
	// We need to find which registered token is tokenIn and which is tokenOut.
	// Convention: for a 2-token pool, we store tokens sorted by address in pools.txt,
	// so token[0] < token[1] in address order.
	// If tokens are in the SAME order as registration, zeroForOne maps directly.
	// We handle this by using the tokens from info.Tokens (registration order)
	// and the fact that pools.txt stores them in address-sorted order.

	if len(p.info.Tokens) < 2 {
		return uint256.Int{}
	}

	// For 2-token pools: token0 < token1 in address ordering = registration order for Balancer
	// The pool collector stores tokens sorted, and registration order should match.
	var indexIn, indexOut int
	if zeroForOne {
		indexIn = 0
		indexOut = 1
	} else {
		indexIn = 1
		indexOut = 0
	}

	// For multi-token pools (>2 tokens), we only handle the 2-token swap case
	// where we know the direction. We'd need actual token addresses for >2 tokens.
	if len(p.info.Tokens) > 2 {
		// Multi-token pools: need to know actual tokenIn/Out addresses.
		// The PoolQuoter interface only gives us zeroForOne, which is insufficient
		// for >2 tokens. Skip these for now.
		return uint256.Int{}
	}

	// Scale amountIn to 18 decimals: toScaled18ApplyRateRoundDown
	// = amountIn * scalingFactor * tokenRate / 1e18
	amountInBig := amountIn.ToBig()
	amountInScaled18 := new(big.Int).Mul(amountInBig, p.decimalScalingFactors[indexIn])
	rateIn := p.tokenRates[indexIn]
	if rateIn.Cmp(fpONE) != 0 {
		amountInScaled18 = new(big.Int).Div(new(big.Int).Mul(amountInScaled18, rateIn), fpONE)
	}

	// Deduct swap fee: feeAmount = amountInScaled18 * swapFee / 1e18 (round up)
	feeAmount := fpMulUp(amountInScaled18, p.swapFeePercentage)
	amountInAfterFee := new(big.Int).Sub(amountInScaled18, feeAmount)

	if amountInAfterFee.Sign() <= 0 {
		return uint256.Int{}
	}

	// Compute amountOutScaled18 using pool-type-specific math
	var amountOutScaled18 *big.Int

	switch p.info.PoolType {
	case BalV3Weighted:
		amountOutScaled18 = WeightedComputeOutGivenExactIn(
			p.balancesLiveScaled18[indexIn],
			p.info.Weights[indexIn],
			p.balancesLiveScaled18[indexOut],
			p.info.Weights[indexOut],
			amountInAfterFee,
		)

	case BalV3Stable:
		invariant := StableComputeInvariant(p.info.Amp, p.balancesLiveScaled18)
		if invariant.Sign() == 0 {
			return uint256.Int{}
		}
		amountOutScaled18 = StableComputeOutGivenExactIn(
			p.info.Amp,
			p.balancesLiveScaled18,
			indexIn, indexOut,
			amountInAfterFee,
			invariant,
		)

	default:
		return uint256.Int{}
	}

	if amountOutScaled18 == nil || amountOutScaled18.Sign() <= 0 {
		return uint256.Int{}
	}

	// Scale back to raw: toRawUndoRateRoundDown
	// = FixedPoint.divDown(amountOutScaled18, scalingFactor * tokenRate)
	// = amountOutScaled18 * 1e18 / (scalingFactor * tokenRate)
	// For STANDARD tokens (rate = 1e18): = amountOutScaled18 / scalingFactor
	scalingOut := p.decimalScalingFactors[indexOut]
	rateOut := p.tokenRates[indexOut]
	var amountOutRaw *big.Int
	if rateOut.Cmp(fpONE) != 0 {
		// divisor = scalingFactor * rate (both are in 1e18 terms together)
		// divDown(a, divisor) = a * 1e18 / divisor
		divisor := new(big.Int).Mul(scalingOut, rateOut)
		amountOutRaw = new(big.Int).Div(new(big.Int).Mul(amountOutScaled18, fpONE), divisor)
	} else {
		amountOutRaw = new(big.Int).Div(amountOutScaled18, scalingOut)
	}

	if amountOutRaw.Sign() <= 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(amountOutRaw)
	if overflow {
		return uint256.Int{}
	}

	return *result
}

// ── Storage slot helpers ──

// balV3MappingSlot computes keccak256(leftPad32(key) ++ leftPad32(slot)) for a simple mapping.
func balV3MappingSlot(key common.Address, slot int) common.Hash {
	var data [64]byte
	copy(data[12:32], key.Bytes()) // left-pad address to 32 bytes
	big.NewInt(int64(slot)).FillBytes(data[32:64])
	return crypto.Keccak256Hash(data[:])
}

// extractBits extracts `length` bits starting at bit `offset` from a big.Int.
// Bit 0 is the least significant bit.
func extractBits(val *big.Int, offset uint, length uint) *big.Int {
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), length), big.NewInt(1))
	return new(big.Int).And(new(big.Int).Rsh(val, offset), mask)
}
