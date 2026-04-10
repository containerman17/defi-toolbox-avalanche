package lightclient

import (
	"math/big"
	"sync"
	"sync/atomic"

	cparams "github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/avalanchego/vms/evm/predicate"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
)

// ─── Versioned linked list nodes ────────────────────────────────────
//
// Each node is immutable after creation. The prev pointer uses atomic.Pointer
// so writers can CAS the head while readers walk the chain lock-free.

type storageEntry struct {
	block uint64
	value common.Hash
	prev  atomic.Pointer[storageEntry]
}

type balanceEntry struct {
	block   uint64
	balance *uint256.Int // immutable after creation
	prev    atomic.Pointer[balanceEntry]
}

type nonceEntry struct {
	block uint64
	nonce uint64
	prev  atomic.Pointer[nonceEntry]
}

type codeEntry struct {
	block    uint64
	code     []byte      // immutable after creation
	codeHash common.Hash // precomputed
	prev     atomic.Pointer[codeEntry]
}

// ─── Composite map keys ─────────────────────────────────────────────

type storageKey struct {
	addr common.Address
	slot common.Hash
}

// ─── VersionedState ─────────────────────────────────────────────────
//
// The core versioned state store. Per-key linked lists ordered by block number
// (newest first). Reads are lock-free: walk the chain from head until finding
// an entry with block <= requested. Writers hold a mutex and CAS the head.

type VersionedState struct {
	storage sync.Map // storageKey → *atomic.Pointer[storageEntry]
	balance sync.Map // common.Address → *atomic.Pointer[balanceEntry]
	nonce   sync.Map // common.Address → *atomic.Pointer[nonceEntry]
	code    sync.Map // common.Address → *atomic.Pointer[codeEntry]

	storageMu sync.Mutex
	balanceMu sync.Mutex
	nonceMu   sync.Mutex
	codeMu    sync.Mutex

	latestBlock atomic.Uint64
}

// NewVersionedState creates an empty versioned state store.
func NewVersionedState() *VersionedState {
	return &VersionedState{}
}

// LatestBlock returns the latest fully-applied block number.
func (vs *VersionedState) LatestBlock() uint64 {
	return vs.latestBlock.Load()
}

// SetLatestBlock atomically advances the latest block pointer.
func (vs *VersionedState) SetLatestBlock(block uint64) {
	vs.latestBlock.Store(block)
}

// ─── Storage ────────────────────────────────────────────────────────

func (vs *VersionedState) SetStorage(addr common.Address, slot common.Hash, value common.Hash, block uint64) {
	key := storageKey{addr, slot}
	entry := &storageEntry{block: block, value: value}

	vs.storageMu.Lock()
	defer vs.storageMu.Unlock()

	headPtr, loaded := vs.storage.LoadOrStore(key, new(atomic.Pointer[storageEntry]))
	hp := headPtr.(*atomic.Pointer[storageEntry])
	if !loaded {
		// Fresh key — just store.
		hp.Store(entry)
		return
	}
	// Prepend: point new entry's prev to current head, then CAS head.
	old := hp.Load()
	entry.prev.Store(old)
	hp.Store(entry)
}

// GetStorage walks the list for (addr, slot) and returns the first value
// with block <= requested. Returns zero hash if not found.
func (vs *VersionedState) GetStorage(addr common.Address, slot common.Hash, block uint64) (common.Hash, bool) {
	key := storageKey{addr, slot}
	headPtr, ok := vs.storage.Load(key)
	if !ok {
		return common.Hash{}, false
	}
	hp := headPtr.(*atomic.Pointer[storageEntry])
	for e := hp.Load(); e != nil; e = e.prev.Load() {
		if e.block <= block {
			return e.value, true
		}
	}
	return common.Hash{}, false
}

// ─── Balance ────────────────────────────────────────────────────────

func (vs *VersionedState) SetBalance(addr common.Address, balance *uint256.Int, block uint64) {
	entry := &balanceEntry{block: block, balance: new(uint256.Int).Set(balance)}

	vs.balanceMu.Lock()
	defer vs.balanceMu.Unlock()

	headPtr, loaded := vs.balance.LoadOrStore(addr, new(atomic.Pointer[balanceEntry]))
	hp := headPtr.(*atomic.Pointer[balanceEntry])
	if !loaded {
		hp.Store(entry)
		return
	}
	old := hp.Load()
	entry.prev.Store(old)
	hp.Store(entry)
}

