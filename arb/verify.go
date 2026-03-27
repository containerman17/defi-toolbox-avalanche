package arb

import (
	"fmt"
	"math/big"
	"os"

	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// swap selector: keccak256("swap(address[],uint8[],address[],uint256[],bytes[])")[:4]
// swap() pulls tokenIn from msg.sender via transferFrom, executes hops, sends tokenOut back.
var SwapSelector = [4]byte{0x46, 0x6a, 0x92, 0x59}

// Verifier handles EVM verification of cycle candidates.
type Verifier struct {
	state      *statedb.StateDB
	cfg        statedb.EVMConfig
	routerAddr common.Address
	caller     common.Address // the wallet that will send the tx
	evmCtx     *statedb.CachedContext
	pt         *PoolTable
	hub        common.Address
	balSlot   common.Hash   // keccak256(wallet, 3) on hub token
	allowSlot common.Hash   // keccak256(router, keccak256(wallet, 4)) on hub token
	walletBal *uint256.Int  // wallet's hub token balance for override
	verbose   bool
}

// SetVerbose enables detailed logging of each EVM verification attempt.
func (v *Verifier) SetVerbose(on bool) { v.verbose = on }

func NewVerifier(
	state *statedb.StateDB,
	cfg statedb.EVMConfig,
	routerAddr common.Address,
	caller common.Address,
	pt *PoolTable,
	hub common.Address,
	walletBalance *uint256.Int, // wallet's hub token balance (e.g. WAVAX)
) *Verifier {
	v := &Verifier{
		state:      state,
		cfg:        cfg,
		routerAddr: routerAddr,
		caller:     caller,
		evmCtx:     statedb.GetCachedContext(cfg),
		pt:         pt,
		hub:        hub,
		walletBal:  walletBalance,
	}

	if walletBalance != nil {
		// WAVAX: balanceOf at slot 3, allowance at slot 4
		var balKey [64]byte
		copy(balKey[12:32], caller[:])
		balKey[63] = 3
		v.balSlot = crypto.Keccak256Hash(balKey[:])

		var innerKey [64]byte
		copy(innerKey[12:32], caller[:])
		innerKey[63] = 4
		innerHash := crypto.Keccak256Hash(innerKey[:])
		var outerKey [64]byte
		copy(outerKey[12:32], routerAddr[:])
		copy(outerKey[32:64], innerHash[:])
		v.allowSlot = crypto.Keccak256Hash(outerKey[:])
	}

	return v
}

// Verify executes a full multi-hop cycle through the HayabusaRouter via EVM.
// Returns (amountOut, gasUsed, success).
func (v *Verifier) Verify(c *Cycle, amountIn *uint256.Int) (*uint256.Int, uint64, bool) {
	calldata := encodeMultiHopSwap(c, v.pt, v.hub, amountIn)

	// Inject wallet's WAVAX balance + approval directly into the base state.
	// These are sticky (persist across blocks) but that's fine — they represent
	// the wallet's actual on-chain state which doesn't change between our trades.
	if v.walletBal != nil && v.balSlot != (common.Hash{}) {
		v.state.SetStorageSlot(v.hub, v.balSlot, common.Hash(v.walletBal.Bytes32()))
		v.state.SetStorageSlot(v.hub, v.allowSlot, common.Hash(v.walletBal.Bytes32()))
	}
	from := v.caller
	cs := statedb.NewCallState(v.state)

	ret, gasUsed, err := v.evmCtx.ExecuteWithCallState(cs, from, v.routerAddr, calldata)

	if v.verbose {
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb/evm] REVERT %d-hop in=%s err=%v gas=%d ret_len=%d ret_hex=%x\n",
				c.Hops, amountIn.Dec(), err, gasUsed, len(ret), ret)
		} else if len(ret) < 32 {
			fmt.Fprintf(os.Stderr, "[arb/evm] SHORT_RET %d-hop in=%s ret_len=%d gas=%d\n",
				c.Hops, amountIn.Dec(), len(ret), gasUsed)
		} else {
			amountOut := new(uint256.Int).SetBytes(ret[len(ret)-32:])
			fmt.Fprintf(os.Stderr, "[arb/evm] OK %d-hop in=%s out=%s gas=%d ret_len=%d\n",
				c.Hops, amountIn.Dec(), amountOut.Dec(), gasUsed, len(ret))
		}
	}

	if err != nil || len(ret) < 32 {
		return nil, gasUsed, false
	}

	amountOut := new(uint256.Int).SetBytes(ret[len(ret)-32:])
	if amountOut.IsZero() {
		return nil, gasUsed, false
	}

	return amountOut, gasUsed, true
}

// EncodeSwapCalldata builds swap() calldata for on-chain execution.
// Same encoding used for both simulation and real tx.
func EncodeSwapCalldata(c *Cycle, pt *PoolTable, hub common.Address, amountIn *uint256.Int) []byte {
	return encodeMultiHopSwap(c, pt, hub, amountIn)
}

