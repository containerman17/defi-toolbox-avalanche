package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"runtime"

	lc "defi-toolbox/lightclient"

	corethcore "github.com/ava-labs/avalanchego/graft/coreth/core"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
)

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	blockNum := flag.Uint64("block", 0, "block number to debug")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	flag.Parse()

	if *blockNum == 0 {
		fmt.Fprintln(os.Stderr, "usage: debug-gas --block <number>")
		os.Exit(1)
	}

	pool, err := lc.NewRPCPool(*rpcURL, *concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	chainCfg, err := lc.FetchChainConfig(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain config: %v\n", err)
		os.Exit(1)
	}

	fetcher := lc.NewBlockFetcher(pool)
	state := lc.NewVersionedState()

	bd, err := fetcher.GetBlock(*blockNum)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch block: %v\n", err)
		os.Exit(1)
	}

	block := lc.BlockDataToTypesBlock(bd)
	miss := fetcher.MissCallbacks(state)
	sv := lc.NewStateView(state, *blockNum-1, miss)

	header := block.Header()
	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}

	getHash := func(n uint64) common.Hash {
		h, _ := fetcher.GetBlockHash(n)
		return h
	}
	blockCtx := lc.BuildBlockCtx(header, chainCfg, getHash)

	gp := new(corethcore.GasPool).AddGas(header.GasLimit)
	signer := types.MakeSigner(chainCfg, header.Number, header.Time)

	for txIndex, tx := range block.Transactions() {
		msg, err := corethcore.TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tx %d: message: %v\n", txIndex, err)
			continue
		}

		sv.SetTxContext(tx.Hash(), txIndex)

		gpBefore := gp.Gas()
		evm := vm.NewEVM(blockCtx, corethcore.NewEVMTxContext(msg), sv, chainCfg, vm.Config{})
		result, err := corethcore.ApplyMessage(evm, msg, gp)
		gpAfter := gp.Gas()

		gasUsed := gpBefore - gpAfter

		receiptGas := getReceiptGas(pool, tx.Hash())

		status := "ok"
		if err != nil {
			status = fmt.Sprintf("err: %v", err)
		} else if result.Failed() {
			status = fmt.Sprintf("reverted: %v", result.Err)
		}

		match := "MATCH"
		if gasUsed != receiptGas && receiptGas > 0 {
			match = fmt.Sprintf("DIFF=%d", int64(gasUsed)-int64(receiptGas))
		}

		fmt.Printf("tx %d hash=%s gas_local=%d gas_chain=%d %s [%s]\n",
			txIndex, tx.Hash().Hex()[:14], gasUsed, receiptGas, match, status)
	}
}

func getReceiptGas(pool *lc.RPCPool, txHash common.Hash) uint64 {
	raw, err := pool.Call("eth_getTransactionReceipt", []interface{}{txHash.Hex()})
	if err != nil {
		return 0
	}
	var receipt struct {
		GasUsed string `json:"gasUsed"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return 0
	}
	var gas uint64
	fmt.Sscanf(receipt.GasUsed, "0x%x", &gas)
	return gas
}
