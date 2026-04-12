package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
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

	"defi-toolbox/statedb/wire"

	"github.com/gorilla/websocket"
)

//go:embed sdk
var sdkFS embed.FS

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

// hexToAddr parses a hex address string into [20]byte.
func hexToAddr(s string) [20]byte {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	b, _ := hex.DecodeString(s)
	var addr [20]byte
	if len(b) <= 20 {
		copy(addr[20-len(b):], b)
	}
	return addr
}

// hexToHash parses a hex hash/slot string into [32]byte.
func hexToHash(s string) [32]byte {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	b, _ := hex.DecodeString(s)
	var h [32]byte
	if len(b) <= 32 {
		copy(h[32-len(b):], b)
	}
	return h
}

// addrHex converts [20]byte to "0x..." hex string.
func addrHex(a [20]byte) string { return "0x" + hex.EncodeToString(a[:]) }

// hashHex converts [32]byte to "0x..." hex string.
func hashHex(h [32]byte) string { return "0x" + hex.EncodeToString(h[:]) }

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
// State cache — typed binary maps with caps
// ---------------------------------------------------------------------------

// Cache caps: reject new entries beyond these limits.
const (
	maxContracts        = 20_000  // max unique contract addresses
	maxSlotsPerContract = 100_000 // max storage slots per contract
)

// blockDiff is the typed output of traceBlockDiffWS.
type blockDiff struct {
	storage map[[20]byte]map[[32]byte][32]byte
	balance map[[20]byte][32]byte
	nonce   map[[20]byte]uint64
	code    map[[20]byte][]byte
}

type stateCache struct {
	mu      sync.RWMutex
	storage map[[20]byte]map[[32]byte][32]byte
	balance map[[20]byte][32]byte
	nonce   map[[20]byte]uint64
	code    map[[20]byte][]byte

	blockNumber int
	timestamp   uint64
	baseFee     uint64
	gasLimit    uint64
}

func newStateCache() *stateCache {
	return &stateCache{
		storage: make(map[[20]byte]map[[32]byte][32]byte),
		balance: make(map[[20]byte][32]byte),
		nonce:   make(map[[20]byte]uint64),
		code:    make(map[[20]byte][]byte),
	}
}

// ── Typed getters ──

func (c *stateCache) getStorage(addr [20]byte, slot [32]byte) ([32]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if slots, ok := c.storage[addr]; ok {
		v, ok := slots[slot]
		return v, ok
	}
	return [32]byte{}, false
}

func (c *stateCache) getBalance(addr [20]byte) ([32]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.balance[addr]
	return v, ok
}

func (c *stateCache) getNonce(addr [20]byte) (uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.nonce[addr]
	return v, ok
}

func (c *stateCache) getCode(addr [20]byte) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.code[addr]
	return v, ok
}

// ── Typed setters (with caps) ──

