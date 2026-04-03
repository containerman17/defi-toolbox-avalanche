package statedb

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
)

// Ensure interface compliance.
var _ vm.StateDB = (*CallState)(nil)

// ─── Journal entries for snapshot/revert ────────────────────────────
// Each entry records how to undo a single mutation. O(mutations) not O(state).

type journalEntry interface {
	revert(s *CallState)
}

type journalStorageChange struct {
	addr common.Address
	key  common.Hash
	prev common.Hash
	had  bool
}

func (j journalStorageChange) revert(s *CallState) {
	if !j.had {
		delete(s.storageOverrides[j.addr], j.key)
		if len(s.storageOverrides[j.addr]) == 0 {
			delete(s.storageOverrides, j.addr)
		}
	} else {
		s.storageOverrides[j.addr][j.key] = j.prev
	}
}

type journalBalanceChange struct {
	addr common.Address
	prev *uint256.Int
	had  bool
}

func (j journalBalanceChange) revert(s *CallState) {
	if !j.had {
		delete(s.balanceOverrides, j.addr)
	} else {
		s.balanceOverrides[j.addr] = j.prev
	}
}

type journalRefundChange struct {
	prev uint64
}

func (j journalRefundChange) revert(s *CallState) {
	s.refund = j.prev
}

type journalAccessListAddr struct {
	addr common.Address
}

func (j journalAccessListAddr) revert(s *CallState) {
	delete(s.accessedAddrs, j.addr)
}

type journalAccessListSlot struct {
	addr    common.Address
	slot    common.Hash
	hadAddr bool
}

func (j journalAccessListSlot) revert(s *CallState) {
	if slots, ok := s.accessedSlots[j.addr]; ok {
		delete(slots, j.slot)
		if len(slots) == 0 {
			delete(s.accessedSlots, j.addr)
		}
	}
	if !j.hadAddr {
		delete(s.accessedAddrs, j.addr)
	}
}

type journalTransientChange struct {
	addr common.Address
	key  common.Hash
	prev common.Hash
	had  bool
}

func (j journalTransientChange) revert(s *CallState) {
	if !j.had {
		delete(s.transient[j.addr], j.key)
		if len(s.transient[j.addr]) == 0 {
			delete(s.transient, j.addr)
		}
	} else {
		s.transient[j.addr][j.key] = j.prev
	}
}

type journalLogChange struct {
	prevLen int
}

func (j journalLogChange) revert(s *CallState) {
	s.logs = s.logs[:j.prevLen]
}

// ─── CallState ──────────────────────────────────────────────────────
// Thin overlay implementing vm.StateDB for a single EVM call.
// Only stores what's different from the base. Code, codeHash, nonce
// delegate directly to the base StateDB — no per-call copying.

type CallState struct {
	base *StateDB

	// Per-call overrides — only what EVM mutates
	storageOverrides map[common.Address]map[common.Hash]common.Hash
	balanceOverrides map[common.Address]*uint256.Int

	// Per-tx state
	transient     map[common.Address]map[common.Hash]common.Hash
	accessedAddrs map[common.Address]bool
	accessedSlots map[common.Address]map[common.Hash]bool
	refund        uint64
	logs          []*types.Log

	// Journal for O(mutations) snapshot/revert
	journal   []journalEntry
	snapshots []int

	// Per-goroutine error from fetch failures. Check with Err() after EVM execution.
	lastErr error
}

// NewCallState creates a thin overlay backed by the given base state.
func NewCallState(base *StateDB) *CallState {
	return &CallState{
		base:             base,
		storageOverrides: make(map[common.Address]map[common.Hash]common.Hash),
		balanceOverrides: make(map[common.Address]*uint256.Int),
		transient:        make(map[common.Address]map[common.Hash]common.Hash),
		accessedAddrs:    make(map[common.Address]bool),
		accessedSlots:    make(map[common.Address]map[common.Hash]bool),
	}
}

// Err returns and clears the last fetch error. Each CallState is per-goroutine,
// so errors are isolated — no cross-contamination between concurrent EVM calls.
func (s *CallState) Err() error {
	err := s.lastErr
	s.lastErr = nil
	return err
}

