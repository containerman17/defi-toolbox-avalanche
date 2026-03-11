package main

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
)

// LocalEVM wraps libevm's EVM for local execution
type LocalEVM struct {
	state          *LazyStateDB
	chainCfg       *params.ChainConfig
	blockNum       uint64
	blockTimestamp uint64
	baseFee        uint64
	gasLimit       uint64
}

// NewLocalEVM creates a new local EVM executor
func NewLocalEVM(cache *SharedStateCache, blockNum uint64, blockTimestamp uint64, baseFee uint64, gasLimitBlock uint64) *LocalEVM {
	return &LocalEVM{
		state:          NewLazyStateDB(cache, blockNum),
		chainCfg:       avalancheCChainConfig(),
		blockNum:       blockNum,
		blockTimestamp: blockTimestamp,
		baseFee:        baseFee,
		gasLimit:       gasLimitBlock,
	}
}

func ptrU64(v uint64) *uint64 {
	return &v
}

// avalancheCChainConfig returns the chain configuration for Avalanche C-Chain
func avalancheCChainConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:             big.NewInt(43114),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		BerlinBlock:         big.NewInt(1640340),
		LondonBlock:         big.NewInt(3308552),
		ShanghaiTime:        ptrU64(1709740800),
		CancunTime:          ptrU64(1734368400),
	}
}

// CloneForWorker creates a new LocalEVM instance with its own StateDB
// but sharing the same WSClient pool for RPC calls.
func (e *LocalEVM) CloneForWorker() *LocalEVM {
	return &LocalEVM{
		state:          NewLazyStateDB(e.state.cache, e.blockNum),
		chainCfg:       e.chainCfg,
		blockNum:       e.blockNum,
		blockTimestamp: e.blockTimestamp,
		baseFee:        e.baseFee,
		gasLimit:       e.gasLimit,
	}
}

// ResetBlock resets the state for a new block
func (e *LocalEVM) ResetBlock(blockNum uint64, blockTimestamp uint64, baseFee uint64, gasLimitBlock uint64) {
	e.blockNum = blockNum
	e.blockTimestamp = blockTimestamp
	e.baseFee = baseFee
	e.gasLimit = gasLimitBlock
	e.state.Reset(blockNum)
}

// ResetMutable resets mutable state but keeps RPC cache
func (e *LocalEVM) ResetMutable() {
	e.state.ResetMutable()
}

// SetStateOverride sets a storage override
func (e *LocalEVM) SetStateOverride(addr common.Address, slot, value common.Hash) {
	e.state.SetOverride(addr, slot, value)
}

// SetBalanceOverride sets a balance override
func (e *LocalEVM) SetBalanceOverride(addr common.Address, balance *big.Int) {
	e.state.SetBalanceOverride(addr, balance)
}

// SetBalanceOverrideU256 sets a balance override with uint256
func (e *LocalEVM) SetBalanceOverrideU256(addr common.Address, balance *uint256.Int) {
	e.state.SetBalanceOverrideU256(addr, balance)
}

// SetCode sets code at an address
func (e *LocalEVM) SetCode(addr common.Address, code []byte) {
	e.state.SetCode(addr, code)
}

// Call executes a contract call and returns (output, gasUsed, error)
func (e *LocalEVM) Call(from, to common.Address, data []byte, gasLimit uint64) ([]byte, uint64, error) {
	blockCtx := vm.BlockContext{
		CanTransfer: canTransfer,
		Transfer:    transfer,
		GetHash:     getHashFn(e.blockNum),
		Coinbase:    common.HexToAddress("0x0100000000000000000000000000000000000000"),
		BlockNumber: new(big.Int).SetUint64(e.blockNum),
		Time:        e.blockTimestamp,
		Difficulty:  big.NewInt(1),
		Random:      &common.Hash{31: 0x01}, // Right-aligned difficulty=1, mimicking coreth's OverrideNewEVMArgs
		GasLimit:    e.gasLimit,
		BaseFee:     new(big.Int).SetUint64(e.baseFee),
	}

	txCtx := vm.TxContext{
		Origin:   from,
		GasPrice: big.NewInt(0), // Zero gas price for simulation
	}

	rules := e.chainCfg.Rules(blockCtx.BlockNumber, blockCtx.Random != nil, blockCtx.Time)
	e.state.Prepare(rules, from, blockCtx.Coinbase, &to, vm.ActivePrecompiles(rules), nil)

	vmConfig := vm.Config{}
	evm := vm.NewEVM(blockCtx, txCtx, e.state, e.chainCfg, vmConfig)

	value := uint256.NewInt(0)
	caller := vm.AccountRef(from)
	ret, gasLeft, err := evm.Call(caller, to, data, gasLimit, value)

	gasUsed := gasLimit - gasLeft
	if err != nil {
		return ret, gasUsed, fmt.Errorf("evm call failed: %w", err)
	}

	return ret, gasUsed, nil
}

// RPCCalls returns the number of RPC calls made
func (e *LocalEVM) RPCCalls() int {
	return e.state.RPCCalls()
}

// ============= Helper Functions for EVM =============

func canTransfer(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
	return db.GetBalance(addr).Cmp(amount) >= 0
}

func transfer(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
	db.SubBalance(sender, amount)
	db.AddBalance(recipient, amount)
}

func getHashFn(blockNum uint64) func(n uint64) common.Hash {
	return func(n uint64) common.Hash {
		return crypto.Keccak256Hash(big.NewInt(int64(n)).Bytes())
	}
}
