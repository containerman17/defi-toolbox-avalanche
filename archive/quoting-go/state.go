package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/libevm/params"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"
)

// wsPoolConn is a blocking WebSocket connection: one request at a time, send-then-receive.
type wsPoolConn struct {
	conn *websocket.Conn
}

// WSClient is a pool of blocking WebSocket connections.
// Callers grab an idle connection, send a request, receive the response, then return it.
type WSClient struct {
	wsURL string
	pool  chan *wsPoolConn
}

var wsPoolSize = runtime.NumCPU()

func NewWSClient(wsURL string) *WSClient {
	c := &WSClient{
		wsURL: wsURL,
		pool:  make(chan *wsPoolConn, wsPoolSize),
	}

	for i := range wsPoolSize {
		conn := c.dialWithRetry(i)
		c.pool <- &wsPoolConn{conn: conn}
	}
	// Start pinger to keep connections alive
	go c.pingLoop()
	return c
}

func (c *WSClient) dialWithRetry(idx int) *websocket.Conn {
	var conn *websocket.Conn
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		conn, _, err = websocket.DefaultDialer.Dial(c.wsURL, nil)
		if err == nil {
			return conn
		}
		if idx == 0 || attempt > 0 {
			fmt.Fprintf(os.Stderr, "ws connect attempt %d (conn %d): %v\n", attempt+1, idx, err)
		}
		sleepMs(500)
	}
	panic(fmt.Sprintf("ws connect %s (conn %d) after retries: %v", c.wsURL, idx, err))
}

// pingLoop periodically pings all idle connections to keep them alive.
func (c *WSClient) pingLoop() {
	for {
		sleepMs(15000) // every 15 seconds
		// Try to grab and ping each connection, put back immediately
		pinged := 0
		for range wsPoolSize {
			select {
			case wc := <-c.pool:
				wc.conn.WriteMessage(websocket.PingMessage, nil)
				c.pool <- wc
				pinged++
			default:
				// Connection in use, skip
			}
		}
		_ = pinged
	}
}

func (c *WSClient) Call(method string, params ...interface{}) (string, error) {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	data, _ := json.Marshal(req)

	// Grab idle connection from pool
	wc := <-c.pool

	err := wc.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		// Reconnect
		fmt.Fprintf(os.Stderr, "ws write failed, reconnecting: %v\n", err)
		newConn := c.dialWithRetry(0)
		wc.conn.Close()
		wc.conn = newConn
		err = wc.conn.WriteMessage(websocket.TextMessage, data)
		if err != nil {
			c.pool <- wc
			return "", err
		}
	}

	_, msg, err := wc.conn.ReadMessage()
	if err != nil {
		// Reconnect and return error (caller will retry via EVM)
		fmt.Fprintf(os.Stderr, "ws read failed, reconnecting: %v\n", err)
		newConn := c.dialWithRetry(0)
		wc.conn.Close()
		wc.conn = newConn
		c.pool <- wc
		return "", fmt.Errorf("ws read: %v", err)
	}

	// Return connection to pool immediately
	c.pool <- wc

	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg, &resp); err != nil {
		return "", fmt.Errorf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("rpc error: %s", resp.Error.Message)
	}

	var s string
	if err := json.Unmarshal(resp.Result, &s); err != nil {
		return "", fmt.Errorf("unexpected result: %s", string(resp.Result))
	}
	return s, nil
}

func (c *WSClient) Close() {
	for range wsPoolSize {
		wc := <-c.pool
		wc.conn.Close()
	}
}

// SharedStateCache holds RPC-fetched data shared across all worker StateDBs.
// Uses sync.Map: lock-free reads, lock only on absent key (first fetch).
type SharedStateCache struct {
	ws       *WSClient
	blockNum uint64

	code    sync.Map // common.Address -> []byte
	storage sync.Map // storageKey -> common.Hash
	balance sync.Map // common.Address -> *uint256.Int
	nonce   sync.Map // common.Address -> uint64

	// singleflight-like dedup for concurrent fetches of same key
	fetching sync.Map // string -> *sync.Once
}

type storageKey struct {
	Addr common.Address
	Slot common.Hash
}

func NewSharedStateCache(ws *WSClient, blockNum uint64) *SharedStateCache {
	return &SharedStateCache{ws: ws, blockNum: blockNum}
}

