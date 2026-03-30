package pathfinder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// executeSwap selector: keccak256("executeSwap(address[],uint8[],address[],uint256[],bytes[])")[:4]
var executeSwapSelector = [4]byte{0x32, 0x39, 0x33, 0x4d}

// V4PoolManager is the Uniswap V4 PoolManager singleton on Avalanche C-Chain.
var V4PoolManager = common.HexToAddress("0x06380C0e0912312B5150364B9DC4542BA0DbBc85")

// EncodeSwapSingle builds executeSwap calldata for a single-pool swap with empty extraData.
// For V4 pools, use EncodeSwapSingleWithExtra to pass fee/tickSpacing/hooks.
func EncodeSwapSingle(pool common.Address, poolType int, tokenIn, tokenOut common.Address, amountIn *uint256.Int) []byte {
	return encodeSwapSingleInner(pool, poolType, tokenIn, tokenOut, amountIn, nil)
}

// EncodeSwapSingleWithExtra builds executeSwap calldata with pool-type-specific
// address and extraData handling. For V4 (poolType 9), it substitutes the
// PoolManager address and ABI-encodes (fee, tickSpacing, hooks, wrapNative=0)
// from the pool's ExtraData string.
func EncodeSwapSingleWithExtra(pool common.Address, poolType int, tokenIn, tokenOut common.Address, amountIn *uint256.Int, extraData string) []byte {
	if poolType == 9 && extraData != "" {
		// V4: substitute PoolManager address, encode extraData
		extra := encodeV4ExtraData(extraData)
		return encodeSwapSingleInner(V4PoolManager, poolType, tokenIn, tokenOut, amountIn, extra)
	}
	return encodeSwapSingleInner(pool, poolType, tokenIn, tokenOut, amountIn, nil)
}

// encodeV4ExtraData parses "id=0x...,fee=18,ts=1,hooks=0x..." and returns
// ABI-encoded (uint256 fee, int256 tickSpacing, address hooks, uint256 wrapNative).
func encodeV4ExtraData(extraData string) []byte {
	parts := make(map[string]string)
	for _, kv := range strings.Split(extraData, ",") {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 {
			parts[kv[:eq]] = kv[eq+1:]
		}
	}

	var fee big.Int
	fmt.Sscan(parts["fee"], &fee)

	var tickSpacing big.Int
	fmt.Sscan(parts["ts"], &tickSpacing)

	hooks := common.HexToAddress(parts["hooks"])

	// ABI-encode: 4 words = 128 bytes
	// word 0: fee (uint256)
	// word 1: tickSpacing (int256, sign-extended)
	// word 2: hooks (address, left-padded)
	// word 3: wrapNative (uint256, always 0 for benchmark)
	buf := make([]byte, 128)
	writeWord(buf, 0, &fee)
	// tickSpacing as int256: if negative, sign-extend to 32 bytes
	if tickSpacing.Sign() >= 0 {
		writeWord(buf, 32, &tickSpacing)
	} else {
		// Two's complement: add 2^256
		mod := new(big.Int).Lsh(big.NewInt(1), 256)
		twos := new(big.Int).Add(&tickSpacing, mod)
		writeWord(buf, 32, twos)
	}
	writeAddress(buf, 64, hooks)
	// word 3: wrapNative = 0 (already zeroed)

	return buf
}