func (vs *VersionedState) GetBalance(addr common.Address, block uint64) (*uint256.Int, bool) {
	headPtr, ok := vs.balance.Load(addr)
	if !ok {
		return nil, false
	}
	hp := headPtr.(*atomic.Pointer[balanceEntry])
	for e := hp.Load(); e != nil; e = e.prev.Load() {
		if e.block <= block {
			return new(uint256.Int).Set(e.balance), true
		}
	}
	return nil, false
}

// ─── Nonce ──────────────────────────────────────────────────────────

func (vs *VersionedState) SetNonce(addr common.Address, nonce uint64, block uint64) {
	entry := &nonceEntry{block: block, nonce: nonce}

	vs.nonceMu.Lock()
	defer vs.nonceMu.Unlock()

	headPtr, loaded := vs.nonce.LoadOrStore(addr, new(atomic.Pointer[nonceEntry]))
	hp := headPtr.(*atomic.Pointer[nonceEntry])
	if !loaded {
		hp.Store(entry)
		return
	}
	old := hp.Load()
	entry.prev.Store(old)
	hp.Store(entry)
}

func (vs *VersionedState) GetNonce(addr common.Address, block uint64) (uint64, bool) {
	headPtr, ok := vs.nonce.Load(addr)
	if !ok {
		return 0, false
	}
	hp := headPtr.(*atomic.Pointer[nonceEntry])
	for e := hp.Load(); e != nil; e = e.prev.Load() {
		if e.block <= block {
			return e.nonce, true
		}
	}
	return 0, false
}

// ─── Code ───────────────────────────────────────────────────────────

func (vs *VersionedState) SetCode(addr common.Address, code []byte, block uint64) {
	codeCopy := make([]byte, len(code))
	copy(codeCopy, code)
	var codeHash common.Hash
	if len(code) > 0 {
		codeHash = crypto.Keccak256Hash(code)
	}
	entry := &codeEntry{block: block, code: codeCopy, codeHash: codeHash}

	vs.codeMu.Lock()
	defer vs.codeMu.Unlock()

	headPtr, loaded := vs.code.LoadOrStore(addr, new(atomic.Pointer[codeEntry]))
	hp := headPtr.(*atomic.Pointer[codeEntry])
	if !loaded {
		hp.Store(entry)
		return
	}
	old := hp.Load()
	entry.prev.Store(old)
	hp.Store(entry)
}

func (vs *VersionedState) GetCode(addr common.Address, block uint64) ([]byte, bool) {
	headPtr, ok := vs.code.Load(addr)
	if !ok {
		return nil, false
	}
	hp := headPtr.(*atomic.Pointer[codeEntry])
	for e := hp.Load(); e != nil; e = e.prev.Load() {
		if e.block <= block {
			return e.code, true
		}
	}
	return nil, false
}

func (vs *VersionedState) GetCodeHash(addr common.Address, block uint64) (common.Hash, bool) {
	headPtr, ok := vs.code.Load(addr)
	if !ok {
		return common.Hash{}, false
	}
	hp := headPtr.(*atomic.Pointer[codeEntry])
	for e := hp.Load(); e != nil; e = e.prev.Load() {
		if e.block <= block {
			return e.codeHash, true
		}
	}
	return common.Hash{}, false
}

// ─── Pruning ────────────────────────────────────────────────────────