func (c *SharedStateCache) Reset(blockNum uint64) {
	c.blockNum = blockNum
	c.code = sync.Map{}
	c.storage = sync.Map{}
	c.balance = sync.Map{}
	c.nonce = sync.Map{}
	c.fetching = sync.Map{}
}

func (c *SharedStateCache) GetCode(addr common.Address) []byte {
	if v, ok := c.code.Load(addr); ok {
		return v.([]byte)
	}
	// Fetch with dedup
	key := "code:" + addr.Hex()
	once, _ := c.fetching.LoadOrStore(key, &sync.Once{})
	var result []byte
	once.(*sync.Once).Do(func() {
		r, err := c.ws.Call("eth_getCode", addr.Hex(), fmt.Sprintf("0x%x", c.blockNum))
		if err != nil {
			panic(fmt.Sprintf("FATAL: fetch code %s: %v", addr.Hex(), err))
		}
		code, _ := hex.DecodeString(strings.TrimPrefix(r, "0x"))
		c.code.Store(addr, code)
		result = code
	})
	if v, ok := c.code.Load(addr); ok {
		return v.([]byte)
	}
	return result
}

func (c *SharedStateCache) GetStorage(addr common.Address, slot common.Hash) common.Hash {
	sk := storageKey{addr, slot}
	if v, ok := c.storage.Load(sk); ok {
		return v.(common.Hash)
	}
	key := "stor:" + addr.Hex() + slot.Hex()
	once, _ := c.fetching.LoadOrStore(key, &sync.Once{})
	var result common.Hash
	once.(*sync.Once).Do(func() {
		r, err := c.ws.Call("eth_getStorageAt", addr.Hex(), slot.Hex(), fmt.Sprintf("0x%x", c.blockNum))
		if err != nil {
			panic(fmt.Sprintf("FATAL: fetch storage %s[%s]: %v", addr.Hex(), slot.Hex(), err))
		}
		result = common.HexToHash(r)
		c.storage.Store(sk, result)
	})
	if v, ok := c.storage.Load(sk); ok {
		return v.(common.Hash)
	}
	return result
}

func (c *SharedStateCache) GetBalance(addr common.Address) *uint256.Int {
	if v, ok := c.balance.Load(addr); ok {
		return v.(*uint256.Int)
	}
	key := "bal:" + addr.Hex()
	once, _ := c.fetching.LoadOrStore(key, &sync.Once{})
	var result *uint256.Int
	once.(*sync.Once).Do(func() {
		r, err := c.ws.Call("eth_getBalance", addr.Hex(), fmt.Sprintf("0x%x", c.blockNum))
		if err != nil {
			panic(fmt.Sprintf("FATAL: fetch balance %s: %v", addr.Hex(), err))
		}
		bal := new(big.Int)
		bal.SetString(strings.TrimPrefix(r, "0x"), 16)
		result = uint256.MustFromBig(bal)
		c.balance.Store(addr, result)
	})
	if v, ok := c.balance.Load(addr); ok {
		return v.(*uint256.Int)
	}
	return result
}

func (c *SharedStateCache) GetNonce(addr common.Address) uint64 {
	if v, ok := c.nonce.Load(addr); ok {
		return v.(uint64)
	}
	key := "nonce:" + addr.Hex()
	once, _ := c.fetching.LoadOrStore(key, &sync.Once{})
	var result uint64
	once.(*sync.Once).Do(func() {
		r, err := c.ws.Call("eth_getTransactionCount", addr.Hex(), fmt.Sprintf("0x%x", c.blockNum))
		if err != nil {
			panic(fmt.Sprintf("FATAL: fetch nonce %s: %v", addr.Hex(), err))
		}
		fmt.Sscanf(r, "0x%x", &result)
		c.nonce.Store(addr, result)
	})
	if v, ok := c.nonce.Load(addr); ok {
		return v.(uint64)
	}
	return result
}