func encodeSwapSingleInner(pool common.Address, poolType int, tokenIn, tokenOut common.Address, amountIn *uint256.Int, extraData []byte) []byte {
	// extraData length rounded up to 32-byte words
	extraLen := len(extraData)
	extraPadded := ((extraLen + 31) / 32) * 32

	// ABI layout for executeSwap(address[], uint8[], address[], uint256[], bytes[]):
	// selector (4 bytes)
	// 5 offset words (5 * 32 = 160 bytes)
	// pools array:      length(1) + pool(1)             = 64 bytes
	// poolTypes array:  length(1) + type(1)             = 64 bytes
	// tokens array:     length(2) + tokenIn + tokenOut  = 96 bytes
	// amountsIn array:  length(1) + amount(1)           = 64 bytes
	// extraDatas array: length(1) + offset(1) + len(1) + extraPadded = 96 + extraPadded bytes
	totalSize := 4 + 160 + 64 + 64 + 96 + 64 + 96 + extraPadded
	data := make([]byte, totalSize)

	// Selector
	copy(data[0:4], executeSwapSelector[:])

	// Offsets (each relative to start of params, i.e. after selector)
	writeWord(data, 4+0*32, big.NewInt(160)) // pools offset
	writeWord(data, 4+1*32, big.NewInt(224)) // poolTypes offset
	writeWord(data, 4+2*32, big.NewInt(288)) // tokens offset
	writeWord(data, 4+3*32, big.NewInt(384)) // amountsIn offset
	writeWord(data, 4+4*32, big.NewInt(448)) // extraDatas offset

	pos := 4 + 160

	// pools array: [1, pool]
	writeWord(data, pos, big.NewInt(1))
	pos += 32
	writeAddress(data, pos, pool)
	pos += 32

	// poolTypes array: [1, type]
	writeWord(data, pos, big.NewInt(1))
	pos += 32
	writeWord(data, pos, big.NewInt(int64(poolType)))
	pos += 32

	// tokens array: [2, tokenIn, tokenOut]
	writeWord(data, pos, big.NewInt(2))
	pos += 32
	writeAddress(data, pos, tokenIn)
	pos += 32
	writeAddress(data, pos, tokenOut)
	pos += 32

	// amountsIn array: [1, amount]
	writeWord(data, pos, big.NewInt(1))
	pos += 32
	writeUint256(data, pos, amountIn)
	pos += 32

	// extraDatas array: [1, offset_to_first_element, length, data...]
	writeWord(data, pos, big.NewInt(1))
	pos += 32
	writeWord(data, pos, big.NewInt(32)) // offset to first bytes element
	pos += 32
	writeWord(data, pos, big.NewInt(int64(extraLen)))
	pos += 32
	if extraLen > 0 {
		copy(data[pos:pos+extraLen], extraData)
	}

	return data
}

// swapSelector: keccak256("swap(address[],uint8[],address[],uint256[],bytes[],uint256)")[:4]
var swapSelector = [4]byte{0x9c, 0x03, 0x60, 0x14}

// EncodeExecuteSwapMulti builds executeSwap() calldata for EVM simulation.
// No transferFrom, no minOutput — the router operates on pool state directly.
func EncodeExecuteSwapMulti(
	poolAddrs []common.Address,
	poolTypes []int,
	tokenPairs []common.Address,
	amountIn *uint256.Int,
	extraDatas []string,
) []byte {
	return encodeSwapMultiInner(executeSwapSelector, poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, nil)
}

// EncodeSwapMulti builds swap() calldata for on-chain execution.
// swap() = executeSwap's 5 array params + uint256 minOutput.
func EncodeSwapMulti(
	poolAddrs []common.Address,
	poolTypes []int,
	tokenPairs []common.Address,
	amountIn *uint256.Int,
	extraDatas []string,
	minOutput *uint256.Int,
) []byte {
	return encodeSwapMultiInner(swapSelector, poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, minOutput)
}

