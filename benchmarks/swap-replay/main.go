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
	poolcollector "defi-toolbox/tools/pool-collector"

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
	transferTopic      = strings.ToLower("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	woofiRouter        = common.HexToAddress("0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7")
	wooPPAddress       = common.HexToAddress("0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4")
	wooPPV2Address     = common.HexToAddress("0xaba7ed514217d51630053d73d358ac2502d3f9bb")
	balancerV2Vault    = common.HexToAddress("0xba12222222228d8ba445958a75a0704d566bf2c8")
	cavalrePool        = common.HexToAddress("0x5f1e8ed8468232bab71eda9f4598bda3161f48ea")
	bentoBox           = common.HexToAddress("0x0711b6026068f736bae6b213031fce978d48e026")
	customFeeProviders = map[string]string{
		"oliveswap": "fee=9980",
		"vapordex":  "fee=9971",
		"lydia":     "fee=9980",
		"hakuswap":  "fee=9980",
		"thorus":    "fee=9990",
	}
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
//     half-buffered flows, Balancer V2 vault, WooPP/WooPP_V2, Cavalre,
//     Trident/BentoBox, ERC4626 wrap/unwrap.
//   - Custom per-provider extraData / fee encodings for replayed swap() calls.
//   - Synthetic catalog helpers from TS: ERC4626 vault injection, generated
//     Balancer buffered edges, and trace-discovered obscure pools missing from
//     normal discovery.
//   - Single-path hop extraction from transfers, including multi-swap pools that
//     must pair in/out by log index and flash-swap pools that require token-chain
//     ordering instead of raw log ordering.
//   - Route repair helpers: missing head bridge into hop 0, missing tail hops to
//     the output token, direct trace-detected pool bridges, Balancer+ERC4626
//     tail paths, unwrap+AMM tails, and short 2-hop tails.
//   - Diamond / parallel-path detection: convert an apparently single route into
//     split replay when the transfer graph fans out and rejoins.
//   - Split-step extraction and chaining: per-transfer split steps, V4-aware
//     split detection, upstream/downstream token-flow ownership checks, sibling
//     downstream reuse, duplicate-step suppression, and final output validation.
//   - Flat split replay through one router swap() call so shared pool state
//     carries across all steps, plus the old retry orderings
//     (original/proportional/topological/greedy).
//   - Router replay plumbing from TS test harness: router bytecode injection,
//     token/hook overrides, single-route replay, split-route flat replay, and
//     TRANSFER_FROM per-step overrides.

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

type traceCall struct {
	Error string      `json:"error"`
	Logs  []traceLog  `json:"logs"`
	Calls []traceCall `json:"calls"`
}

type traceLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type transferEvent struct {
	Token    common.Address
	From     common.Address
	To       common.Address
	Amount   *big.Int
	LogIndex int
}

type poolHop struct {
	Pool     common.Address
	TokenIn  common.Address
	TokenOut common.Address
}

type resolvedStep struct {
	Step     pf.RouteStep
	Provider string
}

type replayOutcome struct {
	ExpectedOut *big.Int
	ActualOut   *big.Int
	Steps       []resolvedStep
	Reason      string
}

type traceReplay struct {
	ChainTx     *chainTx
	Sender      common.Address
	InputToken  common.Address
	OutputToken common.Address
	ExpectedOut *big.Int
	Transfers   []transferEvent
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

type poolCatalog struct {
	pools  []pf.Pool
	byAddr map[common.Address][]pf.Pool
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

	catalog := newPoolCatalog(poolcollector.EmbeddedPools(0))

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

	// Process blocks in parallel.
	workers := runtime.NumCPU() * 2
	sem := make(chan struct{}, workers)
	var printMu sync.Mutex
	var wg sync.WaitGroup
	resultsCh := make(chan txResult, len(txs))

	for _, blk := range blockOrder {
		wg.Add(1)
		sem <- struct{}{} // acquire slot
		go func(bg *blockGroup) {
			defer wg.Done()
			defer func() { <-sem }() // release slot

			clients := make(map[uint64]*lc.LightClient)
			defer closeLightClients(clients)

			for _, tx := range bg.txs {
				r := processTx(rpc, catalog, clients, *wsURL, *dataDir, tx)
				printMu.Lock()
				fmt.Print(r.Line)
				printMu.Unlock()
				resultsCh <- r
			}
		}(blockMap[blk])
	}
	wg.Wait()
	close(resultsCh)

	// Aggregate.
	origOK, origReverted := 0, 0
	routerExact, routerUnder, routerOver, routerUnsupported := 0, 0, 0, 0
	quoteExact, quoteUnder, quoteOver, quoteUnsupported := 0, 0, 0, 0
	routerPass1PPM := 0
	quotePass1PPM := 0
	for r := range resultsCh {
		if r.OrigOK {
			origOK++
		} else {
			origReverted++
		}
		if r.RouterExact {
			routerExact++
		}
		if r.RouterUnder {
			routerUnder++
		}
		if r.RouterOver {
			routerOver++
		}
		if r.RouterUnsupported {
			routerUnsupported++
		}
		if r.QuoteExact {
			quoteExact++
		}
		if r.QuoteUnder {
			quoteUnder++
		}
		if r.QuoteOver {
			quoteOver++
		}
		if r.QuoteUnsupported {
			quoteUnsupported++
		}
		if r.RouterPass1PPM {
			routerPass1PPM++
		}
		if r.QuotePass1PPM {
			quotePass1PPM++
		}
	}

	total := origOK + origReverted
	fmt.Printf("SUMMARY total=%d orig_ok=%d orig_reverted=%d router_exact=%d router_under=%d router_over=%d router_unsupported=%d router_pass_1ppm=%d/%d quote_exact=%d quote_under=%d quote_over=%d quote_unsupported=%d quote_pass_1ppm=%d/%d\n",
		total, origOK, origReverted, routerExact, routerUnder, routerOver, routerUnsupported, routerPass1PPM, total, quoteExact, quoteUnder, quoteOver, quoteUnsupported, quotePass1PPM, total)
}

func processTx(rpc *rpcClient, catalog *poolCatalog, clients map[uint64]*lc.LightClient, wsURL, dataDir string, tx replayTx) txResult {
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

	routerStatus := "UNSUPPORTED"
	routerExtra := ""
	quoteStatus := "UNSUPPORTED"
	quoteExtra := ""

	if oracleOut == nil {
		r.RouterUnsupported = true
		r.QuoteUnsupported = true
		routerExtra = fmt.Sprintf(" routerReason=undecoded_return(%d bytes)", len(ret))
		quoteExtra = fmt.Sprintf(" quoteReason=undecoded_return(%d bytes)", len(ret))
	} else {
		traced, traceErr := traceOriginalSwap(rpc, tx)
		if traceErr != nil {
			r.RouterUnsupported = true
			routerExtra = fmt.Sprintf(" routerReason=%s", traceErr.Error())
		} else {
			outcome, routeErr := replaySingleRoute(rpc, catalog, clients, wsURL, dataDir, tx, traced, oracleOut)
			if routeErr != nil {
				r.RouterUnsupported = true
				routerExtra = fmt.Sprintf(" routerReason=%s", routeErr.Error())
			} else {
				route := formatRoute(outcome.Steps, rpc)
				switch outcome.ActualOut.Cmp(oracleOut) {
				case 0:
					routerStatus = "MATCH"
					r.RouterExact = true
				case -1:
					routerStatus = "UNDER"
					r.RouterUnder = true
				default:
					routerStatus = "OVER"
					r.RouterOver = true
				}
				if withinOnePPMOrBetter(outcome.ActualOut, oracleOut) {
					r.RouterPass1PPM = true
				}
				routerExtra = fmt.Sprintf(" router=%s expected=%s actual=%s hops=%d route=%s",
					routerStatus,
					formatAmount(oracleOut, outMeta),
					formatAmount(outcome.ActualOut, outMeta),
					len(outcome.Steps),
					route,
				)
			}
		}

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
					quoteStatus = "MATCH"
					r.QuoteExact = true
				case -1:
					quoteStatus = "UNDER"
					r.QuoteUnder = true
				default:
					quoteStatus = "OVER"
					r.QuoteOver = true
				}
				if withinOnePPMOrBetter(quoted.ActualOut, oracleOut) {
					r.QuotePass1PPM = true
				}
				quoteExtra = fmt.Sprintf(" quote=%s expected=%s quoted=%s actual=%s hops=%d route=%s",
					quoteStatus,
					formatAmount(oracleOut, outMeta),
					formatAmount(quoted.QuotedOut, outMeta),
					formatAmount(quoted.ActualOut, outMeta),
					len(quoted.Route.Steps),
					quoteRoute,
				)
			}
		}
	}

	r.Line = fmt.Sprintf("%s block=%d orig=OK in=%s amountIn=%s out=%s simulated=%s%s%s\n",
		tx.Hash,
		tx.Block,
		tokenLabel(tx.Summary.InputToken, inMeta),
		formatAmount(tx.Summary.AmountIn, inMeta),
		tokenLabel(tx.Summary.OutputToken, outMeta),
		formatAmount(oracleOut, outMeta),
		routerExtra,
		quoteExtra,
	)
	return r
}

