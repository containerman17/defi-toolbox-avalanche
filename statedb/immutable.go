package statedb

// ImmutableState — read-only snapshot of blockchain state at a specific block.
//
// Once created, it is NEVER modified. Safe for concurrent reads without locks.
// Plain Go maps are safe for concurrent reads as long as no goroutine writes.
// Block updates create a NEW ImmutableState via CloneWithDiff, then swap
// an atomic.Pointer. Old state stays valid for in-flight readers until GC.
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

// CloneWithDiff creates a new ImmutableState by applying a block diff and merging
// in backfill data (slots fetched on demand since the last block).
//
// Only deep-copies accounts that have changes in the diff. Unchanged accounts
// share map pointers with the old state (safe because both are read-only).
func (s *ImmutableState) CloneWithDiff(
	diff *BlockDiff,
	bfStorage map[common.Address]map[common.Hash]common.Hash,
	bfCode map[common.Address][]byte,
	bfBalance map[common.Address]*uint256.Int,
	bfNonce map[common.Address]uint64,
) *ImmutableState {
	newState := &ImmutableState{
		BlockNum:  diff.BlockNum,
		BlockTime: diff.BlockTime,
		Storage:   make(map[common.Address]map[common.Hash]common.Hash, len(s.Storage)),
		Code:      make(map[common.Address][]byte, len(s.Code)+len(bfCode)),
		CodeHash:  make(map[common.Address]common.Hash, len(s.CodeHash)+len(bfCode)),
		Balance:   make(map[common.Address]*uint256.Int, len(s.Balance)+len(bfBalance)),
		Nonce:     make(map[common.Address]uint64, len(s.Nonce)+len(bfNonce)),
	}

	// Build set of addresses that need deep copy (changed in diff or backfill)
	dirty := make(map[common.Address]bool)
	for addr := range diff.Storage {
		dirty[addr] = true
	}
	for addr := range bfStorage {
		dirty[addr] = true
	}

	// Copy storage: share unchanged, deep-copy changed
	for addr, slots := range s.Storage {
		if dirty[addr] {
			// Deep copy — this account has changes
			newSlots := make(map[common.Hash]common.Hash, len(slots))
			for k, v := range slots {
				newSlots[k] = v
			}
			newState.Storage[addr] = newSlots
		} else {
			// Share pointer — this account is unchanged
			newState.Storage[addr] = slots
		}
	}

	// Merge backfill storage FIRST (may contain stale values from a prior block)
	for addr, slots := range bfStorage {
		if newState.Storage[addr] == nil {
			newState.Storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		}
		for k, v := range slots {
			newState.Storage[addr][k] = v
		}
	}

	// Apply diff storage SECOND — diff always wins over backfill, because backfill
	// values may have been fetched from the state-server at a stale block (the
	// state-server releases blockMu between cache check and node fetch, so the
	// block can advance mid-flight and the client gets an old value).
	for addr, slots := range diff.Storage {
		if newState.Storage[addr] == nil {
			newState.Storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		}
		for slot, val := range slots {
			newState.Storage[addr][slot] = val
		}
	}

	// Code: copy existing + merge backfill
	for addr, code := range s.Code {
		newState.Code[addr] = code
	}
	for addr, code := range bfCode {
		newState.Code[addr] = code
	}

	// CodeHash: copy existing + compute for backfill
	for addr, hash := range s.CodeHash {
		newState.CodeHash[addr] = hash
	}
	for addr, code := range bfCode {
		if len(code) > 0 {
			newState.CodeHash[addr] = crypto.Keccak256Hash(code)
		}
	}

	// Balance: copy existing + merge backfill + apply diff (diff wins over stale backfill)
	for addr, bal := range s.Balance {
		newState.Balance[addr] = bal
	}
	for addr, bal := range bfBalance {
		newState.Balance[addr] = bal
	}
	for addr, bal := range diff.Balance {
		newState.Balance[addr] = bal
	}

	// Nonce: copy existing + merge backfill + apply diff (diff wins over stale backfill)
	for addr, nonce := range s.Nonce {
		newState.Nonce[addr] = nonce
	}
	for addr, nonce := range bfNonce {
		newState.Nonce[addr] = nonce
	}
	for addr, nonce := range diff.Nonce {
		newState.Nonce[addr] = nonce
	}

	return newState
}
