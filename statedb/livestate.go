package statedb

// LiveState — Shared state client with block-level concurrency control.
//
// Two-universe concurrency model:
//
//  1. Block updates (exclusive) — a single goroutine applies block_diff messages.
//     It acquires blockMu.Lock(), writes storage slots, updates block metadata,
//     calls the onBlock callback, then releases the lock. No quoting can happen
//     during a block update.
//
//  2. Quoting (shared) — N goroutines read state concurrently under blockMu.RLock().
//     Any read can miss the cache and trigger an on-demand fetch via the transport,
//     which writes the result back to the StateDB. This is safe because all writes
//     are idempotent: within the same block, the same slot always has the same value.
//     StateDB uses sync.Map for both accounts and storage, so concurrent writes from
//     multiple quoting goroutines never corrupt state.
//
// Go's sync.RWMutex prevents writer starvation: once Lock() is waiting, new RLock()
// calls queue behind it. This means in-flight quotes finish, then the block update
// proceeds, then new quotes can start. No quote ever sees a half-applied block.
//
// LiveState implements the Fetcher interface, routing FetchStorage/Balance/Nonce/Code
// through the WSTransport's Call() method to the state server.
//
// Works identically for /live and /debug/<block> endpoints. Frozen (debug) blocks
// simply never send block_diff messages — the same code path handles both.

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

	"defi-toolbox/statedb/wire"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// LiveState wraps a StateDB with block-level concurrency control.
//
// The blockMu RWMutex enforces the two-universe separation:
//   - RLock: quoting (N goroutines, concurrent, cache-miss writes are safe via sync.Map)
//   - Lock:  block update (one goroutine, exclusive, waits for all quotes to finish)
//
// Block metadata (block, timestamp, baseFee, gasLimit) uses atomic uint64 for
// lock-free reads in hot paths that only need approximate values (e.g. logging).
// For consistent snapshots of all metadata, hold RLock.
type LiveState struct {
	state     *StateDB
	transport *WSTransport
	blockMu   sync.RWMutex

	block     atomic.Uint64
	timestamp atomic.Uint64
	baseFee   atomic.Uint64
	gasLimit  atomic.Uint64

	// onBlock is called after each block_diff is fully applied, with blockMu
	// write-locked. The entries parameter contains the raw key-value pairs from
	// the diff so consumers can do pool invalidation without re-parsing.
	// The callback MUST NOT call RLock — it would deadlock (already write-locked).
	onBlock func(ls *LiveState, entries [][2]string)
}

// serverMessage is the wire format for messages from the state server.
// Used for both initial_dump and block_diff messages.
type serverMessage struct {
	Type        string      `json:"type,omitempty"`
	BlockNumber uint64      `json:"blockNumber,omitempty"`
	Timestamp   uint64      `json:"timestamp,omitempty"`
	BaseFee     uint64      `json:"baseFee,omitempty"`
	GasLimit    uint64      `json:"gasLimit,omitempty"`
	Entries     [][2]string `json:"entries,omitempty"`
}

// valueResponse is the wire format for JSON-RPC responses from the state server.
// All state_get* methods return {"value": "0x..."}.
type valueResponse struct {
	Value string `json:"value"`
}

