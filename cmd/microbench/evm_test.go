package main

import (
	"math/big"
	"testing"

	"defi-toolbox/pathfinder"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func setupEVMBench() (*statedb.StateDB, statedb.EVMConfig, []byte) {
	state := statedb.NewStateDB(nil)

	// Set up router with real bytecode at real deployed address
	state.SetAccount(router.DeployedRouter, uint256.NewInt(0), 0, router.RouterBytecode())

	// Set up a fake V2 pool with reserves
	poolAddr := common.HexToAddress("0x0e0100ab771e9288e0aa97e11557e6654c3a9665")
	state.SetAccount(poolAddr, uint256.NewInt(0), 0, make([]byte, 100))

	cfg := statedb.EVMConfig{
		BlockNumber: uint64(router.DeployedBlock),
		Timestamp:   1700000000,
		ChainID:     43114,
		BaseFee:     25000000000,
		GasLimit:    40000000,
	}

	tokenIn := common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7")
	tokenOut := common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e")
	calldata := pathfinder.EncodeSwapSingle(poolAddr, 8, tokenIn, tokenOut, uint256.NewInt(1_000_000_000_000_000_000))

	return state, cfg, calldata
}

func BenchmarkExecuteCallRaw(b *testing.B) {
	state, cfg, calldata := setupEVMBench()
	from := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

	// Warm
	statedb.ExecuteCall(state, cfg, from, router.DeployedRouter, calldata)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		overlay := state.NewOverlay()
		statedb.ExecuteCall(overlay, cfg, from, router.DeployedRouter, calldata)
	}
}

func BenchmarkNewBigInt(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = new(big.Int).SetUint64(uint64(router.DeployedBlock))
		_ = new(big.Int).SetUint64(25000000000)
		_ = new(big.Int).SetUint64(1)
	}
}
