package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// PharaohV1Pool is a pre-loaded Pharaoh V1 (Solidly-fork) pool.
// Construction reads reserves from storage using the registry config.
// Quote reuses QuotePharaohV1 with pre-loaded state.
type PharaohV1Pool struct {
	addr  common.Address
	state *PharaohV1State
}

func newPharaohV1Pool(addr common.Address, reader StorageReader) *PharaohV1Pool {
	poolAddress := strings.ToLower(addr.Hex())

	// Create StateReader adapter for FetchPharaohV1StateStorage
	stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
		a := common.HexToAddress(contractAddr)
		slotHash := common.BigToHash(slot)
		return reader(a, slotHash), nil
	}

	state, err := FetchPharaohV1StateStorage(stateReader, poolAddress)
	if err != nil || state == nil {
		return nil
	}

	if state.Reserve0 == nil || state.Reserve1 == nil ||
		state.Reserve0.Sign() == 0 || state.Reserve1.Sign() == 0 {
		return nil
	}

	return &PharaohV1Pool{
		addr:  addr,
		state: state,
	}
}

func (p *PharaohV1Pool) Address() common.Address {
	return p.addr
}

func (p *PharaohV1Pool) Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	amtIn := amountIn.ToBig()
	out := QuotePharaohV1(p.state, amtIn, zeroForOne)
	if out == nil || out.Sign() <= 0 {
		return uint256.Int{}
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return uint256.Int{}
	}
	return *outU256
}
