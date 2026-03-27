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
	baseIsToken0 bool // true if baseToken is the lower-address token (token0)
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

	return &DODOPool{
		addr:         addr,
		state:        dodoState,
		baseIsToken0: baseIsToken0,
	}
}

func (p *DODOPool) Address() common.Address {
	return p.addr
}

func (p *DODOPool) Quote(amountIn *uint256.Int, zeroForOne bool) (result uint256.Int) {
	defer func() {
		if r := recover(); r != nil {
			result = uint256.Int{}
		}
	}()
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