// LazyStateDB implements vm.StateDB with lazy loading from shared cache.
// Each worker gets its own LazyStateDB for mutable state, but they all
// share a SharedStateCache for RPC-fetched immutable chain state.
type LazyStateDB struct {
	cache    *SharedStateCache
	blockNum uint64

	// Per-instance mutable state (not shared)
	code          map[common.Address][]byte  // overrides (e.g. quoter bytecode)
	codeHash      map[common.Address]common.Hash
	storage       map[common.Address]map[common.Hash]common.Hash
	originStorage map[common.Address]map[common.Hash]common.Hash
	balance       map[common.Address]*uint256.Int
	nonce         map[common.Address]uint64
	exists        map[common.Address]bool

	// Access list tracking (EIP-2930)
	accessedAddrs map[common.Address]bool
	accessedSlots map[common.Address]map[common.Hash]bool

	// Transient storage (EIP-1153)
	transient map[common.Address]map[common.Hash]common.Hash

	// Snapshots for revert support
	snapshots []stateSnapshot

	// Refunds
	refund uint64

	// Logs
	logs   []*types.Log
	txHash common.Hash

	// Stats
	rpcCalls int
}

type stateSnapshot struct {
	storage   map[common.Address]map[common.Hash]common.Hash
	balance   map[common.Address]*uint256.Int
	nonce     map[common.Address]uint64
	transient map[common.Address]map[common.Hash]common.Hash
	refund    uint64
}

func NewLazyStateDB(cache *SharedStateCache, blockNum uint64) *LazyStateDB {
	return &LazyStateDB{
		cache:         cache,
		blockNum:      blockNum,
		code:          make(map[common.Address][]byte),
		codeHash:      make(map[common.Address]common.Hash),
		storage:       make(map[common.Address]map[common.Hash]common.Hash),
		originStorage: make(map[common.Address]map[common.Hash]common.Hash),
		balance:       make(map[common.Address]*uint256.Int),
		nonce:         make(map[common.Address]uint64),
		exists:        make(map[common.Address]bool),
		accessedAddrs: make(map[common.Address]bool),
		accessedSlots: make(map[common.Address]map[common.Hash]bool),
		transient:     make(map[common.Address]map[common.Hash]common.Hash),
	}
}

func (s *LazyStateDB) Reset(blockNum uint64) {
	s.blockNum = blockNum
	s.code = make(map[common.Address][]byte)
	s.codeHash = make(map[common.Address]common.Hash)
	s.storage = make(map[common.Address]map[common.Hash]common.Hash)
	s.originStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.balance = make(map[common.Address]*uint256.Int)
	s.nonce = make(map[common.Address]uint64)
	s.exists = make(map[common.Address]bool)
	s.accessedAddrs = make(map[common.Address]bool)
	s.accessedSlots = make(map[common.Address]map[common.Hash]bool)
	s.transient = make(map[common.Address]map[common.Hash]common.Hash)
	s.snapshots = nil
	s.refund = 0
	s.logs = nil
	s.rpcCalls = 0
}

// ResetMutable clears only mutable state (storage writes, balances, nonces, transient)
// but keeps the RPC cache (code, originStorage) intact.
func (s *LazyStateDB) ResetMutable() {
	// Restore storage to origin values
	s.storage = make(map[common.Address]map[common.Hash]common.Hash)
	for addr, slots := range s.originStorage {
		s.storage[addr] = make(map[common.Hash]common.Hash)
		for k, v := range slots {
			s.storage[addr][k] = v
		}
	}
	// Clear balances (will be re-fetched), but keep code cache
	s.balance = make(map[common.Address]*uint256.Int)
	s.nonce = make(map[common.Address]uint64)
	s.accessedAddrs = make(map[common.Address]bool)
	s.accessedSlots = make(map[common.Address]map[common.Hash]bool)
	s.transient = make(map[common.Address]map[common.Hash]common.Hash)
	s.snapshots = nil
	s.refund = 0
	s.logs = nil
}

func (s *LazyStateDB) RPCCalls() int {
	return s.rpcCalls
}

func (s *LazyStateDB) SetOverride(addr common.Address, slot, value common.Hash) {
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][slot] = value
}

func (s *LazyStateDB) SetBalanceOverride(addr common.Address, balance *big.Int) {
	s.balance[addr] = uint256.MustFromBig(balance)
}

func (s *LazyStateDB) SetBalanceOverrideU256(addr common.Address, balance *uint256.Int) {
	s.balance[addr] = balance
}

func (s *LazyStateDB) TxHash() common.Hash {
	return s.txHash
}

func (s *LazyStateDB) SetTxHash(hash common.Hash) {
	s.txHash = hash
}

// ============= vm.StateDB Interface Implementation =============

