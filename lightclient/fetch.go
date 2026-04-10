package lightclient

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/holiman/uint256"
)

// ─── Block data types ───────────────────────────────────────────────

// BlockData holds a parsed Avalanche C-Chain block with all header fields
// and the full transaction list.
type BlockData struct {
	Hash       common.Hash
	ParentHash common.Hash
	Number     uint64
	Timestamp  uint64
	BaseFee    *big.Int
	GasLimit   uint64
	Coinbase   common.Address
	Difficulty *big.Int
	MixDigest  common.Hash
	Extra      []byte // raw extra data (needed for atomic transactions)

	// Avalanche-specific header extensions
	TimestampMilliseconds *uint64 // sub-second precision
	MinDelayExcess        *uint64 // ACP-226 delay tracking

	Transactions []*types.Transaction
}

// rpcBlockJSON is the raw JSON shape returned by eth_getBlockByNumber.
type rpcBlockJSON struct {
	Hash                  common.Hash       `json:"hash"`
	ParentHash            common.Hash       `json:"parentHash"`
	Number                *hexutil.Big      `json:"number"`
	Timestamp             hexutil.Uint64    `json:"timestamp"`
	TimestampMilliseconds *hexutil.Uint64   `json:"timestampMilliseconds"`
	MinDelayExcess        *hexutil.Uint64   `json:"minDelayExcess"`
	Coinbase              common.Address    `json:"miner"`
	Difficulty            *hexutil.Big      `json:"difficulty"`
	GasLimit              hexutil.Uint64    `json:"gasLimit"`
	BaseFee               *hexutil.Big      `json:"baseFeePerGas"`
	MixDigest             common.Hash       `json:"mixHash"`
	Extra                 hexutil.Bytes     `json:"extraData"`
	Transactions          []json.RawMessage `json:"transactions"`
}

// ─── Trace types ────────────────────────────────────────────────────

