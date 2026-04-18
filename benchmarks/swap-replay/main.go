package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	routercontracts "defi-toolbox/contracts"
	lc "defi-toolbox/lightclient"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/quoter"
	_ "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

const (
	defaultLimit     = 10
	defaultRPCURL    = "http://localhost:9650/ext/bc/C/rpc"
	defaultWSURL     = "ws://127.0.0.1:9650/ext/bc/C/ws"
	defaultChunkSize = uint64(50000)
)

var (
	dummySender        = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	backrunRouter      = common.HexToAddress("0x000000000000000000000000cafebabe00facade")
	zeroAddress        = common.Address{}
	wavaxAddress       = common.HexToAddress("0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7")
)

// TODO(route-replay): port the archived TypeScript same-route replay logic into
// this Go benchmark. Source of truth:
//   - archive/benchmarks/swap-replay-legacy/02_convert.ts
//   - archive/benchmarks/swap-replay-legacy/03_test.ts
//
// Edge cases still to carry over:
//   - Router detection from receipts, not only LFJ log scanning; Odos also existed in TS.
//   - Replay the original tx via debug_traceCall at block-1 and skip backruns /
//     start-of-block reverts.
//   - Compute replayed output from traced transfers, including the WAVAX->AVAX
//     unwrap case where WAVAX lands on the router rather than the user.
//   - Bridge-equivalent token normalization: USDC.e->USDC, USDt.e->USDt, AVAX->WAVAX.
//   - RFQ handling: explicit unsimulatable-pool skip list plus Hashflow vault
//     TRANSFER_FROM simulation with token balance/allowance overrides.
//   - Special pool decoding: V4 PoolManager + poolId lookup + native wrap flag,
//     Wombat swap events, Synapse TokenSwap events, Balancer V3 buffered and
var knownTokens = map[string]tokenMeta{
	"0x0000000000000000000000000000000000000000": {Symbol: "AVAX", Name: "Avalanche", Decimals: 18, HasDecimals: true},
	"0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be": {Symbol: "sAVAX", Name: "BENQI Liquid Staked AVAX", Decimals: 18, HasDecimals: true},
	"0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab": {Symbol: "WETH.e", Name: "Wrapped Ether", Decimals: 18, HasDecimals: true},
	"0x50b7545627a5162f82a992c33b87adc75187b218": {Symbol: "WBTC.e", Name: "Wrapped BTC", Decimals: 8, HasDecimals: true},
	"0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": {Symbol: "USDt", Name: "Tether USD", Decimals: 6, HasDecimals: true},
	"0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": {Symbol: "USDC.e", Name: "USD Coin", Decimals: 6, HasDecimals: true},
	"0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": {Symbol: "WAVAX", Name: "Wrapped AVAX", Decimals: 18, HasDecimals: true},
	"0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": {Symbol: "USDC", Name: "USD Coin", Decimals: 6, HasDecimals: true},
	"0xc7198437980c041c805a1edcba50c1ce5db95118": {Symbol: "USDt.e", Name: "Tether USD", Decimals: 6, HasDecimals: true},
}

var lfjRouter = routerDef{
	Name:      "lfj",
	Address:   common.HexToAddress("0x45a62b090df48243f12a21897e7ed91863e2c86b"),
	SwapTopic: "0xd9a8cfa901e597f6bbb7ea94478cf9ad6f38d0dc3fd24d493e99cb40692e39f1",
	ParseEvent: func(data string) (*swapSummary, error) {
		if len(data) < 322 {
			return nil, fmt.Errorf("short LFJ swap data")
		}
		return &swapSummary{
			InputToken:  common.HexToAddress("0x" + data[90:130]),
			OutputToken: common.HexToAddress("0x" + data[154:194]),
			AmountIn:    mustParseBigHex(data[194:258]),
			AmountOut:   mustParseBigHex(data[258:322]),
		}, nil
	},
}

type tokenMeta struct {
	Symbol      string
	Name        string
	Decimals    uint8
	HasDecimals bool
}

type swapSummary struct {
	InputToken  common.Address
	OutputToken common.Address
	AmountIn    *big.Int
	AmountOut   *big.Int
}

