package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var (
	listenHost    string
	listenPort    int
	upstreamWsURL string
	upstreamHTTP  string
	poolSize      int
	blockPollMs   int
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		log.Fatalf("Invalid %s: %s", key, raw)
	}
	return v
}

func init() {
	listenHost = envOrDefault("STATE_SERVER_HOST", "127.0.0.1")
	listenPort = envIntOrDefault("STATE_SERVER_PORT", 7449)
	upstreamWsURL = envOrDefault("UPSTREAM_RPC_WS_URL", "ws://127.0.0.1:9650/ext/bc/C/ws")
	upstreamHTTP = envOrDefault("UPSTREAM_RPC_HTTP_URL", "http://127.0.0.1:9650/ext/bc/C/rpc")
	poolSize = envIntOrDefault("UPSTREAM_POOL_SIZE", runtime.NumCPU())
	blockPollMs = envIntOrDefault("BLOCK_POLL_MS", 500)
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

func blockHex(n int) string {
	return fmt.Sprintf("0x%x", n)
}

func padSlot(slot string) string {
	s := strings.ToLower(strings.TrimPrefix(slot, "0x"))
	if len(s) < 64 {
		s = strings.Repeat("0", 64-len(s)) + s
	}
	return "0x" + s
}

func storageKey(address, slot string) string {
	return "s:" + strings.ToLower(address) + ":" + padSlot(slot)
}

func balanceKey(address string) string {
	return "b:" + strings.ToLower(address)
}

func nonceKey(address string) string {
	return "n:" + strings.ToLower(address)
}

func codeKey(address string) string {
	return "c:" + strings.ToLower(address)
}

// ---------------------------------------------------------------------------
// JSON-RPC types
// ---------------------------------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// RPC WebSocket pool
// ---------------------------------------------------------------------------

type pendingRequest struct {
	ch chan rpcResult
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcSocket struct {
	url    string
	name   string
	conn   *websocket.Conn
	mu     sync.Mutex
	nextID int64
	pending map[string]*pendingRequest
	ready   chan struct{}
	sem     chan struct{} // capacity 1: only one request at a time
}

func newRpcSocket(url, name string) *rpcSocket {
	s := &rpcSocket{
		url:     url,
		name:    name,
		pending: make(map[string]*pendingRequest),
		ready:   make(chan struct{}),
		sem:     make(chan struct{}, 1),
	}
	s.sem <- struct{}{} // start with one token
	go s.connect()
	return s
}

func (s *rpcSocket) connect() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial(s.url, nil)
		if err != nil {
			log.Printf("%s: connect error: %v, retrying in 250ms", s.name, err)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		s.conn = conn
		// Signal ready (only first time matters, subsequent reconnects just replace conn)
		select {
		case <-s.ready:
			// Already closed/ready — make a new channel for future use
		default:
			close(s.ready)
		}
		s.mu.Unlock()

		s.readLoop(conn)

		// readLoop returned — connection lost
		s.mu.Lock()
		s.failAllLocked(fmt.Errorf("%s closed", s.name))
		s.conn = nil
		s.ready = make(chan struct{})
		s.mu.Unlock()
		time.Sleep(250 * time.Millisecond)
	}
}

func (s *rpcSocket) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var resp struct {
			ID     interface{}     `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}
		idStr, ok := resp.ID.(string)
		if !ok {
			continue
		}
		s.mu.Lock()
		p, exists := s.pending[idStr]
		if exists {
			delete(s.pending, idStr)
		}
		s.mu.Unlock()
		if !exists {
			continue
		}
		if resp.Error != nil {
			p.ch <- rpcResult{err: fmt.Errorf("RPC %d: %s", resp.Error.Code, resp.Error.Message)}
		} else {
			p.ch <- rpcResult{result: resp.Result}
		}
	}
}

func (s *rpcSocket) failAllLocked(err error) {
	for id, p := range s.pending {
		p.ch <- rpcResult{err: err}
		delete(s.pending, id)
	}
}

func (s *rpcSocket) send(method string, params interface{}) (json.RawMessage, error) {
	// Block until this worker is free (one request at a time)
	<-s.sem
	defer func() { s.sem <- struct{}{} }()

	<-s.ready

	s.mu.Lock()
	conn := s.conn
	if conn == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%s not open", s.name)
	}
	id := fmt.Sprintf("%s:%d", s.name, s.nextID)
	s.nextID++
	ch := make(chan rpcResult, 1)
	s.pending[id] = &pendingRequest{ch: ch}
	s.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(req)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	res := <-ch
	return res.result, res.err
}

type rpcPool struct {
	sockets []*rpcSocket
	next    atomic.Int64
}

func newRpcPool(url string, size int) *rpcPool {
	p := &rpcPool{sockets: make([]*rpcSocket, size)}
	for i := 0; i < size; i++ {
		p.sockets[i] = newRpcSocket(url, fmt.Sprintf("rpc-%d", i))
	}
	return p
}

func (p *rpcPool) call(method string, params interface{}) (json.RawMessage, error) {
	// Workers pattern: each socket's semaphore ensures one-at-a-time.
	// Round-robin is fine — if a socket is busy, send() blocks on its sem.
	// Use atomic counter for simple distribution.
	idx := p.next.Add(1) - 1
	s := p.sockets[int(idx)%len(p.sockets)]
	return s.send(method, params)
}

func (p *rpcPool) callString(method string, params interface{}) (string, error) {
	raw, err := p.call(method, params)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("expected string result, got: %s", string(raw))
	}
	return s, nil
}

func (p *rpcPool) ethGetStorageAt(address, slot string, block int) (string, error) {
	return p.callString("eth_getStorageAt", []string{address, slot, blockHex(block)})
}

func (p *rpcPool) ethGetBalance(address string, block int) (string, error) {
	return p.callString("eth_getBalance", []string{address, blockHex(block)})
}

func (p *rpcPool) ethGetTransactionCount(address string, block int) (string, error) {
	return p.callString("eth_getTransactionCount", []string{address, blockHex(block)})
}

func (p *rpcPool) ethGetCode(address string, block int) (string, error) {
	return p.callString("eth_getCode", []string{address, blockHex(block)})
}

func (p *rpcPool) ethBlockNumber() (int, error) {
	s, err := p.callString("eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

type blockInfo struct {
	Timestamp uint64
	BaseFee   uint64
	GasLimit  uint64
}

func (p *rpcPool) ethGetBlockByNumber(block int) (blockInfo, error) {
	raw, err := p.call("eth_getBlockByNumber", []interface{}{blockHex(block), false})
	if err != nil {
		return blockInfo{}, err
	}
	var result struct {
		Timestamp      string `json:"timestamp"`
		BaseFeePerGas  string `json:"baseFeePerGas"`
		GasLimit       string `json:"gasLimit"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return blockInfo{}, err
	}
	ts, _ := strconv.ParseUint(strings.TrimPrefix(result.Timestamp, "0x"), 16, 64)
	baseFee, _ := strconv.ParseUint(strings.TrimPrefix(result.BaseFeePerGas, "0x"), 16, 64)
	gasLimit, _ := strconv.ParseUint(strings.TrimPrefix(result.GasLimit, "0x"), 16, 64)
	return blockInfo{Timestamp: ts, BaseFee: baseFee, GasLimit: gasLimit}, nil
}