// Prune drops linked-list entries older than (latestBlock - keepBlocks).
// Entries at the cutoff boundary are kept (they represent the last known value
// before the pruning window and are needed for reads at the oldest kept block).
func (vs *VersionedState) Prune(keepBlocks uint64) {
	latest := vs.latestBlock.Load()
	if latest <= keepBlocks {
		return
	}
	cutoff := latest - keepBlocks

	// Helper: for each chain, find the last entry at or after cutoff,
	// then nil out its prev pointer to drop the tail.
	vs.storage.Range(func(_, headPtr any) bool {
		hp := headPtr.(*atomic.Pointer[storageEntry])
		for e := hp.Load(); e != nil; e = e.prev.Load() {
			if e.block <= cutoff {
				// This entry is the oldest we keep. Drop everything after it.
				e.prev.Store(nil)
				break
			}
		}
		return true
	})

	vs.balance.Range(func(_, headPtr any) bool {
		hp := headPtr.(*atomic.Pointer[balanceEntry])
		for e := hp.Load(); e != nil; e = e.prev.Load() {
			if e.block <= cutoff {
				e.prev.Store(nil)
				break
			}
		}
		return true
	})

	vs.nonce.Range(func(_, headPtr any) bool {
		hp := headPtr.(*atomic.Pointer[nonceEntry])
		for e := hp.Load(); e != nil; e = e.prev.Load() {
			if e.block <= cutoff {
				e.prev.Store(nil)
				break
			}
		}
		return true
	})

	vs.code.Range(func(_, headPtr any) bool {
		hp := headPtr.(*atomic.Pointer[codeEntry])
		for e := hp.Load(); e != nil; e = e.prev.Load() {
			if e.block <= cutoff {
				e.prev.Store(nil)
				break
			}
		}
		return true
	})
}

// ─── Iteration (for snapshots) ─────────────────────────────────────

// ForEachStorage calls fn for each storage key with its latest (head) value.
func (vs *VersionedState) ForEachStorage(fn func(addr common.Address, slot common.Hash, value common.Hash)) {
	vs.storage.Range(func(key, headPtr any) bool {
		k := key.(storageKey)
		hp := headPtr.(*atomic.Pointer[storageEntry])
		if e := hp.Load(); e != nil {
			fn(k.addr, k.slot, e.value)
		}
		return true
	})
}

// ForEachBalance calls fn for each address with its latest (head) balance.
func (vs *VersionedState) ForEachBalance(fn func(addr common.Address, balance *uint256.Int)) {
	vs.balance.Range(func(key, headPtr any) bool {
		addr := key.(common.Address)
		hp := headPtr.(*atomic.Pointer[balanceEntry])
		if e := hp.Load(); e != nil {
			fn(addr, e.balance)
		}
		return true
	})
}

// ForEachNonce calls fn for each address with its latest (head) nonce.
func (vs *VersionedState) ForEachNonce(fn func(addr common.Address, nonce uint64)) {
	vs.nonce.Range(func(key, headPtr any) bool {
		addr := key.(common.Address)
		hp := headPtr.(*atomic.Pointer[nonceEntry])
		if e := hp.Load(); e != nil {
			fn(addr, e.nonce)
		}
		return true
	})
}

// ForEachCode calls fn for each address with its latest (head) code.
func (vs *VersionedState) ForEachCode(fn func(addr common.Address, code []byte)) {
	vs.code.Range(func(key, headPtr any) bool {
		addr := key.(common.Address)
		hp := headPtr.(*atomic.Pointer[codeEntry])
		if e := hp.Load(); e != nil {
			fn(addr, e.code)
		}
		return true
	})
}

// ─── StateView — vm.StateDB implementation ──────────────────────────
//
// Pins a block number at creation. Reads go to the versioned state at that
// block. Writes go to a local journal (map-based overlay) for EVM execution.
// Supports Snapshot/RevertToSnapshot via journal entries.

// Ensure interface compliance at compile time.
var _ vm.StateDB = (*StateView)(nil)

// OnStorageMiss is called when a storage slot is not in the versioned state.
// The callback should fetch it (e.g., via RPC) and return the value.
type OnStorageMiss func(addr common.Address, slot common.Hash, block uint64) common.Hash

// OnBalanceMiss is called when a balance is not in the versioned state.
type OnBalanceMiss func(addr common.Address, block uint64) *uint256.Int

// OnNonceMiss is called when a nonce is not in the versioned state.
type OnNonceMiss func(addr common.Address, block uint64) uint64

// OnCodeMiss is called when code is not in the versioned state.
type OnCodeMiss func(addr common.Address, block uint64) []byte

// MissCallbacks groups all cache-miss callbacks.
type MissCallbacks struct {
	OnStorage OnStorageMiss
	OnBalance OnBalanceMiss
	OnNonce   OnNonceMiss
	OnCode    OnCodeMiss
}

