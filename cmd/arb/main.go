package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	block        atomic.Uint64
	timestamp    atomic.Uint64
	baseFee      atomic.Uint64
	gasLimit     atomic.Uint64
	cacheMisses  int64
	onSlotChange func(addr common.Address, slot, value common.Hash)
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

	f.block.Store(dump.BlockNumber)
	f.timestamp.Store(dump.Timestamp)
	f.baseFee.Store(dump.BaseFee)
	f.gasLimit.Store(dump.GasLimit)

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
		f.block.Load(), storageCount, accountCount)

	// NOTE: caller must set onSlotChange/onBlock callbacks then call startReadLoop()
	return f, state, nil
}

func (f *wsFetcher) startReadLoop(state *statedb.StateDB) {
	go f.readLoop(state)
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
			f.block.Store(m.BlockNumber)
			f.timestamp.Store(m.Timestamp)
			f.baseFee.Store(m.BaseFee)
			f.gasLimit.Store(m.GasLimit)

			// Buffer slot updates — do NOT write to state from this goroutine.
			// State writes happen on the main goroutine to avoid data races.
			if f.onSlotChange != nil {
				for _, entry := range m.Entries {
					key, value := entry[0], entry[1]
					if strings.HasPrefix(key, "s:") {
						parts := strings.SplitN(key, ":", 3)
						if len(parts) == 3 {
							f.onSlotChange(
								common.HexToAddress(parts[1]),
								common.HexToHash(parts[2]),
								common.HexToHash(value),
							)
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

func (f *wsFetcher) FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error) {
	f.cacheMisses++
	block := f.block.Load()
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"slot":        slot.Hex(),
		"blockNumber": block,
	}
	result, err := f.call("state_getStorageAt", params)
	if err != nil {
		return common.Hash{}, fmt.Errorf("FetchStorage %s slot=%s block=%d: %w", addr.Hex()[:10], slot.Hex()[:14], block, err)
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return common.Hash{}, fmt.Errorf("FetchStorage parse %s slot=%s: %w raw=%s", addr.Hex()[:10], slot.Hex()[:14], err, string(result))
	}
	return common.HexToHash(vr.Value), nil
}

func (f *wsFetcher) FetchBalance(addr common.Address) (*uint256.Int, error) {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block.Load(),
	}
	result, err := f.call("state_getBalance", params)
	if err != nil {
		return nil, fmt.Errorf("FetchBalance %s: %w", addr.Hex()[:10], err)
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return nil, fmt.Errorf("FetchBalance parse %s: %w", addr.Hex()[:10], err)
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("FetchBalance parse hex %s: %s", addr.Hex()[:10], vr.Value)
	}
	val, _ := uint256.FromBig(bi)
	return val, nil
}

func (f *wsFetcher) FetchNonce(addr common.Address) (uint64, error) {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block.Load(),
	}
	result, err := f.call("state_getNonce", params)
	if err != nil {
		return 0, fmt.Errorf("FetchNonce %s: %w", addr.Hex()[:10], err)
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return 0, fmt.Errorf("FetchNonce parse %s: %w", addr.Hex()[:10], err)
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("FetchNonce parse hex %s: %s", addr.Hex()[:10], vr.Value)
	}
	return bi.Uint64(), nil
}

func (f *wsFetcher) FetchCode(addr common.Address) ([]byte, error) {
	f.cacheMisses++
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block.Load(),
	}
	result, err := f.call("state_getCode", params)
	if err != nil {
		return nil, fmt.Errorf("FetchCode %s: %w", addr.Hex()[:10], err)
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		return nil, fmt.Errorf("FetchCode parse %s: %w", addr.Hex()[:10], err)
	}
	if vr.Value == "" || vr.Value == "0x" {
		return nil, nil
	}
	code, _ := hex.DecodeString(strings.TrimPrefix(vr.Value, "0x"))
	return code, nil
}

func (f *wsFetcher) FetchBlockHash(num uint64) (common.Hash, error) {
	return common.Hash{}, nil
}

// ─── Main ──────────────────────────────────────────────────────────