type routerDef struct {
	Name       string
	Address    common.Address
	SwapTopic  string
	ParseEvent func(data string) (*swapSummary, error)
}

type rpcClient struct {
	url       string
	client    *http.Client
	nextID    int64
	metaMu    sync.Mutex
	metaCache map[common.Address]tokenMeta
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type logEntry struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	Data            string   `json:"data"`
	BlockNumber     string   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	LogIndex        string   `json:"logIndex"`
}

type replayTx struct {
	Hash     string
	Block    uint64
	LogIndex uint64
	Summary  *swapSummary
}

type chainTx struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Input string `json:"input"`
	Value string `json:"value"`
}

type chainReceipt struct {
	Status      string     `json:"status"`
	From        string     `json:"from"`
	To          string     `json:"to"`
	GasUsed     string     `json:"gasUsed"`
	BlockNumber string     `json:"blockNumber"`
	Logs        []logEntry `json:"logs"`
}

type quoteOutcome struct {
	QuotedOut *big.Int
	ActualOut *big.Int
	Route     *pf.Route
}

type txResult struct {
	Line             string
	OrigOK           bool
	RouterExact      bool
	RouterUnder      bool
	RouterOver       bool
	RouterUnsupported bool
	QuoteExact       bool
	QuoteUnder       bool
	QuoteOver        bool
	QuoteUnsupported bool
	RouterPass1PPM   bool
	QuotePass1PPM    bool
}

func main() {
	rpcURL := flag.String("rpc", defaultRPCURL, "HTTP RPC URL")
	wsURL := flag.String("ws", defaultWSURL, "WebSocket RPC URL for lightclient replay")
	dataDir := flag.String("data-dir", filepath.Join("benchmarks", "swap-replay", ".lightclient"), "lightclient snapshot directory")
	limit := flag.Int("limit", defaultLimit, "number of transactions to print (0 = all)")
	startBlock := flag.Uint64("start-block", routercontracts.DeployedBlock, "first block to scan")
	endBlock := flag.Uint64("end-block", 0, "last block to scan (0 = latest)")
	chunkSize := flag.Uint64("chunk-size", defaultChunkSize, "block range per eth_getLogs request")
	flag.Parse()

	if *chunkSize == 0 {
		fmt.Fprintf(os.Stderr, "invalid --chunk-size: 0\n")
		os.Exit(1)
	}

	rpc := &rpcClient{
		url: *rpcURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		metaCache: make(map[common.Address]tokenMeta),
	}

	txs, scannedEnd, err := fetchSwapLogs(rpc, lfjRouter, *startBlock, *endBlock, *chunkSize, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch logs: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[swap-replay] router=%s start=%d end=%d found=%d\n", lfjRouter.Name, *startBlock, scannedEnd, len(txs))

	// Group txs by block — one goroutine per block, one light client per block.
	type blockGroup struct {
		block uint64
		txs   []replayTx
	}
	blockOrder := []uint64{}
	blockMap := map[uint64]*blockGroup{}
	for _, tx := range txs {
		bg, ok := blockMap[tx.Block]
		if !ok {
			bg = &blockGroup{block: tx.Block}
			blockMap[tx.Block] = bg
			blockOrder = append(blockOrder, tx.Block)
		}
		bg.txs = append(bg.txs, tx)
	}

	// Worker pool: N workers pull blocks from a channel in order.
	numWorkers := runtime.NumCPU() * 2
	work := make(chan *blockGroup, numWorkers)
	var printMu sync.Mutex
	var wg sync.WaitGroup
	origOK, origReverted := int64(0), int64(0)
	quoteExact, quoteUnder, quoteOver, quoteUnsupported := int64(0), int64(0), int64(0), int64(0)
	quotePass1PPM := int64(0)

	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for bg := range work {
				clients := make(map[uint64]*lc.LightClient)
				for _, tx := range bg.txs {
					r := processTx(rpc, clients, *wsURL, *dataDir, tx)
					printMu.Lock()
					fmt.Print(r.Line)
					printMu.Unlock()
					if r.OrigOK {
						atomic.AddInt64(&origOK, 1)
					} else {
						atomic.AddInt64(&origReverted, 1)
					}
					if r.QuoteExact {
						atomic.AddInt64(&quoteExact, 1)
					}
					if r.QuoteUnder {
						atomic.AddInt64(&quoteUnder, 1)
					}
					if r.QuoteOver {
						atomic.AddInt64(&quoteOver, 1)
					}
					if r.QuoteUnsupported {
						atomic.AddInt64(&quoteUnsupported, 1)
					}
					if r.QuotePass1PPM {
						atomic.AddInt64(&quotePass1PPM, 1)
					}
				}
				closeLightClients(clients)
			}
		}()
	}

	for _, blk := range blockOrder {
		work <- blockMap[blk]
	}
	close(work)
	wg.Wait()

	total := origOK + origReverted
	fmt.Printf("SUMMARY total=%d orig_ok=%d orig_reverted=%d quote_exact=%d quote_under=%d quote_over=%d quote_unsupported=%d quote_pass_1ppm=%d/%d\n",
		total, origOK, origReverted, quoteExact, quoteUnder, quoteOver, quoteUnsupported, quotePass1PPM, total)
}