// StateView implements vm.StateDB against a pinned block in the versioned state.
type StateView struct {
	state *VersionedState
	block uint64
	miss  MissCallbacks

	// Journal overlay — writes during EVM execution
	storageOverrides map[common.Address]map[common.Hash]common.Hash
	balanceOverrides map[common.Address]*uint256.Int
	nonceOverrides   map[common.Address]uint64
	codeOverrides    map[common.Address][]byte

	// Per-tx EVM state
	transient     map[common.Address]map[common.Hash]common.Hash
	accessedAddrs map[common.Address]bool
	accessedSlots map[common.Address]map[common.Hash]bool
	predicates    map[common.Address][]predicate.Predicate
	selfDestructed map[common.Address]bool
	createdAccounts map[common.Address]bool
	refund        uint64
	logs          []*types.Log
	txHash        common.Hash
	txIndex       int

	// Journal for O(mutations) snapshot/revert
	journal   []journalEntry
	snapshots []int // each entry = journal length at snapshot time

	// Dirty storage tracking for diff extraction after block execution
	dirtyStorage map[common.Address]map[common.Hash]common.Hash
	dirtyBalance map[common.Address]*uint256.Int
	dirtyNonce   map[common.Address]uint64
	dirtyCode    map[common.Address][]byte
}

// NewStateView creates a StateView pinned at the given block number.
func NewStateView(state *VersionedState, block uint64, miss MissCallbacks) *StateView {
	return &StateView{
		state:            state,
		block:            block,
		miss:             miss,
		storageOverrides: make(map[common.Address]map[common.Hash]common.Hash),
		balanceOverrides: make(map[common.Address]*uint256.Int),
		nonceOverrides:   make(map[common.Address]uint64),
		codeOverrides:    make(map[common.Address][]byte),
		transient:        make(map[common.Address]map[common.Hash]common.Hash),
		accessedAddrs:    make(map[common.Address]bool),
		accessedSlots:    make(map[common.Address]map[common.Hash]bool),
		predicates:       make(map[common.Address][]predicate.Predicate),
		selfDestructed:   make(map[common.Address]bool),
		createdAccounts:  make(map[common.Address]bool),
		dirtyStorage:     make(map[common.Address]map[common.Hash]common.Hash),
		dirtyBalance:     make(map[common.Address]*uint256.Int),
		dirtyNonce:       make(map[common.Address]uint64),
		dirtyCode:        make(map[common.Address][]byte),
	}
}

// Block returns the pinned block number.
func (sv *StateView) Block() uint64 { return sv.block }

// ─── Journal entries ────────────────────────────────────────────────

type journalEntry interface {
	revert(sv *StateView)
}

type jStorageChange struct {
	addr common.Address
	key  common.Hash
	prev common.Hash
	had  bool
}

func (j jStorageChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.storageOverrides[j.addr], j.key)
		if len(sv.storageOverrides[j.addr]) == 0 {
			delete(sv.storageOverrides, j.addr)
		}
	} else {
		sv.storageOverrides[j.addr][j.key] = j.prev
	}
}

type jBalanceChange struct {
	addr common.Address
	prev *uint256.Int
	had  bool
}

func (j jBalanceChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.balanceOverrides, j.addr)
	} else {
		sv.balanceOverrides[j.addr] = j.prev
	}
}

type jNonceChange struct {
	addr common.Address
	prev uint64
	had  bool
}

func (j jNonceChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.nonceOverrides, j.addr)
	} else {
		sv.nonceOverrides[j.addr] = j.prev
	}
}

type jCodeChange struct {
	addr common.Address
	prev []byte
	had  bool
}

func (j jCodeChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.codeOverrides, j.addr)
	} else {
		sv.codeOverrides[j.addr] = j.prev
	}
}

type jRefundChange struct{ prev uint64 }

func (j jRefundChange) revert(sv *StateView) { sv.refund = j.prev }

type jAccessListAddr struct{ addr common.Address }

func (j jAccessListAddr) revert(sv *StateView) { delete(sv.accessedAddrs, j.addr) }

type jAccessListSlot struct {
	addr    common.Address
	slot    common.Hash
	hadAddr bool
}

