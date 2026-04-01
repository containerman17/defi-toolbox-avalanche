package formulas

import (
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

	state, err := FetchPharaohV1StateStorage(reader, poolAddress)
	if err != nil || state == nil {
		return nil
	}

	if state.Reserve0 == nil || state.Reserve1 == nil ||
		state.Reserve0.IsZero() || state.Reserve1.IsZero() {
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

func (p *PharaohV1Pool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	zeroForOne := tokenIn.Cmp(tokenOut) < 0
	out := QuotePharaohV1(p.state, amountIn, zeroForOne)
	if out == nil || out.IsZero() {
		return uint256.Int{}
	}
	return *out
}
