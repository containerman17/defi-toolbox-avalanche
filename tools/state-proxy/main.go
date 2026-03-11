// state-proxy: WebSocket recording RPC proxy that caches eth_getStorageAt, eth_getCode, eth_getBalance.
//
// Usage:
//   state-proxy -port 8547 -upstream ws://localhost:9650/ext/bc/C/ws -cache ./state-cache
//
// Supports multiple blocks simultaneously. Each block's state is cached in a separate file:
//   ./state-cache/<blockHex>.json
//
// Pathfinder connects via ws://localhost:8547/ws. Proxy forwards to upstream WS, caches everything.
// On shutdown (Ctrl+C), dumps all block caches to disk.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	port := flag.Int("port", 8547, "port to listen on")
	upstream := flag.String("upstream", "ws://localhost:9650/ext/bc/C/ws", "upstream WebSocket RPC URL")
	cacheDir := flag.String("cache", "./state-cache", "cache directory")
	maxConn := flag.Int("upstream-conns", 0, "max concurrent upstream WS connections (default: nproc)")
	flag.Parse()

	conns := *maxConn
	if conns == 0 {
		conns = runtime.NumCPU()
	}

	proxy := NewRecordingProxy(*upstream, *cacheDir, conns)

	// Dump on shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down, saving cache...")
		proxy.SaveAllToDisk()
		os.Exit(0)
	}()

	// Periodic stats
	go func() {
		for range time.Tick(5 * time.Second) {
			h := proxy.stats.hits.Load()
			m := proxy.stats.misses.Load()
			total := h + m
			if total == 0 {
				continue
			}
			pct := float64(h) / float64(total) * 100
			log.Printf("[stats] hits=%d misses=%d (%.1f%% hit rate) blocks=%d inflight=%d",
				h, m, pct, proxy.BlockCount(), proxy.stats.inflight.Load())
		}
	}()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			fmt.Fprintf(w, "state-proxy: connect via ws://localhost:%d/ws\n", *port)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}
		log.Printf("client connected from %s", r.RemoteAddr)
		proxy.HandleClient(conn)
		log.Printf("client disconnected from %s", r.RemoteAddr)
	}

	http.HandleFunc("/ws", handler)
	http.HandleFunc("/", handler)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("state-proxy ws://0.0.0.0%s/ws → %s (multi-block, %d upstream conns)", addr, *upstream, conns)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// ── Types ──

type ContractState struct {
	Storage map[string]string `json:"storage,omitempty"`
	Code    string            `json:"code,omitempty"`
	Balance string            `json:"balance,omitempty"`
}

type jsonRPCRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
	ID      json.RawMessage   `json:"id"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// ── Upstream WS pool ──

type upstreamPool struct {
	url   string
	conns chan *websocket.Conn
	mu    sync.Mutex
}

func newUpstreamPool(url string, size int) *upstreamPool {
	p := &upstreamPool{
		url:   url,
		conns: make(chan *websocket.Conn, size),
	}
	go p.keepAlive()
	return p
}

func (p *upstreamPool) keepAlive() {
	for {
		time.Sleep(15 * time.Second)
		var batch []*websocket.Conn
		for {
			select {
			case c := <-p.conns:
				batch = append(batch, c)
			default:
				goto done
			}
		}
	done:
		for _, c := range batch {
			if err := c.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.Close()
			} else {
				p.conns <- c
			}
		}
	}
}

func (p *upstreamPool) get() (*websocket.Conn, error) {
	select {
	case conn := <-p.conns:
		return conn, nil
	default:
		return p.dial()
	}
}

func (p *upstreamPool) put(conn *websocket.Conn) {
	select {
	case p.conns <- conn:
	default:
		conn.Close()
	}
}

func (p *upstreamPool) dial() (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.Dial(p.url, nil)
	if err != nil {
		return nil, fmt.Errorf("upstream dial: %w", err)
	}
	return conn, nil
}

func (p *upstreamPool) discard(conn *websocket.Conn) {
	conn.Close()
}

// ── Block State (per-block cache) ──

type BlockState struct {
	mu        sync.RWMutex
	contracts map[string]*ContractState
	dirty     bool
}

func newBlockState() *BlockState {
	return &BlockState{contracts: make(map[string]*ContractState)}
}

func (bs *BlockState) getOrCreateContract(addr string) *ContractState {
	cs, ok := bs.contracts[addr]
	if !ok {
		cs = &ContractState{Storage: make(map[string]string)}
		bs.contracts[addr] = cs
	}
	return cs
}

// ── Recording Proxy (multi-block) ──

type RecordingProxy struct {
	cacheDir string
	pool     *upstreamPool

	blocksMu sync.RWMutex
	blocks   map[string]*BlockState // blockHex → state

	stats struct {
		hits     atomic.Int64
		misses   atomic.Int64
		inflight atomic.Int64
	}
}

func NewRecordingProxy(upstream, cacheDir string, maxConn int) *RecordingProxy {
	return &RecordingProxy{
		cacheDir: cacheDir,
		pool:     newUpstreamPool(upstream, maxConn),
		blocks:   make(map[string]*BlockState),
	}
}

func (p *RecordingProxy) BlockCount() int {
	p.blocksMu.RLock()
	defer p.blocksMu.RUnlock()
	return len(p.blocks)
}

func (p *RecordingProxy) getOrCreateBlock(blockHex string) *BlockState {
	p.blocksMu.RLock()
	bs, ok := p.blocks[blockHex]
	p.blocksMu.RUnlock()
	if ok {
		return bs
	}

	p.blocksMu.Lock()
	defer p.blocksMu.Unlock()
	// Double-check after write lock
	if bs, ok = p.blocks[blockHex]; ok {
		return bs
	}
	bs = newBlockState()
	p.blocks[blockHex] = bs
	// Try loading from disk
	p.loadBlockFromDisk(blockHex, bs)
	return bs
}

func (p *RecordingProxy) HandleClient(client *websocket.Conn) {
	defer client.Close()
	var writeMu sync.Mutex

	for {
		_, msg, err := client.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("client read error: %v", err)
			}
			return
		}

		trimmed := strings.TrimSpace(string(msg))
		if len(trimmed) == 0 {
			continue
		}

		if trimmed[0] == '[' {
			var reqs []jsonRPCRequest
			if err := json.Unmarshal(msg, &reqs); err != nil {
				log.Printf("bad batch: %v", err)
				continue
			}
			go func() {
				results := make([]jsonRPCResponse, len(reqs))
				var wg sync.WaitGroup
				for i, req := range reqs {
					wg.Add(1)
					go func(idx int, r jsonRPCRequest) {
						defer wg.Done()
						results[idx] = p.handleRequest(r)
					}(i, req)
				}
				wg.Wait()
				resp, _ := json.Marshal(results)
				writeMu.Lock()
				client.WriteMessage(websocket.TextMessage, resp)
				writeMu.Unlock()
			}()
		} else {
			var req jsonRPCRequest
			if err := json.Unmarshal(msg, &req); err != nil {
				log.Printf("bad request: %v", err)
				continue
			}
			go func() {
				result := p.handleRequest(req)
				resp, _ := json.Marshal(result)
				writeMu.Lock()
				client.WriteMessage(websocket.TextMessage, resp)
				writeMu.Unlock()
			}()
		}
	}
}

// extractBlockParam gets the block hex from the last param of state queries (eth_getStorageAt has it at [2], others at [1]).
func extractBlockParam(method string, params []json.RawMessage) string {
	var idx int
	switch method {
	case "eth_getStorageAt":
		idx = 2
	case "eth_getCode", "eth_getBalance":
		idx = 1
	default:
		return ""
	}
	if idx >= len(params) {
		return ""
	}
	var block string
	json.Unmarshal(params[idx], &block)
	return strings.ToLower(block)
}

func (p *RecordingProxy) handleRequest(req jsonRPCRequest) jsonRPCResponse {
	switch req.Method {
	case "eth_getStorageAt":
		return p.handleGetStorageAt(req)
	case "eth_getCode":
		return p.handleGetCode(req)
	case "eth_getBalance":
		return p.handleGetBalance(req)
	default:
		return p.forwardToUpstream(req)
	}
}

func (p *RecordingProxy) handleGetStorageAt(req jsonRPCRequest) jsonRPCResponse {
	if len(req.Params) < 3 {
		return p.forwardToUpstream(req)
	}
	var addr, slot string
	json.Unmarshal(req.Params[0], &addr)
	json.Unmarshal(req.Params[1], &slot)
	addr = strings.ToLower(addr)
	slot = strings.ToLower(slot)
	blockHex := extractBlockParam(req.Method, req.Params)

	bs := p.getOrCreateBlock(blockHex)
	bs.mu.RLock()
	if cs, ok := bs.contracts[addr]; ok {
		if val, ok := cs.Storage[slot]; ok {
			bs.mu.RUnlock()
			p.stats.hits.Add(1)
			result, _ := json.Marshal(val)
			return jsonRPCResponse{JSONRPC: "2.0", Result: result, ID: req.ID}
		}
	}
	bs.mu.RUnlock()

	p.stats.misses.Add(1)
	resp := p.forwardToUpstream(req)
	if resp.Error == nil && resp.Result != nil {
		var val string
		json.Unmarshal(resp.Result, &val)
		bs.mu.Lock()
		cs := bs.getOrCreateContract(addr)
		cs.Storage[slot] = val
		bs.dirty = true
		bs.mu.Unlock()
	}
	return resp
}

func (p *RecordingProxy) handleGetCode(req jsonRPCRequest) jsonRPCResponse {
	if len(req.Params) < 2 {
		return p.forwardToUpstream(req)
	}
	var addr string
	json.Unmarshal(req.Params[0], &addr)
	addr = strings.ToLower(addr)
	blockHex := extractBlockParam(req.Method, req.Params)

	bs := p.getOrCreateBlock(blockHex)
	bs.mu.RLock()
	if cs, ok := bs.contracts[addr]; ok && cs.Code != "" {
		bs.mu.RUnlock()
		p.stats.hits.Add(1)
		result, _ := json.Marshal(cs.Code)
		return jsonRPCResponse{JSONRPC: "2.0", Result: result, ID: req.ID}
	}
	bs.mu.RUnlock()

	p.stats.misses.Add(1)
	resp := p.forwardToUpstream(req)
	if resp.Error == nil && resp.Result != nil {
		var code string
		json.Unmarshal(resp.Result, &code)
		bs.mu.Lock()
		cs := bs.getOrCreateContract(addr)
		cs.Code = code
		bs.dirty = true
		bs.mu.Unlock()
	}
	return resp
}

func (p *RecordingProxy) handleGetBalance(req jsonRPCRequest) jsonRPCResponse {
	if len(req.Params) < 2 {
		return p.forwardToUpstream(req)
	}
	var addr string
	json.Unmarshal(req.Params[0], &addr)
	addr = strings.ToLower(addr)
	blockHex := extractBlockParam(req.Method, req.Params)

	bs := p.getOrCreateBlock(blockHex)
	bs.mu.RLock()
	if cs, ok := bs.contracts[addr]; ok && cs.Balance != "" {
		bs.mu.RUnlock()
		p.stats.hits.Add(1)
		result, _ := json.Marshal(cs.Balance)
		return jsonRPCResponse{JSONRPC: "2.0", Result: result, ID: req.ID}
	}
	bs.mu.RUnlock()

	p.stats.misses.Add(1)
	resp := p.forwardToUpstream(req)
	if resp.Error == nil && resp.Result != nil {
		var bal string
		json.Unmarshal(resp.Result, &bal)
		bs.mu.Lock()
		cs := bs.getOrCreateContract(addr)
		cs.Balance = bal
		bs.dirty = true
		bs.mu.Unlock()
	}
	return resp
}

func (p *RecordingProxy) forwardToUpstream(req jsonRPCRequest) jsonRPCResponse {
	p.stats.inflight.Add(1)
	defer p.stats.inflight.Add(-1)

	conn, err := p.pool.get()
	if err != nil {
		errMsg, _ := json.Marshal(map[string]any{"code": -32000, "message": err.Error()})
		return jsonRPCResponse{JSONRPC: "2.0", Error: errMsg, ID: req.ID}
	}

	reqBytes, _ := json.Marshal(req)
	if err := conn.WriteMessage(websocket.TextMessage, reqBytes); err != nil {
		p.pool.discard(conn)
		errMsg, _ := json.Marshal(map[string]any{"code": -32000, "message": "upstream write: " + err.Error()})
		return jsonRPCResponse{JSONRPC: "2.0", Error: errMsg, ID: req.ID}
	}

	_, respBytes, err := conn.ReadMessage()
	if err != nil {
		p.pool.discard(conn)
		errMsg, _ := json.Marshal(map[string]any{"code": -32000, "message": "upstream read: " + err.Error()})
		return jsonRPCResponse{JSONRPC: "2.0", Error: errMsg, ID: req.ID}
	}

	p.pool.put(conn)

	var resp jsonRPCResponse
	json.Unmarshal(respBytes, &resp)
	resp.ID = req.ID
	return resp
}

// ── Disk persistence (one file per block) ──

type CacheFile struct {
	Block     string                    `json:"block"`
	Contracts map[string]*ContractState `json:"contracts"`
}

func (p *RecordingProxy) blockCachePath(blockHex string) string {
	return filepath.Join(p.cacheDir, blockHex+".json")
}

func (p *RecordingProxy) SaveAllToDisk() {
	p.blocksMu.RLock()
	defer p.blocksMu.RUnlock()

	os.MkdirAll(p.cacheDir, 0755)

	for blockHex, bs := range p.blocks {
		bs.mu.RLock()
		cf := CacheFile{Block: blockHex, Contracts: bs.contracts}
		data, _ := json.Marshal(cf)
		path := p.blockCachePath(blockHex)
		os.WriteFile(path, data, 0644)

		totalSlots := 0
		for _, cs := range bs.contracts {
			totalSlots += len(cs.Storage)
		}
		log.Printf("saved %d contracts (%d slots) to %s", len(bs.contracts), totalSlots, path)
		bs.mu.RUnlock()
	}
}

func (p *RecordingProxy) loadAllFromDisk() {
	entries, err := os.ReadDir(p.cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		blockHex := strings.TrimSuffix(e.Name(), ".json")
		bs := newBlockState()
		p.loadBlockFromDisk(blockHex, bs)
		if len(bs.contracts) > 0 {
			p.blocks[blockHex] = bs
		}
	}
}

func (p *RecordingProxy) loadBlockFromDisk(blockHex string, bs *BlockState) {
	path := p.blockCachePath(blockHex)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cf CacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		log.Printf("failed to parse cache %s: %v", path, err)
		return
	}

	totalSlots := 0
	for addr, cs := range cf.Contracts {
		if cs.Storage == nil {
			cs.Storage = make(map[string]string)
		}
		bs.contracts[addr] = cs
		totalSlots += len(cs.Storage)
	}
	log.Printf("loaded %d contracts (%d slots) from %s", len(cf.Contracts), totalSlots, path)
}