func (s *LazyStateDB) CreateAccount(addr common.Address) {
	s.exists[addr] = true
}

func (s *LazyStateDB) SubBalance(addr common.Address, amount *uint256.Int) {
	bal := s.GetBalance(addr)
	newBal := new(uint256.Int).Sub(bal, amount)
	s.balance[addr] = newBal
}

func (s *LazyStateDB) AddBalance(addr common.Address, amount *uint256.Int) {
	bal := s.GetBalance(addr)
	newBal := new(uint256.Int).Add(bal, amount)
	s.balance[addr] = newBal
}

func (s *LazyStateDB) GetBalance(addr common.Address) *uint256.Int {
	if bal, ok := s.balance[addr]; ok {
		return bal
	}
	bal := s.fetchBalance(addr)
	s.balance[addr] = bal
	return bal
}

func (s *LazyStateDB) GetNonce(addr common.Address) uint64 {
	if nonce, ok := s.nonce[addr]; ok {
		return nonce
	}
	nonce := s.fetchNonce(addr)
	s.nonce[addr] = nonce
	return nonce
}

func (s *LazyStateDB) SetNonce(addr common.Address, nonce uint64) {
	s.nonce[addr] = nonce
}

func (s *LazyStateDB) GetCodeHash(addr common.Address) common.Hash {
	if hash, ok := s.codeHash[addr]; ok {
		return hash
	}
	code := s.GetCode(addr)
	if len(code) == 0 {
		return common.Hash{}
	}
	hash := crypto.Keccak256Hash(code)
	s.codeHash[addr] = hash
	return hash
}

func (s *LazyStateDB) GetCode(addr common.Address) []byte {
	if code, ok := s.code[addr]; ok {
		return code
	}
	code := s.fetchCode(addr)
	s.code[addr] = code
	return code
}

func (s *LazyStateDB) SetCode(addr common.Address, code []byte) {
	s.code[addr] = code
	s.codeHash[addr] = crypto.Keccak256Hash(code)
}

func (s *LazyStateDB) GetCodeSize(addr common.Address) int {
	return len(s.GetCode(addr))
}

func (s *LazyStateDB) AddRefund(gas uint64) {
	s.refund += gas
}

func (s *LazyStateDB) SubRefund(gas uint64) {
	if gas > s.refund {
		panic("refund counter below zero")
	}
	s.refund -= gas
}

func (s *LazyStateDB) GetRefund() uint64 {
	return s.refund
}

