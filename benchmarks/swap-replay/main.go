package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ava-labs/libevm/common"
)

const (
	defaultLimit      = 10
	defaultRPCURL     = "http://localhost:9650/ext/bc/C/rpc"
	defaultStartBlock = uint64(80091636)
	defaultChunkSize  = uint64(50000)
)

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

func main() {
	rpcURL := flag.String("rpc", defaultRPCURL, "HTTP RPC URL")
	limit := flag.Int("limit", defaultLimit, "number of transactions to print (0 = all)")
	startBlock := flag.Uint64("start-block", defaultStartBlock, "first block to scan")
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

	for _, tx := range txs {
		inMeta := rpc.TokenMeta(tx.Summary.InputToken)
		outMeta := rpc.TokenMeta(tx.Summary.OutputToken)
		fmt.Printf("%s block=%d in=%s amountIn=%s out=%s amountOut=%s\n",
			tx.Hash,
			tx.Block,
			tokenLabel(tx.Summary.InputToken, inMeta),
			formatAmount(tx.Summary.AmountIn, inMeta),
			tokenLabel(tx.Summary.OutputToken, outMeta),
			formatAmount(tx.Summary.AmountOut, outMeta),
		)
	}
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

func (c *rpcClient) Call(method string, params interface{}) (json.RawMessage, error) {
	c.nextID++
	reqBody, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID,
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
