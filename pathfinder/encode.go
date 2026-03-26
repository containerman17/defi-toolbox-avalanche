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
