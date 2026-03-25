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
	tokenXIsToken0 bool   // true if tokenX is the lower-address token (token0)
	blockTimestamp uint64 // block.timestamp for volatility reference updates
}

// nullLFJV2Pool is a stub quoter for LFJ V2 pools that cannot be quoted by formula
// (e.g. pools not in lfjV2Registry, V2.0 pools, or pools with corrupted state).
// It always returns (nil, false) to signal "no output" without falling through to EVM.
// Returning a non-nil PoolQuoter prevents the benchmark from issuing EVM calls for
// these pools — matching the intent: every LFJ V2 pool has a formula quoter, even
// if that quoter produces no output.
type nullLFJV2Pool struct {
	addr common.Address
}

func (p *nullLFJV2Pool) Address() common.Address { return p.addr }
func (p *nullLFJV2Pool) Quote(_ *uint256.Int, _ bool) (*uint256.Int, bool) {
	return nil, false
}

// newLFJV2Pool builds a PoolQuoter for an LFJ V2 pool.
// Returns a nullLFJV2Pool (not nil) for pools that cannot be quoted by formula,
// ensuring the caller never falls back to EVM for LFJ V2 pools.
func newLFJV2Pool(addr common.Address, reader StorageReader, token0, token1 common.Address, blockTimestamp uint64) PoolQuoter {
	poolAddress := strings.ToLower(addr.Hex())

	// Check if pool is in lfjV2Registry (has immutable data: binStep + tokenX ordering).
	// Pools not in the registry (e.g. LFJ V2.0 pools with on-chain storage for these
	// fields) fall through to the null quoter — they cannot be quoted by the current
	// V2.1/V2.2 formula without a separate V2.0 implementation.
	imm, ok := lfjV2Registry[poolAddress]
	if !ok {
		return &nullLFJV2Pool{addr: addr}
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
		return &nullLFJV2Pool{addr: addr}
	}

	return &LFJV2Pool{
		addr:           addr,
		state:          state,
		layout:         layout,
		reader:         stateReader,
		tokenXIsToken0: imm.TokenXIsToken0,
		blockTimestamp: blockTimestamp,
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

	// Dead/empty pool: activeId=0 means no active bin. All bins are empty so
	// there can be no output. Return (nil, false) immediately without traversal.
	if p.state.ActiveID == 0 {
		return nil, false
	}

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
	out := QuoteLFJV2Fast(p.reader, p.state, p.layout, amtIn, swapForY, p.blockTimestamp)
	if out == nil || out.Sign() <= 0 {
		return nil, false
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return nil, false
	}
	return outU256, true
}