func (j jAccessListSlot) revert(sv *StateView) {
	if slots, ok := sv.accessedSlots[j.addr]; ok {
		delete(slots, j.slot)
		if len(slots) == 0 {
			delete(sv.accessedSlots, j.addr)
		}
	}
	if !j.hadAddr {
		delete(sv.accessedAddrs, j.addr)
	}
}

type jTransientChange struct {
	addr common.Address
	key  common.Hash
	prev common.Hash
	had  bool
}

func (j jTransientChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.transient[j.addr], j.key)
		if len(sv.transient[j.addr]) == 0 {
			delete(sv.transient, j.addr)
		}
	} else {
		sv.transient[j.addr][j.key] = j.prev
	}
}

type jLogChange struct{ prevLen int }

func (j jLogChange) revert(sv *StateView) { sv.logs = sv.logs[:j.prevLen] }

type jSelfDestructChange struct {
	addr common.Address
	had  bool
}

func (j jSelfDestructChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.selfDestructed, j.addr)
	} else {
		sv.selfDestructed[j.addr] = true
	}
}

type jCreatedAccountChange struct {
	addr common.Address
	had  bool
}

func (j jCreatedAccountChange) revert(sv *StateView) {
	if !j.had {
		delete(sv.createdAccounts, j.addr)
	} else {
		sv.createdAccounts[j.addr] = true
	}
}

// ─── Storage ────────────────────────────────────────────────────────

func (sv *StateView) GetState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	// 1. Check overlay
	if slots, ok := sv.storageOverrides[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	// 2. Check versioned state
	if val, ok := sv.state.GetStorage(addr, key, sv.block); ok {
		return val
	}
	// 3. Cache miss callback
	if sv.miss.OnStorage != nil {
		return sv.miss.OnStorage(addr, key, sv.block)
	}
	return common.Hash{}
}

func (sv *StateView) GetCommittedState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	// Committed = pre-transaction state, bypass overlay.
	if val, ok := sv.state.GetStorage(addr, key, sv.block); ok {
		return val
	}
	if sv.miss.OnStorage != nil {
		return sv.miss.OnStorage(addr, key, sv.block)
	}
	return common.Hash{}
}

func (sv *StateView) SetState(addr common.Address, key, value common.Hash, _ ...stateconf.StateDBStateOption) {
	var prev common.Hash
	var had bool
	if slots, ok := sv.storageOverrides[addr]; ok {
		prev, had = slots[key]
	}
	sv.journal = append(sv.journal, jStorageChange{addr, key, prev, had})
	if sv.storageOverrides[addr] == nil {
		sv.storageOverrides[addr] = make(map[common.Hash]common.Hash)
	}
	sv.storageOverrides[addr][key] = value

	// Track dirty
	if sv.dirtyStorage[addr] == nil {
		sv.dirtyStorage[addr] = make(map[common.Hash]common.Hash)
	}
	sv.dirtyStorage[addr][key] = value
}

// ─── Balance ────────────────────────────────────────────────────────

func (sv *StateView) GetBalance(addr common.Address) *uint256.Int {
	if bal, ok := sv.balanceOverrides[addr]; ok {
		return new(uint256.Int).Set(bal)
	}
	if bal, ok := sv.state.GetBalance(addr, sv.block); ok {
		return bal
	}
	if sv.miss.OnBalance != nil {
		return sv.miss.OnBalance(addr, sv.block)
	}
	return uint256.NewInt(0)
}

func (sv *StateView) SubBalance(addr common.Address, amount *uint256.Int) {
	prev, had := sv.balanceOverrides[addr]
	if had {
		prev = prev.Clone()
	}
	sv.journal = append(sv.journal, jBalanceChange{addr, prev, had})
	bal := sv.GetBalance(addr)
	newBal := new(uint256.Int).Sub(bal, amount)
	sv.balanceOverrides[addr] = newBal
	sv.dirtyBalance[addr] = newBal
}

func (sv *StateView) AddBalance(addr common.Address, amount *uint256.Int) {
	prev, had := sv.balanceOverrides[addr]
	if had {
		prev = prev.Clone()
	}
	sv.journal = append(sv.journal, jBalanceChange{addr, prev, had})
	bal := sv.GetBalance(addr)
	newBal := new(uint256.Int).Add(bal, amount)
	sv.balanceOverrides[addr] = newBal
	sv.dirtyBalance[addr] = newBal
}