// Connect dials the state server at url, reads the initial_dump to populate
// a StateDB, and starts processing block_diff messages in the background.
//
// Works for both /live and /debug/<block> endpoints. Frozen (debug) blocks
// use the exact same code — they simply never send block_diff messages,
// so the read goroutine just waits forever (or until Close).
func Connect(url string) (*LiveState, error) {
	transport, err := DialWS(url)
	if err != nil {
		return nil, err
	}

	// Subscribe to receive initial_dump + block_diffs.
	if err := transport.WriteSubscribe(); err != nil {
		transport.Close()
		return nil, fmt.Errorf("write subscribe: %w", err)
	}

	// Read gob-encoded initial_dump (binary WebSocket frame).
	raw, err := transport.ReadRawMessage()
	if err != nil {
		transport.Close()
		return nil, fmt.Errorf("read initial_dump: %w", err)
	}

	dump, err := wire.Decode(bytes.NewReader(raw))
	if err != nil {
		transport.Close()
		return nil, fmt.Errorf("decode initial_dump: %w", err)
	}

	ls := &LiveState{
		transport: transport,
	}

	ls.state = NewStateDB(ls)

	// Build initial ImmutableState from the gob dump — goes directly into the fast layer.
	im := NewImmutableState(dump.BlockNumber, dump.Timestamp)
	storageCount, accountCount := loadGobDump(im, dump)
	ls.state.SetImmutable(im)

	ls.block.Store(dump.BlockNumber)
	ls.timestamp.Store(dump.Timestamp)
	ls.baseFee.Store(dump.BaseFee)
	ls.gasLimit.Store(dump.GasLimit)
	fmt.Fprintf(os.Stderr, "[livestate] initial_dump: block=%d, %d storage, %d accounts\n",
		dump.BlockNumber, storageCount, accountCount)

	// Block diffs are processed on a dedicated goroutine, NOT on the readLoop.
	// This avoids a deadlock: if the readLoop called handlePushMessage directly,
	// it would try to acquire blockMu.Lock(). But a quoting goroutine might be
	// holding blockMu.RLock() and waiting for a FetchStorage response — which
	// the readLoop needs to deliver. Deadlock: readLoop blocked on Lock, quoting
	// blocked waiting for readLoop to deliver the response.
	//
	// By decoupling via a channel, the readLoop stays free to deliver RPC responses
	// even while block_diffs queue up waiting for the write lock.
	pushCh := make(chan []byte, 256)
	transport.SetOnMessage(func(msg []byte) {
		select {
		case pushCh <- msg:
		default:
			// Channel full — drop the oldest and enqueue the new one.
			// This prevents the readLoop from blocking (which would deadlock
			// RPC response delivery). The block_diff processor will catch up
			// via subsequent diffs.
			select {
			case <-pushCh:
			default:
			}
			pushCh <- msg
		}
	})
	go func() {
		for msg := range pushCh {
			ls.handlePushMessage(msg)
		}
	}()
	transport.StartReadLoop()

	return ls, nil
}

// loadDumpEntries parses initial_dump entries and populates the StateDB.
// Returns (storageCount, accountCount) for logging.
//
// Entry format from the state server:
//   - "s:<addr>:<slot>" -> hex value  (storage slot)
//   - "b:<addr>" -> hex value         (balance)
//   - "n:<addr>" -> hex value         (nonce)
//   - "c:<addr>" -> hex bytecode      (contract code)
func loadDumpEntries(state *ImmutableState, entries [][2]string) (int, int) {
	storageCount := 0
	accountData := make(map[string]map[string]string)

	for _, entry := range entries {
		key, value := entry[0], entry[1]
		if strings.HasPrefix(key, "s:") {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) == 3 {
				addr := common.HexToAddress(parts[1])
				slot := common.HexToHash(parts[2])
				val := common.HexToHash(value)
				state.SetStorage(addr, slot, val)
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
	}

	return storageCount, len(accountData)
}

// handlePushMessage processes a server push message (block_diff).
// Called from a dedicated goroutine (not the readLoop — see Connect).
func (ls *LiveState) handlePushMessage(msg []byte) {
	var m serverMessage
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	if m.Type != "block_diff" {
		return
	}

	// Acquire exclusive lock — in-flight quotes finish first (RWMutex guarantee).
	ls.blockMu.Lock()

	// Build a BlockDiff from the entries.
	diff := &BlockDiff{
		BlockNum:  m.BlockNumber,
		BlockTime: m.Timestamp,
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Balance:   make(map[common.Address]*uint256.Int),
		Nonce:     make(map[common.Address]uint64),
	}
	for _, entry := range m.Entries {
		key, value := entry[0], entry[1]
		if strings.HasPrefix(key, "s:") {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) == 3 {
				addr := common.HexToAddress(parts[1])
				slot := common.HexToHash(parts[2])
				if diff.Storage[addr] == nil {
					diff.Storage[addr] = make(map[common.Hash]common.Hash)
				}
				diff.Storage[addr][slot] = common.HexToHash(value)
			}
		} else if strings.HasPrefix(key, "b:") {
			addr := common.HexToAddress(key[2:])
			bi, _ := new(big.Int).SetString(strings.TrimPrefix(value, "0x"), 16)
			if bi != nil {
				bal, _ := uint256.FromBig(bi)
				diff.Balance[addr] = bal
			}
		} else if strings.HasPrefix(key, "n:") {
			addr := common.HexToAddress(key[2:])
			ni, _ := new(big.Int).SetString(strings.TrimPrefix(value, "0x"), 16)
			if ni != nil {
				diff.Nonce[addr] = ni.Uint64()
			}
		}
	}

	// Atomic swap: CloneWithDiff merges diff + backfill → new immutable state.
	ls.state.ApplyBlockDiff(diff)

	// Update block metadata.
	ls.block.Store(m.BlockNumber)
	ls.timestamp.Store(m.Timestamp)
	ls.baseFee.Store(m.BaseFee)
	ls.gasLimit.Store(m.GasLimit)

	// Notify consumer (e.g. arb bot does pool invalidation here).
	if ls.onBlock != nil {
		ls.onBlock(ls, m.Entries)
	}

	ls.blockMu.Unlock()
}

