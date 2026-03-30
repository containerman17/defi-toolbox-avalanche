package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	poolSize      int
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
	poolSize = envIntOrDefault("UPSTREAM_POOL_SIZE", runtime.NumCPU())
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
	values      map[string]string
	blockNumber int
	timestamp   uint64
	baseFee     uint64
	gasLimit    uint64
}

func newStateCache() *stateCache {
	return &stateCache{
		values: make(map[string]string),
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
	c.values[key] = value
}

func (c *stateCache) applyDiff(diff map[string]string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	applied := 0
	for k, v := range diff {
		c.values[k] = v
		applied++
	}
	return applied
}

// applyDiffAndUpdateBlock applies a diff and updates block metadata under a single lock.
func (c *stateCache) applyDiffAndUpdateBlock(diff map[string]string, block int, timestamp, baseFee, gasLimit uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	applied := 0
	for k, v := range diff {
		c.values[k] = v
		applied++
	}
	c.blockNumber = block
	c.timestamp = timestamp
	c.baseFee = baseFee
	c.gasLimit = gasLimit
	return applied
}

func (c *stateCache) dump() (int, uint64, uint64, uint64, [][2]string) {
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

// ---------------------------------------------------------------------------
// Block diff via debug_traceBlockByNumber (WebSocket)
// ---------------------------------------------------------------------------

func traceBlockDiffWS(pool *rpcPool, block int) (map[string]string, error) {
	raw, err := pool.call("debug_traceBlockByNumber", []interface{}{
		blockHex(block),
		map[string]interface{}{
			"tracer":       "prestateTracer",
			"tracerConfig": map[string]interface{}{"diffMode": true},
		},
	})
	if err != nil {
		return nil, err
	}

	var txResults []json.RawMessage
	if err := json.Unmarshal(raw, &txResults); err != nil {
		return nil, fmt.Errorf("parse trace result: %w", err)
	}

	diff := make(map[string]string)
	for _, txRaw := range txResults {
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

// proxyEthCall forwards an eth_call to the upstream WS RPC pool and caches the result.
func proxyEthCall(pool *rpcPool, callCache *ethCallCache, params json.RawMessage, id interface{}) []byte {
	key := ethCallCacheKey(params)

	if cached, ok := callCache.get(key); ok {
		resp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": id, "result": cached,
		})
		return resp
	}

	// Miss — forward to upstream via WS pool
	result, err := pool.call("eth_call", params)
	if err != nil {
		errResp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0", ID: id,
			Error: &rpcError{Code: -32603, Message: err.Error()},
		})
		return errResp
	}

	callCache.set(key, result)

	resp, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
	return resp
}

// ---------------------------------------------------------------------------
// State server: independent cache + client set per endpoint
// ---------------------------------------------------------------------------

type stateServer struct {
	cache   *stateCache
	clients *clientManager
	ready   chan struct{} // closed when block info is initialized
	blockMu sync.RWMutex // block update (write) vs client request (read) exclusion
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
	s.cache.timestamp = info.Timestamp
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
		handleEthCallWS(pool, callCache, w, r)
	})

	addr := fmt.Sprintf("%s:%d", listenHost, listenPort)
	logJSON(map[string]interface{}{
		"event": "listening", "host": listenHost, "port": listenPort,
		"upstream": upstreamWsURL, "poolSize": poolSize,
	})
	log.Fatal(http.ListenAndServe(addr, nil))
}

// ---------------------------------------------------------------------------
// State WebSocket handler (for /live and /debug/<block>)
// ---------------------------------------------------------------------------

func handleStateWS(pool *rpcPool, s *stateServer, w http.ResponseWriter, r *http.Request) {
	<-s.ready

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Read first message to determine connection type.
	// {"subscribe": true} → subscriber (gets dump + block_diffs).
	// JSON-RPC request → worker (pure request/response, no dump).
	_, firstMsg, err := conn.ReadMessage()
	if err != nil {
		return
	}

	wmu := &sync.Mutex{}
	wsWrite := func(msg []byte) {
		wmu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, msg)
		wmu.Unlock()
	}

	var peek struct {
		Subscribe bool   `json:"subscribe"`
		Method    string `json:"method"`
	}
	json.Unmarshal(firstMsg, &peek)

	if peek.Subscribe {
		// Subscriber: send initial_dump, add to broadcast list for block_diffs
		s.blockMu.RLock()
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
		s.clients.add(conn)
		s.blockMu.RUnlock()
		defer s.clients.remove(conn)

		logJSON(map[string]interface{}{
			"event": "subscriber_connected", "path": r.URL.Path,
			"dumpSize": len(entries), "block": blockNum,
		})
	} else if peek.Method != "" {
		// Worker: first message is already a JSON-RPC request — handle it
		logJSON(map[string]interface{}{
			"event": "worker_connected", "path": r.URL.Path,
		})
		handleClientRequest(pool, s, firstMsg, wsWrite)
	}

	// Read loop (same for subscriber and worker)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		handleClientRequest(pool, s, data, wsWrite)
	}
}