func encodeSwapMultiInner(
	selector [4]byte,
	poolAddrs []common.Address,
	poolTypes []int,
	tokenPairs []common.Address,
	amountIn *uint256.Int,
	extraDatas []string,
	minOutput *uint256.Int, // nil for executeSwap (no minOutput param)
) []byte {
	n := len(poolAddrs)
	hasMinOutput := minOutput != nil

	amounts := make([]*uint256.Int, n)
	amounts[0] = amountIn
	for i := 1; i < n; i++ {
		amounts[i] = uint256.NewInt(0)
	}

	addrs := make([]common.Address, n)
	copy(addrs, poolAddrs)
	encodedExtras := make([][]byte, n)
	for i := range extraDatas {
		if poolTypes[i] == 9 && extraDatas[i] != "" {
			encodedExtras[i] = encodeV4ExtraData(extraDatas[i])
			addrs[i] = V4PoolManager
		}
	}

	nTokenPairs := len(tokenPairs)
	poolsSize := 32 + n*32
	typesSize := 32 + n*32
	tokensSize := 32 + nTokenPairs*32
	amountsSize := 32 + n*32

	extraOffsetSize := 32 + n*32
	extraDataSize := 0
	for _, ed := range encodedExtras {
		if len(ed) > 0 {
			padded := ((len(ed) + 31) / 32) * 32
			extraDataSize += 32 + padded
		} else {
			extraDataSize += 32
		}
	}

	headWords := 5 // 5 offset words
	if hasMinOutput {
		headWords = 6 // + minOutput
	}
	headSize := headWords * 32
	totalPayload := headSize + poolsSize + typesSize + tokensSize + amountsSize + extraOffsetSize + extraDataSize
	buf := make([]byte, 4+totalPayload)

	copy(buf[0:4], selector[:])
	base := 4

	dataStart := headSize
	poolsOff := dataStart
	typesOff := poolsOff + poolsSize
	tokensOff := typesOff + typesSize
	amountsOff := tokensOff + tokensSize
	extrasOff := amountsOff + amountsSize

	writeWordU64(buf, base+0*32, uint64(poolsOff))
	writeWordU64(buf, base+1*32, uint64(typesOff))
	writeWordU64(buf, base+2*32, uint64(tokensOff))
	writeWordU64(buf, base+3*32, uint64(amountsOff))
	writeWordU64(buf, base+4*32, uint64(extrasOff))
	if hasMinOutput {
		b := minOutput.Bytes32()
		copy(buf[base+5*32:base+6*32], b[:])
	}

	pos := base + poolsOff
	writeWordU64(buf, pos, uint64(n))
	pos += 32
	for _, addr := range addrs {
		writeAddress(buf, pos, addr)
		pos += 32
	}

	writeWordU64(buf, pos, uint64(n))
	pos += 32
	for _, pt := range poolTypes {
		writeWordU64(buf, pos, uint64(pt))
		pos += 32
	}

	writeWordU64(buf, pos, uint64(nTokenPairs))
	pos += 32
	for _, token := range tokenPairs {
		writeAddress(buf, pos, token)
		pos += 32
	}

	writeWordU64(buf, pos, uint64(n))
	pos += 32
	for _, amt := range amounts {
		writeUint256(buf, pos, amt)
		pos += 32
	}

	writeWordU64(buf, pos, uint64(n))
	pos += 32
	offsetBase := pos
	offsetDataStart := offsetBase + n*32
	currentDataPos := offsetDataStart
	for i, ed := range encodedExtras {
		writeWordU64(buf, offsetBase+i*32, uint64(currentDataPos-offsetBase))
		if len(ed) > 0 {
			padded := ((len(ed) + 31) / 32) * 32
			currentDataPos += 32 + padded
		} else {
			currentDataPos += 32
		}
	}

	pos = offsetDataStart
	for _, ed := range encodedExtras {
		if len(ed) > 0 {
			padded := ((len(ed) + 31) / 32) * 32
			writeWordU64(buf, pos, uint64(len(ed)))
			pos += 32
			copy(buf[pos:pos+len(ed)], ed)
			pos += padded
		} else {
			writeWordU64(buf, pos, 0)
			pos += 32
		}
	}

	return buf[:pos]
}

func writeWordU64(buf []byte, offset int, val uint64) {
	for i := 0; i < 8; i++ {
		buf[offset+31-i] = byte(val >> (i * 8))
	}
}

func writeWord(buf []byte, offset int, val *big.Int) {
	b := val.Bytes()
	start := offset + 32 - len(b)
	copy(buf[start:offset+32], b)
}

func writeAddress(buf []byte, offset int, addr common.Address) {
	copy(buf[offset+12:offset+32], addr[:])
}

func writeUint256(buf []byte, offset int, val *uint256.Int) {
	b := val.Bytes32()
	copy(buf[offset:offset+32], b[:])
}
