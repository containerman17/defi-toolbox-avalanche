package statedb

// EVMPool — Fixed pool of long-lived EVM executors with persistent JUMPDEST cache.
//
// Each executor has its own CallerContract whose jumpdests map accumulates
// JUMPDEST analysis across all calls. Router, pool, and token contract bytecodes
// are scanned once and cached for the lifetime of the executor. This saves ~15%
// CPU compared to using AccountRef (which gets a fresh jumpdests map every call).
//
// The pool is a buffered channel — exactly N executors, exactly N calls in flight.
// Grab one to execute, return it when done. No semaphores, no WaitGroups needed
// for concurrency control.
//
// Usage:
//
//	pool := NewEVMPool(runtime.NumCPU(), evmCtx, baseState)
//	ret, gas, err := pool.Execute(from, to, calldata)
//
// For parallel warm-up (benchmark pass 1), fan out goroutines that each call
// pool.Execute() — the channel naturally limits concurrency to N.

import (
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// EVMExecutor is a long-lived EVM worker.
// The caller's jumpdests map grows over the executor's lifetime —
// contract bytecodes are scanned once and cached forever.
type EVMExecutor struct {
	caller *vm.Contract
	cs     *CallState
}

// EVMPool manages a fixed set of reusable EVM executors.
type EVMPool struct {
	executors chan *EVMExecutor
	ctx       *CachedContext
}

// NewEVMPool creates a pool of size executors backed by the given state.
// size should be runtime.NumCPU() or similar.
func NewEVMPool(size int, ctx *CachedContext, base *StateDB) *EVMPool {
	if size < 1 {
		size = 1
	}
	p := &EVMPool{
		executors: make(chan *EVMExecutor, size),
		ctx:       ctx,
	}
	dummySender := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	for i := 0; i < size; i++ {
		exec := &EVMExecutor{
			caller: vm.NewContract(
				vm.AccountRef(dummySender),
				vm.AccountRef(dummySender),
				uint256.NewInt(0),
				0,
			),
			cs: NewCallState(base),
		}
		p.executors <- exec
	}
	return p
}

// Execute runs an EVM call on the next available executor.
// Blocks if all executors are busy (natural backpressure).
// The executor's CallState is Reset between calls; the CallerContract
// (JUMPDEST cache) persists across calls.
func (p *EVMPool) Execute(from, to common.Address, data []byte) ([]byte, uint64, error) {
	exec := <-p.executors
	defer func() { p.executors <- exec }()

	exec.cs.Reset()

	txCtx := vm.TxContext{
		Origin:   from,
		GasPrice: p.ctx.gasPrice,
	}
	precompiles := vm.ActivePrecompiles(p.ctx.rules)
	exec.cs.Prepare(p.ctx.rules, from, p.ctx.blockCtx.Coinbase, &to, precompiles, nil)

	evm := vm.NewEVM(p.ctx.blockCtx, txCtx, exec.cs, p.ctx.chainCfg, vm.Config{})

	gasLimit := uint64(5_000_000)
	ret, gasLeft, err := evm.Call(exec.caller, to, data, gasLimit, uint256.NewInt(0))

	return ret, gasLimit - gasLeft, err
}

// ExecuteWithErr runs an EVM call and also returns any state fetch error
// that occurred during execution (stale block, network failure).
func (p *EVMPool) ExecuteWithErr(from, to common.Address, data []byte) ([]byte, uint64, error, error) {
	exec := <-p.executors
	defer func() { p.executors <- exec }()

	exec.cs.Reset()

	txCtx := vm.TxContext{
		Origin:   from,
		GasPrice: p.ctx.gasPrice,
	}
	precompiles := vm.ActivePrecompiles(p.ctx.rules)
	exec.cs.Prepare(p.ctx.rules, from, p.ctx.blockCtx.Coinbase, &to, precompiles, nil)

	evm := vm.NewEVM(p.ctx.blockCtx, txCtx, exec.cs, p.ctx.chainCfg, vm.Config{})

	gasLimit := uint64(5_000_000)
	ret, gasLeft, err := evm.Call(exec.caller, to, data, gasLimit, uint256.NewInt(0))

	fetchErr := exec.cs.Err()
	return ret, gasLimit - gasLeft, err, fetchErr
}

// UpdateContext updates the shared block context (after a new block).
func (p *EVMPool) UpdateContext(ctx *CachedContext) {
	p.ctx = ctx
}

// Size returns the pool size (number of executors).
func (p *EVMPool) Size() int {
	return cap(p.executors)
}