// ---------------------------------------------------------------------------
// State cache
// ---------------------------------------------------------------------------

type stateCache struct {
	mu          sync.RWMutex
	tracked     map[string]struct{}
	values      map[string]string
	blockNumber int
	timestamp   int
	baseFee     uint64
	gasLimit    uint64
}

func newStateCache() *stateCache {
	return &stateCache{
		tracked: make(map[string]struct{}),
		values:  make(map[string]string),
	}
}

func (c *stateCache) get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[key]
	return v, ok
}

func (c *stateCache) set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracked[key] = struct{}{}
	c.values[key] = value
}

func (c *stateCache) track(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracked[key] = struct{}{}
}

func (c *stateCache) applyDiff(diff map[string]string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	applied := 0
	for k, v := range diff {
		if _, ok := c.tracked[k]; ok {
			c.values[k] = v
			applied++
		}
	}
	return applied
}

func (c *stateCache) dump() (int, int, uint64, uint64, [][2]string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries := make([][2]string, 0, len(c.values))
	for k, v := range c.values {
		entries = append(entries, [2]string{k, v})
	}
	return c.blockNumber, c.timestamp, c.baseFee, c.gasLimit, entries
}

func (c *stateCache) getBlockNumber() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blockNumber
}

func (c *stateCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.values)
}

func (c *stateCache) trackedSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.tracked)
}

func (c *stateCache) isTracked(key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.tracked[key]
	return ok
}