func traceOriginalSwap(rpc *rpcClient, tx replayTx) (*traceReplay, error) {
	chainTx, err := rpc.TransactionByHash(tx.Hash)
	if err != nil {
		return nil, fmt.Errorf("tx: %w", err)
	}
	parentBlock := tx.Block - 1

	trace, err := rpc.DebugTraceCall(chainTx, parentBlock)
	if err != nil {
		return nil, fmt.Errorf("trace: %w", err)
	}
	if trace.Error != "" {
		return nil, fmt.Errorf("trace reverted: %s", trace.Error)
	}

	traceLogs := collectTraceLogs(*trace)
	transfers := parseTraceTransfers(traceLogs)
	if len(transfers) == 0 {
		return nil, fmt.Errorf("no transfer events")
	}

	sender := common.HexToAddress(chainTx.From)
	inputToken := normalizeRouteToken(tx.Summary.InputToken)
	outputToken := normalizeRouteToken(tx.Summary.OutputToken)
	expectedOut := replayOutputAmount(transfers, outputToken, sender, lfjRouter.Address)
	if expectedOut.Sign() == 0 {
		return nil, fmt.Errorf("replay produced zero output")
	}

	return &traceReplay{
		ChainTx:     chainTx,
		Sender:      sender,
		InputToken:  inputToken,
		OutputToken: outputToken,
		ExpectedOut: expectedOut,
		Transfers:   transfers,
	}, nil
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

func replaySingleRoute(rpc *rpcClient, catalog *poolCatalog, clients map[uint64]*lc.LightClient, wsURL, dataDir string, tx replayTx, traced *traceReplay, oracleOut *big.Int) (*replayOutcome, error) {
	parentBlock := tx.Block - 1

	exclude := map[common.Address]bool{
		traced.Sender:     true,
		lfjRouter.Address: true,
	}
	if detectSplit(traced.Transfers, exclude, catalog) {
		return nil, fmt.Errorf("split route")
	}

	hops := extractPoolHops(traced.Transfers, catalog)
	if len(hops) == 0 {
		return nil, fmt.Errorf("no pool hops")
	}
	if normalizeRouteToken(hops[0].TokenIn) != traced.InputToken {
		return nil, fmt.Errorf("missing head bridge: %s->%s", traced.InputToken.Hex()[:10], hops[0].TokenIn.Hex()[:10])
	}
	if normalizeRouteToken(hops[len(hops)-1].TokenOut) != traced.OutputToken {
		return nil, fmt.Errorf("route ends at %s", hops[len(hops)-1].TokenOut.Hex()[:10])
	}

	steps, err := resolveSteps(catalog, hops)
	if err != nil {
		return nil, err
	}

	client, err := lightClientForBlock(clients, wsURL, dataDir, parentBlock)
	if err != nil {
		return nil, fmt.Errorf("lightclient: %w", err)
	}

	actualOut, err := replayRouteOnLightClient(client, steps, traced.InputToken, tx.Summary.AmountIn)
	if err != nil {
		probe := probeSingleRouteSteps(client, steps, tx.Summary.AmountIn)
		return nil, fmt.Errorf("router replay: %w; route=%s; probe=%s", err, formatRoute(steps, rpc), probe)
	}

	return &replayOutcome{
		ExpectedOut: oracleOut,
		ActualOut:   actualOut,
		Steps:       steps,
	}, nil
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

func newPoolCatalog(pools []pf.Pool) *poolCatalog {
	c := &poolCatalog{
		pools:  pools,
		byAddr: make(map[common.Address][]pf.Pool),
	}
	for _, pool := range pools {
		c.byAddr[pool.Address] = append(c.byAddr[pool.Address], pool)
	}
	return c
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
		Quiet:      true,
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

func replayRouteOnLightClient(client *lc.LightClient, steps []resolvedStep, inputToken common.Address, amountIn *big.Int) (*big.Int, error) {
	amountU256, overflow := uint256.FromBig(amountIn)
	if overflow || amountU256.IsZero() {
		return nil, fmt.Errorf("invalid amountIn")
	}

	pools := make([]common.Address, len(steps))
	poolTypes := make([]int, len(steps))
	tokenPairs := make([]common.Address, 0, len(steps)*2)
	extraDatas := make([]string, len(steps))
	overrideTokens := []common.Address{inputToken}

	for i, step := range steps {
		pools[i] = step.Step.Pool
		poolTypes[i] = step.Step.PoolType
		tokenPairs = append(tokenPairs, step.Step.TokenIn, step.Step.TokenOut)
		extraDatas[i] = step.Step.ExtraData
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

func probeSingleRouteSteps(client *lc.LightClient, steps []resolvedStep, amountIn *big.Int) string {
	current := new(big.Int).Set(amountIn)
	for i, step := range steps {
		out, err := replayRouteOnLightClient(client, []resolvedStep{step}, step.Step.TokenIn, current)
		if err != nil {
			debugOut, debugErr := debugSwapSingleOnLightClient(client, step, current)
			debugInfo := ""
			if debugErr != nil {
				debugInfo = fmt.Sprintf("; debugSwapSingle=%s", debugErr.Error())
			} else {
				debugInfo = fmt.Sprintf("; debugSwapSingleOut=%s", debugOut.String())
			}
			return fmt.Sprintf("hop=%d/%d %s %s->%s amountIn=%s err=%s",
				i+1,
				len(steps),
				step.Provider,
				step.Step.TokenIn.Hex()[:10],
				step.Step.TokenOut.Hex()[:10],
				current.String(),
				err.Error(),
			) + debugInfo
		}
		current = out
	}
	return fmt.Sprintf("all-single-hops-ok final=%s", current.String())
}

func debugSwapSingleOnLightClient(client *lc.LightClient, step resolvedStep, amountIn *big.Int) (*big.Int, error) {
	amountU256, overflow := uint256.FromBig(amountIn)
	if overflow || amountU256.IsZero() {
		return nil, fmt.Errorf("invalid amountIn")
	}

	calldata := pf.EncodeSwapSingleWithExtra(
		step.Step.Pool,
		step.Step.PoolType,
		step.Step.TokenIn,
		step.Step.TokenOut,
		amountU256,
		step.Step.ExtraData,
	)
	to := backrunRouter
	overrideTokens := []common.Address{step.Step.TokenIn}
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

func resolveSteps(catalog *poolCatalog, hops []poolHop) ([]resolvedStep, error) {
	steps := make([]resolvedStep, 0, len(hops))
	for _, hop := range hops {
		step, ok := catalog.findStep(hop.Pool, normalizeRouteToken(hop.TokenIn), normalizeRouteToken(hop.TokenOut))
		if !ok {
			return nil, fmt.Errorf("missing pool %s %s->%s", hop.Pool.Hex()[:10], hop.TokenIn.Hex()[:10], hop.TokenOut.Hex()[:10])
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func (c *poolCatalog) isKnownPool(addr common.Address) bool {
	if len(c.byAddr[addr]) > 0 {
		return true
	}
	switch addr {
	case wooPPAddress, wooPPV2Address, balancerV2Vault, pf.V4PoolManager, cavalrePool, bentoBox:
		return true
	default:
		return false
	}
}

func (c *poolCatalog) findStep(addr, tokenIn, tokenOut common.Address) (resolvedStep, bool) {
	if step, ok := c.findDirectStep(addr, tokenIn, tokenOut); ok {
		return step, true
	}

	switch addr {
	case wooPPAddress:
		for _, pool := range c.pools {
			if pool.PoolType == 5 && pool.Address == woofiRouter && poolHasTokens(pool, tokenIn, tokenOut) {
				return resolvedStep{
					Step:     pf.RouteStep{Pool: pool.Address, PoolType: pool.PoolType, TokenIn: tokenIn, TokenOut: tokenOut, ExtraData: pool.ExtraData},
					Provider: pool.Dex,
				}, true
			}
		}
	case wooPPV2Address:
		return resolvedStep{
			Step:     pf.RouteStep{Pool: wooPPV2Address, PoolType: 14, TokenIn: tokenIn, TokenOut: tokenOut},
			Provider: "woopp_v2",
		}, true
	case balancerV2Vault:
		for _, pool := range c.pools {
			if pool.PoolType == 16 && poolHasTokens(pool, tokenIn, tokenOut) {
				return resolvedStep{
					Step:     pf.RouteStep{Pool: pool.Address, PoolType: pool.PoolType, TokenIn: tokenIn, TokenOut: tokenOut, ExtraData: pool.ExtraData},
					Provider: pool.Dex,
				}, true
			}
		}
	case pf.V4PoolManager:
		for _, pool := range c.pools {
			if pool.PoolType == 9 && poolHasTokens(pool, tokenIn, tokenOut) {
				return resolvedStep{
					Step:     pf.RouteStep{Pool: pool.Address, PoolType: pool.PoolType, TokenIn: tokenIn, TokenOut: tokenOut, ExtraData: pool.ExtraData},
					Provider: pool.Dex,
				}, true
			}
		}
	case cavalrePool:
		for _, pool := range c.pools {
			if pool.PoolType == 17 && poolHasTokens(pool, tokenIn, tokenOut) {
				return resolvedStep{
					Step:     pf.RouteStep{Pool: pool.Address, PoolType: pool.PoolType, TokenIn: tokenIn, TokenOut: tokenOut, ExtraData: pool.ExtraData},
					Provider: pool.Dex,
				}, true
			}
		}
	case bentoBox:
		for _, pool := range c.pools {
			if pool.PoolType == 20 && poolHasTokens(pool, tokenIn, tokenOut) {
				return resolvedStep{
					Step:     pf.RouteStep{Pool: pool.Address, PoolType: pool.PoolType, TokenIn: tokenIn, TokenOut: tokenOut, ExtraData: pool.ExtraData},
					Provider: pool.Dex,
				}, true
			}
		}
	}

	return resolvedStep{}, false
}

func (c *poolCatalog) findDirectStep(addr, tokenIn, tokenOut common.Address) (resolvedStep, bool) {
	for _, pool := range c.byAddr[addr] {
		if !poolHasTokens(pool, tokenIn, tokenOut) {
			continue
		}
		extra := pool.ExtraData
		if pool.PoolType == 8 && extra == "" {
			if fee, ok := customFeeProviders[pool.Dex]; ok {
				extra = fee
			}
		}
		return resolvedStep{
			Step: pf.RouteStep{
				Pool:      pool.Address,
				PoolType:  pool.PoolType,
				TokenIn:   tokenIn,
				TokenOut:  tokenOut,
				ExtraData: extra,
			},
			Provider: pool.Dex,
		}, true
	}
	return resolvedStep{}, false
}

func poolHasTokens(pool pf.Pool, tokenIn, tokenOut common.Address) bool {
	hasIn := false
	hasOut := false
	altIn := tokenIn
	altOut := tokenOut
	if tokenIn == wavaxAddress {
		altIn = zeroAddress
	}
	if tokenOut == wavaxAddress {
		altOut = zeroAddress
	}
	for _, token := range pool.Tokens {
		if token == tokenIn || token == altIn {
			hasIn = true
		}
		if token == tokenOut || token == altOut {
			hasOut = true
		}
	}
	return hasIn && hasOut
}

func normalizeRouteToken(token common.Address) common.Address {
	if token == zeroAddress {
		return wavaxAddress
	}
	return token
}

func replayOutputAmount(transfers []transferEvent, outputToken, user, router common.Address) *big.Int {
	sum := new(big.Int)
	for _, transfer := range transfers {
		if transfer.Token == outputToken && transfer.To == user {
			sum.Add(sum, transfer.Amount)
		}
	}
	if sum.Sign() > 0 {
		return sum
	}
	if outputToken == wavaxAddress {
		for _, transfer := range transfers {
			if transfer.Token == wavaxAddress && transfer.To == router {
				sum.Add(sum, transfer.Amount)
			}
		}
	}
	return sum
}

func collectTraceLogs(call traceCall) []traceLog {
	logs := make([]traceLog, 0, len(call.Logs))
	logs = append(logs, call.Logs...)
	for _, sub := range call.Calls {
		logs = append(logs, collectTraceLogs(sub)...)
	}
	return logs
}

func parseTraceTransfers(traceLogs []traceLog) []transferEvent {
	transfers := make([]transferEvent, 0)
	for i, log := range traceLogs {
		if len(log.Topics) < 3 || strings.ToLower(log.Topics[0]) != transferTopic {
			continue
		}
		transfers = append(transfers, transferEvent{
			Token:    common.HexToAddress(log.Address),
			From:     common.HexToAddress(log.Topics[1]),
			To:       common.HexToAddress(log.Topics[2]),
			Amount:   mustParseBigHex(log.Data),
			LogIndex: i,
		})
	}
	return transfers
}

func parseReceiptTransfers(logs []logEntry) []transferEvent {
	transfers := make([]transferEvent, 0)
	for _, log := range logs {
		if len(log.Topics) < 3 || strings.ToLower(log.Topics[0]) != transferTopic {
			continue
		}
		transfers = append(transfers, transferEvent{
			Token:    common.HexToAddress(log.Address),
			From:     common.HexToAddress(log.Topics[1]),
			To:       common.HexToAddress(log.Topics[2]),
			Amount:   mustParseBigHex(log.Data),
			LogIndex: int(parseHexUint64(log.LogIndex)),
		})
	}
	sort.Slice(transfers, func(i, j int) bool { return transfers[i].LogIndex < transfers[j].LogIndex })
	return transfers
}

func detectSplit(transfers []transferEvent, exclude map[common.Address]bool, catalog *poolCatalog) bool {
	senders := make(map[common.Address]bool)
	for _, transfer := range transfers {
		senders[transfer.From] = true
	}

	fanOut := make(map[string]map[common.Address]bool)
	for _, transfer := range transfers {
		if exclude[transfer.From] || exclude[transfer.To] {
			continue
		}
		if transfer.From == zeroAddress {
			continue
		}
		if !senders[transfer.To] && transfer.To != pf.V4PoolManager {
			continue
		}
		if !catalog.isKnownPool(transfer.From) && !catalog.isKnownPool(transfer.To) {
			continue
		}

		key := transfer.From.Hex() + ":" + transfer.Token.Hex()
		if fanOut[key] == nil {
			fanOut[key] = make(map[common.Address]bool)
		}
		fanOut[key][transfer.To] = true
		if len(fanOut[key]) >= 2 {
			return true
		}
	}

	return false
}

func extractPoolHops(transfers []transferEvent, catalog *poolCatalog) []poolHop {
	multiSwapPools := map[common.Address]bool{
		wooPPAddress:    true,
		wooPPV2Address:  true,
		balancerV2Vault: true,
		cavalrePool:     true,
	}

	addresses := make(map[common.Address]bool)
	for _, transfer := range transfers {
		addresses[transfer.From] = true
		addresses[transfer.To] = true
	}

	hops := make([]poolHop, 0)
	seen := make(map[string]bool)
	for addr := range addresses {
		if addr == zeroAddress || addr == pf.V4PoolManager {
			continue
		}
		if !catalog.isKnownPool(addr) {
			continue
		}

		incoming := make([]transferEvent, 0)
		outgoing := make([]transferEvent, 0)
		for _, transfer := range transfers {
			if transfer.To == addr && transfer.From != zeroAddress {
				incoming = append(incoming, transfer)
			}
			if transfer.From == addr && transfer.To != zeroAddress {
				outgoing = append(outgoing, transfer)
			}
		}
		if len(incoming) == 0 || len(outgoing) == 0 {
			continue
		}

		if multiSwapPools[addr] && len(incoming) > 1 && len(outgoing) > 1 {
			sort.Slice(incoming, func(i, j int) bool { return incoming[i].LogIndex < incoming[j].LogIndex })
			sort.Slice(outgoing, func(i, j int) bool { return outgoing[i].LogIndex < outgoing[j].LogIndex })
			pairCount := len(incoming)
			if len(outgoing) < pairCount {
				pairCount = len(outgoing)
			}
			for i := 0; i < pairCount; i++ {
				if incoming[i].Token == outgoing[i].Token {
					continue
				}
				key := addr.Hex() + incoming[i].Token.Hex() + outgoing[i].Token.Hex() + fmt.Sprintf(":%d", i)
				if seen[key] {
					continue
				}
				seen[key] = true
				hops = append(hops, poolHop{Pool: addr, TokenIn: incoming[i].Token, TokenOut: outgoing[i].Token})
			}
			continue
		}

		tokenIn := incoming[0].Token
		tokenOut := outgoing[0].Token
		if tokenIn == tokenOut {
			continue
		}
		key := addr.Hex() + tokenIn.Hex() + tokenOut.Hex()
		if seen[key] {
			continue
		}
		seen[key] = true
		hops = append(hops, poolHop{Pool: addr, TokenIn: tokenIn, TokenOut: tokenOut})
	}

	if len(hops) <= 1 {
		return hops
	}

	produced := make(map[common.Address]bool)
	for _, hop := range hops {
		produced[normalizeRouteToken(hop.TokenOut)] = true
	}
	starts := make([]poolHop, 0)
	for _, hop := range hops {
		if !produced[normalizeRouteToken(hop.TokenIn)] {
			starts = append(starts, hop)
		}
	}
	if len(starts) == 0 {
		starts = hops
	}

	best := make([]poolHop, 0)
	for _, start := range starts {
		chain := []poolHop{start}
		used := map[string]bool{start.Pool.Hex() + start.TokenIn.Hex() + start.TokenOut.Hex(): true}
		current := start
		for len(chain) < len(hops) {
			found := false
			for _, candidate := range hops {
				key := candidate.Pool.Hex() + candidate.TokenIn.Hex() + candidate.TokenOut.Hex()
				if used[key] {
					continue
				}
				if normalizeRouteToken(candidate.TokenIn) != normalizeRouteToken(current.TokenOut) {
					continue
				}
				chain = append(chain, candidate)
				used[key] = true
				current = candidate
				found = true
				break
			}
			if !found {
				break
			}
		}
		if len(chain) > len(best) {
			best = chain
		}
		if len(best) == len(hops) {
			return best
		}
	}
	return best
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

func formatRoute(steps []resolvedStep, rpc *rpcClient) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		inMeta := rpc.TokenMeta(step.Step.TokenIn)
		outMeta := rpc.TokenMeta(step.Step.TokenOut)
		parts = append(parts, fmt.Sprintf("%s(%s→%s)", step.Provider, tokenLabel(step.Step.TokenIn, inMeta), tokenLabel(step.Step.TokenOut, outMeta)))
	}
	return strings.Join(parts, " -> ")
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

func (c *rpcClient) DebugTraceCall(tx *chainTx, block uint64) (*traceCall, error) {
	params := map[string]interface{}{
		"from": tx.From,
		"to":   tx.To,
		"data": tx.Input,
		"gas":  "0x1C9C380",
	}
	if tx.Value != "" && tx.Value != "0x0" {
		params["value"] = tx.Value
	}
	raw, err := c.Call("debug_traceCall", []interface{}{
		params,
		hexUint64(block),
		map[string]interface{}{
			"tracer": "callTracer",
			"tracerConfig": map[string]interface{}{
				"withLog": true,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	var trace traceCall
	if err := json.Unmarshal(raw, &trace); err != nil {
		return nil, err
	}
	return &trace, nil
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