// handleClientRequest processes a single JSON-RPC request.
func handleClientRequest(pool *rpcPool, s *stateServer, data []byte, wsWrite func([]byte)) {
	req, rpcErr := parseRequest(data)
	if rpcErr != nil {
		resp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0", ID: nil, Error: rpcErr,
		})
		wsWrite(resp)
		return
	}

	s.blockMu.RLock()
	currentBlock := s.cache.getBlockNumber()
	req.BlockNumber = currentBlock
	key := cacheKeyForRequest(req)
	cached, cacheHit := s.cache.get(key)
	s.blockMu.RUnlock()

	if cacheHit {
		resp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]string{"value": cached},
		})
		wsWrite(resp)
		return
	}

	value, err := fetchFromNode(pool, req)
	if err != nil {
		resp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32603, Message: err.Error()},
		})
		wsWrite(resp)
		return
	}

	s.blockMu.RLock()
	if s.cache.getBlockNumber() == currentBlock {
		s.cache.set(key, value)
	}
	s.blockMu.RUnlock()

	resp, _ := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0", ID: req.ID,
		Result: map[string]string{"value": value},
	})
	wsWrite(resp)
}

// ---------------------------------------------------------------------------
// eth_call WebSocket handler (independent endpoint)
// ---------------------------------------------------------------------------

func handleEthCallWS(pool *rpcPool, callCache *ethCallCache, w http.ResponseWriter, r *http.Request) {
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

		go func(params json.RawMessage, id interface{}) {
			resp := proxyEthCall(pool, callCache, params, id)
			wsWrite(resp)
		}(peek.Params, peek.ID)
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
	s.cache.timestamp = info.Timestamp
	s.cache.baseFee = info.BaseFee
	s.cache.gasLimit = info.GasLimit
	s.cache.mu.Unlock()

	close(s.ready)
	logJSON(map[string]interface{}{
		"event": "block_loop_start", "block": bn, "timestamp": info.Timestamp,
	})

	// Subscribe to newHeads via a dedicated WebSocket
	var latestBlock atomic.Int64
	latestBlock.Store(int64(bn))
	go subscribeNewHeads(upstreamWsURL, &latestBlock)

	processedBlock := bn

	for {
		latest := int(latestBlock.Load())
		if latest <= processedBlock {
			time.Sleep(1 * time.Millisecond)
			continue
		}

		for block := processedBlock + 1; block <= latest; block++ {
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
				d, e := traceBlockDiffWS(pool, block)
				diffCh <- diffResult{d, e}
			}()
			go func() {
				i, e := pool.ethGetBlockByNumber(block)
				infoCh <- infoResult{i, e}
			}()

			dr := <-diffCh
			ir := <-infoCh
			if dr.err != nil {
				logJSON(map[string]interface{}{"event": "block_error", "block": block, "error": dr.err.Error()})
				break
			}
			if ir.err != nil {
				logJSON(map[string]interface{}{"event": "block_error", "block": block, "error": ir.err.Error()})
				break
			}

			// Hold blockMu.Lock during diff apply + metadata update + broadcast
			s.blockMu.Lock()

			applied := s.cache.applyDiffAndUpdateBlock(dr.diff, block, ir.info.Timestamp, ir.info.BaseFee, ir.info.GasLimit)

			// Broadcast ALL changed keys to all clients
			if len(dr.diff) > 0 && s.clients.count() > 0 {
				entries := make([][2]string, 0, len(dr.diff))
				for k, v := range dr.diff {
					entries = append(entries, [2]string{k, v})
				}
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

			s.blockMu.Unlock()

			processedBlock = block

			elapsed := time.Since(t0).Milliseconds()
			logJSON(map[string]interface{}{
				"event": "block", "block": block, "diffKeys": len(dr.diff),
				"applied": applied, "cached": s.cache.size(),
				"ms": elapsed, "clients": s.clients.count(),
			})
		}
	}
}

// subscribeNewHeads connects to the upstream WS and subscribes to newHeads.
// Updates the atomic counter on each new block.
func subscribeNewHeads(wsURL string, latestBlock *atomic.Int64) {
	for {
		func() {
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				log.Printf("newHeads connect error: %v, retrying...", err)
				time.Sleep(1 * time.Second)
				return
			}
			defer conn.Close()

			// Send eth_subscribe for newHeads
			subReq, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"method": "eth_subscribe",
				"params": []string{"newHeads"},
			})
			if err := conn.WriteMessage(websocket.TextMessage, subReq); err != nil {
				log.Printf("newHeads subscribe write error: %v", err)
				return
			}

			// Read subscription confirmation
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Printf("newHeads subscribe read error: %v", err)
				return
			}
			var subResp struct {
				Result string    `json:"result"`
				Error  *rpcError `json:"error"`
			}
			if json.Unmarshal(msg, &subResp) != nil || subResp.Error != nil {
				log.Printf("newHeads subscribe failed: %s", string(msg))
				return
			}
			logJSON(map[string]interface{}{"event": "newHeads_subscribed", "subId": subResp.Result})

			// Read notifications
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					log.Printf("newHeads read error: %v", err)
					return
				}
				var notif struct {
					Params struct {
						Result struct {
							Number string `json:"number"`
						} `json:"result"`
					} `json:"params"`
				}
				if json.Unmarshal(msg, &notif) != nil {
					continue
				}
				if notif.Params.Result.Number == "" {
					continue
				}
				n, err := strconv.ParseInt(strings.TrimPrefix(notif.Params.Result.Number, "0x"), 16, 64)
				if err != nil {
					continue
				}
				latestBlock.Store(n)
			}
		}()
		time.Sleep(1 * time.Second) // reconnect delay
	}
}

func logJSON(fields map[string]interface{}) {
	data, _ := json.Marshal(fields)
	fmt.Println(string(data))
}
