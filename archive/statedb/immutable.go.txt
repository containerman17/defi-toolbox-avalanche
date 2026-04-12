package statedb

// ImmutableState — blockchain state at a specific block.
//
// Mutated in place under LiveState.blockMu write lock via ApplyDiffInPlace.
// Concurrent reads are safe under RLock — plain Go maps allow concurrent reads
// as long as no goroutine writes, and the RWMutex guarantees this.
//
// This is the "fast layer" — 99.9% of reads hit this. The remaining 0.1%
// (cache misses) fall through to the backfill layer in StateDB.

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// ImmutableState holds blockchain state at a specific block.
// All fields are read-only after creation.
type ImmutableState struct {
	BlockNum  uint64
	BlockTime uint64

	// Storage: address → slot → value
	Storage map[common.Address]map[common.Hash]common.Hash

	// Code: address → bytecode
	Code map[common.Address][]byte

	// CodeHash: address → keccak256(code)
	CodeHash map[common.Address]common.Hash

	// Balance: address → balance
	Balance map[common.Address]*uint256.Int

	// Nonce: address → nonce
	Nonce map[common.Address]uint64
}

// NewImmutableState creates a new empty state.
func NewImmutableState(blockNum, blockTime uint64) *ImmutableState {
	return &ImmutableState{
		BlockNum:  blockNum,
		BlockTime: blockTime,
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Code:      make(map[common.Address][]byte),
		CodeHash:  make(map[common.Address]common.Hash),
		Balance:   make(map[common.Address]*uint256.Int),
		Nonce:     make(map[common.Address]uint64),
	}
}

// GetStorage returns a storage value, or (zero, false) if not cached.
func (s *ImmutableState) GetStorage(addr common.Address, slot common.Hash) (common.Hash, bool) {
	if slots, ok := s.Storage[addr]; ok {
		if val, ok := slots[slot]; ok {
			return val, true
		}
	}
	return common.Hash{}, false
}

// GetCode returns contract bytecode, or (nil, false) if not cached.
func (s *ImmutableState) GetCode(addr common.Address) ([]byte, bool) {
	code, ok := s.Code[addr]
	return code, ok
}

// GetCodeHash returns code hash, or (zero, false) if not cached.
func (s *ImmutableState) GetCodeHash(addr common.Address) (common.Hash, bool) {
	h, ok := s.CodeHash[addr]
	return h, ok
}

// GetBalance returns balance, or (nil, false) if not cached.
func (s *ImmutableState) GetBalance(addr common.Address) (*uint256.Int, bool) {
	bal, ok := s.Balance[addr]
	return bal, ok
}

// GetNonce returns nonce, or (0, false) if not cached.
func (s *ImmutableState) GetNonce(addr common.Address) (uint64, bool) {
	n, ok := s.Nonce[addr]
	return n, ok
}

// HasStorageSlot returns true if the slot exists in this snapshot.
func (s *ImmutableState) HasStorageSlot(addr common.Address, slot common.Hash) bool {
	if slots, ok := s.Storage[addr]; ok {
		_, ok = slots[slot]
		return ok
	}
	return false
}

// SetStorage sets a storage slot (used during construction only).
func (s *ImmutableState) SetStorage(addr common.Address, slot, value common.Hash) {
	if s.Storage[addr] == nil {
		s.Storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.Storage[addr][slot] = value
}

// SetAccount sets account metadata (used during construction only).
func (s *ImmutableState) SetAccount(addr common.Address, balance *uint256.Int, nonce uint64, code []byte) {
	if balance != nil {
		s.Balance[addr] = balance
	}
	s.Nonce[addr] = nonce
	if len(code) > 0 {
		s.Code[addr] = code
		s.CodeHash[addr] = crypto.Keccak256Hash(code)
	}
}

// BlockDiff represents changes from one block to the next.
type BlockDiff struct {
	BlockNum  uint64
	BlockTime uint64
	Storage   map[common.Address]map[common.Hash]common.Hash // changed slots
	Balance   map[common.Address]*uint256.Int                // changed balances
	Nonce     map[common.Address]uint64                      // changed nonces
}

// ApplyDiffInPlace mutates this ImmutableState by applying a block diff and
// merging in backfill data. The caller MUST hold an exclusive lock so no
// readers are accessing this state concurrently.
//
// This is O(diff + backfill) instead of O(total state) — only touches entries
// that actually changed, instead of copying all ~870K entries per block.
func (s *ImmutableState) ApplyDiffInPlace(
	diff *BlockDiff,
	bfStorage map[common.Address]map[common.Hash]common.Hash,
	bfCode map[common.Address][]byte,
	bfBalance map[common.Address]*uint256.Int,
	bfNonce map[common.Address]uint64,
) {
	s.BlockNum = diff.BlockNum
	s.BlockTime = diff.BlockTime

	// Merge backfill storage FIRST (may contain stale values from a prior block)
	for addr, slots := range bfStorage {
		if s.Storage[addr] == nil {
			s.Storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		}
		for k, v := range slots {
			s.Storage[addr][k] = v
		}
	}

	// Apply diff storage SECOND — diff always wins over backfill, because backfill
	// values may have been fetched from the state-server at a stale block (the
	// state-server releases blockMu between cache check and node fetch, so the
	// block can advance mid-flight and the client gets an old value).
	for addr, slots := range diff.Storage {
		if s.Storage[addr] == nil {
			s.Storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		}
		for slot, val := range slots {
			s.Storage[addr][slot] = val
		}
	}

	// Code + CodeHash: merge backfill only (diffs don't include code changes)
	for addr, code := range bfCode {
		s.Code[addr] = code
		if len(code) > 0 {
			s.CodeHash[addr] = crypto.Keccak256Hash(code)
		}
	}

	// Balance: merge backfill first, then diff (diff wins over stale backfill)
	for addr, bal := range bfBalance {
		s.Balance[addr] = bal
	}
	for addr, bal := range diff.Balance {
		s.Balance[addr] = bal
	}

	// Nonce: merge backfill first, then diff (diff wins over stale backfill)
	for addr, nonce := range bfNonce {
		s.Nonce[addr] = nonce
	}
	for addr, nonce := range diff.Nonce {
		s.Nonce[addr] = nonce
	}
}
