package pathfinder

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// executeSwap selector: keccak256("executeSwap(address[],uint8[],address[],uint256[],bytes[])")[:4]
var executeSwapSelector = [4]byte{0x32, 0x39, 0x33, 0x4d}

// EncodeSwapSingle builds executeSwap calldata for a single-pool swap.
// This is the Go equivalent of the JS encodeSwapSingle function.
func EncodeSwapSingle(pool common.Address, poolType int, tokenIn, tokenOut common.Address, amountIn *uint256.Int) []byte {
	// ABI layout for executeSwap(address[], uint8[], address[], uint256[], bytes[]):
	// selector (4 bytes)
	// 5 offset words (5 * 32 = 160 bytes)
	// pools array:      length(1) + pool(1)             = 64 bytes
	// poolTypes array:  length(1) + type(1)             = 64 bytes
	// tokens array:     length(2) + tokenIn + tokenOut  = 96 bytes
	// amountsIn array:  length(1) + amount(1)            = 64 bytes
	// extraDatas array: length(1) + offset(1) + len(0)  = 96 bytes

	data := make([]byte, 4+160+64+64+96+64+96) // 548 bytes total

	// Selector
	copy(data[0:4], executeSwapSelector[:])

	// Offsets (each relative to start of params, i.e. after selector)
	writeWord(data, 4+0*32, big.NewInt(160))   // pools offset
	writeWord(data, 4+1*32, big.NewInt(224))   // poolTypes offset
	writeWord(data, 4+2*32, big.NewInt(288))   // tokens offset
	writeWord(data, 4+3*32, big.NewInt(384))   // amountsIn offset
	writeWord(data, 4+4*32, big.NewInt(448))   // extraDatas offset

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

	// extraDatas array: [1, offset_to_first_element, length_of_first_element(0)]
	writeWord(data, pos, big.NewInt(1))
	pos += 32
	writeWord(data, pos, big.NewInt(32)) // offset to first bytes element
	pos += 32
	writeWord(data, pos, big.NewInt(0)) // length = 0 (empty bytes)

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
