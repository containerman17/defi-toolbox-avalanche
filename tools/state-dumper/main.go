// state-dumper: Fetches full contract storage via debug_storageRangeAt and saves to flat JSON files.
//
// Usage:
//   state-dumper -block 0x4C8A3E0 -rpc http://localhost:9650/ext/bc/C/rpc -out ./state-cache -j 32 < contracts.txt
//
// contracts.txt: one contract address per line (0x...)
// Output: ./state-cache/<block>/<address>.json  (flat map of slot -> value)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	block := flag.String("block", "", "block number in hex (e.g. 0x4C8A3E0) — required")
	rpcURL := flag.String("rpc", "http://localhost:9650/ext/bc/C/rpc", "RPC URL")
	outDir := flag.String("out", "./state-cache", "output directory")
	concurrency := flag.Int("j", 32, "number of parallel fetches")
	flag.Parse()

	if *block == "" {
		log.Fatal("-block is required (hex block number)")
	}

	// Resolve block hash
	blockHash := resolveBlockHash(*rpcURL, *block)
	if blockHash == "" {
		log.Fatal("failed to resolve block hash")
	}
	log.Printf("block %s -> hash %s", *block, blockHash)

	// Read contract addresses from stdin
	var contracts []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "0x") {
			continue
		}
		contracts = append(contracts, strings.ToLower(line))
	}
	if len(contracts) == 0 {
		log.Fatal("no contracts provided on stdin")
	}
	log.Printf("dumping %d contracts with concurrency %d", len(contracts), *concurrency)

	// Create output directory
	outPath := filepath.Join(*outDir, *block)
	os.MkdirAll(outPath, 0755)

	// Skip contracts that already have a cache file
	var todo []string
	for _, addr := range contracts {
		path := filepath.Join(outPath, addr+".json")
		if _, err := os.Stat(path); err == nil {
			continue // already cached
		}
		todo = append(todo, addr)
	}
	log.Printf("%d already cached, %d to fetch", len(contracts)-len(todo), len(todo))

	// Process in parallel
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	var done atomic.Int64
	total := len(todo)
	start := time.Now()

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	for _, addr := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(addr string) {
			defer wg.Done()
			defer func() { <-sem }()

			slots, err := dumpContract(client, *rpcURL, blockHash, addr)
			if err != nil {
				log.Printf("FAIL %s: %v", addr, err)
				return
			}

			path := filepath.Join(outPath, addr+".json")
			data, _ := json.Marshal(slots)
			os.WriteFile(path, data, 0644)

			n := done.Add(1)
			if n%100 == 0 || n == int64(total) {
				elapsed := time.Since(start)
				rate := float64(n) / elapsed.Seconds()
				eta := time.Duration(float64(int64(total)-n) / rate * float64(time.Second))
				log.Printf("progress: %d/%d (%.1f/sec, ETA %s) — last: %s (%d slots)",
					n, total, rate, eta.Round(time.Second), addr, len(slots))
			}
		}(addr)
	}
	wg.Wait()

	elapsed := time.Since(start)
	log.Printf("done: %d contracts in %s (%.1f/sec)", done.Load(), elapsed.Round(time.Millisecond), float64(done.Load())/elapsed.Seconds())
}

func dumpContract(client *http.Client, rpcURL, blockHash, addr string) (map[string]string, error) {
	slots := make(map[string]string)
	startKey := "0x0000000000000000000000000000000000000000000000000000000000000000"

	for {
		reqBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "debug_storageRangeAt",
			"params":  []any{blockHash, 0, addr, startKey, 1024},
			"id":      1,
		})

		httpResp, err := client.Post(rpcURL, "application/json", strings.NewReader(string(reqBody)))
		if err != nil {
			return nil, fmt.Errorf("RPC error: %w", err)
		}
		respBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()

		var resp struct {
			Result *storageRangeResult `json:"result"`
			Error  json.RawMessage     `json:"error"`
		}
		if err := json.Unmarshal(respBody, &resp); err != nil {
			return nil, fmt.Errorf("parse error: %w (body: %s)", err, string(respBody[:min(200, len(respBody))]))
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("RPC error: %s", string(resp.Error))
		}
		if resp.Result == nil {
			return nil, fmt.Errorf("null result")
		}

		for _, entry := range resp.Result.Storage {
			slots[strings.ToLower(entry.Key)] = entry.Value
		}

		if resp.Result.NextKey == "" {
			break
		}
		startKey = resp.Result.NextKey
	}

	return slots, nil
}

type storageRangeResult struct {
	Storage map[string]struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"storage"`
	NextKey string `json:"nextKey"`
}

func resolveBlockHash(rpcURL, blockNum string) string {
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getBlockByNumber",
		"params":  []any{blockNum, false},
		"id":      1,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	httpResp, err := client.Post(rpcURL, "application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		return ""
	}
	defer httpResp.Body.Close()
	respBody, _ := io.ReadAll(httpResp.Body)

	var resp struct {
		Result map[string]any `json:"result"`
	}
	json.Unmarshal(respBody, &resp)
	if hash, ok := resp.Result["hash"].(string); ok {
		return hash
	}
	return ""
}