func processTx(rpc *rpcClient, clients map[uint64]*lc.LightClient, wsURL, dataDir string, tx replayTx) txResult {
	inMeta := rpc.TokenMeta(tx.Summary.InputToken)
	outMeta := rpc.TokenMeta(tx.Summary.OutputToken)

	ret, err := replayOriginalTx(rpc, clients, wsURL, dataDir, tx)
	if err != nil {
		return txResult{
			Line: fmt.Sprintf("%s block=%d orig=REVERT in=%s amountIn=%s out=%s reason=%s\n",
				tx.Hash,
				tx.Block,
				tokenLabel(tx.Summary.InputToken, inMeta),
				formatAmount(tx.Summary.AmountIn, inMeta),
				tokenLabel(tx.Summary.OutputToken, outMeta),
				err.Error(),
			),
		}
	}

	r := txResult{OrigOK: true}
	oracleOut := decodePositiveAmountOut(ret)

	quoteExtra := ""

	if oracleOut == nil {
		r.QuoteUnsupported = true
		quoteExtra = fmt.Sprintf(" quoteReason=undecoded_return(%d bytes)", len(ret))
	} else {
		q := quoter.New(4)
		parentBlock := tx.Block - 1
		inputToken := normalizeRouteToken(tx.Summary.InputToken)
		outputToken := normalizeRouteToken(tx.Summary.OutputToken)
		client, err := lightClientForBlock(clients, wsURL, dataDir, parentBlock)
		if err != nil {
			r.QuoteUnsupported = true
			quoteExtra = fmt.Sprintf(" quoteReason=lightclient: %s", err.Error())
		} else {
			quoted, quoteErr := blindQuotePair(q, client, inputToken, outputToken, tx.Summary.AmountIn)
			if quoteErr != nil {
				r.QuoteUnsupported = true
				quoteExtra = fmt.Sprintf(" quoteReason=%s", quoteErr.Error())
			} else {
				quoteRoute := formatQuoteRoute(quoted.Route, rpc)
				switch quoted.ActualOut.Cmp(oracleOut) {
				case 0:
					r.QuoteExact = true
				case -1:
					r.QuoteUnder = true
				default:
					r.QuoteOver = true
				}
				if withinOnePPMOrBetter(quoted.ActualOut, oracleOut) {
					r.QuotePass1PPM = true
				}
				quoteExtra = fmt.Sprintf(" quote=%s expected=%s quoted=%s actual=%s hops=%d route=%s",
					quoteLabel(r),
					formatAmount(oracleOut, outMeta),
					formatAmount(quoted.QuotedOut, outMeta),
					formatAmount(quoted.ActualOut, outMeta),
					len(quoted.Route.Steps),
					quoteRoute,
				)
			}
		}
	}

	r.Line = fmt.Sprintf("%s block=%d orig=OK in=%s amountIn=%s out=%s simulated=%s%s\n",
		tx.Hash,
		tx.Block,
		tokenLabel(tx.Summary.InputToken, inMeta),
		formatAmount(tx.Summary.AmountIn, inMeta),
		tokenLabel(tx.Summary.OutputToken, outMeta),
		formatAmount(oracleOut, outMeta),
		quoteExtra,
	)
	return r
}

