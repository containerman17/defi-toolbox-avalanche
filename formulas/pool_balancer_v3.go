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

// Vault storage slot indices (ERC20MultiToken uses slots 0-2, then VaultStorage starts).
// _poolConfigBits: mapping(address pool => PoolConfigBits) at slot 3
// _poolTokenBalances: mapping(address pool => mapping(uint256 tokenIndex => bytes32)) at slot 8
const (
	balV3SlotPoolConfigBits     = 3
	balV3SlotPoolTokenBalances  = 8
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
	// Live scaled18 balances = rawBalance * decimalScalingFactor (no rate provider for simplicity)
	balancesLiveScaled18 []*big.Int
}

func newBalancerV3Pool(addr common.Address, reader StorageReader) *BalancerV3Pool {
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

	// 2. Read raw balances from _poolTokenBalances[pool][tokenIndex]
	//    Each entry is PackedTokenBalance: lower 128 bits = rawBalance, upper 128 bits = derivedBalance
	balancesRaw := make([]*big.Int, info.NumTokens)
	balancesLiveScaled18 := make([]*big.Int, info.NumTokens)
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

	// First compute the base slot for this pool's token balances:
	// keccak256(leftPad32(pool) + leftPad32(8))
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
		// For STANDARD tokens (no rate provider), tokenRate = 1e18, so:
		// liveBalance = raw * scalingFactor
		balancesLiveScaled18[i] = new(big.Int).Mul(rawBal, decimalScalingFactors[i])
	}

	return &BalancerV3Pool{
		addr:                  addr,
		info:                  info,
		swapFeePercentage:     swapFeePercentage,
		decimalScalingFactors: decimalScalingFactors,
		balancesRaw:           balancesRaw,
		balancesLiveScaled18:  balancesLiveScaled18,
	}
}

func (p *BalancerV3Pool) Address() common.Address {
	return p.addr
}

func (p *BalancerV3Pool) Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool) {
	if amountIn.IsZero() {
		return nil, false
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
		return nil, false
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
		return nil, false
	}

	// Scale amountIn to 18 decimals
	amountInBig := amountIn.ToBig()
	amountInScaled18 := new(big.Int).Mul(amountInBig, p.decimalScalingFactors[indexIn])
	// For STANDARD tokens, tokenRate = 1e18, so:
	// toScaled18ApplyRateRoundDown = amount * scalingFactor * rate / 1e18
	// = amount * scalingFactor (since rate = 1e18)
	// Already done above for standard tokens.

	// Deduct swap fee: feeAmount = amountInScaled18 * swapFee / 1e18 (round up)
	feeAmount := fpMulUp(amountInScaled18, p.swapFeePercentage)
	amountInAfterFee := new(big.Int).Sub(amountInScaled18, feeAmount)

	if amountInAfterFee.Sign() <= 0 {
		return nil, false
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
			return nil, false
		}
		amountOutScaled18 = StableComputeOutGivenExactIn(
			p.info.Amp,
			p.balancesLiveScaled18,
			indexIn, indexOut,
			amountInAfterFee,
			invariant,
		)

	default:
		return nil, false
	}

	if amountOutScaled18 == nil || amountOutScaled18.Sign() <= 0 {
		return nil, false
	}

	// Scale back to raw: amountOutRaw = amountOutScaled18 * 1e18 / (scalingFactor * tokenRate)
	// For STANDARD tokens (rate = 1e18): amountOutRaw = amountOutScaled18 / scalingFactor
	// Using divDown (round down since this is amountOut)
	scalingOut := p.decimalScalingFactors[indexOut]
	// toRawUndoRateRoundDown = divDown(amount, scalingFactor * tokenRate)
	// For rate = 1e18: divDown(amount, scalingFactor * 1e18)
	// But actually scalingFactor is NOT an FP18 value, it's a raw multiplier (e.g., 1e12 for 6-decimal tokens).
	// In ScalingHelpers: toRawUndoRateRoundDown = FixedPoint.divDown(amount, scalingFactor * tokenRate)
	// where scalingFactor * tokenRate is treated as a single FP18 divisor.
	// So for STANDARD tokens: scalingFactor * 1e18 is the combined divisor.
	// FixedPoint.divDown(amount, divisor) = amount * 1e18 / divisor = amount * 1e18 / (scalingFactor * 1e18) = amount / scalingFactor
	amountOutRaw := new(big.Int).Div(amountOutScaled18, scalingOut)

	if amountOutRaw.Sign() <= 0 {
		return nil, false
	}

	result, overflow := uint256.FromBig(amountOutRaw)
	if overflow {
		return nil, false
	}

	return result, true
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