// TraceDiff holds the canonical state diff for a block as returned by
// debug_traceBlockByNumber with prestateTracer diffMode.
type TraceDiff struct {
	Storage map[common.Address]map[common.Hash]common.Hash
	Balance map[common.Address]*uint256.Int
	Nonce   map[common.Address]uint64
	Code    map[common.Address][]byte
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

// ─── BlockFetcher ───────────────────────────────────────────────────

// BlockFetcher fetches block data and state from an Avalanche node via
// JSON-RPC. It is the only component that talks to the network (through
// the RPCPool).
type BlockFetcher struct {
	pool *RPCPool
}

// NewBlockFetcher creates a fetcher backed by the given RPC pool.
func NewBlockFetcher(pool *RPCPool) *BlockFetcher {
	return &BlockFetcher{pool: pool}
}

// ─── Block fetching ─────────────────────────────────────────────────

// GetBlock fetches a full block (with transactions) by number.
func (f *BlockFetcher) GetBlock(blockNum uint64) (*BlockData, error) {
	raw, err := f.pool.Call("eth_getBlockByNumber", []interface{}{blockHex(blockNum), true})
	if err != nil {
		return nil, fmt.Errorf("eth_getBlockByNumber(%d): %w", blockNum, err)
	}
	return parseBlock(raw, true)
}

// GetBlockHash fetches only the block hash for a given block number.
// Used by the BLOCKHASH opcode. Fetches the header (no transactions).
func (f *BlockFetcher) GetBlockHash(blockNum uint64) (common.Hash, error) {
	raw, err := f.pool.Call("eth_getBlockByNumber", []interface{}{blockHex(blockNum), false})
	if err != nil {
		return common.Hash{}, fmt.Errorf("eth_getBlockByNumber(%d, false): %w", blockNum, err)
	}
	var payload struct {
		Hash common.Hash `json:"hash"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return common.Hash{}, fmt.Errorf("parse block hash response: %w", err)
	}
	return payload.Hash, nil
}

// ─── State fetching (cache miss callbacks) ──────────────────────────

// GetStorageAt fetches a storage slot value at a specific block.
func (f *BlockFetcher) GetStorageAt(addr common.Address, slot common.Hash, block uint64) (common.Hash, error) {
	raw, err := f.pool.Call("eth_getStorageAt", []interface{}{addr.Hex(), slot.Hex(), blockHex(block)})
	if err != nil {
		return common.Hash{}, fmt.Errorf("eth_getStorageAt(%s, %s, %d): %w", addr, slot, block, err)
	}
	s, err := unquoteJSON(raw)
	if err != nil {
		return common.Hash{}, err
	}
	return common.HexToHash(s), nil
}

// GetBalance fetches an account balance at a specific block.
func (f *BlockFetcher) GetBalance(addr common.Address, block uint64) (*uint256.Int, error) {
	raw, err := f.pool.Call("eth_getBalance", []interface{}{addr.Hex(), blockHex(block)})
	if err != nil {
		return nil, fmt.Errorf("eth_getBalance(%s, %d): %w", addr, block, err)
	}
	s, err := unquoteJSON(raw)
	if err != nil {
		return nil, err
	}
	return uint256FromHex(s)
}

// GetCode fetches the contract code at a specific block.
func (f *BlockFetcher) GetCode(addr common.Address, block uint64) ([]byte, error) {
	raw, err := f.pool.Call("eth_getCode", []interface{}{addr.Hex(), blockHex(block)})
	if err != nil {
		return nil, fmt.Errorf("eth_getCode(%s, %d): %w", addr, block, err)
	}
	s, err := unquoteJSON(raw)
	if err != nil {
		return nil, err
	}
	return hexToBytes(s), nil
}

// GetNonce fetches the transaction count (nonce) at a specific block.
func (f *BlockFetcher) GetNonce(addr common.Address, block uint64) (uint64, error) {
	raw, err := f.pool.Call("eth_getTransactionCount", []interface{}{addr.Hex(), blockHex(block)})
	if err != nil {
		return 0, fmt.Errorf("eth_getTransactionCount(%s, %d): %w", addr, block, err)
	}
	s, err := unquoteJSON(raw)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}

// ─── Trace block for verification ───────────────────────────────────

// TraceBlock calls debug_traceBlockByNumber with the prestateTracer in
// diffMode and returns the aggregated state diff across all transactions.
func (f *BlockFetcher) TraceBlock(blockNum uint64) (*TraceDiff, error) {
	raw, err := f.pool.Call("debug_traceBlockByNumber", []interface{}{
		blockHex(blockNum),
		map[string]interface{}{
			"tracer":       "prestateTracer",
			"tracerConfig": map[string]interface{}{"diffMode": true},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("debug_traceBlockByNumber(%d): %w", blockNum, err)
	}

	var txResults []json.RawMessage
	if err := json.Unmarshal(raw, &txResults); err != nil {
		return nil, fmt.Errorf("parse trace result: %w", err)
	}

	merged := &TraceDiff{
		Storage: make(map[common.Address]map[common.Hash]common.Hash),
		Balance: make(map[common.Address]*uint256.Int),
		Nonce:   make(map[common.Address]uint64),
		Code:    make(map[common.Address][]byte),
	}

	for _, txRaw := range txResults {
		var tx tracedBlockTx
		if err := json.Unmarshal(txRaw, &tx); err != nil {
			continue
		}
		mergeTraceDiff(merged, parseTraceDiff(tx.Result))
	}
	return merged, nil
}

// ─── MissCallbacks helpers ──────────────────────────────────────────

// MissCallbacks returns a MissCallbacks struct wired to this fetcher.
// Each callback fetches the value from the node, stores it in the
// VersionedState for future reads, and returns it.
// FetchStats tracks cache miss RPC fetches. Thread-safe (atomic counters).
type FetchStats struct {
	Storage atomic.Int64
	Balance atomic.Int64
	Nonce   atomic.Int64
	Code    atomic.Int64
}

// Total returns the total number of cache miss fetches.
func (s *FetchStats) Total() int {
	return int(s.Storage.Load() + s.Balance.Load() + s.Nonce.Load() + s.Code.Load())
}

// MissCallbacksWithStats returns miss callbacks that count fetches into stats.
func (f *BlockFetcher) MissCallbacksWithStats(state *VersionedState, stats *FetchStats) MissCallbacks {
	return MissCallbacks{
		OnStorage: func(addr common.Address, slot common.Hash, block uint64) common.Hash {
			stats.Storage.Add(1)
			val, err := f.GetStorageAt(addr, slot, block)
			if err != nil {
				return common.Hash{}
			}
			state.SetStorage(addr, slot, val, block)
			return val
		},
		OnBalance: func(addr common.Address, block uint64) *uint256.Int {
			stats.Balance.Add(1)
			val, err := f.GetBalance(addr, block)
			if err != nil {
				return uint256.NewInt(0)
			}
			state.SetBalance(addr, val, block)
			return val
		},
		OnNonce: func(addr common.Address, block uint64) uint64 {
			stats.Nonce.Add(1)
			val, err := f.GetNonce(addr, block)
			if err != nil {
				return 0
			}
			state.SetNonce(addr, val, block)
			return val
		},
		OnCode: func(addr common.Address, block uint64) []byte {
			stats.Code.Add(1)
			val, err := f.GetCode(addr, block)
			if err != nil {
				return nil
			}
			state.SetCode(addr, val, block)
			return val
		},
	}
}

func (f *BlockFetcher) MissCallbacks(state *VersionedState) MissCallbacks {
	return MissCallbacks{
		OnStorage: func(addr common.Address, slot common.Hash, block uint64) common.Hash {
			val, err := f.GetStorageAt(addr, slot, block)
			if err != nil {
				return common.Hash{}
			}
			state.SetStorage(addr, slot, val, block)
			return val
		},
		OnBalance: func(addr common.Address, block uint64) *uint256.Int {
			val, err := f.GetBalance(addr, block)
			if err != nil {
				return uint256.NewInt(0)
			}
			state.SetBalance(addr, val, block)
			return val
		},
		OnNonce: func(addr common.Address, block uint64) uint64 {
			val, err := f.GetNonce(addr, block)
			if err != nil {
				return 0
			}
			state.SetNonce(addr, val, block)
			return val
		},
		OnCode: func(addr common.Address, block uint64) []byte {
			val, err := f.GetCode(addr, block)
			if err != nil {
				return nil
			}
			state.SetCode(addr, val, block)
			return val
		},
	}
}

// ─── Internal helpers ───────────────────────────────────────────────

// blockHex formats a block number as a 0x-prefixed hex string.
func blockHex(n uint64) string {
	return fmt.Sprintf("0x%x", n)
}

// unquoteJSON strips the surrounding quotes from a JSON string value.
func unquoteJSON(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("expected JSON string, got: %s", string(raw))
	}
	return s, nil
}

// uint256FromHex parses a 0x-prefixed hex string into a uint256.Int.
func uint256FromHex(s string) (*uint256.Int, error) {
	trimmed := strings.TrimPrefix(s, "0x")
	if trimmed == "" {
		return uint256.NewInt(0), nil
	}
	bi, ok := new(big.Int).SetString(trimmed, 16)
	if !ok {
		return nil, fmt.Errorf("invalid hex balance: %s", s)
	}
	val, overflow := uint256.FromBig(bi)
	if overflow {
		return nil, fmt.Errorf("balance overflow: %s", s)
	}
	return val, nil
}

// hexToBytes decodes a 0x-prefixed hex string to bytes. Returns nil for
// empty or "0x" input.
func hexToBytes(s string) []byte {
	trimmed := strings.TrimPrefix(s, "0x")
	if trimmed == "" {
		return nil
	}
	b, _ := hex.DecodeString(trimmed)
	return b
}

// parseBlock unmarshals a raw JSON block response into a BlockData.
func parseBlock(raw json.RawMessage, full bool) (*BlockData, error) {
	var payload rpcBlockJSON
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse block JSON: %w", err)
	}

	bd := &BlockData{
		Hash:       payload.Hash,
		ParentHash: payload.ParentHash,
		Timestamp:  uint64(payload.Timestamp),
		Coinbase:   payload.Coinbase,
		GasLimit:   uint64(payload.GasLimit),
		MixDigest:  payload.MixDigest,
		Extra:      bytes.Clone(payload.Extra),
	}

	if payload.Number != nil {
		bd.Number = (*big.Int)(payload.Number).Uint64()
	}
	if payload.Difficulty != nil {
		bd.Difficulty = (*big.Int)(payload.Difficulty)
	} else {
		bd.Difficulty = big.NewInt(0)
	}
	if payload.BaseFee != nil {
		bd.BaseFee = (*big.Int)(payload.BaseFee)
	} else {
		bd.BaseFee = big.NewInt(0)
	}
	if payload.TimestampMilliseconds != nil {
		t := uint64(*payload.TimestampMilliseconds)
		bd.TimestampMilliseconds = &t
	}
	if payload.MinDelayExcess != nil {
		d := uint64(*payload.MinDelayExcess)
		bd.MinDelayExcess = &d
	}

	if full && len(payload.Transactions) > 0 {
		bd.Transactions = make([]*types.Transaction, 0, len(payload.Transactions))
		for _, txRaw := range payload.Transactions {
			if bytes.Equal(txRaw, []byte("null")) {
				continue
			}
			var tx types.Transaction
			if err := json.Unmarshal(txRaw, &tx); err != nil {
				return nil, fmt.Errorf("decode block tx: %w", err)
			}
			bd.Transactions = append(bd.Transactions, &tx)
		}
	}

	return bd, nil
}

// parseTraceDiff extracts a TraceDiff from a single transaction's
// prestate tracer result (with diffMode enabled).
func parseTraceDiff(result tracedTxResult) *TraceDiff {
	diff := &TraceDiff{
		Storage: make(map[common.Address]map[common.Hash]common.Hash),
		Balance: make(map[common.Address]*uint256.Int),
		Nonce:   make(map[common.Address]uint64),
		Code:    make(map[common.Address][]byte),
	}

	// Process post-state: additions and changes.
	for address, account := range result.Post {
		addr := common.HexToAddress(address)
		pre := result.Pre[address]

		if account.Balance != nil {
			if pre.Balance == nil || *pre.Balance != *account.Balance {
				val, err := uint256FromHex(*account.Balance)
				if err == nil {
					diff.Balance[addr] = val
				}
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
					diff.Nonce[addr] = n
				}
			}
		}
		if account.Code != nil {
			if pre.Code == nil || *pre.Code != *account.Code {
				diff.Code[addr] = hexToBytes(*account.Code)
			}
		}
		for slot, value := range account.Storage {
			if pre.Storage != nil {
				if preVal, ok := pre.Storage[slot]; ok && preVal == value {
					continue
				}
			}
			if diff.Storage[addr] == nil {
				diff.Storage[addr] = make(map[common.Hash]common.Hash)
			}
			diff.Storage[addr][common.HexToHash(slot)] = common.HexToHash(value)
		}
	}

	// Process pre-state: only storage slot deletions are meaningful.
	// For balance/nonce/code, absence from post means "not modified" — not "went to zero."
	// For storage slots, absence from post means the slot was cleared to zero.
	for address, pre := range result.Pre {
		addr := common.HexToAddress(address)
		post := result.Post[address]

		for slot := range pre.Storage {
			if post.Storage != nil {
				if _, ok := post.Storage[slot]; ok {
					continue
				}
			}
			if diff.Storage[addr] == nil {
				diff.Storage[addr] = make(map[common.Hash]common.Hash)
			}
			diff.Storage[addr][common.HexToHash(slot)] = common.Hash{}
		}
	}

	return diff
}

// mergeTraceDiff merges src into dst. Later writes win.
func mergeTraceDiff(dst, src *TraceDiff) {
	for addr, slots := range src.Storage {
		if dst.Storage[addr] == nil {
			dst.Storage[addr] = make(map[common.Hash]common.Hash)
		}
		for slot, value := range slots {
			dst.Storage[addr][slot] = value
		}
	}
	for addr, val := range src.Balance {
		dst.Balance[addr] = val
	}
	for addr, val := range src.Nonce {
		dst.Nonce[addr] = val
	}
	for addr, val := range src.Code {
		dst.Code[addr] = val
	}
}