// ─── Public API ─────────────────────────────────────────────────────

// RLock acquires a shared lock for quoting. Multiple goroutines can hold
// RLock concurrently. Block updates wait for all RLocks to be released.
func (ls *LiveState) RLock() { ls.blockMu.RLock() }

// RUnlock releases the shared quoting lock.
func (ls *LiveState) RUnlock() { ls.blockMu.RUnlock() }

// State returns the underlying StateDB for direct state access.
// Callers should hold RLock while reading state.
func (ls *LiveState) State() *StateDB { return ls.state }

// Block returns the current block number (atomic, lock-free).
func (ls *LiveState) Block() uint64 { return ls.block.Load() }

// Timestamp returns the current block timestamp (atomic, lock-free).
func (ls *LiveState) Timestamp() uint64 { return ls.timestamp.Load() }

// BaseFee returns the current block base fee (atomic, lock-free).
func (ls *LiveState) BaseFee() uint64 { return ls.baseFee.Load() }

// GasLimit returns the current block gas limit (atomic, lock-free).
func (ls *LiveState) GasLimit() uint64 { return ls.gasLimit.Load() }

// EVMConfig returns an EVMConfig snapshot for the current block.
// Uses atomic reads — for a fully consistent snapshot, hold RLock.
func (ls *LiveState) EVMConfig() EVMConfig {
	return EVMConfig{
		BlockNumber: ls.block.Load(),
		Timestamp:   ls.timestamp.Load(),
		ChainID:     43114,
		BaseFee:     ls.baseFee.Load(),
		GasLimit:    ls.gasLimit.Load(),
	}
}

// SetOnBlock registers a callback fired after each block_diff is applied.
// The callback runs with blockMu write-locked. The entries parameter contains
// the raw key-value pairs from the block_diff so the consumer can do
// invalidation (e.g. marking dirty pools) without re-parsing storage keys.
//
// The callback MUST NOT call RLock — it would deadlock since the write lock
// is already held.
func (ls *LiveState) SetOnBlock(fn func(ls *LiveState, entries [][2]string)) {
	ls.blockMu.Lock()
	ls.onBlock = fn
	ls.blockMu.Unlock()
}

// Transport returns the underlying WSTransport for direct RPC calls.
// Used by consumers that need to make custom calls (e.g. eth_call).
func (ls *LiveState) Transport() *WSTransport { return ls.transport }

// Close closes the underlying WebSocket connection.
func (ls *LiveState) Close() error { return ls.transport.Close() }

// ─── Fetcher interface implementation ───────────────────────────────
// LiveState implements statedb.Fetcher so it can back a StateDB.
// Each method sends a JSON-RPC request through the transport and parses
// the response. The block number comes from ls.block (atomic read).

// Ensure interface compliance at compile time.
var _ Fetcher = (*LiveState)(nil)

// FetchCount tracks the total number of network fetches (for debugging).
var FetchCount atomic.Int64

// FetchStorage fetches a storage slot from the state server.
func (ls *LiveState) FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error) {
	FetchCount.Add(1)
	block := ls.block.Load()
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"slot":        slot.Hex(),
		"blockNumber": block,
	}
	result, err := ls.transport.Call("state_getStorageAt", params)
	if err != nil {
		return common.Hash{}, fmt.Errorf("FetchStorage %s slot=%s block=%d: %w",
			addr.Hex()[:10], slot.Hex()[:14], block, err)
	}
	var vr valueResponse
	if err := json.Unmarshal(result, &vr); err != nil {
		return common.Hash{}, fmt.Errorf("FetchStorage parse %s slot=%s: %w raw=%s",
			addr.Hex()[:10], slot.Hex()[:14], err, string(result))
	}
	return common.HexToHash(vr.Value), nil
}

