package main

import (
	"fmt"
	"time"

	"defi-toolbox/formulas"
	"defi-toolbox/pathfinder"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func main() {
	pool := common.HexToAddress("0x0e0100ab771e9288e0aa97e11557e6654c3a9665")
	tokenIn := common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7")
	tokenOut := common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e")
	amountIn := uint256.NewInt(1_000_000_000_000_000_000)

	n := 100000

	// 1. EncodeSwapSingle
	t0 := time.Now()
	var calldata []byte
	for i := 0; i < n; i++ {
		calldata = pathfinder.EncodeSwapSingle(pool, 8, tokenIn, tokenOut, amountIn)
	}
	fmt.Printf("EncodeSwapSingle:    %6dns/call\n", time.Since(t0).Nanoseconds()/int64(n))

	// 2. TryQuote decode only (empty registry — just selector check + ABI decode)
	emptyReg := formulas.NewRegistry()
	reader := func(addr common.Address, key common.Hash) common.Hash { return common.Hash{} }
	t1 := time.Now()
	for i := 0; i < n; i++ {
		emptyReg.TryQuote(reader, calldata)
	}
	fmt.Printf("TryQuote (no match): %6dns/call\n", time.Since(t1).Nanoseconds()/int64(n))

	// 3. TryQuote with V2 formula (fake storage, returns zero reserves)
	reg := formulas.LoadEmbeddedRegistry()
	t2 := time.Now()
	for i := 0; i < n; i++ {
		reg.TryQuote(reader, calldata)
	}
	fmt.Printf("TryQuote (V2+read): %6dns/call\n", time.Since(t2).Nanoseconds()/int64(n))

	// 4. V2 formula only (no encode/decode, direct call)
	reservesSlot := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000008")
	// Fake reserves: 1000 ETH and 1M USDC
	var fakeSlot common.Hash
	// reserve0 (bytes 18-32) = 1000e18, reserve1 (bytes 4-18) = 1000000e6
	r0 := uint256.NewInt(0).Mul(uint256.NewInt(1000), uint256.NewInt(1_000_000_000_000_000_000))
	r1 := uint256.NewInt(1_000_000_000_000)
	r0bytes := r0.Bytes32()
	r1bytes := r1.Bytes32()
	copy(fakeSlot[18:32], r0bytes[18:32])
	copy(fakeSlot[4:18], r1bytes[18:32])

	fakeReader := func(addr common.Address, key common.Hash) common.Hash {
		if key == reservesSlot {
			return fakeSlot
		}
		return common.Hash{}
	}
	t3 := time.Now()
	for i := 0; i < n; i++ {
		reg.TryQuote(fakeReader, calldata)
	}
	fmt.Printf("TryQuote (V2+fake): %6dns/call\n", time.Since(t3).Nanoseconds()/int64(n))

	// 5. Raw formula only (no TryQuote overhead)
	t4 := time.Now()
	for i := 0; i < n; i++ {
		formulas.QuoteV2(fakeReader, pool, tokenIn, tokenOut, amountIn)
	}
	fmt.Printf("QuoteV2 (direct):   %6dns/call\n", time.Since(t4).Nanoseconds()/int64(n))

	_ = calldata
}

func init() {
	// Add map lookup benchmarks to main
}