// ---------------------------------------------------------------------------
// Block diff via debug_traceBlockByNumber (HTTP)
// ---------------------------------------------------------------------------

func traceBlockDiff(block int) (map[string]string, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "debug_traceBlockByNumber",
		"params": []interface{}{
			blockHex(block),
			map[string]interface{}{
				"tracer":       "prestateTracer",
				"tracerConfig": map[string]interface{}{"diffMode": true},
			},
		},
	})

	resp, err := http.Post(upstreamHTTP, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResp struct {
		Error  *rpcError         `json:"error"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("trace: %s", rpcResp.Error.Message)
	}

	diff := make(map[string]string)
	if rpcResp.Result == nil {
		return diff, nil
	}

	for _, txRaw := range rpcResp.Result {
		var tx struct {
			Result struct {
				Post map[string]struct {
					Balance *string            `json:"balance"`
					Nonce   *json.Number       `json:"nonce"`
					Code    *string            `json:"code"`
					Storage map[string]string  `json:"storage"`
				} `json:"post"`
			} `json:"result"`
		}
		if json.Unmarshal(txRaw, &tx) != nil {
			continue
		}
		if tx.Result.Post == nil {
			continue
		}
		for address, account := range tx.Result.Post {
			if account.Balance != nil {
				diff[balanceKey(address)] = *account.Balance
			}
			if account.Nonce != nil {
				// Nonce comes as a number — convert to hex
				n, err := strconv.ParseInt(account.Nonce.String(), 10, 64)
				if err == nil {
					diff[nonceKey(address)] = fmt.Sprintf("0x%x", n)
				}
			}
			if account.Code != nil {
				diff[codeKey(address)] = *account.Code
			}
			for slot, value := range account.Storage {
				diff[storageKey(address, slot)] = value
			}
		}
	}
	return diff, nil
}

// ---------------------------------------------------------------------------
// Client message handling
// ---------------------------------------------------------------------------

type clientRequest struct {
	ID          interface{} `json:"id"`
	Method      string      `json:"method"`
	Address     string
	Slot        string
	BlockNumber int
}

type clientMessage struct {
	JSONRPC string `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  struct {
		Address     string `json:"address"`
		Slot        string `json:"slot"`
		BlockNumber *int   `json:"blockNumber"`
	} `json:"params"`
}

func parseRequest(data []byte) (*clientRequest, *rpcError) {
	var msg clientMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, &rpcError{Code: -32600, Message: "invalid JSON"}
	}
	if msg.JSONRPC != "2.0" {
		return nil, &rpcError{Code: -32602, Message: "jsonrpc must be '2.0'"}
	}
	if msg.Params.Address == "" {
		return nil, &rpcError{Code: -32602, Message: "missing address"}
	}
	if msg.Params.BlockNumber == nil {
		return nil, &rpcError{Code: -32602, Message: "blockNumber must be a number"}
	}

	switch msg.Method {
	case "state_getStorageAt":
		if msg.Params.Slot == "" {
			return nil, &rpcError{Code: -32602, Message: "missing slot"}
		}
		return &clientRequest{
			ID: msg.ID, Method: msg.Method,
			Address: msg.Params.Address, Slot: msg.Params.Slot,
			BlockNumber: *msg.Params.BlockNumber,
		}, nil
	case "state_getBalance", "state_getNonce", "state_getCode":
		return &clientRequest{
			ID: msg.ID, Method: msg.Method,
			Address: msg.Params.Address,
			BlockNumber: *msg.Params.BlockNumber,
		}, nil
	default:
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unsupported method: %s", msg.Method)}
	}
}

func cacheKeyForRequest(req *clientRequest) string {
	switch req.Method {
	case "state_getStorageAt":
		return storageKey(req.Address, req.Slot)
	case "state_getBalance":
		return balanceKey(req.Address)
	case "state_getNonce":
		return nonceKey(req.Address)
	case "state_getCode":
		return codeKey(req.Address)
	default:
		return ""
	}
}

func fetchFromNode(pool *rpcPool, req *clientRequest) (string, error) {
	switch req.Method {
	case "state_getStorageAt":
		return pool.ethGetStorageAt(req.Address, req.Slot, req.BlockNumber)
	case "state_getBalance":
		return pool.ethGetBalance(req.Address, req.BlockNumber)
	case "state_getNonce":
		return pool.ethGetTransactionCount(req.Address, req.BlockNumber)
	case "state_getCode":
		return pool.ethGetCode(req.Address, req.BlockNumber)
	default:
		return "", fmt.Errorf("unknown method: %s", req.Method)
	}
}

