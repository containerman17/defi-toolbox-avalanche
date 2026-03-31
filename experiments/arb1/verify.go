package main

import (
	"fmt"
	"math/big"
	"os"

	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// executeSwap selector — for simulation (router already has tokens via override)
var executeSwapSelector = [4]byte{0x32, 0x39, 0x33, 0x4d}

// swap selector — for on-chain tx: swap(address[],uint8[],address[],uint256[],bytes[],uint256)
// The 6th param is minOutput (reverts if output < minOutput).
var swapSelector = [4]byte{0x9c, 0x03, 0x60, 0x14}

// Verifier handles EVM verification of cycle candidates.
type Verifier struct {
	state      *statedb.StateDB
	cfg        statedb.EVMConfig
	routerAddr common.Address
	caller     common.Address
	evmCtx     *statedb.CachedContext
	pt         *PoolTable
	hub     common.Address
	verbose bool
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
) *Verifier {
	return &Verifier{
		state:      state,
		cfg:        cfg,
		routerAddr: routerAddr,
		caller:     caller,
		evmCtx:     statedb.GetCachedContext(cfg),
		pt:         pt,
		hub:        hub,
	}
}

// Block returns the block number this verifier is configured for.
func (v *Verifier) Block() uint64 { return v.cfg.BlockNumber }

// Verify executes a full multi-hop cycle through the HayabusaRouter via EVM.
// Returns (amountOut, gasUsed, success).
func (v *Verifier) Verify(c *Cycle, amountIn *uint256.Int) (*uint256.Int, uint64, bool) {
	r := v.VerifyFull(c, amountIn)
	if r.Reverted || len(r.RetData) < 32 {
		return nil, r.GasUsed, false
	}
	amountOut := new(uint256.Int).SetBytes(r.RetData[len(r.RetData)-32:])
	if amountOut.IsZero() {
		return nil, r.GasUsed, false
	}
	return amountOut, r.GasUsed, true
}

// VerifyFull executes a cycle and returns the full EVMResult including calldata,
// raw return data, gas, and block number — for cross-checking against RPC.
func (v *Verifier) VerifyFull(c *Cycle, amountIn *uint256.Int) EVMResult {
	calldata := EncodeSwapCalldata(c, v.pt, v.hub, amountIn)
	from := v.caller
	cs := statedb.NewCallState(v.state)

	ret, gasUsed, err := v.evmCtx.ExecuteWithCallState(cs, from, v.routerAddr, calldata)

	r := EVMResult{
		Cycle:    c,
		AmountIn: amountIn,
		Calldata: calldata,
		RetData:  ret,
		GasUsed:  gasUsed,
		Block:    v.cfg.BlockNumber,
	}

	if err != nil {
		r.Reverted = true
		r.ErrMsg = err.Error()
	}

	// Check for state fetch errors (stale block, network failure)
	if fetchErr := cs.Err(); fetchErr != nil {
		r.Reverted = true
		r.ErrMsg = fmt.Sprintf("state fetch error: %v", fetchErr)
	}

	if v.verbose {
		if r.Reverted {
			fmt.Fprintf(os.Stderr, "[arb/evm] REVERT %d-hop in=%s err=%v gas=%d ret_len=%d ret_hex=%x sel=%x to=%s from=%s\n",
				c.Hops, amountIn.Dec(), err, gasUsed, len(ret), ret, calldata[:4], v.routerAddr.Hex()[:10], from.Hex()[:10])
		} else if len(ret) < 32 {
			fmt.Fprintf(os.Stderr, "[arb/evm] SHORT_RET %d-hop in=%s ret_len=%d gas=%d\n",
				c.Hops, amountIn.Dec(), len(ret), gasUsed)
		} else {
			amountOut := new(uint256.Int).SetBytes(ret[len(ret)-32:])
			fmt.Fprintf(os.Stderr, "[arb/evm] OK %d-hop in=%s out=%s gas=%d ret_len=%d\n",
				c.Hops, amountIn.Dec(), amountOut.Dec(), gasUsed, len(ret))
		}
	}

	return r
}

// EncodeSwapCalldata builds swap() calldata for on-chain execution.
// swap() has 6 params: the same 5 array params as executeSwap, plus uint256 minOutput.
// minOutput = amountIn (for arb: we want at least our input back).
func EncodeSwapCalldata(c *Cycle, pt *PoolTable, hub common.Address, amountIn *uint256.Int) []byte {
	base := encodeMultiHopSwap(c, pt, hub, amountIn)

	// Insert minOutput as 6th head word. Shift all 5 offsets by +32.
	result := make([]byte, len(base)+32)
	copy(result[0:4], swapSelector[:])

	for i := 0; i < 5; i++ {
		off := 4 + i*32
		var origOff uint64
		for j := 0; j < 8; j++ {
			origOff = origOff<<8 | uint64(base[off+24+j])
		}
		origOff += 32
		for j := 0; j < 8; j++ {
			result[off+31-j] = byte(origOff >> (j * 8))
		}
	}

	// 6th head word: minOutput = amountIn
	b := amountIn.Bytes32()
	copy(result[4+5*32:4+6*32], b[:])

	// Copy array data (shifted by 32)
	copy(result[4+6*32:], base[4+5*32:])

	return result
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

	copy(buf[0:4], executeSwapSelector[:]) // simulation uses executeSwap (router has override balance)
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