func (c *stateCache) setStorage(addr [20]byte, slot [32]byte, value [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slots, exists := c.storage[addr]
	if !exists {
		if len(c.storage) >= maxContracts {
			return
		}
		slots = make(map[[32]byte][32]byte)
		c.storage[addr] = slots
	}
	if _, has := slots[slot]; !has && len(slots) >= maxSlotsPerContract {
		return
	}
	slots[slot] = value
}

func (c *stateCache) setBalance(addr [20]byte, value [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balance[addr] = value
}

func (c *stateCache) setNonce(addr [20]byte, value uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nonce[addr] = value
}

func (c *stateCache) setCode(addr [20]byte, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code[addr] = value
}

// applyDiffAndUpdateBlock applies a typed diff and updates block metadata.
func (c *stateCache) applyDiffAndUpdateBlock(diff *blockDiff, block int, timestamp, baseFee, gasLimit uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	applied := 0

	for addr, slots := range diff.storage {
		dst, exists := c.storage[addr]
		if !exists {
			if len(c.storage) >= maxContracts {
				continue
			}
			dst = make(map[[32]byte][32]byte)
			c.storage[addr] = dst
		}
		for slot, value := range slots {
			if _, has := dst[slot]; !has && len(dst) >= maxSlotsPerContract {
				continue
			}
			dst[slot] = value
			applied++
		}
	}
	for addr, bal := range diff.balance {
		c.balance[addr] = bal
		applied++
	}
	for addr, n := range diff.nonce {
		c.nonce[addr] = n
		applied++
	}
	for addr, code := range diff.code {
		c.code[addr] = code
		applied++
	}

	c.blockNumber = block
	c.timestamp = timestamp
	c.baseFee = baseFee
	c.gasLimit = gasLimit
	return applied
}

// dumpGob builds a gob+zstd encoded dump directly from the typed maps.
func (c *stateCache) dumpGob() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Count total storage entries.
	totalSlots := 0
	for _, slots := range c.storage {
		totalSlots += len(slots)
	}

	d := &wire.GobDump{
		BlockNumber: uint64(c.blockNumber),
		Timestamp:   c.timestamp,
		BaseFee:     c.baseFee,
		GasLimit:    c.gasLimit,
		Storage:     make([]wire.StorageEntry, 0, totalSlots),
		Accounts:    make([]wire.AccountEntry, 0, len(c.balance)),
	}

	for addr, slots := range c.storage {
		for slot, value := range slots {
			d.Storage = append(d.Storage, wire.StorageEntry{
				Addr: addr, Slot: slot, Value: value,
			})
		}
	}

	// Collect all unique addresses that have balance, nonce, or code.
	addrs := make(map[[20]byte]bool)
	for a := range c.balance {
		addrs[a] = true
	}
	for a := range c.nonce {
		addrs[a] = true
	}
	for a := range c.code {
		addrs[a] = true
	}
	for addr := range addrs {
		e := wire.AccountEntry{Addr: addr}
		if bal, ok := c.balance[addr]; ok {
			e.Balance = bal
		}
		if n, ok := c.nonce[addr]; ok {
			e.Nonce = n
		}
		if code, ok := c.code[addr]; ok {
			e.Code = code
		}
		d.Accounts = append(d.Accounts, e)
	}

	var buf bytes.Buffer
	if err := wire.Encode(&buf, d); err != nil {
		log.Printf("[error] dump encode: %v", err)
		return nil
	}
	log.Printf("[dump] %d contracts, %d slots, %d accounts, %d bytes (zstd)",
		len(c.storage), totalSlots, len(d.Accounts), buf.Len())
	return buf.Bytes()
}

func (c *stateCache) getBlockNumber() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blockNumber
}

func (c *stateCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, slots := range c.storage {
		total += len(slots)
	}
	return total + len(c.balance) + len(c.nonce) + len(c.code)
}

// ---------------------------------------------------------------------------
// Block diff via debug_traceBlockByNumber (WebSocket)
// ---------------------------------------------------------------------------

