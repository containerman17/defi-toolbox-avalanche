package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/libevm/common"
)

func parseU256(s string) *big.Int {
	if strings.HasPrefix(s, "0x") {
		v := new(big.Int)
		v.SetString(s[2:], 16)
		return v
	}
	v := new(big.Int)
	v.SetString(s, 10)
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

func fetchBlockInfo(rpcURL string, blockNum uint64) BlockInfo {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params":  []interface{}{fmt.Sprintf("0x%x", blockNum), false},
	}
	jsonData, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(rpcURL, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		panic(fmt.Sprintf("fetch block info: %v", err))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var rpcResp struct {
		Result struct {
			Number       string `json:"number"`
			Timestamp    string `json:"timestamp"`
			BaseFeePerGas string `json:"baseFeePerGas"`
			GasLimit     string `json:"gasLimit"`
		} `json:"result"`
	}
	json.Unmarshal(body, &rpcResp)

	number, _ := strconv.ParseUint(strings.TrimPrefix(rpcResp.Result.Number, "0x"), 16, 64)
	timestamp, _ := strconv.ParseUint(strings.TrimPrefix(rpcResp.Result.Timestamp, "0x"), 16, 64)
	basefee, _ := strconv.ParseUint(strings.TrimPrefix(rpcResp.Result.BaseFeePerGas, "0x"), 16, 64)
	gasLimit, _ := strconv.ParseUint(strings.TrimPrefix(rpcResp.Result.GasLimit, "0x"), 16, 64)

	return BlockInfo{
		Number:    number,
		Timestamp: timestamp,
		Basefee:   basefee,
		GasLimit:  gasLimit,
	}
}

func fetchLatestBlockNumber(rpcURL string) uint64 {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_blockNumber",
		"params":  []interface{}{},
	}
	jsonData, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(rpcURL, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		panic(fmt.Sprintf("fetch block number: %v", err))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var rpcResp struct {
		Result string `json:"result"`
	}
	json.Unmarshal(body, &rpcResp)

	num, _ := strconv.ParseUint(strings.TrimPrefix(rpcResp.Result, "0x"), 16, 64)
	return num
}

func main() {
	args := os.Args
	if len(args) >= 2 && args[1] == "parity" {
		parityTest()
		return
	}
	if len(args) < 2 || args[1] != "quote" {
		fmt.Fprintln(os.Stderr, "usage: quoting-go {quote|parity} ...")
		os.Exit(1)
	}

	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: quoting-go quote <token_overrides_path> [block_number]")
		os.Exit(1)
	}

	tokenOverridesPath := args[2]
	wsURL := os.Getenv("WS_RPC_URL")
	if wsURL == "" {
		wsURL = "ws://127.0.0.1:9650/ext/bc/C/ws"
	}
	// HTTP URL for block info fetches (derived from WS URL or from RPC_URL env)
	httpURL := os.Getenv("RPC_URL")
	if httpURL == "" {
		httpURL = strings.Replace(strings.Replace(wsURL, "ws://", "http://", 1), "wss://", "https://", 1)
		httpURL = strings.Replace(httpURL, "/ws", "/rpc", 1)
	}

	tokenOverrides := loadTokenOverrides(tokenOverridesPath)

	// Load quoter bytecode
	execPath, _ := os.Executable()
	execDir := filepath.Dir(execPath)
	bytecodePath := filepath.Join(execDir, "..", "quoting", "data", "quoter_bytecode.hex")
	// Also try relative to working directory
	if _, err := os.Stat(bytecodePath); err != nil {
		bytecodePath = filepath.Join(".", "..", "quoting", "data", "quoter_bytecode.hex")
	}
	if _, err := os.Stat(bytecodePath); err != nil {
		// Try from the quoting-go directory itself
		bytecodePath = filepath.Join(filepath.Dir(os.Args[0]), "..", "quoting", "data", "quoter_bytecode.hex")
	}

	bytecodeHex, err := os.ReadFile(bytecodePath)
	if err != nil {
		// Last resort: try absolute path relative to parent
		wd, _ := os.Getwd()
		bytecodePath = filepath.Join(wd, "..", "quoting", "data", "quoter_bytecode.hex")
		bytecodeHex, err = os.ReadFile(bytecodePath)
		if err != nil {
			panic(fmt.Sprintf("read bytecode from %s: %v", bytecodePath, err))
		}
	}
	bytecodeClean := strings.TrimSpace(string(bytecodeHex))
	bytecodeClean = strings.TrimPrefix(bytecodeClean, "0x")
	contractCode, err := hex.DecodeString(bytecodeClean)
	if err != nil {
		panic(fmt.Sprintf("bad bytecode hex: %v", err))
	}
	fmt.Fprintf(os.Stderr, "loaded quoter bytecode (%d bytes)\n", len(contractCode))

	// Determine block number
	var blockNum uint64
	if len(args) > 3 {
		blockNum, _ = strconv.ParseUint(args[3], 10, 64)
	} else {
		blockNum = fetchLatestBlockNumber(httpURL)
	}

	// Fetch block info
	blockInfo := fetchBlockInfo(httpURL, blockNum)
	fmt.Fprintf(os.Stderr, "block %d (basefee: %d gwei, gas_limit: %d)\n",
		blockInfo.Number, blockInfo.Basefee/1_000_000_000, blockInfo.GasLimit)

	// Connect WebSocket pool for state fetching
	wsClient := NewWSClient(wsURL)
	defer wsClient.Close()
	fmt.Fprintf(os.Stderr, "connected to %s (%d connections)\n", wsURL, wsPoolSize)

	// Create shared state cache and EVM
	stateCache := NewSharedStateCache(wsClient, blockInfo.Number)
	evm := NewLocalEVM(stateCache, blockInfo.Number, blockInfo.Timestamp, blockInfo.Basefee, blockInfo.GasLimit)

	// Send ready
	ready := map[string]interface{}{"op": "ready", "block": blockInfo.Number}
	readyJSON, _ := json.Marshal(ready)
	fmt.Println(string(readyJSON))

	// Main loop
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0), 10*1024*1024) // 10MB buffer for large pool lists
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			errResp, _ := json.Marshal(map[string]interface{}{
				"op": "error", "msg": fmt.Sprintf("invalid json: %v", err),
			})
			fmt.Println(string(errResp))
			continue
		}

		op, _ := msg["op"].(string)

		switch op {
		case "quote_with_pools":
			id := uint64(0)
			if v, ok := msg["id"].(float64); ok {
				id = uint64(v)
			}
			start := time.Now()

			// Parse pools
			poolsStr, _ := msg["pools"].(string)
			edges := parsePools(poolsStr)
			poolsByToken := buildPoolsByToken(edges)
			parseMs := float64(time.Since(start).Microseconds()) / 1000.0

			// Parse tokens and amount
			tokenInStr, _ := msg["token_in"].(string)
			tokenOutStr, _ := msg["token_out"].(string)
			if tokenInStr == "" || tokenOutStr == "" {
				errResp, _ := json.Marshal(map[string]interface{}{
					"op": "error", "id": id, "msg": "invalid token_in or token_out",
				})
				fmt.Println(string(errResp))
				continue
			}
			tokenIn := common.HexToAddress(tokenInStr)
			tokenOut := common.HexToAddress(tokenOutStr)

			amountStr, _ := msg["amount"].(string)
			amountIn := parseU256(amountStr)
			inputAvaxStr, _ := msg["input_avax"].(string)
			inputAvax := parseU256(inputAvaxStr)

			maxHops := 3
			if v, ok := msg["max_hops"].(float64); ok {
				maxHops = int(v)
			}
			quoteGasLimit := uint64(2_000_000)
			if v, ok := msg["gas_limit"].(float64); ok {
				quoteGasLimit = uint64(v)
			}
			gasAware := inputAvax.Sign() > 0
			requoteFlag := true
			if v, ok := msg["requote"].(bool); ok {
				requoteFlag = v
			}

			bfsStart := time.Now()
			result := bfsRoute(
				evm, contractCode, tokenOverrides, poolsByToken,
				tokenIn, tokenOut, amountIn, inputAvax,
				maxHops, &blockInfo, quoteGasLimit, gasAware, requoteFlag,
			)
			bfsMs := float64(time.Since(bfsStart).Microseconds()) / 1000.0
			elapsedMs := time.Since(start).Milliseconds()

			fmt.Fprintf(os.Stderr, "  timing: parse=%.1fms bfs=%.1fms total=%dms\n", parseMs, bfsMs, elapsedMs)
			routeJSON := make([]map[string]interface{}, len(result.Route))
			for i, step := range result.Route {
				routeJSON[i] = map[string]interface{}{
					"pool":       strings.ToLower(step.Pool.Hex()),
					"pool_type":  step.PoolType,
					"token_in":   strings.ToLower(step.TokenIn.Hex()),
					"token_out":  strings.ToLower(step.TokenOut.Hex()),
					"amount_in":  step.AmountIn.String(),
					"amount_out": step.AmountOut.String(),
				}
			}

			resp := map[string]interface{}{
				"op":             "result",
				"id":             id,
				"amount_out":     result.AmountOut.String(),
				"gas":            result.GasUsed,
				"evm_calls":      result.EvmCalls,
				"requote_calls":  result.RequoteCalls,
				"hops":           len(result.Route),
				"route":          routeJSON,
				"elapsed_ms":     elapsedMs,
			}
			respJSON, _ := json.Marshal(resp)
			fmt.Println(string(respJSON))

		case "set_block":
			id := uint64(0)
			if v, ok := msg["id"].(float64); ok {
				id = uint64(v)
			}
			num := uint64(0)
			if v, ok := msg["block"].(float64); ok {
				num = uint64(v)
			}
			if num > 0 {
				blockInfo = fetchBlockInfo(httpURL, num)
				evm.ResetBlock(blockInfo.Number, blockInfo.Timestamp, blockInfo.Basefee, blockInfo.GasLimit)

				resp := map[string]interface{}{
					"op":      "result",
					"id":      id,
					"number":  blockInfo.Number,
					"basefee": blockInfo.Basefee,
				}
				respJSON, _ := json.Marshal(resp)
				fmt.Println(string(respJSON))
			}

		default:
			errResp, _ := json.Marshal(map[string]interface{}{
				"op": "error", "msg": fmt.Sprintf("unknown op: %s", op),
			})
			fmt.Println(string(errResp))
		}
	}
}
