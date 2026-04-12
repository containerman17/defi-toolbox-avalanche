package statedb

import (
	"bytes"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corethcore "github.com/ava-labs/avalanchego/graft/coreth/core"
	cparams "github.com/ava-labs/avalanchego/graft/coreth/params"
	cextras "github.com/ava-labs/avalanchego/graft/coreth/params/extras"
	"github.com/ava-labs/avalanchego/graft/coreth/plugin/evm/customheader"
	ccustomtypes "github.com/ava-labs/avalanchego/graft/coreth/plugin/evm/customtypes"
	warpcontract "github.com/ava-labs/avalanchego/graft/coreth/precompile/contracts/warp"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/vms/evm/acp226"
	"github.com/ava-labs/avalanchego/vms/evm/predicate"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"
)

func init() {
	// Guard against double registration when both statedb and lightclient
	// are imported in the same binary.
	defer func() { recover() }()
	cparams.RegisterExtras()
	ccustomtypes.Register()
}

type ThinClientConfig struct {
	RPCURL               string
	PoolSize             int
	SnapshotPath         string
	MaxSnapshotStaleness uint64
	SnapshotEveryBlocks  uint64
}

func DefaultThinClientConfig() ThinClientConfig {
	return ThinClientConfig{
		RPCURL:               "ws://127.0.0.1:9650/ext/bc/C/ws",
		PoolSize:             runtime.NumCPU(),
		MaxSnapshotStaleness: 1000,
		SnapshotEveryBlocks:  25,
	}
}

type ThinClient struct {
	cfg               ThinClientConfig
	mu                sync.RWMutex
	pool              *rpcPool
	chainCfg          *params.ChainConfig
	fetcher           *thinFetcher
	state             *StateDB
	head              uint64
	timestamp         uint64
	baseFee           *big.Int
	gasLimit          uint64
	headHash          common.Hash
	lastSnapshotBlock uint64
	onBlock           func(*ThinClient, [][2]string)
}

type ThinSnapshot struct {
	Version     uint32
	BlockNumber uint64
	Timestamp   uint64
	BaseFee     []byte
	GasLimit    uint64
	Storage     []ThinSnapshotStorage
	Accounts    []ThinSnapshotAccount
}

type ThinSnapshotStorage struct {
	Address common.Address
	Slot    common.Hash
	Value   common.Hash
}

type ThinSnapshotAccount struct {
	Address    common.Address
	Balance    [32]byte
	HasBalance bool
	Nonce      uint64
	HasNonce   bool
	Code       []byte
	HasCode    bool
}

type ThinBlockStats struct {
	Block        uint64
	Txs          int
	LocalDiff    int
	TracedDiff   int
	ExecElapsed  time.Duration
	TotalElapsed time.Duration
	Fetches      fetchStats
}

type thinJSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type pendingRequest struct {
	ch chan rpcResult
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcSocket struct {
	url     string
	name    string
	conn    *websocket.Conn
	mu      sync.Mutex
	nextID  int64
	pending map[string]*pendingRequest
	ready   chan struct{}
	sem     chan struct{}
}

func newRPCSocket(url, name string) *rpcSocket {
	s := &rpcSocket{
		url:     url,
		name:    name,
		pending: make(map[string]*pendingRequest),
		ready:   make(chan struct{}),
		sem:     make(chan struct{}, 1),
	}
	s.sem <- struct{}{}
	go s.connect()
	return s
}

func (s *rpcSocket) connect() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial(s.url, nil)
		if err != nil {
			log.Printf("rpc socket %s connect error: %v", s.name, err)
			time.Sleep(250 * time.Millisecond)
			continue
		}

		s.mu.Lock()
		s.conn = conn
		select {
		case <-s.ready:
		default:
			close(s.ready)
		}
		s.mu.Unlock()

		s.readLoop(conn)

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
		if err := json.Unmarshal(msg, &resp); err != nil {
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
			p.ch <- rpcResult{err: fmt.Errorf("rpc %d: %s", resp.Error.Code, resp.Error.Message)}
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

	req := thinJSONRPCRequest{
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

func newRPCPool(url string, size int) *rpcPool {
	if size < 1 {
		size = 1
	}
	p := &rpcPool{sockets: make([]*rpcSocket, size)}
	for i := 0; i < size; i++ {
		p.sockets[i] = newRPCSocket(url, fmt.Sprintf("rpc-%d", i))
	}
	return p
}

func (p *rpcPool) call(method string, params interface{}) (json.RawMessage, error) {
	idx := p.next.Add(1) - 1
	return p.sockets[int(idx)%len(p.sockets)].send(method, params)
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

func (p *rpcPool) ethBlockNumber() (uint64, error) {
	s, err := p.callString("eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}

func (p *rpcPool) ethGetStorageAt(address, slot string, block uint64) (string, error) {
	return p.callString("eth_getStorageAt", []interface{}{address, slot, blockHex(block)})
}

func (p *rpcPool) ethGetBalance(address string, block uint64) (string, error) {
	return p.callString("eth_getBalance", []interface{}{address, blockHex(block)})
}

func (p *rpcPool) ethGetTransactionCount(address string, block uint64) (string, error) {
	return p.callString("eth_getTransactionCount", []interface{}{address, blockHex(block)})
}

func (p *rpcPool) ethGetCode(address string, block uint64) (string, error) {
	return p.callString("eth_getCode", []interface{}{address, blockHex(block)})
}

func (p *rpcPool) ethGetChainConfig() (*params.ChainConfig, error) {
	raw, err := p.call("eth_getChainConfig", []interface{}{})
	if err != nil {
		return nil, err
	}
	var cfg cparams.ChainConfigWithUpgradesJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	extra := cparams.GetExtra(&cfg.ChainConfig)
	extra.UpgradeConfig = cfg.UpgradeConfig
	if extra.DurangoBlockTimestamp != nil && !hasPrecompileUpgrade(extra.UpgradeConfig, warpcontract.ConfigKey) {
		extra.PrecompileUpgrades = append(extra.PrecompileUpgrades, cextras.PrecompileUpgrade{
			Config: warpcontract.NewDefaultConfig(extra.DurangoBlockTimestamp),
		})
	}
	if err := cparams.SetEthUpgrades(&cfg.ChainConfig); err != nil {
		return nil, err
	}
	return &cfg.ChainConfig, nil
}

func hasPrecompileUpgrade(cfg cextras.UpgradeConfig, key string) bool {
	for _, upgrade := range cfg.PrecompileUpgrades {
		if upgrade.Config != nil && upgrade.Key() == key {
			return true
		}
	}
	return false
}

func deriveInfoAPIURL(rpcURL string) (string, error) {
	parsed, err := url.Parse(rpcURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	parsed.Path = "/ext/info"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func infoAPICall(infoURL, method string, params interface{}, out interface{}) error {
	payload, err := json.Marshal(thinJSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, infoURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode info api response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("info api rpc %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return json.Unmarshal(envelope.Result, out)
}

func fetchSnowContext(infoURL string) (*snow.Context, error) {
	var networkResp struct {
		NetworkID string `json:"networkID"`
	}
	if err := infoAPICall(infoURL, "info.getNetworkID", map[string]interface{}{}, &networkResp); err != nil {
		return nil, err
	}
	networkID, err := strconv.ParseUint(networkResp.NetworkID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse network id %q: %w", networkResp.NetworkID, err)
	}

	var chainResp struct {
		BlockchainID string `json:"blockchainID"`
	}
	if err := infoAPICall(infoURL, "info.getBlockchainID", map[string]interface{}{"alias": "C"}, &chainResp); err != nil {
		return nil, err
	}
	chainID, err := ids.FromString(chainResp.BlockchainID)
	if err != nil {
		return nil, fmt.Errorf("parse blockchain id %q: %w", chainResp.BlockchainID, err)
	}

	return &snow.Context{
		NetworkID: uint32(networkID),
		ChainID:   chainID,
		CChainID:  chainID,
	}, nil
}

type rpcBlock struct {
	Hash           common.Hash
	ParentHash     common.Hash
	Number         *big.Int
	Time           uint64
	TimeMS         *uint64
	MinDelayExcess *uint64
	Coinbase       common.Address
	Difficulty     *big.Int
	GasLimit       uint64
	BaseFee        *big.Int
	MixDigest      common.Hash
	Extra          []byte
	Transactions   []*types.Transaction
}

type rpcBlockPayload struct {
	Hash             common.Hash       `json:"hash"`
	ParentHash       common.Hash       `json:"parentHash"`
	Number           *hexutil.Big      `json:"number"`
	Time             hexutil.Uint64    `json:"timestamp"`
	TimeMilliseconds *hexutil.Uint64   `json:"timestampMilliseconds"`
	MinDelayExcess   *hexutil.Uint64   `json:"minDelayExcess"`
	Coinbase         common.Address    `json:"miner"`
	Difficulty       *hexutil.Big      `json:"difficulty"`
	GasLimit         hexutil.Uint64    `json:"gasLimit"`
	BaseFee          *hexutil.Big      `json:"baseFeePerGas"`
	MixDigest        common.Hash       `json:"mixHash"`
	Extra            hexutil.Bytes     `json:"extraData"`
	Transactions     []json.RawMessage `json:"transactions"`
}

func (p *rpcPool) ethGetBlockByNumber(block uint64, full bool) (*rpcBlock, error) {
	raw, err := p.call("eth_getBlockByNumber", []interface{}{blockHex(block), full})
	if err != nil {
		return nil, err
	}
	var payload rpcBlockPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	result := &rpcBlock{
		Hash:       payload.Hash,
		ParentHash: payload.ParentHash,
		Time:       uint64(payload.Time),
		Coinbase:   payload.Coinbase,
		GasLimit:   uint64(payload.GasLimit),
		MixDigest:  payload.MixDigest,
		Extra:      bytes.Clone(payload.Extra),
	}
	if payload.TimeMilliseconds != nil {
		t := uint64(*payload.TimeMilliseconds)
		result.TimeMS = &t
	}
	if payload.MinDelayExcess != nil {
		d := uint64(*payload.MinDelayExcess)
		result.MinDelayExcess = &d
	}
	if payload.Number != nil {
		result.Number = (*big.Int)(payload.Number)
	}
	if payload.Difficulty != nil {
		result.Difficulty = (*big.Int)(payload.Difficulty)
	} else {
		result.Difficulty = big.NewInt(0)
	}
	if payload.BaseFee != nil {
		result.BaseFee = (*big.Int)(payload.BaseFee)
	} else {
		result.BaseFee = big.NewInt(0)
	}
	if full && len(payload.Transactions) > 0 {
		result.Transactions = make([]*types.Transaction, 0, len(payload.Transactions))
		for _, txRaw := range payload.Transactions {
			if bytes.Equal(txRaw, []byte("null")) {
				continue
			}
			var tx types.Transaction
			if err := json.Unmarshal(txRaw, &tx); err != nil {
				return nil, fmt.Errorf("decode block tx: %w", err)
			}
			result.Transactions = append(result.Transactions, &tx)
		}
	}
	return result, nil
}

type tracedAccount struct {
	Balance *string           `json:"balance"`
	Nonce   *json.Number      `json:"nonce"`
	Code    *string           `json:"code"`
	Storage map[string]string `json:"storage"`
}

type tracedTxResult struct {
	Pre  map[string]tracedAccount `json:"pre"`
	Post map[string]tracedAccount `json:"post"`
}

type tracedBlockTx struct {
	TxHash common.Hash    `json:"txHash"`
	Result tracedTxResult `json:"result"`
}

func parseTracedTxDiff(result tracedTxResult) *blockDiff {
	diff := newBlockDiff()
	for address, account := range result.Post {
		addr := common.HexToAddress(address)
		pre := result.Pre[address]
		if account.Balance != nil {
			if pre.Balance == nil || *pre.Balance != *account.Balance {
				diff.balance[addr] = common.HexToHash(*account.Balance)
			}
		}
		if account.Nonce != nil {
			n, err := strconv.ParseUint(account.Nonce.String(), 10, 64)
			if err == nil {
				preNonce := uint64(0)
				if pre.Nonce != nil {
					preNonce, _ = strconv.ParseUint(pre.Nonce.String(), 10, 64)
				}
				if pre.Nonce == nil || preNonce != n {
					diff.nonce[addr] = n
				}
			}
		}
		if account.Code != nil {
			if pre.Code == nil || *pre.Code != *account.Code {
				code := strings.TrimPrefix(*account.Code, "0x")
				b, _ := hex.DecodeString(code)
				diff.code[addr] = b
			}
		}
		for slot, value := range account.Storage {
			if pre.Storage != nil {
				if preValue, ok := pre.Storage[slot]; ok && preValue == value {
					continue
				}
			}
			if diff.storage[addr] == nil {
				diff.storage[addr] = make(map[common.Hash]common.Hash)
			}
			diff.storage[addr][common.HexToHash(slot)] = common.HexToHash(value)
		}
	}
	for address, pre := range result.Pre {
		addr := common.HexToAddress(address)
		post := result.Post[address]
		if pre.Balance != nil && post.Balance == nil {
			diff.balance[addr] = common.Hash{}
		}
		if pre.Nonce != nil && post.Nonce == nil {
			diff.nonce[addr] = 0
		}
		if pre.Code != nil && post.Code == nil {
			diff.code[addr] = nil
		}
		for slot := range pre.Storage {
			if post.Storage != nil {
				if _, ok := post.Storage[slot]; ok {
					continue
				}
			}
			if diff.storage[addr] == nil {
				diff.storage[addr] = make(map[common.Hash]common.Hash)
			}
			diff.storage[addr][common.HexToHash(slot)] = common.Hash{}
		}
	}
	return diff
}

func traceBlockDiffWS(pool *rpcPool, block uint64) (*blockDiff, error) {
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

	diff := newBlockDiff()
	for _, txRaw := range txResults {
		var tx tracedBlockTx
		if err := json.Unmarshal(txRaw, &tx); err != nil {
			continue
		}
		txDiff := parseTracedTxDiff(tx.Result)
		mergeBlockDiff(diff, txDiff)
	}
	return filterTraceCandidates(pool, block, diff)
}

func mergeBlockDiff(dst, src *blockDiff) {
	for addr, slots := range src.storage {
		if dst.storage[addr] == nil {
			dst.storage[addr] = make(map[common.Hash]common.Hash)
		}
		for slot, value := range slots {
			dst.storage[addr][slot] = value
		}
	}
	for addr, value := range src.balance {
		dst.balance[addr] = value
	}
	for addr, value := range src.nonce {
		dst.nonce[addr] = value
	}
	for addr, value := range src.code {
		dst.code[addr] = value
	}
}

func traceBlockTxDiffWS(pool *rpcPool, block uint64, txIndex int) (*blockDiff, error) {
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
	if txIndex < 0 || txIndex >= len(txResults) {
		return nil, fmt.Errorf("tx index %d out of range for block %d", txIndex, block)
	}
	var tx tracedBlockTx
	if err := json.Unmarshal(txResults[txIndex], &tx); err != nil {
		return nil, fmt.Errorf("parse traced tx result: %w", err)
	}
	return parseTracedTxDiff(tx.Result), nil
}

func filterTraceCandidates(pool *rpcPool, block uint64, candidate *blockDiff) (*blockDiff, error) {
	if block == 0 {
		return candidate, nil
	}
	parent := block - 1
	filtered := newBlockDiff()

	for addr, slots := range candidate.storage {
		for slot := range slots {
			parentValue, err := pool.ethGetStorageAt(addr.Hex(), slot.Hex(), parent)
			if err != nil {
				return nil, err
			}
			currentValue, err := pool.ethGetStorageAt(addr.Hex(), slot.Hex(), block)
			if err != nil {
				return nil, err
			}
			if currentValue == parentValue {
				continue
			}
			if filtered.storage[addr] == nil {
				filtered.storage[addr] = make(map[common.Hash]common.Hash)
			}
			filtered.storage[addr][slot] = common.HexToHash(currentValue)
		}
	}

	for addr := range candidate.balance {
		parentValue, err := pool.ethGetBalance(addr.Hex(), parent)
		if err != nil {
			return nil, err
		}
		currentValue, err := pool.ethGetBalance(addr.Hex(), block)
		if err != nil {
			return nil, err
		}
		if currentValue == parentValue {
			continue
		}
		filtered.balance[addr] = common.HexToHash(currentValue)
	}

	for addr := range candidate.nonce {
		parentValue, err := pool.ethGetTransactionCount(addr.Hex(), parent)
		if err != nil {
			return nil, err
		}
		currentValue, err := pool.ethGetTransactionCount(addr.Hex(), block)
		if err != nil {
			return nil, err
		}
		if currentValue == parentValue {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(currentValue, "0x"), 16, 64)
		if err != nil {
			return nil, err
		}
		filtered.nonce[addr] = n
	}

	for addr := range candidate.code {
		parentValue, err := pool.ethGetCode(addr.Hex(), parent)
		if err != nil {
			return nil, err
		}
		currentValue, err := pool.ethGetCode(addr.Hex(), block)
		if err != nil {
			return nil, err
		}
		if currentValue == parentValue {
			continue
		}
		code := strings.TrimPrefix(currentValue, "0x")
		b, err := hex.DecodeString(code)
		if err != nil {
			return nil, err
		}
		filtered.code[addr] = b
	}

	return filtered, nil
}

func pruneLocalNoops(pool *rpcPool, block uint64, local *blockDiff) (*blockDiff, error) {
	if block == 0 {
		return local, nil
	}
	parent := block - 1
	pruned := newBlockDiff()

	for addr, slots := range local.storage {
		for slot, value := range slots {
			parentValue, err := pool.ethGetStorageAt(addr.Hex(), slot.Hex(), parent)
			if err != nil {
				return nil, err
			}
			currentValue, err := pool.ethGetStorageAt(addr.Hex(), slot.Hex(), block)
			if err != nil {
				return nil, err
			}
			if currentValue == parentValue {
				continue
			}
			if pruned.storage[addr] == nil {
				pruned.storage[addr] = make(map[common.Hash]common.Hash)
			}
			pruned.storage[addr][slot] = value
		}
	}

	for addr, value := range local.balance {
		parentValue, err := pool.ethGetBalance(addr.Hex(), parent)
		if err != nil {
			return nil, err
		}
		currentValue, err := pool.ethGetBalance(addr.Hex(), block)
		if err != nil {
			return nil, err
		}
		if currentValue == parentValue {
			continue
		}
		pruned.balance[addr] = value
	}

	for addr, value := range local.nonce {
		parentValue, err := pool.ethGetTransactionCount(addr.Hex(), parent)
		if err != nil {
			return nil, err
		}
		currentValue, err := pool.ethGetTransactionCount(addr.Hex(), block)
		if err != nil {
			return nil, err
		}
		if currentValue == parentValue {
			continue
		}
		pruned.nonce[addr] = value
	}

	for addr, value := range local.code {
		parentValue, err := pool.ethGetCode(addr.Hex(), parent)
		if err != nil {
			return nil, err
		}
		currentValue, err := pool.ethGetCode(addr.Hex(), block)
		if err != nil {
			return nil, err
		}
		if currentValue == parentValue {
			continue
		}
		pruned.code[addr] = append([]byte(nil), value...)
	}

	return pruned, nil
}

type thinFetcher struct {
	pool           *rpcPool
	baseBlock      atomic.Uint64
	blockHashMu    sync.RWMutex
	blockHashByNum map[uint64]common.Hash
	storageFetches atomic.Uint64
	balanceFetches atomic.Uint64
	nonceFetches   atomic.Uint64
	codeFetches    atomic.Uint64
	blockFetches   atomic.Uint64
}

func newThinFetcher(pool *rpcPool, baseBlock uint64) *thinFetcher {
	f := &thinFetcher{
		pool:           pool,
		blockHashByNum: make(map[uint64]common.Hash),
	}
	f.baseBlock.Store(baseBlock)
	return f
}

func (f *thinFetcher) SetBaseBlock(block uint64) {
	f.baseBlock.Store(block)
}

func (f *thinFetcher) BaseBlock() uint64 {
	return f.baseBlock.Load()
}

func (f *thinFetcher) SeedBlockHash(block uint64, hash common.Hash) {
	f.blockHashMu.Lock()
	f.blockHashByNum[block] = hash
	f.blockHashMu.Unlock()
}

func (f *thinFetcher) FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error) {
	f.storageFetches.Add(1)
	value, err := f.pool.ethGetStorageAt(addr.Hex(), slot.Hex(), f.BaseBlock())
	if err != nil {
		return common.Hash{}, err
	}
	return common.HexToHash(value), nil
}

func (f *thinFetcher) FetchBalance(addr common.Address) (*uint256.Int, error) {
	f.balanceFetches.Add(1)
	value, err := f.pool.ethGetBalance(addr.Hex(), f.BaseBlock())
	if err != nil {
		return nil, err
	}
	return uint256FromHex(value)
}

func (f *thinFetcher) FetchNonce(addr common.Address) (uint64, error) {
	f.nonceFetches.Add(1)
	value, err := f.pool.ethGetTransactionCount(addr.Hex(), f.BaseBlock())
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 64)
}

func (f *thinFetcher) FetchCode(addr common.Address) ([]byte, error) {
	f.codeFetches.Add(1)
	value, err := f.pool.ethGetCode(addr.Hex(), f.BaseBlock())
	if err != nil {
		return nil, err
	}
	if value == "0x" || value == "" {
		return nil, nil
	}
	return hex.DecodeString(strings.TrimPrefix(value, "0x"))
}

func (f *thinFetcher) FetchBlockHash(num uint64) (common.Hash, error) {
	f.blockHashMu.RLock()
	hash, ok := f.blockHashByNum[num]
	f.blockHashMu.RUnlock()
	if ok {
		return hash, nil
	}

	f.blockFetches.Add(1)
	blk, err := f.pool.ethGetBlockByNumber(num, false)
	if err != nil {
		return common.Hash{}, err
	}
	f.SeedBlockHash(num, blk.Hash)
	return blk.Hash, nil
}

type fetchStats struct {
	Storage uint64
	Balance uint64
	Nonce   uint64
	Code    uint64
	Block   uint64
}

func (s fetchStats) Total() uint64 {
	return s.Storage + s.Balance + s.Nonce + s.Code + s.Block
}

func (f *thinFetcher) SnapshotStats() fetchStats {
	return fetchStats{
		Storage: f.storageFetches.Load(),
		Balance: f.balanceFetches.Load(),
		Nonce:   f.nonceFetches.Load(),
		Code:    f.codeFetches.Load(),
		Block:   f.blockFetches.Load(),
	}
}

func (f *thinFetcher) StatsDelta(before fetchStats) fetchStats {
	after := f.SnapshotStats()
	return fetchStats{
		Storage: after.Storage - before.Storage,
		Balance: after.Balance - before.Balance,
		Nonce:   after.Nonce - before.Nonce,
		Code:    after.Code - before.Code,
		Block:   after.Block - before.Block,
	}
}

func (f *thinFetcher) GetHashForBlock(currentBlock, num uint64) common.Hash {
	if num >= currentBlock || currentBlock-num > 256 {
		return common.Hash{}
	}
	hash, err := f.FetchBlockHash(num)
	if err != nil {
		return common.Hash{}
	}
	return hash
}

type storageKey struct {
	addr common.Address
	slot common.Hash
}

type storageChange struct {
	pre  common.Hash
	post common.Hash
}

type balanceChange struct {
	pre  common.Hash
	post common.Hash
}

type nonceChange struct {
	pre  uint64
	post uint64
}

type codeChange struct {
	pre  []byte
	post []byte
}

type diffTracker struct {
	storage map[storageKey]storageChange
	balance map[common.Address]balanceChange
	nonce   map[common.Address]nonceChange
	code    map[common.Address]codeChange
}

func newDiffTracker() *diffTracker {
	return &diffTracker{
		storage: make(map[storageKey]storageChange),
		balance: make(map[common.Address]balanceChange),
		nonce:   make(map[common.Address]nonceChange),
		code:    make(map[common.Address]codeChange),
	}
}

func (t *diffTracker) record(state *StateDB, cs *CallState) {
	for addr, slots := range cs.StorageOverrides() {
		for slot, newValue := range slots {
			key := storageKey{addr: addr, slot: slot}
			change, ok := t.storage[key]
			if !ok {
				change.pre = state.GetState(addr, slot)
			}
			change.post = newValue
			t.storage[key] = change
		}
	}

	for addr, newBalance := range cs.BalanceOverrides() {
		change, ok := t.balance[addr]
		if !ok {
			change.pre = uint256ToHash(state.GetBalance(addr))
		}
		change.post = uint256ToHash(newBalance)
		t.balance[addr] = change
	}

	for addr, newNonce := range cs.NonceOverrides() {
		change, ok := t.nonce[addr]
		if !ok {
			change.pre = state.GetNonce(addr)
		}
		change.post = newNonce
		t.nonce[addr] = change
	}

	for addr, newCode := range cs.CodeOverrides() {
		change, ok := t.code[addr]
		if !ok {
			change.pre = state.GetCode(addr)
		}
		change.post = append([]byte(nil), newCode...)
		t.code[addr] = change
	}
}

func (t *diffTracker) Finalize() *blockDiff {
	diff := newBlockDiff()
	for key, change := range t.storage {
		if change.pre == change.post {
			continue
		}
		if diff.storage[key.addr] == nil {
			diff.storage[key.addr] = make(map[common.Hash]common.Hash)
		}
		diff.storage[key.addr][key.slot] = change.post
	}
	for addr, change := range t.balance {
		if change.pre == change.post {
			continue
		}
		diff.balance[addr] = change.post
	}
	for addr, change := range t.nonce {
		if change.pre == change.post {
			continue
		}
		diff.nonce[addr] = change.post
	}
	for addr, change := range t.code {
		if bytes.Equal(change.pre, change.post) {
			continue
		}
		diff.code[addr] = append([]byte(nil), change.post...)
	}
	return diff
}

type blockDiff struct {
	storage map[common.Address]map[common.Hash]common.Hash
	balance map[common.Address]common.Hash
	nonce   map[common.Address]uint64
	code    map[common.Address][]byte
}

func newBlockDiff() *blockDiff {
	return &blockDiff{
		storage: make(map[common.Address]map[common.Hash]common.Hash),
		balance: make(map[common.Address]common.Hash),
		nonce:   make(map[common.Address]uint64),
		code:    make(map[common.Address][]byte),
	}
}

func (d *blockDiff) Size() int {
	total := len(d.balance) + len(d.nonce) + len(d.code)
	for _, slots := range d.storage {
		total += len(slots)
	}
	return total
}

func compareDiffs(local, traced *blockDiff) error {
	if err := compareStorageDiffs(local, traced); err != nil {
		return err
	}
	if err := compareBalanceDiffs(local, traced); err != nil {
		return err
	}
	if err := compareNonceDiffs(local, traced); err != nil {
		return err
	}
	return compareCodeDiffs(local, traced)
}

func compareStorageDiffs(local, traced *blockDiff) error {
	for addr, slots := range local.storage {
		for slot, localValue := range slots {
			tracedSlots := traced.storage[addr]
			if tracedSlots == nil {
				return fmt.Errorf("storage missing in trace: addr=%s slot=%s local=%s", addr.Hex(), slot.Hex(), localValue.Hex())
			}
			tracedValue, ok := tracedSlots[slot]
			if !ok {
				return fmt.Errorf("storage missing in trace: addr=%s slot=%s local=%s", addr.Hex(), slot.Hex(), localValue.Hex())
			}
			if tracedValue != localValue {
				return fmt.Errorf("storage mismatch: addr=%s slot=%s local=%s traced=%s", addr.Hex(), slot.Hex(), localValue.Hex(), tracedValue.Hex())
			}
		}
	}
	for addr, slots := range traced.storage {
		for slot, tracedValue := range slots {
			localSlots := local.storage[addr]
			if localSlots == nil {
				return fmt.Errorf("storage missing locally: addr=%s slot=%s traced=%s", addr.Hex(), slot.Hex(), tracedValue.Hex())
			}
			if _, ok := localSlots[slot]; !ok {
				return fmt.Errorf("storage missing locally: addr=%s slot=%s traced=%s", addr.Hex(), slot.Hex(), tracedValue.Hex())
			}
		}
	}
	return nil
}

func compareBalanceDiffs(local, traced *blockDiff) error {
	for addr, localValue := range local.balance {
		tracedValue, ok := traced.balance[addr]
		if !ok {
			return fmt.Errorf("balance missing in trace: addr=%s local=%s", addr.Hex(), localValue.Hex())
		}
		if tracedValue != localValue {
			return fmt.Errorf("balance mismatch: addr=%s local=%s traced=%s", addr.Hex(), localValue.Hex(), tracedValue.Hex())
		}
	}
	for addr, tracedValue := range traced.balance {
		if _, ok := local.balance[addr]; !ok {
			return fmt.Errorf("balance missing locally: addr=%s traced=%s", addr.Hex(), tracedValue.Hex())
		}
	}
	return nil
}

func compareNonceDiffs(local, traced *blockDiff) error {
	for addr, localValue := range local.nonce {
		tracedValue, ok := traced.nonce[addr]
		if !ok {
			return fmt.Errorf("nonce missing in trace: addr=%s local=%d", addr.Hex(), localValue)
		}
		if tracedValue != localValue {
			return fmt.Errorf("nonce mismatch: addr=%s local=%d traced=%d", addr.Hex(), localValue, tracedValue)
		}
	}
	for addr, tracedValue := range traced.nonce {
		if _, ok := local.nonce[addr]; !ok {
			return fmt.Errorf("nonce missing locally: addr=%s traced=%d", addr.Hex(), tracedValue)
		}
	}
	return nil
}

func compareCodeDiffs(local, traced *blockDiff) error {
	for addr, localValue := range local.code {
		tracedValue, ok := traced.code[addr]
		if !ok {
			return fmt.Errorf("code missing in trace: addr=%s local=%s", addr.Hex(), hex.EncodeToString(localValue))
		}
		if !bytes.Equal(tracedValue, localValue) {
			return fmt.Errorf("code mismatch: addr=%s local=%s traced=%s", addr.Hex(), hex.EncodeToString(localValue), hex.EncodeToString(tracedValue))
		}
	}
	for addr, tracedValue := range traced.code {
		if _, ok := local.code[addr]; !ok {
			return fmt.Errorf("code missing locally: addr=%s traced=%s", addr.Hex(), hex.EncodeToString(tracedValue))
		}
	}
	return nil
}

func diffEntries(diff *blockDiff) [][2]string {
	entries := make([][2]string, 0, diff.Size())
	for addr, slots := range diff.storage {
		for slot, value := range slots {
			entries = append(entries, [2]string{
				fmt.Sprintf("s:%s:%s", addr.Hex(), slot.Hex()),
				value.Hex(),
			})
		}
	}
	for addr, value := range diff.balance {
		entries = append(entries, [2]string{
			"b:" + addr.Hex(),
			value.Hex(),
		})
	}
	for addr, value := range diff.nonce {
		entries = append(entries, [2]string{
			"n:" + addr.Hex(),
			blockHex(value),
		})
	}
	for addr, value := range diff.code {
		entries = append(entries, [2]string{
			"c:" + addr.Hex(),
			"0x" + hex.EncodeToString(value),
		})
	}
	return entries
}

func blockHex(n uint64) string {
	return fmt.Sprintf("0x%x", n)
}

func uint256FromHex(value string) (*uint256.Int, error) {
	trimmed := strings.TrimPrefix(value, "0x")
	if trimmed == "" {
		return uint256.NewInt(0), nil
	}
	bi, ok := new(big.Int).SetString(trimmed, 16)
	if !ok {
		return nil, fmt.Errorf("invalid hex uint256: %s", value)
	}
	out, overflow := uint256.FromBig(bi)
	if overflow {
		return nil, fmt.Errorf("uint256 overflow: %s", value)
	}
	return out, nil
}

func uint256ToHash(v *uint256.Int) common.Hash {
	if v == nil {
		return common.Hash{}
	}
	var out common.Hash
	v.WriteToSlice(out[:])
	return out
}

func mustBaseFee(baseFee *big.Int) *big.Int {
	if baseFee != nil {
		return new(big.Int).Set(baseFee)
	}
	return big.NewInt(0)
}

func executeBlock(fetcher *thinFetcher, chainCfg *params.ChainConfig, state *StateDB, block *rpcBlock) (*blockDiff, error) {
	tracker := newDiffTracker()
	blockNum := block.Number.Uint64()
	signer := types.MakeSigner(chainCfg, block.Number, block.Time)
	rules := chainCfg.Rules(block.Number, cparams.IsMergeTODO, block.Time)
	baseFee := mustBaseFee(block.BaseFee)
	blockHashGetter := func(n uint64) common.Hash {
		return fetcher.GetHashForBlock(blockNum, n)
	}
	blockDifficulty := new(big.Int).Set(block.Difficulty)
	blockRandom := block.MixDigest
	if rules.IsShanghai {
		blockRandom.SetBytes(blockDifficulty.Bytes())
		blockDifficulty = new(big.Int)
	}
	header := &types.Header{
		ParentHash: block.ParentHash,
		Number:     new(big.Int).Set(block.Number),
		Time:       block.Time,
		Difficulty: new(big.Int).Set(block.Difficulty),
		GasLimit:   block.GasLimit,
		BaseFee:    new(big.Int).Set(baseFee),
		Extra:      bytes.Clone(block.Extra),
		MixDigest:  block.MixDigest,
		Coinbase:   block.Coinbase,
	}
	headerExtra := &ccustomtypes.HeaderExtra{
		TimeMilliseconds: block.TimeMS,
	}
	if block.MinDelayExcess != nil {
		delay := acp226.DelayExcess(*block.MinDelayExcess)
		headerExtra.MinDelayExcess = &delay
	}
	ccustomtypes.SetHeaderExtra(header, headerExtra)
	blockCtx := vm.BlockContext{
		CanTransfer: func(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
			return db.GetBalance(addr).Cmp(amount) >= 0
		},
		Transfer: func(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
			db.SubBalance(sender, amount)
			db.AddBalance(recipient, amount)
		},
		GetHash:     blockHashGetter,
		Coinbase:    block.Coinbase,
		BlockNumber: new(big.Int).Set(block.Number),
		Time:        block.Time,
		Difficulty:  blockDifficulty,
		Random:      &blockRandom,
		GasLimit:    block.GasLimit,
		BaseFee:     baseFee,
		Header:      header,
	}
	gp := new(corethcore.GasPool).AddGas(block.GasLimit)

	for idx, tx := range block.Transactions {
		msg, err := corethcore.TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			return nil, fmt.Errorf("tx %d message conversion: %w", idx, err)
		}

		cs := NewCallState(state)
		cs.SetTxContext(tx.Hash(), idx)
		evm := vm.NewEVM(blockCtx, corethcore.NewEVMTxContext(msg), cs, chainCfg, vm.Config{})
		result, err := corethcore.ApplyMessage(evm, msg, gp)
		if err != nil {
			return nil, fmt.Errorf("tx %d apply failed: %w", idx, err)
		}
		if fetchErr := cs.Err(); fetchErr != nil {
			return nil, fmt.Errorf("tx %d fetch failed: %w", idx, fetchErr)
		}

		tracker.record(state, cs)
		cs.CommitToBase()

		if result.Failed() {
			log.Printf("tx %d reverted: %v", idx, result.Err)
		}
	}

	return tracker.Finalize(), nil
}

func retryBlockOperation[T any](block uint64, label string, fn func() (T, error)) (T, error) {
	var zero T
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		if time.Now().After(deadline) {
			return zero, fmt.Errorf("%s block %d: %w", label, block, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func publishHead(updateCh chan uint64, block uint64) {
	select {
	case updateCh <- block:
	default:
		select {
		case <-updateCh:
		default:
		}
		updateCh <- block
	}
}

func subscribeNewHeads(wsURL string, updateCh chan uint64) {
	for {
		func() {
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				log.Printf("newHeads connect error: %v", err)
				time.Sleep(time.Second)
				return
			}
			defer conn.Close()

			subReq, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "eth_subscribe",
				"params":  []string{"newHeads"},
			})
			if err := conn.WriteMessage(websocket.TextMessage, subReq); err != nil {
				log.Printf("newHeads subscribe write error: %v", err)
				return
			}

			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Printf("newHeads subscribe read error: %v", err)
				return
			}
			var subResp struct {
				Result string    `json:"result"`
				Error  *rpcError `json:"error"`
			}
			if err := json.Unmarshal(msg, &subResp); err != nil || subResp.Error != nil {
				log.Printf("newHeads subscribe failed: %s", string(msg))
				return
			}
			log.Printf("subscribed to newHeads: %s", subResp.Result)

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
				if err := json.Unmarshal(msg, &notif); err != nil {
					continue
				}
				if notif.Params.Result.Number == "" {
					continue
				}
				n, err := strconv.ParseUint(strings.TrimPrefix(notif.Params.Result.Number, "0x"), 16, 64)
				if err != nil {
					continue
				}
				publishHead(updateCh, n)
			}
		}()
		time.Sleep(time.Second)
	}
}

func formatAddresses(diff *blockDiff) []string {
	seen := make(map[string]struct{})
	for addr := range diff.balance {
		seen[addr.Hex()] = struct{}{}
	}
	for addr := range diff.nonce {
		seen[addr.Hex()] = struct{}{}
	}
	for addr := range diff.code {
		seen[addr.Hex()] = struct{}{}
	}
	for addr := range diff.storage {
		seen[addr.Hex()] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for addr := range seen {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

func debugReplayBlock(pool *rpcPool, fetcher *thinFetcher, chainCfg *params.ChainConfig, state *StateDB, blockNum uint64, debugTxIndex int) error {
	fetcher.SetBaseBlock(blockNum - 1)
	block, err := retryBlockOperation[*rpcBlock](blockNum, "eth_getBlockByNumber", func() (*rpcBlock, error) {
		return pool.ethGetBlockByNumber(blockNum, true)
	})
	if err != nil {
		return err
	}

	rules := chainCfg.Rules(block.Number, cparams.IsMergeTODO, block.Time)
	rulesExtra := cparams.GetRulesExtra(rules)
	var blockResults predicate.BlockResults
	if rulesExtra != nil {
		if predBytes := customheader.PredicateBytesFromExtra(rulesExtra.AvalancheRules, block.Extra); len(predBytes) > 0 {
			blockResults, err = predicate.ParseBlockResults(predBytes)
			if err != nil {
				return fmt.Errorf("parse block predicate results: %w", err)
			}
		}
	}

	tracker := newDiffTracker()
	signer := types.MakeSigner(chainCfg, block.Number, block.Time)
	baseFee := mustBaseFee(block.BaseFee)
	blockHashGetter := func(n uint64) common.Hash {
		return fetcher.GetHashForBlock(blockNum, n)
	}
	blockDifficulty := new(big.Int).Set(block.Difficulty)
	blockRandom := block.MixDigest
	if rules.IsShanghai {
		blockRandom.SetBytes(blockDifficulty.Bytes())
		blockDifficulty = new(big.Int)
	}
	header := &types.Header{
		ParentHash: block.ParentHash,
		Number:     new(big.Int).Set(block.Number),
		Time:       block.Time,
		Difficulty: new(big.Int).Set(block.Difficulty),
		GasLimit:   block.GasLimit,
		BaseFee:    new(big.Int).Set(baseFee),
		Extra:      bytes.Clone(block.Extra),
		MixDigest:  block.MixDigest,
		Coinbase:   block.Coinbase,
	}
	headerExtra := &ccustomtypes.HeaderExtra{TimeMilliseconds: block.TimeMS}
	if block.MinDelayExcess != nil {
		delay := acp226.DelayExcess(*block.MinDelayExcess)
		headerExtra.MinDelayExcess = &delay
	}
	ccustomtypes.SetHeaderExtra(header, headerExtra)
	blockCtx := vm.BlockContext{
		CanTransfer: func(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
			return db.GetBalance(addr).Cmp(amount) >= 0
		},
		Transfer: func(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
			db.SubBalance(sender, amount)
			db.AddBalance(recipient, amount)
		},
		GetHash:     blockHashGetter,
		Coinbase:    block.Coinbase,
		BlockNumber: new(big.Int).Set(block.Number),
		Time:        block.Time,
		Difficulty:  blockDifficulty,
		Random:      &blockRandom,
		GasLimit:    block.GasLimit,
		BaseFee:     baseFee,
		Header:      header,
	}
	gp := new(corethcore.GasPool).AddGas(block.GasLimit)

	for idx, tx := range block.Transactions {
		msg, err := corethcore.TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			return fmt.Errorf("tx %d message conversion: %w", idx, err)
		}

		cs := NewCallState(state)
		cs.SetTxContext(tx.Hash(), idx)
		evm := vm.NewEVM(blockCtx, corethcore.NewEVMTxContext(msg), cs, chainCfg, vm.Config{})
		result, err := corethcore.ApplyMessage(evm, msg, gp)
		if err != nil {
			return fmt.Errorf("tx %d apply failed: %w", idx, err)
		}
		if fetchErr := cs.Err(); fetchErr != nil {
			return fmt.Errorf("tx %d fetch failed: %w", idx, fetchErr)
		}

		txTracker := newDiffTracker()
		txTracker.record(state, cs)
		localTxDiff := txTracker.Finalize()
		tracker.record(state, cs)

		if idx == debugTxIndex {
			tracedTxDiff, err := traceBlockTxDiffWS(pool, blockNum, idx)
			if err != nil {
				return fmt.Errorf("trace tx %d: %w", idx, err)
			}
			warpPred, warpPredOK := cs.GetPredicate(warpcontract.ContractAddress, 0)
			warpResultBits := "<none>"
			if blockResults != nil {
				warpResultBits = blockResults.Get(tx.Hash(), warpcontract.ContractAddress).String()
			}
			log.Printf("debug block=%d tx=%d hash=%s status_failed=%t err=%v gas_used=%d localDiff=%d tracedDiff=%d warp_predicate=%t warp_predicate_bytes=%d warp_result_bits=%s",
				blockNum,
				idx,
				tx.Hash().Hex(),
				result.Failed(),
				result.Err,
				result.UsedGas,
				localTxDiff.Size(),
				tracedTxDiff.Size(),
				warpPredOK,
				len(warpPred),
				warpResultBits,
			)
			if rulesExtra != nil {
				log.Printf("debug rules warp_has_predicate=%t predicater_count=%d tx_access_list_len=%d",
					rulesExtra.HasPredicate(warpcontract.ContractAddress),
					len(rulesExtra.Predicaters),
					len(tx.AccessList()),
				)
			}
			if err := compareDiffs(localTxDiff, tracedTxDiff); err != nil {
				targetAddr := common.HexToAddress("0xE40895D055bccd2053dD0638C9695E326152b1A4")
				targetSlot := common.HexToHash("0xcb8ea4e37a23786e88d0aaa1d22243cb1e8a0ad24a7b17ae7bebf979ac1c0d3c")
				localVal, localOK := localTxDiff.storage[targetAddr][targetSlot]
				tracedVal, tracedOK := tracedTxDiff.storage[targetAddr][targetSlot]
				log.Printf("debug mismatch: %v", err)
				log.Printf("debug target slot addr=%s slot=%s local_present=%t local=%s traced_present=%t traced=%s",
					targetAddr.Hex(),
					targetSlot.Hex(),
					localOK,
					localVal.Hex(),
					tracedOK,
					tracedVal.Hex(),
				)
				log.Printf("debug local touched: %s", strings.Join(formatAddresses(localTxDiff), ", "))
				log.Printf("debug traced touched: %s", strings.Join(formatAddresses(tracedTxDiff), ", "))
				return err
			}
			log.Printf("debug tx matched block=%d tx=%d hash=%s", blockNum, idx, tx.Hash().Hex())
		}

		cs.CommitToBase()
	}

	finalDiff := tracker.Finalize()
	log.Printf("debug block replay complete block=%d txs=%d diff=%d", blockNum, len(block.Transactions), finalDiff.Size())
	return nil
}

func mergeThinClientConfig(cfg ThinClientConfig) ThinClientConfig {
	defaults := DefaultThinClientConfig()
	if cfg.RPCURL == "" {
		cfg.RPCURL = defaults.RPCURL
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = defaults.PoolSize
	}
	if cfg.MaxSnapshotStaleness == 0 {
		cfg.MaxSnapshotStaleness = defaults.MaxSnapshotStaleness
	}
	if cfg.SnapshotEveryBlocks == 0 {
		cfg.SnapshotEveryBlocks = defaults.SnapshotEveryBlocks
	}
	return cfg
}

func NewThinClient(cfg ThinClientConfig) (*ThinClient, error) {
	cfg = mergeThinClientConfig(cfg)
	pool := newRPCPool(cfg.RPCURL, cfg.PoolSize)
	head, err := retryBlockOperation[uint64](0, "eth_blockNumber", pool.ethBlockNumber)
	if err != nil {
		return nil, fmt.Errorf("startup failed: %w", err)
	}
	chainCfg, err := retryBlockOperation[*params.ChainConfig](0, "eth_getChainConfig", pool.ethGetChainConfig)
	if err != nil {
		return nil, fmt.Errorf("startup chain config fetch failed: %w", err)
	}
	infoURL, err := deriveInfoAPIURL(cfg.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("startup info api url derivation failed: %w", err)
	}
	snowCtx, err := fetchSnowContext(infoURL)
	if err != nil {
		return nil, fmt.Errorf("startup snow context fetch failed: %w", err)
	}
	cparams.GetExtra(chainCfg).AvalancheContext = cextras.AvalancheContext{SnowCtx: snowCtx}
	headBlock, err := retryBlockOperation[*rpcBlock](head, "eth_getBlockByNumber", func() (*rpcBlock, error) {
		return pool.ethGetBlockByNumber(head, false)
	})
	if err != nil {
		return nil, fmt.Errorf("startup head fetch failed: %w", err)
	}

	client := &ThinClient{
		cfg:       cfg,
		pool:      pool,
		chainCfg:  chainCfg,
		fetcher:   newThinFetcher(pool, head),
		state:     NewStateDB(nil),
		head:      head,
		timestamp: headBlock.Time,
		baseFee:   mustBaseFee(headBlock.BaseFee),
		gasLimit:  headBlock.GasLimit,
		headHash:  headBlock.Hash,
	}
	client.fetcher.SeedBlockHash(head, headBlock.Hash)

	loadedSnapshot := false
	if cfg.SnapshotPath != "" {
		var loaded bool
		loaded, err = client.loadLiveSnapshot(head)
		if err != nil {
			return nil, err
		}
		loadedSnapshot = loaded
	}

	if !loadedSnapshot {
		client.state = NewStateDB(client.fetcher)
		client.fetcher.SetBaseBlock(head)
	}
	if client.head < head {
		if err := client.catchUpTo(head); err != nil {
			return nil, err
		}
	}
	client.fetcher.SetBaseBlock(client.head)
	if client.head == head {
		client.headHash = headBlock.Hash
		client.fetcher.SeedBlockHash(head, headBlock.Hash)
	}
	return client, nil
}

func (c *ThinClient) State() *StateDB {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *ThinClient) RLock() {
	c.mu.RLock()
}

func (c *ThinClient) RUnlock() {
	c.mu.RUnlock()
}

func (c *ThinClient) Head() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.head
}

func (c *ThinClient) Block() uint64 {
	return c.Head()
}

func (c *ThinClient) Timestamp() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.timestamp
}

func (c *ThinClient) BaseFee() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.baseFee == nil {
		return 0
	}
	return c.baseFee.Uint64()
}

func (c *ThinClient) GasLimit() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gasLimit
}

func (c *ThinClient) SetOnBlock(fn func(*ThinClient, [][2]string)) {
	c.mu.Lock()
	c.onBlock = fn
	c.mu.Unlock()
}

func (c *ThinClient) EVMConfig() EVMConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	baseFee := uint64(0)
	if c.baseFee != nil {
		baseFee = c.baseFee.Uint64()
	}
	return EVMConfig{
		BlockNumber: c.head,
		Timestamp:   c.timestamp,
		ChainID:     43114,
		BaseFee:     baseFee,
		GasLimit:    c.gasLimit,
	}
}

func (c *ThinClient) RunLive(maxBlocks uint64) error {
	headUpdates := make(chan uint64, 1)
	go subscribeNewHeads(c.cfg.RPCURL, headUpdates)

	c.mu.RLock()
	processed := c.head
	headHash := c.headHash
	c.mu.RUnlock()
	targetHead := processed
	processedCount := uint64(0)
	log.Printf("thin client started at head=%d hash=%s", processed, headHash.Hex())

	for {
		if processed >= targetHead {
			targetHead = <-headUpdates
			continue
		}

		stats, err := c.ProcessNextBlock()
		if err != nil {
			return err
		}
		processed = stats.Block
		processedCount++
		log.Printf("block=%d txs=%d localDiff=%d tracedDiff=%d execElapsed=%s totalElapsed=%s fetches=%d storage=%d balance=%d nonce=%d code=%d blockhash=%d",
			stats.Block,
			stats.Txs,
			stats.LocalDiff,
			stats.TracedDiff,
			stats.ExecElapsed.Round(time.Millisecond),
			stats.TotalElapsed.Round(time.Millisecond),
			stats.Fetches.Total(),
			stats.Fetches.Storage,
			stats.Fetches.Balance,
			stats.Fetches.Nonce,
			stats.Fetches.Code,
			stats.Fetches.Block,
		)
		if maxBlocks > 0 && processedCount >= maxBlocks {
			log.Printf("max-blocks reached processed=%d startHead=%d endHead=%d", processedCount, processed-processedCount, processed)
			if c.cfg.SnapshotPath != "" {
				if err := c.SaveSnapshot(c.cfg.SnapshotPath); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

func (c *ThinClient) ProcessNextBlock() (ThinBlockStats, error) {
	c.mu.RLock()
	next := c.head + 1
	c.mu.RUnlock()
	return c.ProcessBlock(next)
}

func (c *ThinClient) ProcessBlock(blockNum uint64) (ThinBlockStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if blockNum != c.head+1 {
		return ThinBlockStats{}, fmt.Errorf("expected next block %d, got %d", c.head+1, blockNum)
	}

	start := time.Now()
	c.fetcher.SetBaseBlock(blockNum - 1)

	block, err := retryBlockOperation[*rpcBlock](blockNum, "eth_getBlockByNumber", func() (*rpcBlock, error) {
		return c.pool.ethGetBlockByNumber(blockNum, true)
	})
	if err != nil {
		return ThinBlockStats{}, fmt.Errorf("block fetch failed: %w", err)
	}
	traced, err := retryBlockOperation[*blockDiff](blockNum, "debug_traceBlockByNumber", func() (*blockDiff, error) {
		return traceBlockDiffWS(c.pool, blockNum)
	})
	if err != nil {
		return ThinBlockStats{}, fmt.Errorf("trace fetch failed: %w", err)
	}

	execStart := time.Now()
	fetchStatsBefore := c.fetcher.SnapshotStats()
	local, err := executeBlock(c.fetcher, c.chainCfg, c.state, block)
	execElapsed := time.Since(execStart)
	fetchStatsDelta := c.fetcher.StatsDelta(fetchStatsBefore)
	if err != nil {
		return ThinBlockStats{}, fmt.Errorf("local execution failed on block %d: %w", blockNum, err)
	}
	local, err = pruneLocalNoops(c.pool, blockNum, local)
	if err != nil {
		return ThinBlockStats{}, fmt.Errorf("local diff prune failed: %w", err)
	}
	if err := compareDiffs(local, traced); err != nil {
		log.Printf("diff mismatch on block=%d txs=%d localDiff=%d tracedDiff=%d", blockNum, len(block.Transactions), local.Size(), traced.Size())
		log.Printf("touched addresses: %s", strings.Join(formatAddresses(local), ", "))
		return ThinBlockStats{}, fmt.Errorf("first mismatch: %w", err)
	}
	c.head = blockNum
	c.timestamp = block.Time
	c.baseFee = mustBaseFee(block.BaseFee)
	c.gasLimit = block.GasLimit
	c.headHash = block.Hash
	c.fetcher.SetBaseBlock(blockNum)
	c.fetcher.SeedBlockHash(blockNum, block.Hash)
	if c.onBlock != nil {
		c.onBlock(c, diffEntries(local))
	}

	if c.cfg.SnapshotPath != "" && c.cfg.SnapshotEveryBlocks > 0 && blockNum-c.lastSnapshotBlock >= c.cfg.SnapshotEveryBlocks {
		if err := c.saveSnapshotLocked(c.cfg.SnapshotPath); err != nil {
			return ThinBlockStats{}, err
		}
		c.lastSnapshotBlock = blockNum
	}

	return ThinBlockStats{
		Block:        blockNum,
		Txs:          len(block.Transactions),
		LocalDiff:    local.Size(),
		TracedDiff:   traced.Size(),
		ExecElapsed:  execElapsed,
		TotalElapsed: time.Since(start),
		Fetches:      fetchStatsDelta,
	}, nil
}

func (c *ThinClient) catchUpTo(target uint64) error {
	for c.head < target {
		if _, err := c.ProcessNextBlock(); err != nil {
			return err
		}
	}
	return nil
}

func (c *ThinClient) SaveSnapshot(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveSnapshotLocked(path)
}

func (c *ThinClient) saveSnapshotLocked(path string) error {
	if path == "" {
		return nil
	}
	snapshot := c.buildSnapshotLocked()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	tmpPath := path + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create snapshot temp file: %w", err)
	}
	enc := gob.NewEncoder(file)
	if err := enc.Encode(snapshot); err != nil {
		file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("encode snapshot: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return nil
}

func (c *ThinClient) loadLiveSnapshot(currentHead uint64) (bool, error) {
	snapshot, err := readThinSnapshot(c.cfg.SnapshotPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read snapshot: %w", err)
	}
	if snapshot.BlockNumber > currentHead {
		return false, nil
	}
	if currentHead-snapshot.BlockNumber > c.cfg.MaxSnapshotStaleness {
		return false, nil
	}
	c.applySnapshot(snapshot)
	c.lastSnapshotBlock = snapshot.BlockNumber
	return true, nil
}

func readThinSnapshot(path string) (*ThinSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var snapshot ThinSnapshot
	if err := gob.NewDecoder(file).Decode(&snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (c *ThinClient) applySnapshot(snapshot *ThinSnapshot) {
	state := NewStateDB(c.fetcher)
	for _, entry := range snapshot.Storage {
		state.SetStorageSlot(entry.Address, entry.Slot, entry.Value)
	}
	for _, account := range snapshot.Accounts {
		state.backfillMu.Lock()
		if account.HasBalance {
			balance := new(uint256.Int)
			balance.SetBytes32(account.Balance[:])
			state.bfBalance[account.Address] = balance
		}
		if account.HasNonce {
			state.bfNonce[account.Address] = account.Nonce
		}
		if account.HasCode {
			code := append([]byte(nil), account.Code...)
			state.bfCode[account.Address] = code
			if len(code) > 0 {
				state.bfCodeHash[account.Address] = crypto.Keccak256Hash(code)
			}
		}
		state.backfillMu.Unlock()
	}
	c.state = state
	c.head = snapshot.BlockNumber
	c.timestamp = snapshot.Timestamp
	c.baseFee = new(big.Int).SetBytes(snapshot.BaseFee)
	c.gasLimit = snapshot.GasLimit
	c.fetcher.SetBaseBlock(snapshot.BlockNumber)
}

func (c *ThinClient) buildSnapshotLocked() *ThinSnapshot {
	merged := c.state.mergedSnapshotState(c.head, c.timestamp)
	snapshot := &ThinSnapshot{
		Version:     1,
		BlockNumber: c.head,
		Timestamp:   c.timestamp,
		BaseFee:     append([]byte(nil), c.baseFee.Bytes()...),
		GasLimit:    c.gasLimit,
		Storage:     make([]ThinSnapshotStorage, 0),
		Accounts:    make([]ThinSnapshotAccount, 0),
	}
	for addr, slots := range merged.Storage {
		for slot, value := range slots {
			snapshot.Storage = append(snapshot.Storage, ThinSnapshotStorage{
				Address: addr,
				Slot:    slot,
				Value:   value,
			})
		}
	}
	accountAddrs := make(map[common.Address]struct{}, len(merged.Balance)+len(merged.Nonce)+len(merged.Code))
	for addr := range merged.Balance {
		accountAddrs[addr] = struct{}{}
	}
	for addr := range merged.Nonce {
		accountAddrs[addr] = struct{}{}
	}
	for addr := range merged.Code {
		accountAddrs[addr] = struct{}{}
	}
	for addr := range accountAddrs {
		account := ThinSnapshotAccount{
			Address: addr,
		}
		if bal, ok := merged.Balance[addr]; ok && bal != nil {
			var balance [32]byte
			account.HasBalance = true
			bal.WriteToSlice(balance[:])
			account.Balance = balance
		}
		if nonce, ok := merged.Nonce[addr]; ok {
			account.HasNonce = true
			account.Nonce = nonce
		}
		if code, ok := merged.Code[addr]; ok {
			account.HasCode = true
			account.Code = append([]byte(nil), code...)
		}
		snapshot.Accounts = append(snapshot.Accounts, account)
	}
	return snapshot
}

func (s *StateDB) mergedSnapshotState(blockNum, blockTime uint64) *ImmutableState {
	base := s.immutable.Load()
	merged := NewImmutableState(blockNum, blockTime)
	for addr, slots := range base.Storage {
		copied := make(map[common.Hash]common.Hash, len(slots))
		for slot, value := range slots {
			copied[slot] = value
		}
		merged.Storage[addr] = copied
	}
	for addr, code := range base.Code {
		merged.Code[addr] = append([]byte(nil), code...)
	}
	for addr, hash := range base.CodeHash {
		merged.CodeHash[addr] = hash
	}
	for addr, bal := range base.Balance {
		merged.Balance[addr] = new(uint256.Int).Set(bal)
	}
	for addr, nonce := range base.Nonce {
		merged.Nonce[addr] = nonce
	}

	s.backfillMu.RLock()
	defer s.backfillMu.RUnlock()
	for addr, slots := range s.bfStorage {
		if merged.Storage[addr] == nil {
			merged.Storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		}
		for slot, value := range slots {
			merged.Storage[addr][slot] = value
		}
	}
	for addr, code := range s.bfCode {
		merged.Code[addr] = append([]byte(nil), code...)
		if len(code) > 0 {
			merged.CodeHash[addr] = crypto.Keccak256Hash(code)
		}
	}
	for addr, bal := range s.bfBalance {
		merged.Balance[addr] = new(uint256.Int).Set(bal)
	}
	for addr, nonce := range s.bfNonce {
		merged.Nonce[addr] = nonce
	}
	return merged
}