// ---------------------------------------------------------------------------
// Client manager
// ---------------------------------------------------------------------------

type clientManager struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]*sync.Mutex
}

func newClientManager() *clientManager {
	return &clientManager{clients: make(map[*websocket.Conn]*sync.Mutex)}
}

func (m *clientManager) add(c *websocket.Conn) *sync.Mutex {
	wmu := &sync.Mutex{}
	m.mu.Lock()
	m.clients[c] = wmu
	m.mu.Unlock()
	return wmu
}

func (m *clientManager) remove(c *websocket.Conn) {
	m.mu.Lock()
	delete(m.clients, c)
	m.mu.Unlock()
}

func (m *clientManager) broadcast(msg []byte) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for c, wmu := range m.clients {
		wmu.Lock()
		_ = c.WriteMessage(websocket.TextMessage, msg)
		wmu.Unlock()
	}
}

func (m *clientManager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.clients)
}

// ---------------------------------------------------------------------------
// eth_call caching proxy
// ---------------------------------------------------------------------------

type ethCallCache struct {
	mu    sync.RWMutex
	cache map[string]json.RawMessage
}

func newEthCallCache() *ethCallCache {
	return &ethCallCache{cache: make(map[string]json.RawMessage)}
}

func ethCallCacheKey(params json.RawMessage) string {
	h := sha256.Sum256(params)
	return hex.EncodeToString(h[:])
}

func (c *ethCallCache) get(key string) (json.RawMessage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.cache[key]
	return v, ok
}

func (c *ethCallCache) set(key string, v json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = v
}

func (c *ethCallCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// proxyEthCall forwards an eth_call to the upstream HTTP RPC and caches the result.
// Returns the full JSON-RPC response body.
func proxyEthCall(callCache *ethCallCache, reqBody []byte, params json.RawMessage, id interface{}) []byte {
	key := ethCallCacheKey(params)

	if cached, ok := callCache.get(key); ok {
		resp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": id, "result": cached,
		})
		return resp
	}

	// Miss — forward to upstream
	httpResp, err := http.Post(upstreamHTTP, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		errResp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0", ID: id,
			Error: &rpcError{Code: -32603, Message: err.Error()},
		})
		return errResp
	}
	defer httpResp.Body.Close()
	body, _ := io.ReadAll(httpResp.Body)

	// Cache successful results
	var parsed struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error == nil && parsed.Result != nil {
		callCache.set(key, parsed.Result)
	}

	return body
}

// ---------------------------------------------------------------------------
// State server: independent cache + client set per endpoint
// ---------------------------------------------------------------------------

type stateServer struct {
	cache   *stateCache
	clients *clientManager
	ready   chan struct{} // closed when block info is initialized
}

var (
	servers   = map[int]*stateServer{} // block 0 = live, block N = frozen at N
	serversMu sync.Mutex
)

func getOrCreateServer(pool *rpcPool, block int) *stateServer {
	serversMu.Lock()
	defer serversMu.Unlock()
	if s, ok := servers[block]; ok {
		return s
	}
	s := &stateServer{
		cache:   newStateCache(),
		clients: newClientManager(),
		ready:   make(chan struct{}),
	}
	servers[block] = s

	if block == 0 {
		go func() {
			if err := blockLoop(pool, s); err != nil {
				log.Fatalf("live block loop: %v", err)
			}
		}()
	} else {
		go initFrozen(pool, s, block)
	}
	return s
}

