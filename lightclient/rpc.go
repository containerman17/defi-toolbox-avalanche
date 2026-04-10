package lightclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// --------------------------------------------------------------------
// JSON-RPC types
// --------------------------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonRPCError   `json:"error"`
	Method  string          `json:"method"` // for subscription notifications
	Params  json.RawMessage `json:"params"` // for subscription notifications
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// subscriptionNotification is the params envelope for eth_subscription pushes.
type subscriptionNotification struct {
	Subscription string          `json:"subscription"`
	Result       json.RawMessage `json:"result"`
}

// --------------------------------------------------------------------
// rpcWork is a unit of work submitted to the pool's work queue.
// --------------------------------------------------------------------

type rpcWork struct {
	method string
	params interface{}
	result chan rpcWorkResult
}

type rpcWorkResult struct {
	data json.RawMessage
	err  error
}

// --------------------------------------------------------------------
// RPCPool — blocking-workers WebSocket RPC pool
// --------------------------------------------------------------------

// RPCPool maintains a pool of WebSocket connections to an Avalanche node.
// Each connection is serviced by a dedicated worker goroutine that picks
// work from a shared channel, providing natural backpressure.
type RPCPool struct {
	url    string
	size   int
	queue  chan *rpcWork
	nextID atomic.Int64
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewRPCPool creates a pool with the given WebSocket URL and number of
// worker connections. If size <= 0, it defaults to 2 * runtime.NumCPU().
// The pool starts connecting immediately; Call will block until at least
// one worker is connected.
func NewRPCPool(url string, size int) (*RPCPool, error) {
	if size <= 0 {
		size = 2 * runtime.NumCPU()
	}
	p := &RPCPool{
		url:  url,
		size: size,
		// Buffer the queue so callers don't block immediately when workers
		// are available but haven't pulled from the channel yet.
		queue: make(chan *rpcWork, size),
		done:  make(chan struct{}),
	}
	p.wg.Add(size)
	for i := 0; i < size; i++ {
		go p.worker(i)
	}
	return p, nil
}

// Call sends a JSON-RPC request and blocks until the response arrives.
func (p *RPCPool) Call(method string, params interface{}) (json.RawMessage, error) {
	w := &rpcWork{
		method: method,
		params: params,
		result: make(chan rpcWorkResult, 1),
	}
	select {
	case p.queue <- w:
	case <-p.done:
		return nil, fmt.Errorf("rpc pool closed")
	}
	select {
	case res := <-w.result:
		return res.data, res.err
	case <-p.done:
		return nil, fmt.Errorf("rpc pool closed")
	}
}

// Close shuts down all workers and their WebSocket connections.
func (p *RPCPool) Close() {
	select {
	case <-p.done:
		return // already closed
	default:
		close(p.done)
	}
	p.wg.Wait()
}

// --------------------------------------------------------------------
// Worker goroutine — one per WebSocket connection
// --------------------------------------------------------------------

func (p *RPCPool) worker(id int) {
	defer p.wg.Done()
	name := fmt.Sprintf("rpc-worker-%d", id)
	var conn *websocket.Conn

	connect := func() *websocket.Conn {
		backoff := 100 * time.Millisecond
		const maxBackoff = 5 * time.Second
		for {
			select {
			case <-p.done:
				return nil
			default:
			}
			c, _, err := websocket.DefaultDialer.Dial(p.url, nil)
			if err != nil {
				log.Printf("%s: connect error: %v (retry in %v)", name, err, backoff)
				select {
				case <-time.After(backoff):
				case <-p.done:
					return nil
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			return c
		}
	}

	// Initial connection.
	conn = connect()
	if conn == nil {
		return
	}

	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	for {
		// Pick work from the shared queue.
		var w *rpcWork
		select {
		case w = <-p.queue:
		case <-p.done:
			return
		}

		// Execute the request, retrying on connection failure.
		for {
			data, err := p.executeOnce(conn, w.method, w.params)
			if err != nil {
				if isRPCError(err) {
					// RPC-level error (e.g. invalid method) — connection
					// is fine, return the error to the caller.
					w.result <- rpcWorkResult{err: err}
					break
				}
				// Connection-level error: reconnect and retry the same request.
				log.Printf("%s: request failed (%s): %v — reconnecting", name, w.method, err)
				conn.Close()
				conn = connect()
				if conn == nil {
					// Pool is shutting down.
					w.result <- rpcWorkResult{err: fmt.Errorf("rpc pool closed")}
					return
				}
				continue
			}
			w.result <- rpcWorkResult{data: data}
			break
		}
	}
}

// executeOnce sends a single JSON-RPC request on the given connection
// and blocks until the response arrives. Returns a connection-level
// error (should reconnect) or an RPC-level error (returned to caller
// as a Go error).
func (p *RPCPool) executeOnce(conn *websocket.Conn, method string, params interface{}) (json.RawMessage, error) {
	id := p.nextID.Add(1)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	// Block waiting for response. Since each worker owns its socket
	// exclusively, the next message is our response.
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	var resp jsonRPCResponse
	if err := json.Unmarshal(msg, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if resp.Error != nil {
		// RPC-level error — connection is fine, return the error to the caller.
		// We wrap it so executeOnce's caller can distinguish, but this is NOT
		// a connection error so we return it via a special path.
		return nil, &rpcError{resp.Error}
	}

	return resp.Result, nil
}

// rpcError wraps a JSON-RPC error. It is not a connection-level failure.
type rpcError struct {
	err *jsonRPCError
}

func (e *rpcError) Error() string { return e.err.Error() }

// isRPCError returns true if the error is a JSON-RPC protocol error
// (not a connection failure). Used by the worker loop to decide whether
// to retry with reconnect or return the error to the caller.
func isRPCError(err error) bool {
	_, ok := err.(*rpcError)
	return ok
}

// --------------------------------------------------------------------
// SubscribeNewHeads — dedicated subscription socket
// --------------------------------------------------------------------

// SubscribeNewHeads opens a dedicated WebSocket connection (outside the
// worker pool) and subscribes to newHeads. It returns a channel that
// receives raw JSON header objects. The subscription runs until ctx is
// cancelled, at which point the channel is closed. Reconnects
// automatically on connection failure.
func (p *RPCPool) SubscribeNewHeads(ctx context.Context) (<-chan json.RawMessage, error) {
	ch := make(chan json.RawMessage, 16)
	go p.subscriptionLoop(ctx, ch)
	return ch, nil
}

func (p *RPCPool) subscriptionLoop(ctx context.Context, ch chan<- json.RawMessage) {
	defer close(ch)

	backoff := 100 * time.Millisecond
	const maxBackoff = 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		default:
		}

		err := p.runSubscription(ctx, ch)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("newHeads subscription error: %v — reconnecting in %v", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			case <-p.done:
				return
			}
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (p *RPCPool) runSubscription(ctx context.Context, ch chan<- json.RawMessage) error {
	conn, _, err := websocket.DefaultDialer.Dial(p.url, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	// Send eth_subscribe request.
	id := p.nextID.Add(1)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "eth_subscribe",
		Params:  []interface{}{"newHeads"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal subscribe: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("write subscribe: %w", err)
	}

	// Read the subscription confirmation.
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read subscribe response: %w", err)
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(msg, &resp); err != nil {
		return fmt.Errorf("unmarshal subscribe response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}

	// Read notifications until the connection drops or context is cancelled.
	readDone := make(chan error, 1)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				readDone <- err
				return
			}
			var notif jsonRPCResponse
			if err := json.Unmarshal(msg, &notif); err != nil {
				continue
			}
			// Extract the result from the subscription notification params.
			if notif.Method == "eth_subscription" && len(notif.Params) > 0 {
				var sub subscriptionNotification
				if err := json.Unmarshal(notif.Params, &sub); err != nil {
					continue
				}
				select {
				case ch <- sub.Result:
				default:
					// Drop if consumer is too slow — better than blocking
					// the read loop and causing the socket to back up.
				}
			}
		}
	}()

	select {
	case err := <-readDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return fmt.Errorf("pool closed")
	}
}
