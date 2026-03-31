//go:build js && wasm

package shared

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// BrowserTransport implements statedb.Fetcher using the browser WebSocket API.
// It multiplexes concurrent JSON-RPC calls over a single WebSocket connection,
// using the same ID-based routing pattern as statedb.WSTransport.
type BrowserTransport struct {
	ws     js.Value
	mu     sync.Mutex
	nextID int
	pending map[int]chan json.RawMessage
	onPush  func([]byte) // called for non-RPC push messages (block_diff)
	ready   chan struct{}
	closed  bool

	// firstMsg is used to synchronously capture the initial_dump before
	// the read loop starts routing messages.
	firstMsg chan []byte
}

// DialBrowser opens a WebSocket to the given URL via the browser API.
func DialBrowser(url string) (*BrowserTransport, error) {
	bt := &BrowserTransport{
		pending:  make(map[int]chan json.RawMessage),
		ready:    make(chan struct{}),
		firstMsg: make(chan []byte, 1),
	}

	bt.ws = js.Global().Get("WebSocket").New(url)
	bt.ws.Set("binaryType", "blob")

	bt.ws.Set("onopen", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		close(bt.ready)
		return nil
	}))

	bt.ws.Set("onmessage", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		data := args[0].Get("data").String()
		bt.handleMessage([]byte(data))
		return nil
	}))

	bt.ws.Set("onerror", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		fmt.Fprintf(os.Stderr, "[wasm] websocket error\n")
		return nil
	}))

	bt.ws.Set("onclose", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		bt.mu.Lock()
		bt.closed = true
		for _, ch := range bt.pending {
			close(ch)
		}
		bt.mu.Unlock()
		return nil
	}))

	// Wait for connection to open.
	select {
	case <-bt.ready:
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("websocket connect timeout")
	}

	return bt, nil
}

func (bt *BrowserTransport) handleMessage(data []byte) {
	// Sniff: if it has an "id" field, it's a JSON-RPC response.
	var probe struct {
		ID   int    `json:"id"`
		Type string `json:"type"`
	}
	json.Unmarshal(data, &probe)

	if probe.ID > 0 {
		// RPC response — route to pending caller.
		bt.mu.Lock()
		ch, ok := bt.pending[probe.ID]
		if ok {
			delete(bt.pending, probe.ID)
		}
		bt.mu.Unlock()
		if ok {
			var resp struct {
				Result json.RawMessage `json:"result"`
			}
			json.Unmarshal(data, &resp)
			ch <- resp.Result
		}
		return
	}

	// Push message (initial_dump or block_diff).
	// Route first message to firstMsg channel for synchronous initial_dump read.
	select {
	case bt.firstMsg <- data:
		return
	default:
	}

	if bt.onPush != nil {
		bt.onPush(data)
	}
}

func (bt *BrowserTransport) sendJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	bt.ws.Call("send", string(b))
	return nil
}

// Call sends a JSON-RPC request and blocks until the response arrives.
func (bt *BrowserTransport) Call(method string, params interface{}) (json.RawMessage, error) {
	bt.mu.Lock()
	if bt.closed {
		bt.mu.Unlock()
		return nil, fmt.Errorf("websocket closed")
	}
	bt.nextID++
	id := bt.nextID
	ch := make(chan json.RawMessage, 1)
	bt.pending[id] = ch
	bt.mu.Unlock()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := bt.sendJSON(req); err != nil {
		bt.mu.Lock()
		delete(bt.pending, id)
		bt.mu.Unlock()
		return nil, err
	}

	select {
	case result, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("websocket closed while waiting for response")
		}
		return result, nil
	case <-time.After(30 * time.Second):
		bt.mu.Lock()
		delete(bt.pending, id)
		bt.mu.Unlock()
		return nil, fmt.Errorf("rpc timeout: %s", method)
	}
}

// ── Fetcher interface ──

type valueResponse struct {
	Value string `json:"value"`
}

func (bt *BrowserTransport) FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error) {
	params := map[string]interface{}{"address": addr.Hex(), "slot": slot.Hex(), "blockNumber": 0}
	result, err := bt.Call("state_getStorageAt", params)
	if err != nil {
		return common.Hash{}, err
	}
	var vr valueResponse
	json.Unmarshal(result, &vr)
	return common.HexToHash(vr.Value), nil
}

func (bt *BrowserTransport) FetchBalance(addr common.Address) (*uint256.Int, error) {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": 0}
	result, err := bt.Call("state_getBalance", params)
	if err != nil {
		return nil, err
	}
	var vr valueResponse
	json.Unmarshal(result, &vr)
	bi, _ := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if bi == nil {
		return uint256.NewInt(0), nil
	}
	val, _ := uint256.FromBig(bi)
	return val, nil
}

func (bt *BrowserTransport) FetchNonce(addr common.Address) (uint64, error) {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": 0}
	result, err := bt.Call("state_getNonce", params)
	if err != nil {
		return 0, err
	}
	var vr valueResponse
	json.Unmarshal(result, &vr)
	bi, _ := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if bi == nil {
		return 0, nil
	}
	return bi.Uint64(), nil
}

func (bt *BrowserTransport) FetchCode(addr common.Address) ([]byte, error) {
	params := map[string]interface{}{"address": addr.Hex(), "blockNumber": 0}
	result, err := bt.Call("state_getCode", params)
	if err != nil {
		return nil, err
	}
	var vr valueResponse
	json.Unmarshal(result, &vr)
	if vr.Value == "" || vr.Value == "0x" {
		return nil, nil
	}
	code, _ := hex.DecodeString(strings.TrimPrefix(vr.Value, "0x"))
	return code, nil
}

func (bt *BrowserTransport) FetchBlockHash(num uint64) (common.Hash, error) {
	return common.Hash{}, nil
}

// ── ConnectBrowser ──

// ConnectBrowser connects to a state server via the browser WebSocket API,
// reads the initial_dump, and returns a LiveState ready for quoting.
func ConnectBrowser(url string) (*statedb.LiveState, error) {
	bt, err := DialBrowser(url)
	if err != nil {
		return nil, err
	}

	// Subscribe.
	bt.sendJSON(map[string]interface{}{"subscribe": true})

	// Read initial_dump (first push message).
	var raw []byte
	select {
	case raw = <-bt.firstMsg:
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("initial_dump timeout")
	}

	var dump statedb.ServerMessage
	if err := json.Unmarshal(raw, &dump); err != nil {
		return nil, fmt.Errorf("parse initial_dump: %w", err)
	}
	if dump.Type != "initial_dump" {
		return nil, fmt.Errorf("expected initial_dump, got %q", dump.Type)
	}

	// Build state from dump.
	state := statedb.NewStateDB(bt)
	im := statedb.NewImmutableState(dump.BlockNumber, dump.Timestamp)
	statedb.LoadDumpEntries(im, dump.Entries)
	state.SetImmutable(im)

	ls := statedb.NewLiveStateFromState(state, dump.BlockNumber, dump.Timestamp, dump.BaseFee, dump.GasLimit)

	// Route future block_diffs to LiveState.
	bt.onPush = func(msg []byte) {
		ls.HandleBlockDiff(msg)
	}

	fmt.Fprintf(os.Stderr, "[wasm] connected, block=%d\n", dump.BlockNumber)
	return ls, nil
}