func (sv *StateView) CreateAccount(addr common.Address) {
	had := sv.createdAccounts[addr]
	sv.journal = append(sv.journal, jCreatedAccountChange{addr: addr, had: had})
	sv.createdAccounts[addr] = true
}

// ─── Nonce ──────────────────────────────────────────────────────────

func (sv *StateView) GetNonce(addr common.Address) uint64 {
	if n, ok := sv.nonceOverrides[addr]; ok {
		return n
	}
	if n, ok := sv.state.GetNonce(addr, sv.block); ok {
		return n
	}
	if sv.miss.OnNonce != nil {
		return sv.miss.OnNonce(addr, sv.block)
	}
	return 0
}

func (sv *StateView) SetNonce(addr common.Address, n uint64) {
	prev, had := sv.nonceOverrides[addr]
	if !had {
		prev = sv.GetNonce(addr)
		had = prev != 0
	}
	sv.journal = append(sv.journal, jNonceChange{addr, prev, had})
	sv.nonceOverrides[addr] = n
	sv.dirtyNonce[addr] = n
}

// ─── Code ───────────────────────────────────────────────────────────

func (sv *StateView) GetCode(addr common.Address) []byte {
	if code, ok := sv.codeOverrides[addr]; ok {
		return append([]byte(nil), code...)
	}
	if code, ok := sv.state.GetCode(addr, sv.block); ok {
		return code
	}
	if sv.miss.OnCode != nil {
		return sv.miss.OnCode(addr, sv.block)
	}
	return nil
}

func (sv *StateView) GetCodeHash(addr common.Address) common.Hash {
	if code, ok := sv.codeOverrides[addr]; ok {
		if len(code) == 0 {
			return common.Hash{}
		}
		return crypto.Keccak256Hash(code)
	}
	if h, ok := sv.state.GetCodeHash(addr, sv.block); ok {
		return h
	}
	// Fall through to fetching code for hash
	code := sv.GetCode(addr)
	if len(code) == 0 {
		return common.Hash{}
	}
	return crypto.Keccak256Hash(code)
}

func (sv *StateView) SetCode(addr common.Address, code []byte) {
	prev, had := sv.codeOverrides[addr]
	if !had {
		prev = sv.GetCode(addr)
		had = len(prev) > 0
	}
	sv.journal = append(sv.journal, jCodeChange{addr: addr, prev: append([]byte(nil), prev...), had: had})
	sv.codeOverrides[addr] = append([]byte(nil), code...)
	sv.dirtyCode[addr] = append([]byte(nil), code...)
}

func (sv *StateView) GetCodeSize(addr common.Address) int { return len(sv.GetCode(addr)) }

// ─── Refund ─────────────────────────────────────────────────────────

func (sv *StateView) AddRefund(gas uint64) {
	sv.journal = append(sv.journal, jRefundChange{sv.refund})
	sv.refund += gas
}

func (sv *StateView) SubRefund(gas uint64) {
	sv.journal = append(sv.journal, jRefundChange{sv.refund})
	sv.refund -= gas
}

func (sv *StateView) GetRefund() uint64 { return sv.refund }

// ─── Transient storage (EIP-1153) ───────────────────────────────────

func (sv *StateView) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := sv.transient[addr]; ok {
		return slots[key]
	}
	return common.Hash{}
}

func (sv *StateView) SetTransientState(addr common.Address, key, value common.Hash) {
	var prev common.Hash
	var had bool
	if slots, ok := sv.transient[addr]; ok {
		prev, had = slots[key]
	}
	sv.journal = append(sv.journal, jTransientChange{addr, key, prev, had})
	if sv.transient[addr] == nil {
		sv.transient[addr] = make(map[common.Hash]common.Hash)
	}
	sv.transient[addr][key] = value
}

// ─── Self-destruct ──────────────────────────────────────────────────

