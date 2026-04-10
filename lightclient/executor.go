package lightclient

import (
	"fmt"
	"math/big"
	"strings"

	corethcore "github.com/ava-labs/avalanchego/graft/coreth/core"
	cparams "github.com/ava-labs/avalanchego/graft/coreth/params"
	avaxatomic "github.com/ava-labs/avalanchego/graft/coreth/plugin/evm/atomic"
	ccustomtypes "github.com/ava-labs/avalanchego/graft/coreth/plugin/evm/customtypes"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
)

// MainnetAVAXAssetID is the CB58-encoded asset ID for AVAX on mainnet.
const mainnetAVAXAssetID = "FvwEAhmxKfeiG8SnEvq42hc6whRyY3EFYAvebMqDNDGCgxN5Z"

var snowCtx *snow.Context

func init() {
	cparams.RegisterExtras()
	ccustomtypes.Register()

	avaxAssetID, err := ids.FromString(mainnetAVAXAssetID)
	if err != nil {
		panic(fmt.Sprintf("invalid AVAX asset ID: %v", err))
	}
	snowCtx = &snow.Context{AVAXAssetID: avaxAssetID}
}

// ─── BlockDiff ─────────────────────────────────────────────────────
// Captures all state changes produced by executing a block.

type BlockDiff struct {
	Storage  map[common.Address]map[common.Hash]common.Hash
	Balances map[common.Address]*uint256.Int
	Nonces   map[common.Address]uint64
	Code     map[common.Address][]byte
}

// ─── CallMsg ───────────────────────────────────────────────────────
// Parameters for a simulated call (eth_call equivalent).

type CallMsg struct {
	From     common.Address
	To       *common.Address
	Gas      uint64
	GasPrice *big.Int
	Value    *big.Int
	Data     []byte
}

// GetHashFunc is an alias for vm.GetHashFunc — callback for the BLOCKHASH
// opcode. Caller provides this so the executor does not need direct access
// to historical block hashes.
type GetHashFunc = vm.GetHashFunc

// ─── Helper: build vm.BlockContext ──────────────────────────────────

// BuildBlockCtx constructs a vm.BlockContext from a block header.
func BuildBlockCtx(header *types.Header, chainCfg *params.ChainConfig, getHash GetHashFunc) vm.BlockContext {
	return buildBlockContext(header, chainCfg, getHash)
}

func buildBlockContext(header *types.Header, chainCfg *params.ChainConfig, getHash GetHashFunc) vm.BlockContext {
	rules := chainCfg.Rules(header.Number, cparams.IsMergeTODO, header.Time)

	blockDifficulty := new(big.Int)
	if header.Difficulty != nil {
		blockDifficulty.Set(header.Difficulty)
	}
	blockRandom := header.MixDigest
	if rules.IsShanghai {
		blockRandom.SetBytes(blockDifficulty.Bytes())
		blockDifficulty = new(big.Int)
	}

	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	} else {
		baseFee = new(big.Int).Set(baseFee)
	}

	return vm.BlockContext{
		CanTransfer: func(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
			return db.GetBalance(addr).Cmp(amount) >= 0
		},
		Transfer: func(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
			db.SubBalance(sender, amount)
			db.AddBalance(recipient, amount)
		},
		GetHash:     getHash,
		Coinbase:    header.Coinbase,
		BlockNumber: new(big.Int).Set(header.Number),
		Time:        header.Time,
		Difficulty:  blockDifficulty,
		Random:      &blockRandom,
		GasLimit:    header.GasLimit,
		BaseFee:     baseFee,
		Header:      header,
	}
}

// ─── ExecuteBlock ──────────────────────────────────────────────────
// Executes all transactions in a block sequentially against the provided
// StateView. Returns the accumulated state diff.
//
// The caller is responsible for:
//   - Providing a StateView pinned at (blockNum - 1)
//   - Setting Avalanche header extras on the block header before calling
//     (TimeMilliseconds, MinDelayExcess via ccustomtypes.SetHeaderExtra)
//   - Providing a GetHashFunc for the BLOCKHASH opcode
//   - Providing the chain config (fetch via eth_getChainConfig or hardcode)

