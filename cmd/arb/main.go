package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"defi-toolbox/arb"
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/router"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"
)

// WAVAX is the hub token for cyclic arb on Avalanche C-Chain.
var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")

// ─── WebSocket state fetcher (same pattern as cmd/native) ──────────

type wsFetcher struct {
	conn         *websocket.Conn
	mu           sync.Mutex
	nextID       int
	pending      map[int]chan json.RawMessage
	block        uint64
	timestamp    uint64
	baseFee      uint64
	gasLimit     uint64
	cacheMisses  int64
	onSlotChange func(addr common.Address, slot common.Hash)
	onBlock      func(block, timestamp, baseFee, gasLimit uint64)
}

type stateServerMessage struct {
	Type        string      `json:"type,omitempty"`
	BlockNumber uint64      `json:"blockNumber,omitempty"`
	Timestamp   uint64      `json:"timestamp,omitempty"`
	BaseFee     uint64      `json:"baseFee,omitempty"`
	GasLimit    uint64      `json:"gasLimit,omitempty"`
	Entries     [][2]string `json:"entries,omitempty"`

	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      int             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type valueResult struct {
	Value string `json:"value"`
}

func newWSFetcher(url string) (*wsFetcher, *statedb.StateDB, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ws connect: %w", err)
	}

	f := &wsFetcher{
		conn:    conn,
		pending: make(map[int]chan json.RawMessage),
	}

	state := statedb.NewStateDB(f)

	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, nil, fmt.Errorf("read initial_dump: %w", err)
	}

	var dump stateServerMessage
	if err := json.Unmarshal(msg, &dump); err != nil {
		return nil, nil, fmt.Errorf("parse initial_dump: %w", err)
	}
	if dump.Type != "initial_dump" {
		return nil, nil, fmt.Errorf("expected initial_dump, got %s", dump.Type)
	}

	f.block = dump.BlockNumber
	f.timestamp = dump.Timestamp
	f.baseFee = dump.BaseFee
	f.gasLimit = dump.GasLimit

	storageCount := 0
	accountData := make(map[string]map[string]string)

	for _, entry := range dump.Entries {
		key, value := entry[0], entry[1]
		if strings.HasPrefix(key, "s:") {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) == 3 {
				addr := common.HexToAddress(parts[1])
				slot := common.HexToHash(parts[2])
				val := common.HexToHash(value)
				state.SetStorageSlot(addr, slot, val)
				storageCount++
			}
		} else if strings.HasPrefix(key, "b:") {
			addr := key[2:]
			if accountData[addr] == nil {
				accountData[addr] = make(map[string]string)
			}
			accountData[addr]["balance"] = value
		} else if strings.HasPrefix(key, "n:") {
			addr := key[2:]
			if accountData[addr] == nil {
				accountData[addr] = make(map[string]string)
			}
			accountData[addr]["nonce"] = value
		} else if strings.HasPrefix(key, "c:") {
			addr := key[2:]
			if accountData[addr] == nil {
				accountData[addr] = make(map[string]string)
			}
			accountData[addr]["code"] = value
		}
	}

	accountCount := 0
	for addrStr, data := range accountData {
		addr := common.HexToAddress(addrStr)
		balance := uint256.NewInt(0)
		if b, ok := data["balance"]; ok {
			bi, _ := new(big.Int).SetString(strings.TrimPrefix(b, "0x"), 16)
			if bi != nil {
				balance, _ = uint256.FromBig(bi)
			}
		}
		var nonce uint64
		if n, ok := data["nonce"]; ok {
			ni, _ := new(big.Int).SetString(strings.TrimPrefix(n, "0x"), 16)
			if ni != nil {
				nonce = ni.Uint64()
			}
		}
		var code []byte
		if c, ok := data["code"]; ok && c != "0x" && c != "" {
			code, _ = hex.DecodeString(strings.TrimPrefix(c, "0x"))
		}
		state.SetAccount(addr, balance, nonce, code)
		accountCount++
	}

	fmt.Fprintf(os.Stderr, "[arb] initial_dump: block=%d, %d storage, %d accounts\n",
		f.block, storageCount, accountCount)

	go f.readLoop(state)

	return f, state, nil
}