func (s *LazyStateDB) GetCommittedState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	if slots, ok := s.originStorage[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	val := s.fetchStorage(addr, key)
	if s.originStorage[addr] == nil {
		s.originStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.originStorage[addr][key] = val
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	if _, exists := s.storage[addr][key]; !exists {
		s.storage[addr][key] = val
	}
	return val
}

func (s *LazyStateDB) GetState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	if slots, ok := s.storage[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	val := s.fetchStorage(addr, key)
	if s.originStorage[addr] == nil {
		s.originStorage[addr] = make(map[common.Hash]common.Hash)
	}
	if _, exists := s.originStorage[addr][key]; !exists {
		s.originStorage[addr][key] = val
	}
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = val
	return val
}

func (s *LazyStateDB) SetState(addr common.Address, key, value common.Hash, _ ...stateconf.StateDBStateOption) {
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = value
}

func (s *LazyStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.transient[addr]; ok {
		return slots[key]
	}
	return common.Hash{}
}

func (s *LazyStateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	if s.transient[addr] == nil {
		s.transient[addr] = make(map[common.Hash]common.Hash)
	}
	s.transient[addr][key] = value
}

func (s *LazyStateDB) SelfDestruct(addr common.Address) {
	s.balance[addr] = uint256.NewInt(0)
}

func (s *LazyStateDB) HasSelfDestructed(addr common.Address) bool {
	return false
}

func (s *LazyStateDB) Selfdestruct6780(addr common.Address) {}

func (s *LazyStateDB) Exist(addr common.Address) bool {
	if exists, ok := s.exists[addr]; ok {
		return exists
	}
	code := s.GetCode(addr)
	if len(code) > 0 {
		s.exists[addr] = true
		return true
	}
	bal := s.GetBalance(addr)
	if bal.Sign() > 0 {
		s.exists[addr] = true
		return true
	}
	nonce := s.GetNonce(addr)
	if nonce > 0 {
		s.exists[addr] = true
		return true
	}
	s.exists[addr] = false
	return false
}

func (s *LazyStateDB) Empty(addr common.Address) bool {
	return !s.Exist(addr)
}

func (s *LazyStateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessedAddrs[addr]
}

func (s *LazyStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool) {
	addressOk = s.accessedAddrs[addr]
	if slots, ok := s.accessedSlots[addr]; ok {
		slotOk = slots[slot]
	}
	return
}

func (s *LazyStateDB) AddAddressToAccessList(addr common.Address) {
	s.accessedAddrs[addr] = true
}

func (s *LazyStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	s.accessedAddrs[addr] = true
	if s.accessedSlots[addr] == nil {
		s.accessedSlots[addr] = make(map[common.Hash]bool)
	}
	s.accessedSlots[addr][slot] = true
}

func (s *LazyStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	s.accessedAddrs = make(map[common.Address]bool)
	s.accessedSlots = make(map[common.Address]map[common.Hash]bool)
	s.transient = make(map[common.Address]map[common.Hash]common.Hash)

	s.accessedAddrs[sender] = true
	if dest != nil {
		s.accessedAddrs[*dest] = true
	}
	s.accessedAddrs[coinbase] = true

	for _, addr := range precompiles {
		s.accessedAddrs[addr] = true
	}

	for _, el := range txAccesses {
		s.accessedAddrs[el.Address] = true
		for _, slot := range el.StorageKeys {
			if s.accessedSlots[el.Address] == nil {
				s.accessedSlots[el.Address] = make(map[common.Hash]bool)
			}
			s.accessedSlots[el.Address][slot] = true
		}
	}
}

func (s *LazyStateDB) RevertToSnapshot(revid int) {
	if revid < 0 || revid >= len(s.snapshots) {
		return
	}
	snap := s.snapshots[revid]

	s.storage = copyStorageMap(snap.storage)
	s.balance = copyBalanceMap(snap.balance)
	s.nonce = copyNonceMap(snap.nonce)
	s.transient = copyStorageMap(snap.transient)
	s.refund = snap.refund

	s.snapshots = s.snapshots[:revid]
}

func (s *LazyStateDB) Snapshot() int {
	snap := stateSnapshot{
		storage:   copyStorageMap(s.storage),
		balance:   copyBalanceMap(s.balance),
		nonce:     copyNonceMap(s.nonce),
		transient: copyStorageMap(s.transient),
		refund:    s.refund,
	}
	s.snapshots = append(s.snapshots, snap)
	return len(s.snapshots) - 1
}

func (s *LazyStateDB) AddLog(log *types.Log) {
	s.logs = append(s.logs, log)
}

func (s *LazyStateDB) AddPreimage(hash common.Hash, preimage []byte) {}

// ============= RPC Fetching (via shared cache) =============

func (s *LazyStateDB) fetchCode(addr common.Address) []byte {
	s.rpcCalls++
	return s.cache.GetCode(addr)
}

func (s *LazyStateDB) fetchBalance(addr common.Address) *uint256.Int {
	s.rpcCalls++
	return s.cache.GetBalance(addr)
}

func (s *LazyStateDB) fetchNonce(addr common.Address) uint64 {
	s.rpcCalls++
	return s.cache.GetNonce(addr)
}

func (s *LazyStateDB) fetchStorage(addr common.Address, slot common.Hash) common.Hash {
	s.rpcCalls++
	return s.cache.GetStorage(addr, slot)
}

func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// ============= Helper Functions =============

func copyStorageMap(m map[common.Address]map[common.Hash]common.Hash) map[common.Address]map[common.Hash]common.Hash {
	result := make(map[common.Address]map[common.Hash]common.Hash)
	for addr, slots := range m {
		result[addr] = make(map[common.Hash]common.Hash)
		for k, v := range slots {
			result[addr][k] = v
		}
	}
	return result
}

func copyBalanceMap(m map[common.Address]*uint256.Int) map[common.Address]*uint256.Int {
	result := make(map[common.Address]*uint256.Int)
	for k, v := range m {
		result[k] = new(uint256.Int).Set(v)
	}
	return result
}

func copyNonceMap(m map[common.Address]uint64) map[common.Address]uint64 {
	result := make(map[common.Address]uint64)
	for k, v := range m {
		result[k] = v
	}
	return result
}
