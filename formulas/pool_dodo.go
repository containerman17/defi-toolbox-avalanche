package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// DODOPool is a pre-loaded DODO V2 PMM pool.
// Construction reads 5-6 storage slots. Quote reuses QuoteDODO with pre-loaded state.
type DODOPool struct {
	addr         common.Address
	state        *DODOState
	baseIsToken0 bool          // true if baseToken is the lower-address token (token0)
	minBaseSwap  *uint256.Int  // DPP/DSP: _MIN_BASE_SWAP_AMOUNT_ (slot 10), nil if not applicable
	minQuoteSwap *uint256.Int  // DPP/DSP: _MIN_QUOTE_SWAP_AMOUNT_ (slot 11), nil if not applicable
}

func newDODOPool(addr common.Address, reader StorageReader, token0 common.Address) *DODOPool {
	poolAddress := strings.ToLower(addr.Hex())

	// Create StateReader adapter for FetchDODOState
	stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
		a := common.HexToAddress(contractAddr)
		slotHash := common.BigToHash(slot)
		return reader(a, slotHash), nil
	}

	dodoState, err := FetchDODOState(stateReader, poolAddress, "", "")
	if err != nil || dodoState == nil {
		return nil
	}

	// Determine whether baseToken is token0 (lower address) or token1 (higher address).
	// DODO's base token is independent of Uniswap-style token sort order.
	baseIsToken0 := strings.EqualFold(dodoState.BaseToken, strings.ToLower(token0.Hex()))

	pool := &DODOPool{
		addr:         addr,
		state:        dodoState,
		baseIsToken0: baseIsToken0,
	}

	// Read min swap amounts from slots 10/11 (if non-zero).
	// DPP and DSP layout pools store _MIN_BASE_SWAP_AMOUNT_ / _MIN_QUOTE_SWAP_AMOUNT_
	// at these slots. DVM pools store unrelated state variables there (e.g. slot 11
	// holds a huge cumulative-price value), so skip DVM to avoid false positives.
	if dodoState.Layout == DODOLayoutDPP || dodoState.Layout == DODOLayoutDSP {
		if minBase, err := stateReader(poolAddress, big.NewInt(10)); err == nil {
			var v uint256.Int
			v.SetBytes32(minBase[:])
			if !v.IsZero() {
				pool.minBaseSwap = new(uint256.Int).Set(&v)
			}
		}
		if minQuote, err := stateReader(poolAddress, big.NewInt(11)); err == nil {
			var v uint256.Int
			v.SetBytes32(minQuote[:])
			if !v.IsZero() {
				pool.minQuoteSwap = new(uint256.Int).Set(&v)
			}
		}
	}

	return pool
}

func (p *DODOPool) Address() common.Address {
	return p.addr
}

func (p *DODOPool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) (result uint256.Int) {
	zeroForOne := tokenIn.Cmp(tokenOut) < 0
	defer func() {
		if r := recover(); r != nil {
			result = uint256.Int{}
		}
	}()

	// DPP/DSP pools enforce _MIN_BASE_SWAP_AMOUNT_ / _MIN_QUOTE_SWAP_AMOUNT_.
	// Check against known minimums to avoid false positives.
	if p.minBaseSwap != nil || p.minQuoteSwap != nil {
		sellBase := zeroForOne == p.baseIsToken0
		if sellBase && p.minBaseSwap != nil && amountIn.Lt(p.minBaseSwap) {
			return uint256.Int{}
		}
		if !sellBase && p.minQuoteSwap != nil && amountIn.Lt(p.minQuoteSwap) {
			return uint256.Int{}
		}
	}

	amtIn := amountIn.ToBig()
	// Convert zeroForOne to sellBase: if baseToken is token0, then zeroForOne means sellBase.
	// If baseToken is token1, then zeroForOne means sellQuote (so sellBase = !zeroForOne).
	sellBase := zeroForOne == p.baseIsToken0
	out := QuoteDODO(p.state, amtIn, sellBase)
	if out == nil || out.Sign() < 0 {
		return uint256.Int{}
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return uint256.Int{}
	}
	return *outU256
}
