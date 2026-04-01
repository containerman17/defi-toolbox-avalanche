package pathfinder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// debugSwapSingleSelector: keccak256("debugSwapSingle(address,uint8,address,address,uint256,bytes)")[:4]
var debugSwapSingleSelector = [4]byte{0x51, 0xe1, 0x6d, 0x05}

// V4PoolManager is the Uniswap V4 PoolManager singleton on Avalanche C-Chain.
var V4PoolManager = common.HexToAddress("0x06380C0e0912312B5150364B9DC4542BA0DbBc85")

// EncodeSwapSingle builds debugSwapSingle calldata for a single-pool swap with empty extraData.
// For V4 pools, use EncodeSwapSingleWithExtra to pass fee/tickSpacing/hooks.
func EncodeSwapSingle(pool common.Address, poolType int, tokenIn, tokenOut common.Address, amountIn *uint256.Int) []byte {
	return encodeSwapSingleInner(pool, poolType, tokenIn, tokenOut, amountIn, nil)
}

// EncodeSwapSingleWithExtra builds debugSwapSingle calldata with pool-type-specific
// address and extraData handling. For V4 (poolType 9), it substitutes the
// PoolManager address and ABI-encodes (fee, tickSpacing, hooks, wrapNative=0)
// from the pool's ExtraData string.
func EncodeSwapSingleWithExtra(pool common.Address, poolType int, tokenIn, tokenOut common.Address, amountIn *uint256.Int, extraData string) []byte {
	if poolType == 9 && extraData != "" {
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
	// ABI layout for debugSwapSingle(address, uint8, address, address, uint256, bytes):
	// selector (4 bytes)
	// pool (address, 32 bytes)
	// poolType (uint8, 32 bytes)
	// tokenIn (address, 32 bytes)
	// tokenOut (address, 32 bytes)
	// amountIn (uint256, 32 bytes)
	// extraData offset (32 bytes) → points to extraData length
	// extraData length (32 bytes)
	// extraData content (padded to 32 bytes)
	extraLen := len(extraData)
	extraPadded := ((extraLen + 31) / 32) * 32
	totalSize := 4 + 6*32 + 32 + extraPadded // selector + 6 params + length word + padded data
	data := make([]byte, totalSize)

	copy(data[0:4], debugSwapSingleSelector[:])

	pos := 4
	writeAddress(data, pos, pool)            // pool
	pos += 32
	writeWordU64(data, pos, uint64(poolType)) // poolType (uint8)
	pos += 32
	writeAddress(data, pos, tokenIn)          // tokenIn
	pos += 32
	writeAddress(data, pos, tokenOut)         // tokenOut
	pos += 32
	writeUint256(data, pos, amountIn)         // amountIn
	pos += 32
	writeWordU64(data, pos, 192)              // extraData offset (6 * 32 = 192 from start of params)
	pos += 32
	writeWordU64(data, pos, uint64(extraLen)) // extraData length
	pos += 32
	if extraLen > 0 {
		copy(data[pos:pos+extraLen], extraData)
	}

	return data
}

// swapSelector: keccak256("swap(address[],uint8[],address[],uint256[],bytes[],uint256)")[:4]
var swapSelector = [4]byte{0x9c, 0x03, 0x60, 0x14}

// EncodeSwapMulti builds swap() calldata for on-chain execution.
// swap() takes 5 array params + uint256 minOutput.
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
