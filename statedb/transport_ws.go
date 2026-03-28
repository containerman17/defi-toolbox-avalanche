package statedb

// WSTransport — JSON-RPC transport over WebSocket.
//
// Extracts the duplicated connection/call/readLoop pattern shared by all consumers
// (arb, benchmark, native, discover). Each had its own wsFetcher with:
//   - a websocket.Conn
//   - a write mutex protecting conn.WriteMessage
//   - an auto-incrementing request ID
//   - a pending map (int -> chan) for request/response matching
//   - a call(method, params) function with 30s timeout
//   - a readLoop that routes responses by ID
//
// The transport distinguishes two kinds of messages from the server:
//
//  1. JSON-RPC responses — have an "id" field. Routed to the pending channel
//     so the caller of Call() gets its response.
//
//  2. Server push messages — have a "type" field (initial_dump, block_diff).
//     Forwarded to the onMessage callback set by the consumer (LiveState).
//
// Reconnect is NOT handled — that's the consumer's responsibility.

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSTransport is a JSON-RPC transport over a single WebSocket connection.
// It multiplexes concurrent Call() requests over the shared connection and
// routes responses back to the correct caller via pending channels.
type WSTransport struct {
	conn    *websocket.Conn
	mu      sync.Mutex          // protects conn writes + nextID + pending
	nextID  int                 // auto-incrementing request ID
	pending map[int]chan wsResult // request ID -> response channel

	// onMessage is called for non-RPC messages (block_diff, initial_dump).
	// Set by the consumer before starting the read goroutine.
	onMessage func([]byte)
}

// wsResult carries either a successful JSON-RPC result or an error string
// from the server. Sent through the pending channel to unblock Call().
type wsResult struct {
	data json.RawMessage
	err  error
}

// jsonRPCRequest is the wire format for outgoing JSON-RPC requests.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// jsonRPCResponse is the wire format for incoming JSON-RPC responses.
// Also used to sniff whether a message is an RPC response (has ID > 0)
// or a server push (has Type field).
type jsonRPCResponse struct {
	ID     int             `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
	Type   string          `json:"type,omitempty"`
}

// DialWS connects to a WebSocket URL and returns a ready-to-use transport.
// The caller must call StartReadLoop() after setting onMessage, or the
// transport will deadlock on the first Call().
func DialWS(url string) (*WSTransport, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("ws dial %s: %w", url, err)
	}
	return &WSTransport{
		conn:    conn,
		pending: make(map[int]chan wsResult),
	}, nil
}

// SetOnMessage sets the callback for server push messages (block_diff etc).
// Must be called before StartReadLoop.
func (t *WSTransport) SetOnMessage(fn func([]byte)) {
	t.onMessage = fn
}

// ReadRawMessage reads a single raw message from the WebSocket.
// Used during connection setup to read the initial_dump synchronously
// before the read goroutine is started.
func (t *WSTransport) ReadRawMessage() ([]byte, error) {
	_, msg, err := t.conn.ReadMessage()
	return msg, err
}

// StartReadLoop launches a goroutine that reads messages from the WebSocket,
// routes JSON-RPC responses to their pending channels, and forwards push
// messages to the onMessage callback.
//
// This goroutine runs until the connection is closed or errors out.
func (t *WSTransport) StartReadLoop() {
	go t.readLoop()
}

func (t *WSTransport) readLoop() {
	for {
		_, msg, err := t.conn.ReadMessage()
		if err != nil {
			// Connection closed — wake up all pending callers.
			t.mu.Lock()
			for id, ch := range t.pending {
				ch <- wsResult{err: fmt.Errorf("ws closed: %w", err)}
				delete(t.pending, id)
			}
			t.mu.Unlock()
			return
		}

		// Sniff the message to determine if it's an RPC response or a push.
		var resp jsonRPCResponse
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}

		if resp.ID > 0 {
			// JSON-RPC response — route to the pending caller.
			t.mu.Lock()
			ch, ok := t.pending[resp.ID]
			if ok {
				delete(t.pending, resp.ID)
			}
			t.mu.Unlock()
			if ok {
				if resp.Error != nil && string(resp.Error) != "null" {
					ch <- wsResult{err: fmt.Errorf("rpc error: %s", string(resp.Error))}
				} else {
					ch <- wsResult{data: resp.Result}
				}
			}
			continue
		}

		// Server push message (block_diff, initial_dump, etc).
		if t.onMessage != nil {
			t.onMessage(msg)
		}
	}
}

// Call sends a JSON-RPC request and waits for the response.
// Thread-safe — multiple goroutines can call concurrently.
// Returns the result field on success, or an error on timeout/failure.
func (t *WSTransport) Call(method string, params interface{}) (json.RawMessage, error) {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	ch := make(chan wsResult, 1)
	t.pending[id] = ch

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(req)
	err := t.conn.WriteMessage(websocket.TextMessage, data)
	t.mu.Unlock()

	if err != nil {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, fmt.Errorf("ws write: %w", err)
	}

	select {
	case result := <-ch:
		return result.data, result.err
	case <-time.After(30 * time.Second):
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for %s response (id=%d)", method, id)
	}
}

// Close closes the underlying WebSocket connection.
func (t *WSTransport) Close() error {
	return t.conn.Close()
}