func ExecuteBlock(
	block *types.Block,
	state *StateView,
	chainCfg *params.ChainConfig,
	getHash GetHashFunc,
) (*BlockDiff, error) {
	header := block.Header()
	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}

	blockCtx := buildBlockContext(header, chainCfg, getHash)
	signer := types.MakeSigner(chainCfg, header.Number, header.Time)

	gp := new(corethcore.GasPool).AddGas(header.GasLimit)

	for txIndex, tx := range block.Transactions() {
		msg, err := corethcore.TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			return nil, fmt.Errorf("block %d tx %d: message conversion: %w", header.Number.Uint64(), txIndex, err)
		}

		state.SetTxContext(tx.Hash(), txIndex)

		evm := vm.NewEVM(blockCtx, corethcore.NewEVMTxContext(msg), state, chainCfg, vm.Config{})
		_, err = corethcore.ApplyMessage(evm, msg, gp)
		if err != nil {
			// Pre-check failures (insufficient funds, nonce mismatch) can happen
			// when the sender's balance or nonce was modified by a platform-level
			// operation (staking rewards, atomic txs in prior blocks we missed).
			// Fetch the real pre-block state from RPC and retry once.
			if state.miss.OnBalance != nil && (strings.Contains(err.Error(), "insufficient funds") || strings.Contains(err.Error(), "nonce too")) {
				state.PrimeBalance(msg.From, state.miss.OnBalance(msg.From, state.Block()))
				state.PrimeNonce(msg.From, state.miss.OnNonce(msg.From, state.Block()))
				evm = vm.NewEVM(blockCtx, corethcore.NewEVMTxContext(msg), state, chainCfg, vm.Config{})
				_, err = corethcore.ApplyMessage(evm, msg, gp)
			}
			if err != nil {
				return nil, fmt.Errorf("block %d tx %d: apply failed: %w", header.Number.Uint64(), txIndex, err)
			}
		}
		// Snapshot the overlay as committed state for the next tx.
		// This is needed for GetCommittedState to return correct pre-tx values,
		// which affects SSTORE gas/refund calculations (EIP-2200/EIP-3529).
		state.CommitTx()
	}

	// Apply atomic transactions (cross-chain imports/exports from block extra data).
	// These directly credit/debit AVAX balances outside of normal EVM transactions.
	extData := ccustomtypes.BlockExtData(block)
	if len(extData) > 0 {
		rules := chainCfg.Rules(header.Number, cparams.IsMergeTODO, header.Time)
		isAP5 := false
		if rulesExtra := cparams.GetRulesExtra(rules); rulesExtra != nil {
			isAP5 = rulesExtra.AvalancheRules.IsApricotPhase5
		}

		atomicTxs, err := avaxatomic.ExtractAtomicTxs(extData, isAP5, avaxatomic.Codec)
		if err != nil {
			return nil, fmt.Errorf("block %d: extract atomic txs: %w", header.Number.Uint64(), err)
		}

		for i, tx := range atomicTxs {
			if err := tx.UnsignedAtomicTx.EVMStateTransfer(snowCtx, state); err != nil {
				return nil, fmt.Errorf("block %d atomic tx %d: state transfer: %w", header.Number.Uint64(), i, err)
			}
		}
	}

	// Extract diffs from the StateView's overlay. We use the overlay (not the
	// dirty tracker) because the dirty tracker doesn't undo on revert — reverted
	// tx writes would leak into the diff. The overlay IS the final state.
	return &BlockDiff{
		Storage:  state.StorageOverrides(),
		Balances: state.BalanceOverrides(),
		Nonces:   state.NonceOverrides(),
		Code:     state.CodeOverrides(),
	}, nil
}

// ─── Call ──────────────────────────────────────────────────────────
// Executes a single call against the provided StateView without modifying
// the underlying versioned state. The StateView's overlay absorbs all writes.
//
// Returns the raw return data, gas used, and any execution error.

func Call(
	msg CallMsg,
	state *StateView,
	header *types.Header,
	chainCfg *params.ChainConfig,
	getHash GetHashFunc,
) ([]byte, uint64, error) {
	blockCtx := buildBlockContext(header, chainCfg, getHash)

	gas := msg.Gas
	if gas == 0 {
		gas = 50_000_000 // default gas cap for calls
	}

	gasPrice := msg.GasPrice
	if gasPrice == nil {
		gasPrice = new(big.Int)
	}

	// GasFeeCap must be >= BaseFee or the EVM rejects the call.
	// Use the block's BaseFee — same as what eth_call does.
	gasFeeCap := new(big.Int).Set(gasPrice)
	if header.BaseFee != nil && gasFeeCap.Cmp(header.BaseFee) < 0 {
		gasFeeCap.Set(header.BaseFee)
	}

	value := msg.Value
	if value == nil {
		value = new(big.Int)
	}

	coreMsg := &corethcore.Message{
		From:              msg.From,
		To:                msg.To,
		Nonce:             state.GetNonce(msg.From),
		Value:             value,
		GasLimit:          gas,
		GasPrice:          gasPrice,
		GasFeeCap:         gasFeeCap,
		GasTipCap:         new(big.Int),
		Data:              msg.Data,
		AccessList:        nil,
		SkipAccountChecks: true,
	}

	gp := new(corethcore.GasPool).AddGas(gas)

	evm := vm.NewEVM(blockCtx, corethcore.NewEVMTxContext(coreMsg), state, chainCfg, vm.Config{})
	result, err := corethcore.ApplyMessage(evm, coreMsg, gp)
	if err != nil {
		return nil, 0, fmt.Errorf("call failed: %w", err)
	}

	if result.Failed() {
		return result.ReturnData, result.UsedGas, result.Err
	}

	return result.ReturnData, result.UsedGas, nil
}
