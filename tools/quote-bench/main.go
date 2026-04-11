// quote-bench compares our BFS pathfinder against real on-chain aggregator
// swaps from benchmarks/swap-replay/txs.txt.
//
// For each tx it fetches the swap details (tokenIn, tokenOut, amountIn,
// actualOut) by tracing the tx at block-1, then runs our BFS quoter at the
// same pre-block state and compares.
//
// Usage: go run ./tools/quote-bench/ [--limit 50] [--rpc ws://...]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	lc "defi-toolbox/lightclient"
	"defi-toolbox/quoter"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

type txEntry struct {
	hash  string
	block uint64
}

type swapInfo struct {
	InputToken  common.Address
	OutputToken common.Address
	AmountIn    *big.Int
	AmountOut   *big.Int // what the aggregator achieved
}

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	limit := flag.Int("limit", 50, "number of txs to test (0 = all)")
	txsFile := flag.String("txs", "benchmarks/swap-replay/txs.txt", "path to txs.txt")
	flag.Parse()

	// Parse txs.txt.
	txs, err := parseTxsFile(*txsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse txs: %v\n", err)
		os.Exit(1)
	}
	if *limit > 0 && len(txs) > *limit {
		txs = txs[:*limit]
	}
	fmt.Fprintf(os.Stderr, "loaded %d txs\n", len(txs))

	// RPC pool.
	pool, err := lc.NewRPCPool(*rpcURL, *concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	fetcher := lc.NewBlockFetcher(pool)
	state := lc.NewVersionedState()

	// Quoter.
	q := quoter.New(4)
	fmt.Fprintf(os.Stderr, "loaded %d pools\n", len(q.Pools()))

	wins, losses, skipped := 0, 0, 0

	for i, tx := range txs {
		// Fetch swap details from the tx receipt + trace.
		info, err := fetchSwapInfo(pool, tx.hash, tx.block)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tx %s: skip (%v)\n", tx.hash[:10], err)
			skipped++
			continue
		}

		amountIn, overflow := uint256.FromBig(info.AmountIn)
		if overflow || amountIn.IsZero() {
			skipped++
			continue
		}

		// Skip tokens not in our adjacency graph — we can't route them.
		adj := q.Adj()
		if _, ok := adj[info.InputToken]; !ok {
			skipped++
			continue
		}
		if _, ok := adj[info.OutputToken]; !ok {
			skipped++
			continue
		}

		// Quote at block-1 (same state the aggregator saw).
		miss := fetcher.MissCallbacks(state)
		sv := lc.NewStateView(state, tx.block-1, miss)

		// Fetch block timestamp for LFJ V2 fee calculations.
		blockTs := fetchBlockTimestamp(pool, tx.block)

		start := time.Now()
		result := q.QuotePair(sv, blockTs, info.InputToken, info.OutputToken, amountIn)
		elapsed := time.Since(start)

		ourOut := new(big.Int)
		if result.Route != nil && result.Route.AmountOut != nil {
			ourOut = result.Route.AmountOut.ToBig()
		}

		won := ourOut.Cmp(info.AmountOut) >= 0
		hops := 0
		if result.Route != nil {
			hops = len(result.Route.Steps)
		}

		status := "LOST"
		if won {
			status = "WON "
			wins++
		} else {
			losses++
		}

		fmt.Printf("[%d/%d] %s %s hops=%d theirs=%s ours=%s %v\n",
			i+1, len(txs), status, tx.hash[:10], hops,
			info.AmountOut.String(), ourOut.String(), elapsed.Round(time.Millisecond))
	}

	total := wins + losses
	if total == 0 {
		fmt.Println("no comparable txs")
		return
	}
	fmt.Printf("\n--- %d won, %d lost, %d skipped | win rate: %.1f%% ---\n",
		wins, losses, skipped, float64(wins)/float64(total)*100)
}

func parseTxsFile(path string) ([]txEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var txs []txEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}
		hash := parts[0]
		block := uint64(0)
		// Parse "# block 12345" comment
		for i, p := range parts {
			if p == "block" && i+1 < len(parts) {
				block, _ = strconv.ParseUint(parts[i+1], 10, 64)
			}
		}
		txs = append(txs, txEntry{hash: hash, block: block})
	}
	return txs, scanner.Err()
}

// fetchSwapInfo extracts the swap's tokenIn, tokenOut, amountIn, amountOut
// from a transaction receipt by looking at Transfer events.
func fetchSwapInfo(pool *lc.RPCPool, txHash string, block uint64) (*swapInfo, error) {
	// Fetch receipt to get logs.
	raw, err := pool.Call("eth_getTransactionReceipt", []interface{}{txHash})
	if err != nil {
		return nil, fmt.Errorf("receipt: %w", err)
	}

	var receipt struct {
		Status string `json:"status"`
		From   string `json:"from"`
		To     string `json:"to"`
		Logs   []struct {
			Address string   `json:"address"`
			Topics  []string `json:"topics"`
			Data    string   `json:"data"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, fmt.Errorf("parse receipt: %w", err)
	}
	if receipt.Status != "0x1" {
		return nil, fmt.Errorf("reverted")
	}

	// Find Transfer events (topic0 = 0xddf252ad...)
	transferTopic := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	sender := common.HexToAddress(receipt.From)

	var firstIn, lastOut *transferEvent
	for _, log := range receipt.Logs {
		if len(log.Topics) < 3 || log.Topics[0] != transferTopic {
			continue
		}
		from := common.HexToAddress(log.Topics[1])
		to := common.HexToAddress(log.Topics[2])
		amount := new(big.Int)
		amount.SetString(strings.TrimPrefix(log.Data, "0x"), 16)
		token := common.HexToAddress(log.Address)

		te := &transferEvent{token: token, from: from, to: to, amount: amount}

		// First transfer FROM sender = input
		if from == sender && firstIn == nil {
			firstIn = te
		}
		// Last transfer TO sender = output
		if to == sender {
			lastOut = te
		}
	}

	if firstIn == nil || lastOut == nil {
		return nil, fmt.Errorf("no transfer events found")
	}

	return &swapInfo{
		InputToken:  firstIn.token,
		OutputToken: lastOut.token,
		AmountIn:    firstIn.amount,
		AmountOut:   lastOut.amount,
	}, nil
}

type transferEvent struct {
	token  common.Address
	from   common.Address
	to     common.Address
	amount *big.Int
}

func fetchBlockTimestamp(pool *lc.RPCPool, blockNum uint64) uint64 {
	raw, err := pool.Call("eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", blockNum), false})
	if err != nil {
		return 0
	}
	var block struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return 0
	}
	ts, _ := strconv.ParseUint(strings.TrimPrefix(block.Timestamp, "0x"), 16, 64)
	return ts
}