func (sv *StateView) SelfDestruct(addr common.Address) {
	prev, had := sv.balanceOverrides[addr]
	if had {
		prev = prev.Clone()
	}
	sv.journal = append(sv.journal, jBalanceChange{addr, prev, had})
	sv.journal = append(sv.journal, jSelfDestructChange{addr: addr, had: sv.selfDestructed[addr]})
	sv.selfDestructed[addr] = true
	sv.balanceOverrides[addr] = uint256.NewInt(0)
}

func (sv *StateView) HasSelfDestructed(addr common.Address) bool {
	return sv.selfDestructed[addr]
}

func (sv *StateView) Selfdestruct6780(addr common.Address) {
	if sv.createdAccounts[addr] {
		sv.SelfDestruct(addr)
	}
}

// ─── Account queries ────────────────────────────────────────────────

func (sv *StateView) Exist(addr common.Address) bool {
	if sv.createdAccounts[addr] {
		return true
	}
	if _, ok := sv.balanceOverrides[addr]; ok {
		return true
	}
	if _, ok := sv.nonceOverrides[addr]; ok {
		return true
	}
	if _, ok := sv.codeOverrides[addr]; ok {
		return true
	}
	if sv.GetCodeSize(addr) > 0 {
		return true
	}
	if sv.GetBalance(addr).Sign() > 0 {
		return true
	}
	if sv.GetNonce(addr) > 0 {
		return true
	}
	return false
}

func (sv *StateView) Empty(addr common.Address) bool {
	return sv.GetBalance(addr).IsZero() && sv.GetNonce(addr) == 0 && sv.GetCodeSize(addr) == 0
}

// ─── Access list ────────────────────────────────────────────────────

func (sv *StateView) AddressInAccessList(addr common.Address) bool {
	return sv.accessedAddrs[addr]
}

func (sv *StateView) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	addrOk := sv.accessedAddrs[addr]
	if slots, ok := sv.accessedSlots[addr]; ok {
		return addrOk, slots[slot]
	}
	return addrOk, false
}

func (sv *StateView) AddAddressToAccessList(addr common.Address) {
	if !sv.accessedAddrs[addr] {
		sv.journal = append(sv.journal, jAccessListAddr{addr})
	}
	sv.accessedAddrs[addr] = true
}

func (sv *StateView) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	hadAddr := sv.accessedAddrs[addr]
	hadSlot := false
	if slots, ok := sv.accessedSlots[addr]; ok {
		hadSlot = slots[slot]
	}
	if !hadSlot {
		sv.journal = append(sv.journal, jAccessListSlot{addr, slot, hadAddr})
	}
	sv.accessedAddrs[addr] = true
	if sv.accessedSlots[addr] == nil {
		sv.accessedSlots[addr] = make(map[common.Hash]bool)
	}
	sv.accessedSlots[addr][slot] = true
}

func (sv *StateView) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	sv.accessedAddrs = make(map[common.Address]bool)
	sv.accessedSlots = make(map[common.Address]map[common.Hash]bool)
	sv.predicates = make(map[common.Address][]predicate.Predicate)
	sv.transient = make(map[common.Address]map[common.Hash]common.Hash)

	if rulesExtra := safeGetRulesExtra(rules); rulesExtra != nil {
		sv.predicates = predicate.FromAccessList(rulesExtra, txAccesses)
	}

	sv.accessedAddrs[sender] = true
	if dest != nil {
		sv.accessedAddrs[*dest] = true
	}
	sv.accessedAddrs[coinbase] = true
	for _, addr := range precompiles {
		sv.accessedAddrs[addr] = true
	}
	for _, el := range txAccesses {
		sv.accessedAddrs[el.Address] = true
		for _, slot := range el.StorageKeys {
			if sv.accessedSlots[el.Address] == nil {
				sv.accessedSlots[el.Address] = make(map[common.Hash]bool)
			}
			sv.accessedSlots[el.Address][slot] = true
		}
	}
}

func safeGetRulesExtra(rules params.Rules) predicate.Predicates {
	defer func() {
		if recover() != nil {
		}
	}()
	return cparams.GetRulesExtra(rules)
}

// ─── Snapshots — journal-based, O(mutations) ────────────────────────

func (sv *StateView) Snapshot() int {
	id := len(sv.snapshots)
	sv.snapshots = append(sv.snapshots, len(sv.journal))
	return id
}