func quoteLabel(r txResult) string {
	switch {
	case r.QuoteExact:
		return "MATCH"
	case r.QuoteUnder:
		return "UNDER"
	case r.QuoteOver:
		return "OVER"
	default:
		return "UNSUPPORTED"
	}
}

func blindQuotePair(q *quoter.Quoter, client *lc.LightClient, inputToken, outputToken common.Address, amountIn *big.Int) (*quoteOutcome, error) {
	amountU256, overflow := uint256.FromBig(amountIn)
	if overflow || amountU256.IsZero() {
		return nil, fmt.Errorf("invalid amountIn")
	}

	sv, err := client.StateView(0)
	if err != nil {
		return nil, fmt.Errorf("state view: %w", err)
	}
	blockTimestamp, err := client.BlockTimestamp(0)
	if err != nil {
		return nil, fmt.Errorf("block timestamp: %w", err)
	}

	result := q.QuotePair(sv, blockTimestamp, inputToken, outputToken, amountU256)
	if result == nil || result.Route == nil || result.Route.AmountOut == nil || result.Route.AmountOut.IsZero() {
		return nil, fmt.Errorf("no route")
	}
	actualOut, err := replayPathfinderRouteOnLightClient(client, result.Route, inputToken, amountIn)
	if err != nil {
		return nil, fmt.Errorf("quote route replay: %w", err)
	}

	return &quoteOutcome{
		QuotedOut: result.Route.AmountOut.ToBig(),
		ActualOut: actualOut,
		Route:     result.Route,
	}, nil
}

func replayPathfinderRouteOnLightClient(client *lc.LightClient, route *pf.Route, inputToken common.Address, amountIn *big.Int) (*big.Int, error) {
	if route == nil || len(route.Steps) == 0 {
		return nil, fmt.Errorf("empty route")
	}

	amountU256, overflow := uint256.FromBig(amountIn)
	if overflow || amountU256.IsZero() {
		return nil, fmt.Errorf("invalid amountIn")
	}

	pools := make([]common.Address, len(route.Steps))
	poolTypes := make([]int, len(route.Steps))
	tokenPairs := make([]common.Address, 0, len(route.Steps)*2)
	extraDatas := make([]string, len(route.Steps))
	overrideTokens := []common.Address{inputToken}

	for i, step := range route.Steps {
		pools[i] = step.Pool
		poolTypes[i] = step.PoolType
		tokenPairs = append(tokenPairs, step.TokenIn, step.TokenOut)
		extraDatas[i] = step.ExtraData
	}

	calldata := pf.EncodeSwapMulti(pools, poolTypes, tokenPairs, amountU256, extraDatas, uint256.NewInt(0))
	to := backrunRouter
	ret, _, err := client.DirectCallWithState(lc.CallMsg{
		From: dummySender,
		To:   &to,
		Data: calldata,
		Gas:  50_000_000,
	}, 0, func(sv *lc.StateView) {
		sv.AddBalance(dummySender, new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(30)))
		routercontracts.ApplyTokenOverrides(sv, dummySender, backrunRouter, overrideTokens)
	})
	if err != nil {
		if reason := decodeRevertReason(ret); reason != "" {
			return nil, fmt.Errorf("%w (%s)", err, reason)
		}
		return nil, err
	}

	out := decodeSigned256(ret)
	if out == nil {
		return nil, fmt.Errorf("empty return")
	}
	if out.Sign() < 0 {
		return nil, fmt.Errorf("negative output %s", out.String())
	}
	return out, nil
}

func formatQuoteRoute(route *pf.Route, rpc *rpcClient) string {
	if route == nil || len(route.Steps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(route.Steps))
	for _, step := range route.Steps {
		inMeta := rpc.TokenMeta(step.TokenIn)
		outMeta := rpc.TokenMeta(step.TokenOut)
		parts = append(parts, fmt.Sprintf("%s→%s", tokenLabel(step.TokenIn, inMeta), tokenLabel(step.TokenOut, outMeta)))
	}
	return strings.Join(parts, " -> ")
}