func initFrozen(pool *rpcPool, s *stateServer, block int) {
	defer close(s.ready)
	info, err := pool.ethGetBlockByNumber(block)
	if err != nil {
		logJSON(map[string]interface{}{
			"event": "frozen_init_error", "block": block, "error": err.Error(),
		})
		return
	}
	s.cache.mu.Lock()
	s.cache.blockNumber = block
	s.cache.timestamp = int(info.Timestamp)
	s.cache.baseFee = info.BaseFee
	s.cache.gasLimit = info.GasLimit
	s.cache.mu.Unlock()

	logJSON(map[string]interface{}{
		"event": "frozen_init", "block": block,
		"timestamp": info.Timestamp, "baseFee": info.BaseFee, "gasLimit": info.GasLimit,
	})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	pool := newRpcPool(upstreamWsURL, poolSize)
	callCache := newEthCallCache()

	// GET / — info page
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		serversMu.Lock()
		endpoints := make([]string, 0, len(servers))
		for block := range servers {
			if block == 0 {
				endpoints = append(endpoints, "/live")
			} else {
				endpoints = append(endpoints, fmt.Sprintf("/debug/%d", block))
			}
		}
		serversMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"endpoints": endpoints,
			"ethCall":   "/eth-call",
			"upstream":  upstreamWsURL,
		})
	})

	// /live — follows the chain
	http.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) {
		s := getOrCreateServer(pool, 0)
		handleStateWS(pool, s, w, r)
	})

	// /debug/<block> — frozen at a specific block
	http.HandleFunc("/debug/", func(w http.ResponseWriter, r *http.Request) {
		blockStr := strings.TrimPrefix(r.URL.Path, "/debug/")
		block, err := strconv.Atoi(blockStr)
		if err != nil || block <= 0 {
			http.Error(w, "invalid block number", http.StatusBadRequest)
			return
		}
		s := getOrCreateServer(pool, block)
		handleStateWS(pool, s, w, r)
	})

	// /eth-call — independent eth_call caching proxy
	http.HandleFunc("/eth-call", func(w http.ResponseWriter, r *http.Request) {
		handleEthCallWS(callCache, w, r)
	})

	addr := fmt.Sprintf("%s:%d", listenHost, listenPort)
	logJSON(map[string]interface{}{
		"event": "listening", "host": listenHost, "port": listenPort,
		"upstream": upstreamWsURL, "upstreamHttp": upstreamHTTP,
		"poolSize": poolSize, "blockPollMs": blockPollMs,
	})
	log.Fatal(http.ListenAndServe(addr, nil))
}

// ---------------------------------------------------------------------------
// State WebSocket handler (for /live and /debug/<block>)
// ---------------------------------------------------------------------------

func handleStateWS(pool *rpcPool, s *stateServer, w http.ResponseWriter, r *http.Request) {
	// Wait for server initialization (block info fetch)
	<-s.ready

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Send initial_dump BEFORE adding to clients, so no block_diff can race ahead
	blockNum, ts, baseFee, gasLimit, entries := s.cache.dump()
	dumpMsg, _ := json.Marshal(map[string]interface{}{
		"type":        "initial_dump",
		"blockNumber": blockNum,
		"timestamp":   ts,
		"baseFee":     baseFee,
		"gasLimit":    gasLimit,
		"entries":     entries,
	})
	_ = conn.WriteMessage(websocket.TextMessage, dumpMsg)

	wmu := s.clients.add(conn)
	defer s.clients.remove(conn)

	wsWrite := func(msg []byte) {
		wmu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, msg)
		wmu.Unlock()
	}
	logJSON(map[string]interface{}{
		"event": "client_connected", "path": r.URL.Path,
		"dumpSize": len(entries), "block": blockNum,
	})

	// Read loop
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		req, rpcErr := parseRequest(data)
		if rpcErr != nil {
			resp, _ := json.Marshal(jsonRPCResponse{
				JSONRPC: "2.0", ID: nil, Error: rpcErr,
			})
			wsWrite(resp)
			continue
		}

		key := cacheKeyForRequest(req)
		isLatest := req.BlockNumber == s.cache.getBlockNumber()

		// Cache hit
		if isLatest {
			if cached, ok := s.cache.get(key); ok {
				resp, _ := json.Marshal(jsonRPCResponse{
					JSONRPC: "2.0", ID: req.ID,
					Result: map[string]string{"value": cached},
				})
				wsWrite(resp)
				continue
			}
		}

		// Fetch from upstream
		value, err := fetchFromNode(pool, req)
		if err != nil {
			resp, _ := json.Marshal(jsonRPCResponse{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32603, Message: err.Error()},
			})
			wsWrite(resp)
			continue
		}

		s.cache.track(key)
		if req.BlockNumber == s.cache.getBlockNumber() {
			s.cache.set(key, value)
		}
		resp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]string{"value": value},
		})
		wsWrite(resp)
	}
}

// ---------------------------------------------------------------------------
// eth_call WebSocket handler (independent endpoint)
// ---------------------------------------------------------------------------