func (f *wsFetcher) readLoop(state *statedb.StateDB) {
	for {
		_, msg, err := f.conn.ReadMessage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] ws read error: %v\n", err)
			return
		}

		var m stateServerMessage
		if err := json.Unmarshal(msg, &m); err != nil {
			continue
		}

		if m.Type == "block_diff" {
			f.block = m.BlockNumber
			f.timestamp = m.Timestamp
			f.baseFee = m.BaseFee
			f.gasLimit = m.GasLimit

			for _, entry := range m.Entries {
				key, value := entry[0], entry[1]
				if strings.HasPrefix(key, "s:") {
					parts := strings.SplitN(key, ":", 3)
					if len(parts) == 3 {
						addr := common.HexToAddress(parts[1])
						slot := common.HexToHash(parts[2])
						val := common.HexToHash(value)
						state.SetStorageSlot(addr, slot, val)
						if f.onSlotChange != nil {
							f.onSlotChange(addr, slot)
						}
					}
				}
			}
			if f.onBlock != nil {
				f.onBlock(m.BlockNumber, m.Timestamp, m.BaseFee, m.GasLimit)
			}
			continue
		}

		// JSON-RPC response
		if m.ID > 0 {
			f.mu.Lock()
			ch, ok := f.pending[m.ID]
			if ok {
				delete(f.pending, m.ID)
			}
			f.mu.Unlock()
			if ok {
				if m.Error != nil && string(m.Error) != "null" {
					ch <- m.Error
				} else {
					ch <- m.Result
				}
			}
		}
	}
}

func (f *wsFetcher) call(method string, params interface{}) (json.RawMessage, error) {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	ch := make(chan json.RawMessage, 1)
	f.pending[id] = ch

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(req)
	err := f.conn.WriteMessage(websocket.TextMessage, data)
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}

	select {
	case result := <-ch:
		return result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for %s response", method)
	}
}

func (f *wsFetcher) FetchStorage(addr common.Address, slot common.Hash) common.Hash {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"slot":        slot.Hex(),
		"blockNumber": f.block,
	}
	result, err := f.call("state_getStorageAt", params)
	if err != nil {
		return common.Hash{}
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return common.Hash{}
	}
	return common.HexToHash(vr.Value)
}

func (f *wsFetcher) FetchBalance(addr common.Address) *uint256.Int {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block,
	}
	result, err := f.call("state_getBalance", params)
	if err != nil {
		return uint256.NewInt(0)
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return uint256.NewInt(0)
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok {
		return uint256.NewInt(0)
	}
	val, _ := uint256.FromBig(bi)
	return val
}

func (f *wsFetcher) FetchNonce(addr common.Address) uint64 {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block,
	}
	result, err := f.call("state_getNonce", params)
	if err != nil {
		return 0
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return 0
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok {
		return 0
	}
	return bi.Uint64()
}

func (f *wsFetcher) FetchCode(addr common.Address) []byte {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block,
	}
	result, err := f.call("state_getCode", params)
	if err != nil {
		return nil
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return nil
	}
	if vr.Value == "" || vr.Value == "0x" {
		return nil
	}
	code, _ := hex.DecodeString(strings.TrimPrefix(vr.Value, "0x"))
	return code
}

func (f *wsFetcher) FetchBlockHash(num uint64) common.Hash {
	return common.Hash{}
}

// ─── Main ──────────────────────────────────────────────────────────