func fetchSwapLogs(rpc *rpcClient, router routerDef, startBlock, endBlock, chunkSize uint64, limit int) ([]replayTx, uint64, error) {
	if endBlock == 0 {
		latest, err := rpc.BlockNumber()
		if err != nil {
			return nil, 0, err
		}
		endBlock = latest
	}
	if endBlock < startBlock {
		return nil, 0, fmt.Errorf("end block %d is before start block %d", endBlock, startBlock)
	}

	cursor := startBlock
	seen := make(map[string]bool)
	var txs []replayTx

	for cursor <= endBlock && (limit == 0 || len(txs) < limit) {
		chunkEnd := cursor + chunkSize - 1
		if chunkEnd < cursor || chunkEnd > endBlock {
			chunkEnd = endBlock
		}

		logs, err := rpc.GetLogs(router.Address, router.SwapTopic, cursor, chunkEnd)
		if err != nil {
			return nil, chunkEnd, err
		}

		for _, log := range logs {
			hash := strings.ToLower(log.TransactionHash)
			if seen[hash] {
				continue
			}
			summary, err := router.ParseEvent(log.Data)
			if err != nil {
				continue
			}
			block := parseHexUint64(log.BlockNumber)
			logIndex := parseHexUint64(log.LogIndex)
			seen[hash] = true
			txs = append(txs, replayTx{
				Hash:     hash,
				Block:    block,
				LogIndex: logIndex,
				Summary:  summary,
			})
			if limit > 0 && len(txs) >= limit {
				break
			}
		}

		if chunkEnd == math.MaxUint64 {
			break
		}
		cursor = chunkEnd + 1
	}

	sort.Slice(txs, func(i, j int) bool {
		if txs[i].Block != txs[j].Block {
			return txs[i].Block < txs[j].Block
		}
		if txs[i].LogIndex != txs[j].LogIndex {
			return txs[i].LogIndex < txs[j].LogIndex
		}
		return txs[i].Hash < txs[j].Hash
	})

	return txs, endBlock, nil
}

func closeLightClients(clients map[uint64]*lc.LightClient) {
	for _, client := range clients {
		client.Close()
	}
}

func replayOriginalTx(rpc *rpcClient, clients map[uint64]*lc.LightClient, wsURL, dataDir string, tx replayTx) ([]byte, error) {
	chainTx, err := rpc.TransactionByHash(tx.Hash)
	if err != nil {
		return nil, fmt.Errorf("tx: %w", err)
	}
	if chainTx.To == "" {
		return nil, fmt.Errorf("contract creation")
	}

	parentBlock := tx.Block - 1
	client, err := lightClientForBlock(clients, wsURL, dataDir, parentBlock)
	if err != nil {
		return nil, fmt.Errorf("lightclient: %w", err)
	}

	to := common.HexToAddress(chainTx.To)
	ret, _, err := client.DirectCallWithState(lc.CallMsg{
		From:  common.HexToAddress(chainTx.From),
		To:    &to,
		Data:  common.FromHex(chainTx.Input),
		Value: mustParseBigHex(chainTx.Value),
		Gas:   50_000_000,
	}, 0, nil)
	if err != nil {
		if reason := decodeRevertReason(ret); reason != "" {
			return nil, fmt.Errorf("%w (%s)", err, reason)
		}
		return nil, err
	}
	return ret, nil
}

func decodePositiveAmountOut(ret []byte) *big.Int {
	out := decodeSigned256(ret)
	if out == nil || out.Sign() < 0 {
		return nil
	}
	return out
}

func withinOnePPMOrBetter(actual, expected *big.Int) bool {
	if actual == nil || expected == nil {
		return false
	}
	if actual.Cmp(expected) >= 0 {
		return true
	}
	lhs := new(big.Int).Mul(new(big.Int).Set(actual), big.NewInt(1_000_000))
	rhs := new(big.Int).Mul(new(big.Int).Set(expected), big.NewInt(999_999))
	return lhs.Cmp(rhs) >= 0
}

func lightClientForBlock(cache map[uint64]*lc.LightClient, wsURL, dataDir string, block uint64) (*lc.LightClient, error) {
	if client, ok := cache[block]; ok {
		return client, nil
	}
	client, err := lc.New(lc.Config{
		RPCURL:     wsURL,
		DataDir:    dataDir,
		FixedBlock: block,
	})
	if err != nil {
		return nil, err
	}
	if err := client.Start(context.Background()); err != nil {
		client.Close()
		return nil, err
	}
	cache[block] = client
	return client, nil
}

