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
	addr  common.Address
	state *DODOState
}

func newDODOPool(addr common.Address, reader StorageReader) *DODOPool {
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

	return &DODOPool{
		addr:  addr,
		state: dodoState,
	}
}

func (p *DODOPool) Address() common.Address {
	return p.addr
}

func (p *DODOPool) Quote(amountIn *uint256.Int, zeroForOne bool) (result *uint256.Int, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			ok = false
		}
	}()
	amtIn := amountIn.ToBig()
	out := QuoteDODO(p.state, amtIn, zeroForOne)
	if out == nil || out.Sign() <= 0 {
		return nil, false
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return nil, false
	}
	return outU256, true
}