func main() {
	stateServerURL := "ws://localhost:7449/live"
	rpcURL := ""
	maxHops := 4
	poolLimit := 1500
	dryRun := true

	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--rpc" && i+1 < len(os.Args) {
			rpcURL = os.Args[i+1]
		}
		if arg == "--max-hops" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &maxHops)
		}
		if arg == "--pool-limit" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
		}
		if arg == "--execute" {
			dryRun = false
		}
	}

	fmt.Fprintf(os.Stderr, "[arb] WAVAX cyclic arb scanner starting...\n")
	fmt.Fprintf(os.Stderr, "[arb] state server: %s, max hops: %d, pool limit: %d\n",
		stateServerURL, maxHops, poolLimit)
	if dryRun {
		fmt.Fprintf(os.Stderr, "[arb] mode: DRY RUN (pass --execute --rpc <url> and set ARB_PRIVATE_KEY to go live)\n")
	} else {
		fmt.Fprintf(os.Stderr, "[arb] mode: LIVE EXECUTION\n")
	}

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
	pm.SetBlockTimestamp(fetcher.timestamp.Load())

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

	// Set up executor if live mode
	var executor *arb.Executor
	if !dryRun {
		privKey := os.Getenv("ARB_PRIVATE_KEY")
		if privKey == "" {
			fmt.Fprintf(os.Stderr, "[arb] ERROR: ARB_PRIVATE_KEY env var required for --execute\n")
			os.Exit(1)
		}
		if rpcURL == "" {
			fmt.Fprintf(os.Stderr, "[arb] ERROR: --rpc <url> required for --execute\n")
			os.Exit(1)
		}
		var err error
		executor, err = arb.NewExecutor(privKey, rpcURL, router.DeployedRouter, pt, WAVAX)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] ERROR: %v\n", err)
			os.Exit(1)
		}
		nonce, err := executor.FetchNonce()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] ERROR fetching nonce: %v\n", err)
			os.Exit(1)
		}
		executor.SetNonce(nonce)

		// Query WAVAX (ERC-20) balance — this is what we trade.
		// Also seed the local state with the real balance + allowance slots
		// so EVM simulation sees the same state as on-chain.
		wavaxBal, err := executor.FetchERC20Balance(WAVAX)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] WARNING: could not fetch WAVAX balance: %v\n", err)
		} else {
			balF := new(big.Float).Quo(new(big.Float).SetInt(wavaxBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb] WAVAX balance: %s\n", balF.Text('f', 6))

		}
		// Also show native AVAX (for gas)
		nativeBal, err := executor.FetchBalance()
		if err == nil {
			balF := new(big.Float).Quo(new(big.Float).SetInt(nativeBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb] native AVAX (gas): %s\n", balF.Text('f', 6))
		}
		// Check WAVAX approval for router, approve if needed
		allowance, err := executor.CheckAllowance(WAVAX)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb] WARNING: could not check allowance: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[arb] WAVAX allowance: %s\n", allowance.String())

			// Need at least 1000 WAVAX allowance to be useful
			minAllowance := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
			if allowance.Cmp(minAllowance) < 0 {
				fmt.Fprintf(os.Stderr, "[arb] WAVAX allowance too low (%s), approving router...\n", allowance.String())
				txHash, err := executor.Approve(WAVAX, fetcher.baseFee.Load())
				if err != nil {
					fmt.Fprintf(os.Stderr, "[arb] ERROR: approve failed: %v\n", err)
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "[arb] approve tx: %s (waiting 3s for confirmation)\n", txHash.Hex())
				time.Sleep(3 * time.Second)
				// Re-fetch nonce after approval
				nonce, _ = executor.FetchNonce()
				executor.SetNonce(nonce)
			} else {
				fmt.Fprintf(os.Stderr, "[arb] WAVAX allowance OK\n")
			}
		}

		fmt.Fprintf(os.Stderr, "[arb] executor ready, nonce=%d\n", nonce)
	}

	// Cap trade size to WAVAX balance (the token we trade, not native AVAX)
	if executor != nil {
		wavaxBal, err := executor.FetchERC20Balance(WAVAX)
		if err == nil && wavaxBal.Sign() > 0 {
			u, _ := uint256.FromBig(wavaxBal)
			scanner.MaxSize = u
			balF := new(big.Float).Quo(new(big.Float).SetInt(wavaxBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb] max trade size: %s WAVAX\n", balF.Text('f', 6))
		}
	}

	// Buffer ALL state changes from readLoop goroutine, apply on main goroutine.
	// StateDB and PoolManager are NOT thread-safe — all writes must be on the main goroutine.
	type slotUpdate struct {
		addr  common.Address
		slot  common.Hash
		value common.Hash
	}
	var slotMu sync.Mutex
	var pendingSlots []slotUpdate

	fetcher.onSlotChange = func(addr common.Address, slot, value common.Hash) {
		slotMu.Lock()
		pendingSlots = append(pendingSlots, slotUpdate{addr, slot, value})
		slotMu.Unlock()
	}

	blockCh := make(chan blockInfo, 4)

	fetcher.onBlock = func(block, timestamp, baseFee, gasLimit uint64) {
		select {
		case blockCh <- blockInfo{block, timestamp, baseFee, gasLimit}:
		default:
			// Drop if channel full (processing previous block)
			fmt.Fprintf(os.Stderr, "[arb] WARNING: dropped block %d (processing backlog)\n", block)
		}
	}

	// Start readLoop BEFORE any fetches (GetCode etc need responses from the server)
	fetcher.startReadLoop(state)

	// Verify key contracts are in state (may trigger code fetch on demand)
	fmt.Fprintf(os.Stderr, "[arb] router code: %d bytes, WAVAX code: %d bytes\n",
		state.GetCodeSize(router.DeployedRouter), state.GetCodeSize(WAVAX))

	// Initial rate sweep BEFORE block processing (PoolManager is not thread-safe)
	fmt.Fprintf(os.Stderr, "[arb] running initial rate sweep...\n")
	scanner.InitRates()
	fmt.Fprintf(os.Stderr, "[arb] ready. Waiting for blocks...\n")

	// Process blocks
	for bi := range blockCh {
		// Drain pending slot changes on main goroutine
		slotMu.Lock()
		slots := pendingSlots
		pendingSlots = nil
		slotMu.Unlock()

		pm.SetBlockTimestamp(bi.timestamp)

		// Apply updates in-place: only overwrite slots already in cache
		for _, su := range slots {
			if state.HasStorageSlot(su.addr, su.slot) {
				state.SetStorageSlot(su.addr, su.slot, su.value)
			}
		}

		// Invalidate dirty pools in PoolManager
		dirtySet := make(map[common.Address]bool)
		var dp []common.Address
		for _, su := range slots {
			poolAddr := pm.InvalidateBySlot(su.addr, su.slot)
			if poolAddr != (common.Address{}) && !dirtySet[poolAddr] {
				dirtySet[poolAddr] = true
				dp = append(dp, poolAddr)
			}
		}

		if len(dp) == 0 {
			continue
		}

		// Build EVM config + verifier for this block (immutable state — safe)
		cfg := statedb.EVMConfig{
			BlockNumber: bi.block,
			Timestamp:   bi.timestamp,
			ChainID:     43114,
			BaseFee:     bi.baseFee,
			GasLimit:    bi.gasLimit,
		}
		var verifier *arb.Verifier
		var caller common.Address
		if executor != nil {
			caller = executor.Address()
		}
		verifier = arb.NewVerifier(state, cfg, router.DeployedRouter, caller, pt, WAVAX)

		// Stages 1+2+3: rate screening + formula quoting + local EVM (top 50)
		opp, evmResults := scanner.OnBlock(dp, verifier, bi.baseFee)

		fmt.Fprintf(os.Stderr, "[arb] block=%d dirty=%d | %s\n",
			bi.block, len(dp), arb.FormatOpportunity(opp, pt))

		// ── Stage 3b: RPC cross-check ALL local EVM results ──
		if executor != nil && len(evmResults) > 0 {
			matched, mismatched, rpcErrors := 0, 0, 0
			for _, er := range evmResults {
				rpc := executor.EthCallAtBlock(er.Calldata, er.Block)

				if er.Reverted && rpc.Reverted {
					matched++ // both reverted — OK
					continue
				}
				if er.Reverted != rpc.Reverted {
					mismatched++
					fmt.Fprintf(os.Stderr, "[arb] *** MISMATCH *** block=%d local_revert=%v rpc_revert=%v local_err=%s rpc_err=%s\n",
						er.Block, er.Reverted, rpc.Reverted, er.ErrMsg, rpc.ErrMsg)
					continue
				}
				if rpc.ErrMsg != "" && rpc.RetData == nil {
					rpcErrors++
					continue
				}
				// Both succeeded — compare return data byte-for-byte
				if !bytes.Equal(er.RetData, rpc.RetData) {
					mismatched++
					// Extract amountOut from both for readable log
					var localOut, rpcOut string
					if len(er.RetData) >= 32 {
						localOut = fmt.Sprintf("%x", er.RetData[len(er.RetData)-32:])
					}
					if len(rpc.RetData) >= 32 {
						rpcOut = fmt.Sprintf("%x", rpc.RetData[len(rpc.RetData)-32:])
					}
					fmt.Fprintf(os.Stderr, "[arb] *** OUTPUT MISMATCH *** block=%d local_out=%s rpc_out=%s local_gas=%d local_ret_len=%d rpc_ret_len=%d\n",
						er.Block, localOut, rpcOut, er.GasUsed, len(er.RetData), len(rpc.RetData))
				} else {
					matched++
				}
			}
			fmt.Fprintf(os.Stderr, "[arb] stage3 cross-check: %d/%d matched, %d mismatched, %d rpc-errors\n",
				matched, len(evmResults), mismatched, rpcErrors)
		}

		// ── Stage 4: Execute if profitable and verified ──
		if opp != nil && opp.EVMVerified && opp.EVMProfit > 0 && executor != nil {
			txHash, err := executor.Execute(opp, bi.baseFee)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[arb] exec error: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "\n[arb] *** TRADE EXECUTED ***\n")
				fmt.Fprintf(os.Stderr, "[arb] tx: %s\n", txHash.Hex())
				fmt.Fprintf(os.Stderr, "[arb] snowtrace: https://snowtrace.io/tx/%s\n\n", txHash.Hex())
				execOut, _ := json.Marshal(map[string]interface{}{
					"type":   "tx_sent",
					"block":  bi.block,
					"txHash": txHash.Hex(),
				})
				fmt.Println(string(execOut))
			}
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