func normalizeRouteToken(token common.Address) common.Address {
	if token == zeroAddress {
		return wavaxAddress
	}
	return token
}


func decodeSigned256(data []byte) *big.Int {
	if len(data) == 0 {
		return nil
	}
	if len(data) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(data):], data)
		data = padded
	}
	data = data[len(data)-32:]
	n := new(big.Int).SetBytes(data)
	if data[0]&0x80 != 0 {
		mod := new(big.Int).Lsh(big.NewInt(1), 256)
		n.Sub(n, mod)
	}
	return n
}

func decodeRevertReason(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	switch {
	case bytes.Equal(data[:4], []byte{0x08, 0xc3, 0x79, 0xa0}):
		if len(data) < 4+32+32 {
			return "Error(?)"
		}
		offset := int(binary.BigEndian.Uint64(data[28:36]))
		start := 4 + offset
		if start+32 > len(data) {
			return "Error(?)"
		}
		size := int(binary.BigEndian.Uint64(data[start+24 : start+32]))
		msgStart := start + 32
		msgEnd := msgStart + size
		if size < 0 || msgEnd > len(data) {
			return "Error(?)"
		}
		return fmt.Sprintf("Error(%q)", string(data[msgStart:msgEnd]))
	case bytes.Equal(data[:4], []byte{0x4e, 0x48, 0x7b, 0x71}):
		if len(data) < 36 {
			return "Panic(?)"
		}
		code := binary.BigEndian.Uint64(data[28:36])
		return fmt.Sprintf("Panic(0x%x)", code)
	default:
		return fmt.Sprintf("revert(0x%x)", data[:4])
	}
}

func (c *rpcClient) BlockNumber() (uint64, error) {
	raw, err := c.Call("eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, err
	}
	return parseHexUint64(result), nil
}

func (c *rpcClient) GetLogs(address common.Address, topic string, fromBlock, toBlock uint64) ([]logEntry, error) {
	raw, err := c.Call("eth_getLogs", []interface{}{
		map[string]interface{}{
			"address":   address.Hex(),
			"topics":    []string{topic},
			"fromBlock": hexUint64(fromBlock),
			"toBlock":   hexUint64(toBlock),
		},
	})
	if err != nil {
		return nil, err
	}
	var logs []logEntry
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

func (c *rpcClient) TransactionByHash(hash string) (*chainTx, error) {
	raw, err := c.Call("eth_getTransactionByHash", []interface{}{hash})
	if err != nil {
		return nil, err
	}
	var tx chainTx
	if err := json.Unmarshal(raw, &tx); err != nil {
		return nil, err
	}
	if tx.From == "" || tx.To == "" || tx.Input == "" {
		return nil, fmt.Errorf("missing tx fields")
	}
	return &tx, nil
}

func (c *rpcClient) TransactionReceipt(hash string) (*chainReceipt, error) {
	raw, err := c.Call("eth_getTransactionReceipt", []interface{}{hash})
	if err != nil {
		return nil, err
	}
	var receipt chainReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, err
	}
	if receipt.BlockNumber == "" {
		return nil, fmt.Errorf("missing receipt")
	}
	return &receipt, nil
}

func (c *rpcClient) Call(method string, params interface{}) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	reqBody, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	return rpcResp.Result, nil
}

func (c *rpcClient) TokenMeta(addr common.Address) tokenMeta {
	if meta, ok := knownTokens[strings.ToLower(addr.Hex())]; ok {
		return meta
	}

	c.metaMu.Lock()
	if meta, ok := c.metaCache[addr]; ok {
		c.metaMu.Unlock()
		return meta
	}
	c.metaMu.Unlock()

	meta := tokenMeta{}
	if symbol, err := c.callStringMethod(addr, "0x95d89b41"); err == nil {
		meta.Symbol = symbol
	}
	if name, err := c.callStringMethod(addr, "0x06fdde03"); err == nil {
		meta.Name = name
	}
	if decimals, err := c.callUint8Method(addr, "0x313ce567"); err == nil {
		meta.Decimals = decimals
		meta.HasDecimals = true
	}

	c.metaMu.Lock()
	c.metaCache[addr] = meta
	c.metaMu.Unlock()
	return meta
}