func traceBlockDiffWS(pool *rpcPool, block int) (*blockDiff, error) {
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

	diff := &blockDiff{
		storage: make(map[[20]byte]map[[32]byte][32]byte),
		balance: make(map[[20]byte][32]byte),
		nonce:   make(map[[20]byte]uint64),
		code:    make(map[[20]byte][]byte),
	}
	for _, txRaw := range txResults {
		var tx struct {
			Result struct {
				Post map[string]struct {
					Balance *string           `json:"balance"`
					Nonce   *json.Number      `json:"nonce"`
					Code    *string           `json:"code"`
					Storage map[string]string `json:"storage"`
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
			addr := hexToAddr(address)
			if account.Balance != nil {
				diff.balance[addr] = hexToHash(*account.Balance)
			}
			if account.Nonce != nil {
				n, err := strconv.ParseInt(account.Nonce.String(), 10, 64)
				if err == nil {
					diff.nonce[addr] = uint64(n)
				}
			}
			if account.Code != nil {
				code := strings.TrimPrefix(*account.Code, "0x")
				b, _ := hex.DecodeString(code)
				diff.code[addr] = b
			}
			for slot, value := range account.Storage {
				if diff.storage[addr] == nil {
					diff.storage[addr] = make(map[[32]byte][32]byte)
				}
				diff.storage[addr][hexToHash(slot)] = hexToHash(value)
			}
		}
	}
	return diff, nil
}

// diffSize returns the total number of entries in a blockDiff.
func diffSize(d *blockDiff) int {
	n := len(d.balance) + len(d.nonce) + len(d.code)
	for _, slots := range d.storage {
		n += len(slots)
	}
	return n
}

// diffToEntries converts a typed blockDiff to the [][2]string wire format
// used for block_diff JSON broadcasts.
func diffToEntries(d *blockDiff) [][2]string {
	entries := make([][2]string, 0, diffSize(d))
	for addr, slots := range d.storage {
		ah := addrHex(addr)
		for slot, value := range slots {
			entries = append(entries, [2]string{
				"s:" + ah + ":" + hashHex(slot),
				hashHex(value),
			})
		}
	}
	for addr, bal := range d.balance {
		entries = append(entries, [2]string{"b:" + addrHex(addr), hashHex(bal)})
	}
	for addr, n := range d.nonce {
		entries = append(entries, [2]string{"n:" + addrHex(addr), fmt.Sprintf("0x%x", n)})
	}
	for addr, code := range d.code {
		entries = append(entries, [2]string{"c:" + addrHex(addr), "0x" + hex.EncodeToString(code)})
	}
	return entries
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

// cacheLookup checks the typed cache for a request. Returns hex string value and hit bool.
func cacheLookup(c *stateCache, req *clientRequest) (string, bool) {
	addr := hexToAddr(req.Address)
	switch req.Method {
	case "state_getStorageAt":
		v, ok := c.getStorage(addr, hexToHash(req.Slot))
		if ok {
			return hashHex(v), true
		}
	case "state_getBalance":
		v, ok := c.getBalance(addr)
		if ok {
			return hashHex(v), true
		}
	case "state_getNonce":
		v, ok := c.getNonce(addr)
		if ok {
			return fmt.Sprintf("0x%x", v), true
		}
	case "state_getCode":
		v, ok := c.getCode(addr)
		if ok {
			if len(v) == 0 {
				return "0x", true
			}
			return "0x" + hex.EncodeToString(v), true
		}
	}
	return "", false
}

// cacheStore writes a hex string value from an RPC response into the typed cache.
func cacheStore(c *stateCache, req *clientRequest, hexValue string) {
	addr := hexToAddr(req.Address)
	switch req.Method {
	case "state_getStorageAt":
		c.setStorage(addr, hexToHash(req.Slot), hexToHash(hexValue))
	case "state_getBalance":
		c.setBalance(addr, hexToHash(hexValue))
	case "state_getNonce":
		n, err := strconv.ParseUint(strings.TrimPrefix(hexValue, "0x"), 16, 64)
		if err == nil {
			c.setNonce(addr, n)
		}
	case "state_getCode":
		code := strings.TrimPrefix(hexValue, "0x")
		if code == "" {
			c.setCode(addr, nil)
		} else {
			b, _ := hex.DecodeString(code)
			c.setCode(addr, b)
		}
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

func (m *clientManager) addWithMu(c *websocket.Conn, wmu *sync.Mutex) {
	m.mu.Lock()
	m.clients[c] = wmu
	m.mu.Unlock()
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

	// /sdk/ — serve embedded WASM SDK files with CORS
	http.HandleFunc("/sdk/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		name := strings.TrimPrefix(r.URL.Path, "/sdk/")
		data, err := sdkFS.ReadFile("sdk/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(name, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		} else if strings.HasSuffix(name, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
		}
		w.Write(data)
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
		// Subscriber: send gob-encoded initial_dump as binary WebSocket frame,
		// then register for JSON block_diff broadcasts (text frames).
		s.blockMu.RLock()
		gobBytes := s.cache.dumpGob()
		wmu.Lock()
		_ = conn.WriteMessage(websocket.BinaryMessage, gobBytes)
		wmu.Unlock()
		s.clients.addWithMu(conn, wmu)
		s.blockMu.RUnlock()
		defer s.clients.remove(conn)

		logJSON(map[string]interface{}{
			"event": "subscriber_connected", "path": r.URL.Path,
			"dumpSize": len(gobBytes), "block": s.cache.getBlockNumber(),
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
	cached, cacheHit := cacheLookup(s.cache, req)
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
		cacheStore(s.cache, req, value)
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
				diff *blockDiff
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
			ds := diffSize(dr.diff)
			if ds > 0 && s.clients.count() > 0 {
				entries := diffToEntries(dr.diff)
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
				"event": "block", "block": block, "diffKeys": ds,
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
