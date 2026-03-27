package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// AlgebraPool implements PoolQuoter for Algebra V1 Integral pools.
// Unlike V3 which pre-loads ticks via bitmap, Algebra uses a linked-list
// of initialized ticks that must be traversed at quote time.
type AlgebraPool struct {
	addr        common.Address
	stateReader StateReader
}

func newAlgebraPool(addr common.Address, reader StorageReader) *AlgebraPool {
	poolAddress := strings.ToLower(addr.Hex())

	// Create StateReader adapter
	stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
		a := common.HexToAddress(contractAddr)
		slotHash := common.BigToHash(slot)
		return reader(a, slotHash), nil
	}

	// Pre-read globalState (slot 2) and packed slot (slot 9) to register
	// dependency tracking. These reads also validate the pool is initialized.
	sqrtPrice, _, _, _, err := algebraReadGlobalState(stateReader, poolAddress)
	if err != nil || sqrtPrice == nil || sqrtPrice.Sign() == 0 {
		return nil
	}

	_, _, _, err = algebraReadPackedSlot(stateReader, poolAddress)
	if err != nil {
		return nil
	}

	return &AlgebraPool{
		addr:        addr,
		stateReader: stateReader,
	}
}

func (p *AlgebraPool) Address() common.Address {
	return p.addr
}

func (p *AlgebraPool) Quote(amountIn *uint256.Int, zeroForOne bool) (result uint256.Int) {
	defer func() {
		if r := recover(); r != nil {
			result = uint256.Int{}
		}
	}()

	poolAddress := strings.ToLower(p.addr.Hex())
	out, err := QuoteAlgebraStorage(p.stateReader, poolAddress, amountIn.ToBig(), zeroForOne)
	if err != nil || out == nil || out.Sign() <= 0 {
		return uint256.Int{}
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return uint256.Int{}
	}
	return *outU256
}