// FetchBalance fetches an account balance from the state server.
func (ls *LiveState) FetchBalance(addr common.Address) (*uint256.Int, error) {
	FetchCount.Add(1)
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": ls.block.Load(),
	}
	result, err := ls.transport.Call("state_getBalance", params)
	if err != nil {
		return nil, fmt.Errorf("FetchBalance %s: %w", addr.Hex()[:10], err)
	}
	var vr valueResponse
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

// FetchNonce fetches an account nonce from the state server.
func (ls *LiveState) FetchNonce(addr common.Address) (uint64, error) {
	FetchCount.Add(1)
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": ls.block.Load(),
	}
	result, err := ls.transport.Call("state_getNonce", params)
	if err != nil {
		return 0, fmt.Errorf("FetchNonce %s: %w", addr.Hex()[:10], err)
	}
	var vr valueResponse
	if err := json.Unmarshal(result, &vr); err != nil {
		return 0, fmt.Errorf("FetchNonce parse %s: %w", addr.Hex()[:10], err)
	}
	bi, ok := new(big.Int).SetString(strings.TrimPrefix(vr.Value, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("FetchNonce parse hex %s: %s", addr.Hex()[:10], vr.Value)
	}
	return bi.Uint64(), nil
}

// FetchCode fetches contract bytecode from the state server.
func (ls *LiveState) FetchCode(addr common.Address) ([]byte, error) {
	FetchCount.Add(1)
	params := map[string]interface{}{
		"address":     addr.Hex(),
		"blockNumber": ls.block.Load(),
	}
	result, err := ls.transport.Call("state_getCode", params)
	if err != nil {
		return nil, fmt.Errorf("FetchCode %s: %w", addr.Hex()[:10], err)
	}
	var vr valueResponse
	if err := json.Unmarshal(result, &vr); err != nil {
		return nil, fmt.Errorf("FetchCode parse %s: %w", addr.Hex()[:10], err)
	}
	if vr.Value == "" || vr.Value == "0x" {
		return nil, nil
	}
	code, _ := hex.DecodeString(strings.TrimPrefix(vr.Value, "0x"))
	return code, nil
}

// FetchBlockHash returns an empty hash — the state server does not serve
// historical block hashes, and they are not needed for quoting.
func (ls *LiveState) FetchBlockHash(num uint64) (common.Hash, error) {
	return common.Hash{}, nil
}

// ─── External construction (for WASM and other non-gorilla transports) ──

// NewLiveStateFromState creates a LiveState from a pre-built StateDB.
// Used by WASM (or other environments) where gorilla/websocket is unavailable.
// The caller is responsible for feeding block diffs via HandleBlockDiff.
func NewLiveStateFromState(state *StateDB, block, timestamp, baseFee, gasLimit uint64) *LiveState {
	ls := &LiveState{state: state}
	ls.block.Store(block)
	ls.timestamp.Store(timestamp)
	ls.baseFee.Store(baseFee)
	ls.gasLimit.Store(gasLimit)
	return ls
}

// HandleBlockDiff processes a raw block_diff JSON message from an external source.
// This is the external equivalent of the internal handlePushMessage method.
func (ls *LiveState) HandleBlockDiff(msg []byte) {
	ls.handlePushMessage(msg)
}

// LoadDumpEntries populates an ImmutableState from initial_dump entries.
// Returns (storageCount, accountCount). This is the exported wrapper of
// loadDumpEntries, for use by WASM and other external constructors.
func LoadDumpEntries(state *ImmutableState, entries [][2]string) (int, int) {
	return loadDumpEntries(state, entries)
}

// loadGobDump populates an ImmutableState from a gob-decoded GobDump.
// Type-casts [20]byte → common.Address and [32]byte → common.Hash (zero-cost).
func loadGobDump(state *ImmutableState, dump *wire.GobDump) (int, int) {
	for _, e := range dump.Storage {
		state.SetStorage(common.Address(e.Addr), common.Hash(e.Slot), common.Hash(e.Value))
	}
	for _, a := range dump.Accounts {
		balance := new(uint256.Int)
		balance.SetBytes32(a.Balance[:])
		state.SetAccount(common.Address(a.Addr), balance, a.Nonce, a.Code)
	}
	return len(dump.Storage), len(dump.Accounts)
}

// LoadGobDump is the exported wrapper of loadGobDump, for use by WASM.
func LoadGobDump(state *ImmutableState, dump *wire.GobDump) (int, int) {
	return loadGobDump(state, dump)
}

// ServerMessage is the exported alias of the wire format for state server messages.
// Used by WASM to parse initial_dump and block_diff messages externally.
type ServerMessage = serverMessage
