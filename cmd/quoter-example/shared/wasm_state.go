//go:build js && wasm

package shared

import (
	"bytes"
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
	"defi-toolbox/statedb/wire"

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

	// Fetch counters for diagnostics.
	FetchCount int64
}

// DialBrowser opens a WebSocket to the given URL via the browser API.
func DialBrowser(url string) (*BrowserTransport, error) {
	bt := &BrowserTransport{
		pending:  make(map[int]chan json.RawMessage),
		ready:    make(chan struct{}),
		firstMsg: make(chan []byte, 1),
	}

	bt.ws = js.Global().Get("WebSocket").New(url)
	bt.ws.Set("binaryType", "arraybuffer")

	bt.ws.Set("onopen", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		close(bt.ready)
		return nil
	}))

	// Route messages by WebSocket frame type:
	//   Binary (ArrayBuffer) = gob-encoded initial_dump → firstMsg channel
	//   Text (string) = JSON block_diff or RPC response → handleMessage
	bt.ws.Set("onmessage", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		data := args[0].Get("data")
		if data.InstanceOf(js.Global().Get("ArrayBuffer")) {
			// Binary frame: copy ArrayBuffer → Go []byte via Uint8Array
			uint8Array := js.Global().Get("Uint8Array").New(data)
			buf := make([]byte, uint8Array.Get("length").Int())
			js.CopyBytesToGo(buf, uint8Array)
			// Route to firstMsg (initial_dump) — non-blocking, drops if already received
			select {
			case bt.firstMsg <- buf:
			default:
			}
		} else {
			// Text frame: JSON (block_diff or RPC response)
			bt.handleMessage([]byte(data.String()))
		}
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
		go bt.onPush(data)
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
	bt.FetchCount++
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
	bt.FetchCount++
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
	bt.FetchCount++
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
	bt.FetchCount++
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
func ConnectBrowser(url string) (*statedb.LiveState, *BrowserTransport, error) {
	bt, err := DialBrowser(url)
	if err != nil {
		return nil, nil, err
	}

	// Subscribe.
	bt.sendJSON(map[string]interface{}{"subscribe": true})

	// Read gob-encoded initial_dump (binary WebSocket frame → firstMsg channel).
	var raw []byte
	select {
	case raw = <-bt.firstMsg:
	case <-time.After(30 * time.Second):
		return nil, nil, fmt.Errorf("initial_dump timeout")
	}

	dump, err := wire.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("decode initial_dump: %w", err)
	}

	// Build state from gob dump.
	state := statedb.NewStateDB(bt)
	im := statedb.NewImmutableState(dump.BlockNumber, dump.Timestamp)
	statedb.LoadGobDump(im, dump)
	state.SetImmutable(im)

	ls := statedb.NewLiveStateFromState(state, dump.BlockNumber, dump.Timestamp, dump.BaseFee, dump.GasLimit)

	// Route future block_diffs to LiveState.
	bt.onPush = func(msg []byte) {
		ls.HandleBlockDiff(msg)
	}

	fmt.Fprintf(os.Stderr, "[wasm] connected, block=%d, %d storage keys, %d accounts\n", dump.BlockNumber, len(dump.Storage), len(dump.Accounts))
	return ls, bt, nil
}