// Reset clears per-call state for reuse between EVM calls.
// Only clears what Prepare() doesn't handle. Avoids redundant map allocations.
func (s *CallState) Reset() {
	clear(s.storageOverrides)
	clear(s.balanceOverrides)
	// accessedAddrs, accessedSlots, transient — Prepare() handles these
	s.refund = 0
	s.logs = s.logs[:0]
	s.journal = s.journal[:0]
	s.snapshots = s.snapshots[:0]
	s.lastErr = nil
}

// ─── Storage ────────────────────────────────────────────────────────

func (s *CallState) GetState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	if slots, ok := s.storageOverrides[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	val, err := s.base.getStorageWithErr(addr, key)
	if err != nil {
		s.lastErr = err
		return common.Hash{}
	}
	return val
}

func (s *CallState) GetCommittedState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	// Committed = pre-transaction. Bypass our overrides, read from base.
	return s.base.GetState(addr, key)
}

func (s *CallState) SetState(addr common.Address, key, value common.Hash, _ ...stateconf.StateDBStateOption) {
	var prev common.Hash
	var had bool
	if slots, ok := s.storageOverrides[addr]; ok {
		prev, had = slots[key]
	}
	s.journal = append(s.journal, journalStorageChange{addr, key, prev, had})
	if s.storageOverrides[addr] == nil {
		s.storageOverrides[addr] = make(map[common.Hash]common.Hash)
	}
	s.storageOverrides[addr][key] = value
}

// ─── Balance ────────────────────────────────────────────────────────

func (s *CallState) GetBalance(addr common.Address) *uint256.Int {
	if bal, ok := s.balanceOverrides[addr]; ok {
		return new(uint256.Int).Set(bal)
	}
	val, err := s.base.getBalanceWithErr(addr)
	if err != nil {
		s.lastErr = err
		return uint256.NewInt(0)
	}
	return val
}

func (s *CallState) SubBalance(addr common.Address, amount *uint256.Int) {
	prev, had := s.balanceOverrides[addr]
	if had {
		prev = prev.Clone()
	}
	s.journal = append(s.journal, journalBalanceChange{addr, prev, had})
	bal := s.GetBalance(addr)
	s.balanceOverrides[addr] = new(uint256.Int).Sub(bal, amount)
}

func (s *CallState) AddBalance(addr common.Address, amount *uint256.Int) {
	prev, had := s.balanceOverrides[addr]
	if had {
		prev = prev.Clone()
	}
	s.journal = append(s.journal, journalBalanceChange{addr, prev, had})
	bal := s.GetBalance(addr)
	s.balanceOverrides[addr] = new(uint256.Int).Add(bal, amount)
}

func (s *CallState) CreateAccount(addr common.Address) {}

// ─── Code — delegate to base with error propagation ─────────────────

func (s *CallState) GetCodeHash(addr common.Address) common.Hash {
	// Delegate to base StateDB which has pre-computed hashes in the immutable
	// layer and backfill cache. Avoids re-hashing full bytecode on every call.
	return s.base.GetCodeHash(addr)
}

func (s *CallState) GetCode(addr common.Address) []byte {
	code, err := s.base.getCodeWithErr(addr)
	if err != nil {
		s.lastErr = err
		return nil
	}
	return code
}

func (s *CallState) SetCode(addr common.Address, code []byte) {}

func (s *CallState) GetCodeSize(addr common.Address) int {
	return len(s.GetCode(addr))
}

// ─── Nonce — delegate to base with error propagation ────────────────

func (s *CallState) GetNonce(addr common.Address) uint64 {
	return s.base.GetNonce(addr)
}

func (s *CallState) SetNonce(addr common.Address, n uint64) {}

// ─── Refund ─────────────────────────────────────────────────────────

func (s *CallState) AddRefund(gas uint64) {
	s.journal = append(s.journal, journalRefundChange{s.refund})
	s.refund += gas
}

func (s *CallState) SubRefund(gas uint64) {
	s.journal = append(s.journal, journalRefundChange{s.refund})
	s.refund -= gas
}

func (s *CallState) GetRefund() uint64 { return s.refund }

// ─── Transient storage (EIP-1153) ───────────────────────────────────

func (s *CallState) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.transient[addr]; ok {
		return slots[key]
	}
	return common.Hash{}
}

func (s *CallState) SetTransientState(addr common.Address, key, value common.Hash) {
	var prev common.Hash
	var had bool
	if slots, ok := s.transient[addr]; ok {
		prev, had = slots[key]
	}
	s.journal = append(s.journal, journalTransientChange{addr, key, prev, had})
	if s.transient[addr] == nil {
		s.transient[addr] = make(map[common.Hash]common.Hash)
	}
	s.transient[addr][key] = value
}

