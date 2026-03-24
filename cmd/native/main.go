package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"
)

// ─── WebSocket state fetcher ───────────────────────────────────────

type wsFetcher struct {
	conn         *websocket.Conn
	mu           sync.Mutex
	nextID       int
	pending      map[int]chan json.RawMessage
	block        uint64
	timestamp    uint64
	baseFee      uint64
	gasLimit     uint64
	cacheMisses  int64 // counts state server fetches (cache misses)
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type stateServerMessage struct {
	Type        string     `json:"type,omitempty"`
	BlockNumber uint64     `json:"blockNumber,omitempty"`
	Timestamp   uint64     `json:"timestamp,omitempty"`
	BaseFee     uint64     `json:"baseFee,omitempty"`
	GasLimit    uint64     `json:"gasLimit,omitempty"`
	Entries     [][2]string `json:"entries,omitempty"`

	// JSON-RPC fields
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      int             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
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

	// Create state DB with this fetcher
	state := statedb.NewStateDB(f)

	// Read initial_dump
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

	// Parse entries
	storageCount := 0
	accountData := make(map[string]map[string]string) // addr -> {balance, nonce, code}

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

	fmt.Fprintf(os.Stderr, "[native] initial_dump: block=%d, %d storage, %d accounts\n",
		f.block, storageCount, accountCount)

	// Start background reader for responses and block_diff
	go f.readLoop(state)

	return f, state, nil
}

func (f *wsFetcher) readLoop(state *statedb.StateDB) {
	for {
		_, msg, err := f.conn.ReadMessage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[native] ws read error: %v\n", err)
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
			currentBlock = m.BlockNumber
			currentTimestamp = m.Timestamp
			currentBaseFee = m.BaseFee
			currentGasLimit = m.GasLimit
			for _, entry := range m.Entries {
				key, value := entry[0], entry[1]
				if strings.HasPrefix(key, "s:") {
					parts := strings.SplitN(key, ":", 3)
					if len(parts) == 3 {
						addr := common.HexToAddress(parts[1])
						slot := common.HexToHash(parts[2])
						val := common.HexToHash(value)
						state.SetStorageSlot(addr, slot, val)
					}
				}
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

type valueResult struct {
	Value string `json:"value"`
}

func (f *wsFetcher) FetchStorage(addr common.Address, slot common.Hash) common.Hash {
	f.cacheMisses++
	if f.cacheMisses <= 3 {
		fmt.Fprintf(os.Stderr, "[native] CACHE MISS #%d: storage %s slot %s\n", f.cacheMisses, addr.Hex(), slot.Hex())
	}
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"slot":        slot.Hex(),
		"blockNumber": f.block,
	}
	result, err := f.call("state_getStorageAt", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[native] FetchStorage error: %v\n", err)
		return common.Hash{}
	}
	var vr valueResult
	if err := json.Unmarshal(result, &vr); err != nil {
		fmt.Fprintf(os.Stderr, "[native] FetchStorage parse error: %v\n", err)
		return common.Hash{}
	}
	return common.HexToHash(vr.Value)
}

func (f *wsFetcher) FetchBalance(addr common.Address) *uint256.Int {
	f.cacheMisses++
	if f.cacheMisses <= 3 {
		fmt.Fprintf(os.Stderr, "[native] CACHE MISS #%d: balance %s\n", f.cacheMisses, addr.Hex())
	}
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": f.block,
	}
	result, err := f.call("state_getBalance", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[native] FetchBalance error: %v\n", err)
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
	if f.cacheMisses <= 3 {
		fmt.Fprintf(os.Stderr, "[native] CACHE MISS #%d: code %s\n", f.cacheMisses, addr.Hex())
	}
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
	// Block hash fetching not supported via state server; return empty
	return common.Hash{}
}

// ─── Stdin/stdout JSON-RPC ─────────────────────────────────────────

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type ethCallParams struct {
	To             string                       `json:"to"`
	Data           string                       `json:"data"`
	From           string                       `json:"from"`
	StateOverrides map[string]stateOverrideEntry `json:"stateOverrides,omitempty"`
}

type batchCallParams struct {
	StateOverrides map[string]stateOverrideEntry `json:"stateOverrides,omitempty"`
	Calls          []batchCallEntry              `json:"calls"`
	SkipFormulas   bool                          `json:"skipFormulas,omitempty"`
}

type batchCallEntry struct {
	To   string `json:"to"`
	Data string `json:"data"`
	From string `json:"from"`
}

type batchCallResult struct {
	ReturnData string `json:"returnData"`
	GasUsed    uint64 `json:"gasUsed"`
	Error      string `json:"error,omitempty"`
}

type stateOverrideEntry struct {
	Code      string            `json:"code,omitempty"`
	Balance   string            `json:"balance,omitempty"`
	Nonce     *uint64           `json:"nonce,omitempty"`
	StateDiff map[string]string `json:"stateDiff,omitempty"`
}

type response struct {
	ID     int         `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// parsedOverride holds pre-parsed override data for fast per-call application.
type parsedOverride struct {
	addr    common.Address
	balance *uint256.Int
	nonce   uint64
	code    []byte
	slots   []struct {
		slot  common.Hash
		value common.Hash
	}
}

// parseOverrides pre-parses state overrides (once per batch).
func parseOverrides(raw map[string]stateOverrideEntry) []parsedOverride {
	result := make([]parsedOverride, 0, len(raw))
	for addrHex, entry := range raw {
		po := parsedOverride{addr: common.HexToAddress(addrHex)}
		if entry.Code != "" && entry.Code != "0x" {
			po.code, _ = hex.DecodeString(strings.TrimPrefix(entry.Code, "0x"))
			po.balance = uint256.NewInt(0)
			if entry.Balance != "" {
				if bi, ok := new(big.Int).SetString(strings.TrimPrefix(entry.Balance, "0x"), 16); ok {
					po.balance, _ = uint256.FromBig(bi)
				}
			}
			if entry.Nonce != nil {
				po.nonce = *entry.Nonce
			}
		}
		for slotHex, valueHex := range entry.StateDiff {
			po.slots = append(po.slots, struct {
				slot  common.Hash
				value common.Hash
			}{common.HexToHash(slotHex), common.HexToHash(valueHex)})
		}
		result = append(result, po)
	}
	return result
}

// applyParsedOverrides creates a fresh overlay and applies pre-parsed overrides.
func applyParsedOverrides(base *statedb.StateDB, overrides []parsedOverride) *statedb.StateDB {
	if len(overrides) == 0 {
		return base
	}
	overlay := base.NewOverlay()
	for _, po := range overrides {
		if po.code != nil {
			overlay.SetAccount(po.addr, po.balance, po.nonce, po.code)
		}
		for _, s := range po.slots {
			overlay.SetStorageSlot(po.addr, s.slot, s.value)
		}
	}
	return overlay
}

// applyOverrides creates an overlay with state overrides applied (parses from raw).
func applyOverrides(base *statedb.StateDB, raw map[string]stateOverrideEntry) *statedb.StateDB {
	return applyParsedOverrides(base, parseOverrides(raw))
}

var (
	currentBlock     uint64
	currentTimestamp  uint64
	currentBaseFee   uint64
	currentGasLimit  uint64
)

func main() {
	counter := &statedb.Counter{}

	// Check for flags in args
	stateServerURL := ""
	registryPath := ""
	for i, arg := range os.Args {
		if arg == "--state-server" && i+1 < len(os.Args) {
			stateServerURL = os.Args[i+1]
		}
		if arg == "--registry" && i+1 < len(os.Args) {
			registryPath = os.Args[i+1]
		}
	}

	// Load formula registry
	registry := formulas.LoadRegistry(registryPath)
	validated, invalid := registry.RegistryStats()
	if validated+invalid > 0 {
		fmt.Fprintf(os.Stderr, "[native] formula registry: %d validated, %d invalid\n", validated, invalid)
	}

	var fetcher *wsFetcher
	var state *statedb.StateDB

	if stateServerURL != "" {
		var err error
		fetcher, state, err = newWSFetcher(stateServerURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect to state server: %v\n", err)
			os.Exit(1)
		}
		currentBlock = fetcher.block
		currentTimestamp = fetcher.timestamp
		currentBaseFee = fetcher.baseFee
		currentGasLimit = fetcher.gasLimit
		fmt.Fprintf(os.Stderr, "[native] connected to state server %s\n", stateServerURL)
	}

	// Allocate state if no state server (for standalone prefill usage)
	if state == nil {
		state = statedb.NewStateDB(nil)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024*1024), 64*1024*1024) // 64MB buffer (large batches)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "invalid json: %v\n", err)
			continue
		}

		var resp response
		resp.ID = req.ID

		switch req.Method {
		case "increment":
			resp.Result = counter.Increment()

		case "eth_call":
			var params ethCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = fmt.Sprintf("invalid params: %v", err)
				break
			}

			from := common.HexToAddress(params.From)
			to := common.HexToAddress(params.To)
			data, err := hex.DecodeString(strings.TrimPrefix(params.Data, "0x"))
			if err != nil {
				resp.Error = fmt.Sprintf("invalid data: %v", err)
				break
			}

			execState := applyOverrides(state, params.StateOverrides)
			cfg := statedb.EVMConfig{
				BlockNumber: currentBlock,
				Timestamp:   currentTimestamp,
				ChainID:     43114,
				BaseFee:     currentBaseFee,
				GasLimit:    currentGasLimit,
			}

			ret, gasUsed, evmErr := statedb.ExecuteCall(execState, cfg, from, to, data)

			result := map[string]interface{}{
				"returnData": "0x" + hex.EncodeToString(ret),
				"gasUsed":    gasUsed,
			}
			if evmErr != nil {
				result["error"] = evmErr.Error()
			}
			resp.Result = result

		case "eth_call_batch":
			var params batchCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = fmt.Sprintf("invalid params: %v", err)
				break
			}

			parsed := parseOverrides(params.StateOverrides)
			cfg := statedb.EVMConfig{
				BlockNumber: currentBlock,
				Timestamp:   currentTimestamp,
				ChainID:     43114,
				BaseFee:     currentBaseFee,
				GasLimit:    currentGasLimit,
			}

			missesBefore := int64(0)
			if fetcher != nil {
				missesBefore = fetcher.cacheMisses
			}

			// Apply overrides once — EVM calls get thin scratch overlays on top
			baseWithOverrides := applyParsedOverrides(state, parsed)

			results := make([]batchCallResult, len(params.Calls))
			for i, call := range params.Calls {
				data, err := hex.DecodeString(strings.TrimPrefix(call.Data, "0x"))
				if err != nil {
					results[i] = batchCallResult{Error: fmt.Sprintf("invalid data: %v", err)}
					continue
				}

				// Try formula shortcut (unless skipFormulas is set).
				// Reads from base state — pool reserves are not affected by overrides.
				if !params.SkipFormulas {
					reader := func(addr common.Address, key common.Hash) common.Hash { return state.GetState(addr, key) }
					if ret, ok := registry.TryQuote(reader, data); ok {
						results[i] = batchCallResult{
							ReturnData: "0x" + hex.EncodeToString(ret),
							GasUsed:    0,
						}
						continue
					}
				}

				// EVM path — thin scratch overlay on pre-overridden base
				from := common.HexToAddress(call.From)
				to := common.HexToAddress(call.To)
				execState := baseWithOverrides.NewOverlay()
				ret, gasUsed, evmErr := statedb.ExecuteCall(execState, cfg, from, to, data)
				results[i] = batchCallResult{
					ReturnData: "0x" + hex.EncodeToString(ret),
					GasUsed:    gasUsed,
				}
				if evmErr != nil {
					results[i].Error = evmErr.Error()
				}
			}

			missesAfter := int64(0)
			if fetcher != nil {
				missesAfter = fetcher.cacheMisses
			}
			resp.Result = map[string]interface{}{
				"results":     results,
				"cacheMisses": missesAfter - missesBefore,
			}

		case "find_route":
			var params struct {
				TokenIn      string                       `json:"tokenIn"`
				TokenOut     string                       `json:"tokenOut"`
				AmountIn     string                       `json:"amountIn"`
				Pools        string                       `json:"pools"`        // pools.txt content
				MaxHops      int                          `json:"maxHops"`
				PoolLimit    int                          `json:"poolLimit"`
				StateOverrides map[string]stateOverrideEntry `json:"stateOverrides,omitempty"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = fmt.Sprintf("invalid params: %v", err)
				break
			}

			tokenIn := common.HexToAddress(params.TokenIn)
			tokenOut := common.HexToAddress(params.TokenOut)
			amtBig, ok := new(big.Int).SetString(strings.TrimPrefix(params.AmountIn, "0x"), 16)
			if !ok {
				resp.Error = "invalid amountIn"
				break
			}
			amountIn, _ := uint256.FromBig(amtBig)

			limit := params.PoolLimit
			if limit <= 0 {
				limit = 1000
			}
			pools := pf.ParsePools(params.Pools, limit)
			graph := pf.BuildGraph(pools)

			// Parse overrides
			var overrides []pf.ParsedOverride
			for addrHex, entry := range params.StateOverrides {
				po := pf.ParsedOverride{Addr: common.HexToAddress(addrHex)}
				if entry.Code != "" && entry.Code != "0x" {
					po.Code, _ = hex.DecodeString(strings.TrimPrefix(entry.Code, "0x"))
					po.Balance = uint256.NewInt(0)
					if entry.Balance != "" {
						if bi, bOk := new(big.Int).SetString(strings.TrimPrefix(entry.Balance, "0x"), 16); bOk {
							po.Balance, _ = uint256.FromBig(bi)
						}
					}
					if entry.Nonce != nil {
						po.Nonce = *entry.Nonce
					}
				}
				for slotHex, valueHex := range entry.StateDiff {
					po.Slots = append(po.Slots, struct {
						Slot  common.Hash
						Value common.Hash
					}{common.HexToHash(slotHex), common.HexToHash(valueHex)})
				}
				overrides = append(overrides, po)
			}

			cfg := statedb.EVMConfig{
				BlockNumber: currentBlock,
				Timestamp:   currentTimestamp,
				ChainID:     43114,
				BaseFee:     currentBaseFee,
				GasLimit:    currentGasLimit,
			}

			maxHops := params.MaxHops
			if maxHops <= 0 {
				maxHops = 4
			}

			route := pf.FindBestRoute(state, cfg, registry, overrides, graph, tokenIn, tokenOut, amountIn, maxHops)
			if route == nil {
				resp.Result = map[string]interface{}{"route": nil}
			} else {
				resp.Result = route
			}

		default:
			resp.Error = "unknown method: " + req.Method
		}

		out, _ := json.Marshal(resp)
		fmt.Println(string(out))
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
		os.Exit(1)
	}
}