func (sv *StateView) RevertToSnapshot(id int) {
	target := sv.snapshots[id]
	sv.snapshots = sv.snapshots[:id]
	for i := len(sv.journal) - 1; i >= target; i-- {
		sv.journal[i].revert(sv)
	}
	sv.journal = sv.journal[:target]
}

// ─── Logs ───────────────────────────────────────────────────────────

func (sv *StateView) AddLog(l *types.Log) {
	sv.journal = append(sv.journal, jLogChange{len(sv.logs)})
	sv.logs = append(sv.logs, l)
}

func (sv *StateView) Logs() []*types.Log { return sv.logs }

func (sv *StateView) AddPreimage(common.Hash, []byte) {}

// ─── Tx context (StateDBRemainder) ──────────────────────────────────

func (sv *StateView) SetTxContext(hash common.Hash, index int) {
	sv.txHash = hash
	sv.txIndex = index
}

func (sv *StateView) TxHash() common.Hash { return sv.txHash }
func (sv *StateView) TxIndex() int        { return sv.txIndex }

// ─── Avalanche-specific methods ─────────────────────────────────────

func (sv *StateView) GetPredicate(address common.Address, index int) (predicate.Predicate, bool) {
	preds, exists := sv.predicates[address]
	if !exists || index < 0 || index >= len(preds) {
		return nil, false
	}
	return preds[index], true
}

func (sv *StateView) GetBalanceMultiCoin(addr common.Address, coinID common.Hash) *big.Int {
	normalizeCoinID(&coinID)
	return sv.GetState(addr, coinID, stateconf.SkipStateKeyTransformation()).Big()
}

func (sv *StateView) AddBalanceMultiCoin(addr common.Address, coinID common.Hash, amount *big.Int) {
	if amount == nil || amount.Sign() == 0 {
		return
	}
	newAmount := new(big.Int).Add(sv.GetBalanceMultiCoin(addr, coinID), amount)
	normalizeCoinID(&coinID)
	sv.SetState(addr, coinID, common.BigToHash(newAmount), stateconf.SkipStateKeyTransformation())
}

func (sv *StateView) SubBalanceMultiCoin(addr common.Address, coinID common.Hash, amount *big.Int) {
	if amount == nil || amount.Sign() == 0 {
		return
	}
	newAmount := new(big.Int).Sub(sv.GetBalanceMultiCoin(addr, coinID), amount)
	normalizeCoinID(&coinID)
	sv.SetState(addr, coinID, common.BigToHash(newAmount), stateconf.SkipStateKeyTransformation())
}

func normalizeCoinID(coinID *common.Hash) {
	coinID[0] |= 0x01
}

// ─── Dirty state extraction ─────────────────────────────────────────
// Used after block execution to extract the diffs for applying to versioned state.

// DirtyStorage returns all storage slots written during EVM execution.
func (sv *StateView) DirtyStorage() map[common.Address]map[common.Hash]common.Hash {
	return sv.dirtyStorage
}

// DirtyBalances returns all balances modified during EVM execution.
func (sv *StateView) DirtyBalances() map[common.Address]*uint256.Int {
	return sv.dirtyBalance
}

// DirtyNonces returns all nonces modified during EVM execution.
func (sv *StateView) DirtyNonces() map[common.Address]uint64 {
	return sv.dirtyNonce
}

// DirtyCode returns all code modified during EVM execution.
func (sv *StateView) DirtyCode() map[common.Address][]byte {
	return sv.dirtyCode
}

// StorageOverrides returns the current overlay storage (final state after all
// txs, with reverts properly applied). Use this for block diffs instead of
// DirtyStorage which doesn't undo on revert.
func (sv *StateView) StorageOverrides() map[common.Address]map[common.Hash]common.Hash {
	return sv.storageOverrides
}

// BalanceOverrides returns the current overlay balances.
func (sv *StateView) BalanceOverrides() map[common.Address]*uint256.Int {
	return sv.balanceOverrides
}

// NonceOverrides returns the current overlay nonces.
func (sv *StateView) NonceOverrides() map[common.Address]uint64 {
	return sv.nonceOverrides
}

// CodeOverrides returns the current overlay code.
func (sv *StateView) CodeOverrides() map[common.Address][]byte {
	return sv.codeOverrides
}