// ─── Self-destruct ──────────────────────────────────────────────────

func (s *CallState) SelfDestruct(addr common.Address) {
	prev, had := s.balanceOverrides[addr]
	if had {
		prev = prev.Clone()
	}
	s.journal = append(s.journal, journalBalanceChange{addr, prev, had})
	s.balanceOverrides[addr] = uint256.NewInt(0)
}

func (s *CallState) HasSelfDestructed(addr common.Address) bool { return false }
func (s *CallState) Selfdestruct6780(addr common.Address)       {}

// ─── Account queries ────────────────────────────────────────────────

func (s *CallState) Exist(addr common.Address) bool {
	if s.GetCodeSize(addr) > 0 {
		return true
	}
	if s.GetBalance(addr).Sign() > 0 {
		return true
	}
	return false
}

func (s *CallState) Empty(addr common.Address) bool {
	return s.GetBalance(addr).IsZero() && s.GetNonce(addr) == 0 && s.GetCodeSize(addr) == 0
}

// ─── Access list ────────────────────────────────────────────────────

func (s *CallState) AddressInAccessList(addr common.Address) bool {
	return s.accessedAddrs[addr]
}

func (s *CallState) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	addrOk := s.accessedAddrs[addr]
	if slots, ok := s.accessedSlots[addr]; ok {
		return addrOk, slots[slot]
	}
	return addrOk, false
}

func (s *CallState) AddAddressToAccessList(addr common.Address) {
	if !s.accessedAddrs[addr] {
		s.journal = append(s.journal, journalAccessListAddr{addr})
	}
	s.accessedAddrs[addr] = true
}

func (s *CallState) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	hadAddr := s.accessedAddrs[addr]
	hadSlot := false
	if slots, ok := s.accessedSlots[addr]; ok {
		hadSlot = slots[slot]
	}
	if !hadSlot {
		s.journal = append(s.journal, journalAccessListSlot{addr, slot, hadAddr})
	}
	s.accessedAddrs[addr] = true
	if s.accessedSlots[addr] == nil {
		s.accessedSlots[addr] = make(map[common.Hash]bool)
	}
	s.accessedSlots[addr][slot] = true
}

func (s *CallState) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	s.accessedAddrs = make(map[common.Address]bool)
	s.accessedSlots = make(map[common.Address]map[common.Hash]bool)
	s.transient = make(map[common.Address]map[common.Hash]common.Hash)

	s.accessedAddrs[sender] = true
	if dest != nil {
		s.accessedAddrs[*dest] = true
	}
	s.accessedAddrs[coinbase] = true
	for _, addr := range precompiles {
		s.accessedAddrs[addr] = true
	}
	for _, el := range txAccesses {
		s.accessedAddrs[el.Address] = true
		for _, slot := range el.StorageKeys {
			if s.accessedSlots[el.Address] == nil {
				s.accessedSlots[el.Address] = make(map[common.Hash]bool)
			}
			s.accessedSlots[el.Address][slot] = true
		}
	}
}

// ─── Snapshots — journal-based, O(mutations) ────────────────────────

func (s *CallState) Snapshot() int {
	id := len(s.snapshots)
	s.snapshots = append(s.snapshots, len(s.journal))
	return id
}

func (s *CallState) RevertToSnapshot(id int) {
	target := s.snapshots[id]
	s.snapshots = s.snapshots[:id]
	for i := len(s.journal) - 1; i >= target; i-- {
		s.journal[i].revert(s)
	}
	s.journal = s.journal[:target]
}

// ─── Logs ───────────────────────────────────────────────────────────

func (s *CallState) AddLog(log *types.Log) {
	s.journal = append(s.journal, journalLogChange{len(s.logs)})
	s.logs = append(s.logs, log)
}

func (s *CallState) AddPreimage(hash common.Hash, data []byte) {}

// StorageOverrides returns the dirty storage slots from EVM execution.
// The returned map is the internal reference — do not modify.
func (s *CallState) StorageOverrides() map[common.Address]map[common.Hash]common.Hash {
	return s.storageOverrides
}

// ─── Block hash — delegate to base ─────────────────────────────────

func (s *CallState) GetBlockHash(num uint64) common.Hash {
	return s.base.GetBlockHash(num)
}
