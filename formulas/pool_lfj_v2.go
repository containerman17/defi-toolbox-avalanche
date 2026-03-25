package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// LFJV2Pool is a pre-loaded LFJ V2 (Liquidity Book) pool.
// Construction reads the parameters slot to get activeId, binStep, and fee params.
// Bin reserves and tree traversal are read on demand during Quote via the StateReader.
type LFJV2Pool struct {
	addr           common.Address
	state          *LFJV2State
	layout         *lfjV2LayoutFast
	reader         StateReader
	tokenXIsToken0 bool // true if tokenX is the lower-address token (token0)
}

func newLFJV2Pool(addr common.Address, reader StorageReader, token0, token1 common.Address) *LFJV2Pool {
	poolAddress := strings.ToLower(addr.Hex())

	// Check if pool is in lfjV2Registry (has immutable data: binStep + tokenX ordering)
	imm, ok := lfjV2Registry[poolAddress]
	if !ok {
		return nil
	}

	// Create StateReader adapter
	stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
		a := common.HexToAddress(contractAddr)
		slotHash := common.BigToHash(slot)
		return reader(a, slotHash), nil
	}

	token0Hex := strings.ToLower(token0.Hex())
	token1Hex := strings.ToLower(token1.Hex())
	// Ensure token0 < token1
	if token0Hex > token1Hex {
		token0Hex, token1Hex = token1Hex, token0Hex
	}

	state, layout, err := FetchLFJV2StateFast(stateReader, poolAddress, token0Hex, token1Hex)
	if err != nil || state == nil {
		return nil
	}

	return &LFJV2Pool{
		addr:           addr,
		state:          state,
		layout:         layout,
		reader:         stateReader,
		tokenXIsToken0: imm.TokenXIsToken0,
	}
}

func (p *LFJV2Pool) Address() common.Address {
	return p.addr
}

func (p *LFJV2Pool) Quote(amountIn *uint256.Int, zeroForOne bool) (result *uint256.Int, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			ok = false
		}
	}()

	// Map zeroForOne to swapForY:
	// zeroForOne=true means selling token0 (lower address) to get token1.
	// swapForY=true means selling tokenX to get tokenY.
	// If tokenX IS token0, then zeroForOne == swapForY.
	// If tokenX IS token1, then zeroForOne != swapForY.
	swapForY := zeroForOne
	if !p.tokenXIsToken0 {
		swapForY = !zeroForOne
	}

	amtIn := amountIn.ToBig()
	out := QuoteLFJV2Fast(p.reader, p.state, p.layout, amtIn, swapForY, 0)
	if out == nil || out.Sign() <= 0 {
		return nil, false
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return nil, false
	}
	return outU256, true
}