func (c *rpcClient) callStringMethod(addr common.Address, calldata string) (string, error) {
	raw, err := c.Call("eth_call", []interface{}{
		map[string]string{
			"to":   addr.Hex(),
			"data": calldata,
		},
		"latest",
	})
	if err != nil {
		return "", err
	}

	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result == "" || result == "0x" {
		return "", fmt.Errorf("empty result")
	}
	decoded := decodeStringLike(result)
	if decoded == "" {
		return "", fmt.Errorf("unparseable string result")
	}
	return decoded, nil
}

func (c *rpcClient) callUint8Method(addr common.Address, calldata string) (uint8, error) {
	raw, err := c.Call("eth_call", []interface{}{
		map[string]string{
			"to":   addr.Hex(),
			"data": calldata,
		},
		"latest",
	})
	if err != nil {
		return 0, err
	}

	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, err
	}
	if result == "" || result == "0x" {
		return 0, fmt.Errorf("empty result")
	}
	value := mustParseBigHex(result)
	if !value.IsUint64() {
		return 0, fmt.Errorf("invalid uint8 result")
	}
	return uint8(value.Uint64()), nil
}

func tokenLabel(addr common.Address, meta tokenMeta) string {
	normalized := strings.ToLower(addr.Hex())
	fallback, hasFallback := knownTokens[normalized]
	switch {
	case meta.Symbol != "" && meta.Name != "" && !strings.EqualFold(meta.Symbol, meta.Name):
		return fmt.Sprintf("%s[%s]", meta.Symbol, meta.Name)
	case meta.Symbol != "":
		return meta.Symbol
	case meta.Name != "":
		return meta.Name
	case hasFallback && fallback.Symbol != "":
		return fallback.Symbol
	default:
		return normalized
	}
}

func formatAmount(amount *big.Int, meta tokenMeta) string {
	if amount == nil {
		return "0"
	}
	if !meta.HasDecimals {
		return amount.String()
	}
	return formatUnits(amount, int(meta.Decimals))
}

func formatUnits(amount *big.Int, decimals int) string {
	if amount == nil {
		return "0"
	}
	if decimals <= 0 {
		return amount.String()
	}

	sign := ""
	if amount.Sign() < 0 {
		sign = "-"
	}

	digits := new(big.Int).Abs(amount).String()
	if len(digits) <= decimals {
		frac := strings.Repeat("0", decimals-len(digits)) + digits
		frac = strings.TrimRight(frac, "0")
		if frac == "" {
			return sign + "0"
		}
		return sign + "0." + frac
	}

	intPart := digits[:len(digits)-decimals]
	fracPart := strings.TrimRight(digits[len(digits)-decimals:], "0")
	if fracPart == "" {
		return sign + intPart
	}
	return sign + intPart + "." + fracPart
}

func mustParseBigHex(hex string) *big.Int {
	normalized := strings.TrimPrefix(hex, "0x")
	if normalized == "" {
		return new(big.Int)
	}
	n := new(big.Int)
	n.SetString(normalized, 16)
	return n
}

func parseHexUint64(hex string) uint64 {
	value := mustParseBigHex(hex)
	if !value.IsUint64() {
		return 0
	}
	return value.Uint64()
}

func hexUint64(v uint64) string {
	return fmt.Sprintf("0x%x", v)
}

func decodeStringLike(result string) string {
	data := common.FromHex(result)
	if len(data) == 0 {
		return ""
	}
	if len(data) == 32 {
		return sanitizeString(bytes.TrimRight(data, "\x00"))
	}
	if len(data) >= 64 {
		offset := int(new(big.Int).SetBytes(data[:32]).Int64())
		if offset >= 0 && offset+32 <= len(data) {
			length := int(new(big.Int).SetBytes(data[offset : offset+32]).Int64())
			start := offset + 32
			end := start + length
			if length >= 0 && end <= len(data) {
				return sanitizeString(data[start:end])
			}
		}
	}
	return sanitizeString(bytes.TrimRight(data, "\x00"))
}

func sanitizeString(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