// encodeMultiHopSwap builds executeSwap calldata for EVM simulation.
func encodeMultiHopSwap(c *Cycle, pt *PoolTable, hub common.Address, amountIn *uint256.Int) []byte {
	nPools := int(c.Hops)
	nTokensPairs := nPools * 2

	// Expand cycle data from compact form
	tokens := c.ExpandTokens(pt, hub)
	poolAddrs := c.ExpandPoolAddrs(pt)
	poolTypes := c.ExpandPoolTypes(pt)
	extraDatas := c.ExpandExtraDatas(pt)

	// Build paired tokens: [tokenIn0, tokenOut0, tokenIn1, tokenOut1, ...]
	pairedTokens := make([]common.Address, nTokensPairs)
	for i := 0; i < nPools; i++ {
		pairedTokens[i*2] = tokens[i]
		pairedTokens[i*2+1] = tokens[i+1]
	}

	// amountsIn: [realAmount, 0, 0, ...]
	amounts := make([]*uint256.Int, nPools)
	amounts[0] = amountIn
	for i := 1; i < nPools; i++ {
		amounts[i] = uint256.NewInt(0)
	}

	poolsSize := 32 + nPools*32
	typesSize := 32 + nPools*32
	tokensSize := 32 + nTokensPairs*32
	amountsSize := 32 + nPools*32

	extraOffsetSize := 32 + nPools*32
	extraDataSize := 0
	for _, ed := range extraDatas {
		if ed != "" {
			encoded := encodeV4Extra(ed)
			padded := ((len(encoded) + 31) / 32) * 32
			extraDataSize += 32 + padded
		} else {
			extraDataSize += 32
		}
	}

	dataOffset := 5 * 32
	totalPayload := dataOffset + poolsSize + typesSize + tokensSize + amountsSize + extraOffsetSize + extraDataSize
	buf := make([]byte, 4+totalPayload)

	copy(buf[0:4], SwapSelector[:]) // swap() pulls from caller, matches on-chain behavior
	base := 4

	poolsOff := dataOffset
	typesOff := poolsOff + poolsSize
	tokensOff := typesOff + typesSize
	amountsOff := tokensOff + tokensSize
	extrasOff := amountsOff + amountsSize

	writeWord256(buf, base+0*32, uint64(poolsOff))
	writeWord256(buf, base+1*32, uint64(typesOff))
	writeWord256(buf, base+2*32, uint64(tokensOff))
	writeWord256(buf, base+3*32, uint64(amountsOff))
	writeWord256(buf, base+4*32, uint64(extrasOff))

	pos := base + poolsOff
	writeWord256(buf, pos, uint64(nPools))
	pos += 32
	for _, pool := range poolAddrs {
		writeAddr(buf, pos, pool)
		pos += 32
	}

	writeWord256(buf, pos, uint64(nPools))
	pos += 32
	for _, pt := range poolTypes {
		writeWord256(buf, pos, uint64(pt))
		pos += 32
	}

	writeWord256(buf, pos, uint64(nTokensPairs))
	pos += 32
	for _, token := range pairedTokens {
		writeAddr(buf, pos, token)
		pos += 32
	}

	writeWord256(buf, pos, uint64(nPools))
	pos += 32
	for _, amt := range amounts {
		b := amt.Bytes32()
		copy(buf[pos:pos+32], b[:])
		pos += 32
	}

	writeWord256(buf, pos, uint64(nPools))
	pos += 32
	offsetBase := pos
	dataStart := offsetBase + nPools*32
	currentDataPos := dataStart
	for i, ed := range extraDatas {
		writeWord256(buf, offsetBase+i*32, uint64(currentDataPos-offsetBase))
		if ed != "" {
			encoded := encodeV4Extra(ed)
			padded := ((len(encoded) + 31) / 32) * 32
			currentDataPos += 32 + padded
		} else {
			currentDataPos += 32
		}
	}

	pos = dataStart
	for _, ed := range extraDatas {
		if ed != "" {
			encoded := encodeV4Extra(ed)
			padded := ((len(encoded) + 31) / 32) * 32
			writeWord256(buf, pos, uint64(len(encoded)))
			pos += 32
			copy(buf[pos:pos+len(encoded)], encoded)
			pos += padded
		} else {
			writeWord256(buf, pos, 0)
			pos += 32
		}
	}

	return buf[:pos]
}

func encodeV4Extra(extraData string) []byte {
	if extraData == "" {
		return nil
	}
	parts := make(map[string]string)
	for _, kv := range splitComma(extraData) {
		eq := indexOf(kv, '=')
		if eq > 0 {
			parts[kv[:eq]] = kv[eq+1:]
		}
	}
	var fee big.Int
	if v, ok := parts["fee"]; ok {
		fee.SetString(v, 10)
	}
	var tickSpacing big.Int
	if v, ok := parts["ts"]; ok {
		tickSpacing.SetString(v, 10)
	}
	hooks := common.HexToAddress(parts["hooks"])

	buf := make([]byte, 128)
	writeBigWord(buf, 0, &fee)
	if tickSpacing.Sign() >= 0 {
		writeBigWord(buf, 32, &tickSpacing)
	} else {
		mod := new(big.Int).Lsh(big.NewInt(1), 256)
		twos := new(big.Int).Add(&tickSpacing, mod)
		writeBigWord(buf, 32, twos)
	}
	copy(buf[64+12:64+32], hooks[:])
	return buf
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func writeWord256(buf []byte, offset int, val uint64) {
	for i := 0; i < 8; i++ {
		buf[offset+31-i] = byte(val >> (i * 8))
	}
}

func writeAddr(buf []byte, offset int, addr common.Address) {
	copy(buf[offset+12:offset+32], addr[:])
}

func writeBigWord(buf []byte, offset int, val *big.Int) {
	b := val.Bytes()
	start := offset + 32 - len(b)
	copy(buf[start:offset+32], b)
}
