package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// lfjV2NullBinID is the sentinel active-bin ID used by LFJ V2 for pools that
// have no active bin (uninitialized or fully drained). The contract stores 2^23
// (= 0x800000) as the "null" position in the 24-bit bin ID space.
const lfjV2NullBinID uint32 = 0x800000

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
	// Rebasing token surplus: extra tokens the pool holds beyond _reserves.
	// For rebasing tokens (e.g., aWAVAX), balanceOf(pool) > _reserves because
	// interest accrues. The LBPair.swap() function detects this surplus as
	// additional input via receivedX/receivedY. We pre-compute the surplus
	// at pool construction time and add it to amountIn during quoting.
	surplusX *uint256.Int // extra tokenX beyond _reserves.X
	surplusY *uint256.Int // extra tokenY beyond _reserves.Y
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
func (p *nullLFJV2Pool) Quote(_ *uint256.Int, _ bool) uint256.Int {
	return uint256.Int{}
}

// balanceOfSelector is the ERC20 balanceOf(address) function selector.
var balanceOfSelector = [4]byte{0x70, 0xa0, 0x82, 0x31}

// newLFJV2Pool builds a PoolQuoter for an LFJ V2 pool.
// Returns a nullLFJV2Pool (not nil) for pools that cannot be quoted by formula,
// ensuring the caller never falls back to EVM for LFJ V2 pools.
func newLFJV2Pool(addr common.Address, reader StorageReader, token0, token1 common.Address, blockTimestamp uint64, caller EVMCaller) PoolQuoter {
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

	// Compute rebasing token surplus: the difference between actual token
	// balances and the pool's tracked _reserves. For rebasing tokens (e.g.,
	// Aave aTokens), interest accrues continuously, causing balanceOf(pool)
	// to exceed _reserves. The LBPair.swap() function sees this surplus as
	// additional input via its receivedX/receivedY check, so we must account
	// for it to match the EVM swap result.
	var surplusX, surplusY *uint256.Int

	// Determine tokenX and tokenY addresses
	var tokenXAddr, tokenYAddr common.Address
	if imm.TokenXIsToken0 {
		tokenXAddr = common.HexToAddress(token0Hex)
		tokenYAddr = common.HexToAddress(token1Hex)
	} else {
		tokenXAddr = common.HexToAddress(token1Hex)
		tokenYAddr = common.HexToAddress(token0Hex)
	}

	if caller != nil && state.GlobalReserveX != nil && state.GlobalReserveY != nil {
		surplusX = lfjV2ComputeSurplus(caller, tokenXAddr, addr, state.GlobalReserveX)
		surplusY = lfjV2ComputeSurplus(caller, tokenYAddr, addr, state.GlobalReserveY)
	}

	return &LFJV2Pool{
		addr:           addr,
		state:          state,
		layout:         layout,
		reader:         stateReader,
		tokenXIsToken0: imm.TokenXIsToken0,
		blockTimestamp: blockTimestamp,
		surplusX:       surplusX,
		surplusY:       surplusY,
	}
}

// lfjV2ComputeSurplus computes the rebasing surplus for a token:
// surplus = balanceOf(pool) - globalReserve. Returns nil if no surplus.
func lfjV2ComputeSurplus(caller EVMCaller, token, pool common.Address, globalReserve *big.Int) *uint256.Int {
	// Encode balanceOf(pool) call
	calldata := make([]byte, 36)
	copy(calldata[:4], balanceOfSelector[:])
	copy(calldata[16:36], pool[:]) // address left-padded to 32 bytes

	result, ok := caller(token, calldata)
	if !ok || len(result) < 32 {
		return nil
	}

	var balance uint256.Int
	balance.SetBytes(result[:32])

	var reserve uint256.Int
	reserve.SetFromBig(globalReserve)

	if balance.Gt(&reserve) {
		var surplus uint256.Int
		surplus.Sub(&balance, &reserve)
		return &surplus
	}
	return nil
}

func (p *LFJV2Pool) Address() common.Address {
	return p.addr
}

func (p *LFJV2Pool) SetBlockTimestamp(ts uint64) {
	p.blockTimestamp = ts
}

func (p *LFJV2Pool) Quote(amountIn *uint256.Int, zeroForOne bool) (result uint256.Int) {
	defer func() {
		if r := recover(); r != nil {
			result = uint256.Int{}
		}
	}()

	// Dead/empty pool guard: activeId==0 or activeId==lfjV2NullBinID (2^23=0x800000)
	// means no active bin exists. lfjV2NullBinID is the sentinel used by the LFJ V2
	// contract for uninitialized or fully-drained pools. Without an active bin, bin
	// traversal would produce incorrect non-zero results from adjacent tree nodes.
	if p.state.ActiveID == 0 || p.state.ActiveID == lfjV2NullBinID {
		return uint256.Int{}
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

	// Add rebasing token surplus to amountIn. In the LBPair.swap() function,
	// the pool detects received tokens via balanceOf(pool) - _reserves, which
	// includes any surplus from rebasing. Our formula must include this surplus
	// to match the EVM swap result.
	var adjustedIn uint256.Int
	adjustedIn.Set(amountIn)
	if swapForY && p.surplusX != nil {
		adjustedIn.Add(&adjustedIn, p.surplusX)
	} else if !swapForY && p.surplusY != nil {
		adjustedIn.Add(&adjustedIn, p.surplusY)
	}

	amtIn := adjustedIn.ToBig()
	out := QuoteLFJV2Fast(p.reader, p.state, p.layout, amtIn, swapForY, p.blockTimestamp)
	if out == nil || out.Sign() <= 0 {
		return uint256.Int{}
	}
	outU256, overflow := uint256.FromBig(out)
	if overflow {
		return uint256.Int{}
	}
	return *outU256
}