func main() {
	stateServerURL := "ws://localhost:7449/live"
	maxHops := 4
	poolLimit := 7500

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--max-hops" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &maxHops)
		}
		if arg == "--pool-limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
	}

	fmt.Fprintf(os.Stderr, "[arb] WAVAX cyclic arb scanner starting...\n")
	fmt.Fprintf(os.Stderr, "[arb] state server: %s, max hops: %d, pool limit: %d\n",
		stateServerURL, maxHops, poolLimit)

	// Load formula registry
	registry := formulas.LoadEmbeddedRegistry()
	validated, invalid := registry.RegistryStats()
	fmt.Fprintf(os.Stderr, "[arb] formula registry: %d validated, %d invalid\n", validated, invalid)

	// Load pools and build graph
	embeddedPools := poolcollector.EmbeddedPools(poolLimit)
	graph := pf.BuildGraph(embeddedPools)
	fmt.Fprintf(os.Stderr, "[arb] loaded %d pools\n", len(embeddedPools))

	fmt.Fprintf(os.Stderr, "[arb] router: %s\n", router.DeployedRouter.Hex())

	// Connect to state server
	fetcher, state, err := newWSFetcher(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb] failed to connect: %v\n", err)
		os.Exit(1)
	}

	// Create PoolManager
	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)
	for _, p := range embeddedPools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	pm.SetBlockTimestamp(fetcher.timestamp)

	// Build pool table and enumerate cycles
	pt := arb.NewPoolTable(embeddedPools)
	t0 := time.Now()
	cycles := arb.EnumerateCycles(graph, embeddedPools, WAVAX, maxHops, registry, pt)
	enumTime := time.Since(t0)
	fmt.Fprintf(os.Stderr, "[arb] enumerated %d cycles in %v\n", len(cycles), enumTime.Round(time.Millisecond))

	// Create scanner
	scanner := arb.NewScanner(cycles, pm, pt, WAVAX)

	// Register pool token0 for direction resolution in rate table
	for _, p := range embeddedPools {
		if len(p.Tokens) >= 2 {
			scanner.RateTable().SetPoolToken0(p.Address, p.Tokens[0])
		}
	}

	// Track dirty pools per block
	var dirtyMu sync.Mutex
	var dirtyPools []common.Address
	dirtySet := make(map[common.Address]bool)

	fetcher.onSlotChange = func(addr common.Address, slot common.Hash) {
		poolAddr := pm.InvalidateBySlot(addr, slot)
		if poolAddr != (common.Address{}) {
			dirtyMu.Lock()
			if !dirtySet[poolAddr] {
				dirtySet[poolAddr] = true
				dirtyPools = append(dirtyPools, poolAddr)
			}
			dirtyMu.Unlock()
		}
	}

	// Initial rate sweep (all pools dirty)
	fmt.Fprintf(os.Stderr, "[arb] running initial rate sweep...\n")
	scanner.InitRates()
	fmt.Fprintf(os.Stderr, "[arb] ready. Waiting for blocks...\n")

	// Block handler
	blockCh := make(chan blockInfo, 4)

	fetcher.onBlock = func(block, timestamp, baseFee, gasLimit uint64) {
		pm.SetBlockTimestamp(timestamp)
		select {
		case blockCh <- blockInfo{block, timestamp, baseFee, gasLimit}:
		default:
			// Drop if channel full (processing previous block)
			fmt.Fprintf(os.Stderr, "[arb] WARNING: dropped block %d (processing backlog)\n", block)
		}
	}

	// Process blocks
	for bi := range blockCh {
		dirtyMu.Lock()
		dp := dirtyPools
		dirtyPools = nil
		dirtySet = make(map[common.Address]bool)
		dirtyMu.Unlock()

		if len(dp) == 0 {
			continue
		}

		cfg := statedb.EVMConfig{
			BlockNumber: bi.block,
			Timestamp:   bi.timestamp,
			ChainID:     43114,
			BaseFee:     bi.baseFee,
			GasLimit:    bi.gasLimit,
		}

		// Create verifier for this block
		verifier := arb.NewVerifier(state, cfg, router.DeployedRouter, pt, WAVAX)
		verifier.SetVerbose(true)

		opp := scanner.OnBlock(dp, verifier, bi.baseFee)

		fmt.Fprintf(os.Stderr, "[arb] block=%d dirty=%d | %s\n",
			bi.block, len(dp), arb.FormatOpportunity(opp, pt))

		if opp != nil && opp.EVMVerified && opp.EVMProfit > 0 {
			// Log profitable opportunity as JSON to stdout
			poolAddrs := opp.Cycle.ExpandPoolAddrs(pt)
			out, _ := json.Marshal(map[string]interface{}{
				"type":          "opportunity",
				"block":         bi.block,
				"hops":          opp.Cycle.Hops,
				"amountIn":      opp.AmountIn.Hex(),
				"formulaOut":    opp.FormulaOut.Hex(),
				"evmOut":        opp.EVMOut.Hex(),
				"evmGasUsed":    opp.EVMGasUsed,
				"evmProfitWei":  fmt.Sprintf("%.0f", opp.EVMProfit),
				"evmProfitAvax": opp.EVMProfit / 1e18,
				"pools":         poolsHex(poolAddrs),
			})
			fmt.Println(string(out))
		}
	}
}

type blockInfo struct {
	block     uint64
	timestamp uint64
	baseFee   uint64
	gasLimit  uint64
}

func poolsHex(pools []common.Address) []string {
	s := make([]string, len(pools))
	for i, p := range pools {
		s[i] = p.Hex()
	}
	return s
}
