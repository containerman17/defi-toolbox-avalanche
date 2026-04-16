package formulas

// pool_balancer_v2.go — PoolQuoter implementation for Balancer V2 Weighted pools.
//
// Balancer V2 weighted pool math (same as V3 weighted):
//   amountOut = balOut * (1 - (balIn / (balIn + amountIn))^(wIn/wOut)) * (1 - swapFee)
//
// Pool config (weights, swap fee) is fetched via EVM calls at benchmark startup and
// registered with RegisterBalancerV2Pool.  Balances are read from Vault storage on
// each Quote() call so they stay live without re-registration.
//
// Supported pool types:
//   - WeightedPool (specialization MINIMAL_SWAP_INFO = 1, balances per-token)
//   - WeightedPool2Tokens (specialization TWO_TOKEN = 2, balances packed in sharedCash)
// Multi-token GENERAL pools are also MinimalSwapInfo so they work too.

import (
	"math/big"
	"sync"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// BalancerV2PoolInfo holds per-pool configuration registered at startup via EVM calls.
type BalancerV2PoolInfo struct {
	// PoolId is the 32-byte Balancer pool ID (encodes address + specialization + nonce).
	PoolId [32]byte

	// Specialization: balV2SpecMinimalSwapInfo (1) or balV2SpecTwoToken (2).
	Specialization int

	// NumTokens is the number of registered tokens.
	NumTokens int

	// Tokens in registration order (sorted ascending by address for most pools).
	Tokens []common.Address

	// Weights: normalized 18-decimal FP weights, one per token.
	Weights []*big.Int

	// SwapFeePercentage: 18-decimal FP swap fee (e.g. 3e15 = 0.3%).
	SwapFeePercentage *big.Int

	// ScalingFactors: per-token multiplier to scale to 18 decimals.
	// e.g. 1e12 for a 6-decimal token, 1e0=1 for an 18-decimal token.
	ScalingFactors []*big.Int
}

// balV2PoolInfos maps lowercase pool address to its info.
var balV2PoolInfos sync.Map // map[string]*BalancerV2PoolInfo

// RegisterBalancerV2Pool registers a Balancer V2 pool for formula quoting.
func RegisterBalancerV2Pool(poolAddress string, info *BalancerV2PoolInfo) {
	balV2PoolInfos.Store(poolAddress, info)
}

// BalancerV2Pool is the PoolQuoter for Balancer V2 weighted pools.
type BalancerV2Pool struct {
	addr   common.Address
	info   *BalancerV2PoolInfo
	reader StorageReader
}

func newBalancerV2Pool(addr common.Address, reader StorageReader) *BalancerV2Pool {
	v, ok := balV2PoolInfos.Load(poolHex(addr))
	if !ok {
		return nil
	}
	info := v.(*BalancerV2PoolInfo)
	if info.NumTokens < 2 || len(info.Tokens) < 2 || len(info.Weights) < 2 {
		return nil
	}
	return &BalancerV2Pool{addr: addr, info: info, reader: reader}
}

func (p *BalancerV2Pool) Address() common.Address {
	return p.addr
}

func (p *BalancerV2Pool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if amountIn.IsZero() {
		return uint256.Int{}
	}
	if len(p.info.Tokens) < 2 {
		return uint256.Int{}
	}

	// Find token indices in registration order.
	indexIn, indexOut := -1, -1
	for i, tok := range p.info.Tokens {
		if tok == tokenIn {
			indexIn = i
		}
		if tok == tokenOut {
			indexOut = i
		}
	}
	if indexIn < 0 || indexOut < 0 || indexIn == indexOut {
		return uint256.Int{}
	}

	// Read live balances from Vault storage.
	var rawBalIn, rawBalOut *big.Int

	switch p.info.Specialization {
	case balV2SpecGeneral, balV2SpecMinimalSwapInfo:
		rawBalIn = balV2ReadMinimalSwapInfoBalance(p.reader, p.info.PoolId, p.info.Tokens[indexIn])
		rawBalOut = balV2ReadMinimalSwapInfoBalance(p.reader, p.info.PoolId, p.info.Tokens[indexOut])

	case balV2SpecTwoToken:
		// tokenA < tokenB (sorted by address)
		tokenA := p.info.Tokens[0]
		tokenB := p.info.Tokens[1]
		balA, balB := balV2ReadTwoTokenBalances(p.reader, p.info.PoolId, tokenA, tokenB)
		if indexIn == 0 {
			rawBalIn, rawBalOut = balA, balB
		} else {
			rawBalIn, rawBalOut = balB, balA
		}

	default:
		return uint256.Int{}
	}

	if rawBalIn == nil || rawBalOut == nil || rawBalIn.Sign() <= 0 || rawBalOut.Sign() <= 0 {
		return uint256.Int{}
	}

	sf := p.info.ScalingFactors
	// Scale raw balances to 18 decimals.
	balIn18 := new(big.Int).Mul(rawBalIn, sf[indexIn])
	balOut18 := new(big.Int).Mul(rawBalOut, sf[indexOut])
	amountIn18 := new(big.Int).Mul(amountIn.ToBig(), sf[indexIn])

	// Deduct swap fee: feeAmount = amountIn18 * swapFee / 1e18 (round up)
	feeAmount := fpMulUp(amountIn18, p.info.SwapFeePercentage)
	amountInAfterFee := new(big.Int).Sub(amountIn18, feeAmount)
	if amountInAfterFee.Sign() <= 0 {
		return uint256.Int{}
	}

	// Weighted math: amountOut18 = balOut18 * (1 - (balIn18/(balIn18+amountInAfterFee))^(wIn/wOut))
	amountOut18 := WeightedComputeOutGivenExactIn(
		balIn18,
		p.info.Weights[indexIn],
		balOut18,
		p.info.Weights[indexOut],
		amountInAfterFee,
	)

	if amountOut18 == nil || amountOut18.Sign() <= 0 {
		return uint256.Int{}
	}

	// Scale back from 18 decimals: amountOutRaw = amountOut18 / scalingFactorOut
	amountOutRaw := new(big.Int).Div(amountOut18, sf[indexOut])
	if amountOutRaw.Sign() <= 0 {
		return uint256.Int{}
	}

	result, overflow := uint256.FromBig(amountOutRaw)
	if overflow {
		return uint256.Int{}
	}
	return *result
}