func handleEthCallWS(callCache *ethCallCache, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("eth-call upgrade error: %v", err)
		return
	}
	defer conn.Close()

	var writeMu sync.Mutex
	wsWrite := func(msg []byte) {
		writeMu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, msg)
		writeMu.Unlock()
	}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var peek struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      interface{}     `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(data, &peek) != nil {
			continue
		}

		if peek.Method != "eth_call" {
			resp, _ := json.Marshal(jsonRPCResponse{
				JSONRPC: "2.0", ID: peek.ID,
				Error: &rpcError{Code: -32602, Message: "only eth_call supported on this endpoint"},
			})
			wsWrite(resp)
			continue
		}

		go func(rawReq []byte, params json.RawMessage, id interface{}) {
			resp := proxyEthCall(callCache, rawReq, params, id)
			wsWrite(resp)
		}(data, peek.Params, peek.ID)
	}
}

// ---------------------------------------------------------------------------
// Block loop (live server only)
// ---------------------------------------------------------------------------

func blockLoop(pool *rpcPool, s *stateServer) error {
	bn, err := pool.ethBlockNumber()
	if err != nil {
		return fmt.Errorf("initial block number: %w", err)
	}
	s.cache.mu.Lock()
	s.cache.blockNumber = bn
	s.cache.mu.Unlock()

	info, err := pool.ethGetBlockByNumber(bn)
	if err != nil {
		return fmt.Errorf("initial block info: %w", err)
	}
	s.cache.mu.Lock()
	s.cache.timestamp = int(info.Timestamp)
	s.cache.baseFee = info.BaseFee
	s.cache.gasLimit = info.GasLimit
	s.cache.mu.Unlock()

	close(s.ready)
	logJSON(map[string]interface{}{
		"event": "block_loop_start", "block": bn, "timestamp": info.Timestamp,
	})

	ticker := time.NewTicker(time.Duration(blockPollMs) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logJSON(map[string]interface{}{"event": "block_error", "error": fmt.Sprint(r)})
				}
			}()

			latest, err := pool.ethBlockNumber()
			if err != nil {
				logJSON(map[string]interface{}{"event": "block_error", "error": err.Error()})
				return
			}
			currentBlock := s.cache.getBlockNumber()
			if latest <= currentBlock {
				return
			}

			for block := currentBlock + 1; block <= latest; block++ {
				t0 := time.Now()

				type diffResult struct {
					diff map[string]string
					err  error
				}
				type infoResult struct {
					info blockInfo
					err  error
				}
				diffCh := make(chan diffResult, 1)
				infoCh := make(chan infoResult, 1)

				go func() {
					d, e := traceBlockDiff(block)
					diffCh <- diffResult{d, e}
				}()
				go func() {
					i, e := pool.ethGetBlockByNumber(block)
					infoCh <- infoResult{i, e}
				}()

				dr := <-diffCh
				ir := <-infoCh
				if dr.err != nil {
					logJSON(map[string]interface{}{"event": "block_error", "error": dr.err.Error()})
					return
				}
				if ir.err != nil {
					logJSON(map[string]interface{}{"event": "block_error", "error": ir.err.Error()})
					return
				}

				applied := s.cache.applyDiff(dr.diff)
				s.cache.mu.Lock()
				s.cache.blockNumber = block
				s.cache.timestamp = int(ir.info.Timestamp)
				s.cache.baseFee = ir.info.BaseFee
				s.cache.gasLimit = ir.info.GasLimit
				s.cache.mu.Unlock()

				elapsed := time.Since(t0).Milliseconds()
				logJSON(map[string]interface{}{
					"event": "block", "block": block, "diffKeys": len(dr.diff),
					"applied": applied, "cached": s.cache.size(), "tracked": s.cache.trackedSize(),
					"ms": elapsed, "clients": s.clients.count(),
				})

				if applied > 0 && s.clients.count() > 0 {
					entries := make([][2]string, 0)
					for k, v := range dr.diff {
						if s.cache.isTracked(k) {
							entries = append(entries, [2]string{k, v})
						}
					}
					if len(entries) > 0 {
						msg, _ := json.Marshal(map[string]interface{}{
							"type":        "block_diff",
							"blockNumber": block,
							"timestamp":   ir.info.Timestamp,
							"baseFee":     ir.info.BaseFee,
							"gasLimit":    ir.info.GasLimit,
							"entries":     entries,
						})
						s.clients.broadcast(msg)
					}
				}
			}
		}()
	}
	return nil
}

func logJSON(fields map[string]interface{}) {
	data, _ := json.Marshal(fields)
	fmt.Println(string(data))
}
