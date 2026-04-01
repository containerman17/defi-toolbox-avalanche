package statedb

// StateDB — EVM state with RWMutex-protected reads and in-place block updates.
//
// Two-layer architecture:
//
//  1. Primary layer (fast): plain Go maps, mutated in place under write lock.
//     Readers hold RLock — concurrent reads are safe. This is where 99.9% of
//     accesses go after warm-up.
//
//  2. Backfill layer (slow): RWMutex-protected maps for on-demand cache misses.
//     When a read misses the primary layer, it fetches from the network and
//     stores here. On the next block update, backfill is merged into the primary
//     layer and cleared. After warm-up, this layer is empty.
//
// Block updates: ApplyDiffInPlace mutates the state in O(diff + backfill) time,
// then clears the backfill. The write lock (LiveState.blockMu) ensures no
// readers are active during mutation.
//
// The EVM's vm.StateDB interface requires mutable operations (SetState, SetCode,
// SubBalance, etc.). These go through the backfill layer or are handled by
// CallState (the per-EVM-call overlay with journal-based snapshot/revert).

import (
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"

	"github.com/ava-labs/libevm/libevm/stateconf"
)

// Ensure interface compliance.
var _ vm.StateDB = (*StateDB)(nil)

// Fetcher is the interface for on-demand state fetching.
type Fetcher interface {
	FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error)
	FetchBalance(addr common.Address) (*uint256.Int, error)
	FetchNonce(addr common.Address) (uint64, error)
	FetchCode(addr common.Address) ([]byte, error)
	FetchBlockHash(num uint64) (common.Hash, error)
}

// StateDB implements vm.StateDB with immutable snapshots + backfill.
type StateDB struct {
	// Fast layer: immutable, lock-free reads. Swapped atomically on block update.
	immutable atomic.Pointer[ImmutableState]

	// Slow layer: cache misses stored here, merged into immutable on next block.
	backfillMu sync.RWMutex
	bfStorage  map[common.Address]map[common.Hash]common.Hash
	bfCode     map[common.Address][]byte
	bfCodeHash map[common.Address]common.Hash
	bfBalance  map[common.Address]*uint256.Int
	bfNonce    map[common.Address]uint64

	fetcher Fetcher

	// Per-call EVM state (access list, refund, logs, snapshots, transient storage).
	// These are used by the vm.StateDB interface for direct EVM execution.
	// CallState has its own copies of these for the overlay pattern.
	accessList       map[common.Address]map[common.Hash]bool
	refund           uint64
	logs             []*types.Log
	snapshots        []stateSnapshot
	snapID           int
	transientStorage map[common.Address]map[common.Hash]common.Hash
}

type stateSnapshot struct {
	id        int
	immutable *ImmutableState // snapshot of immutable at this point
	refund    uint64
}

// NewStateDB creates a new StateDB backed by the given fetcher.
func NewStateDB(fetcher Fetcher) *StateDB {
	s := &StateDB{
		fetcher:          fetcher,
		bfStorage:        make(map[common.Address]map[common.Hash]common.Hash),
		bfCode:           make(map[common.Address][]byte),
		bfCodeHash:       make(map[common.Address]common.Hash),
		bfBalance:        make(map[common.Address]*uint256.Int),
		bfNonce:          make(map[common.Address]uint64),
		accessList:       make(map[common.Address]map[common.Hash]bool),
		transientStorage: make(map[common.Address]map[common.Hash]common.Hash),
	}
	s.immutable.Store(NewImmutableState(0, 0))
	return s
}

// Immutable returns the current immutable state snapshot.
func (s *StateDB) Immutable() *ImmutableState {
	return s.immutable.Load()
}

// SetImmutable replaces the immutable state (used during initial dump loading).
func (s *StateDB) SetImmutable(im *ImmutableState) {
	s.immutable.Store(im)
}

// ApplyBlockDiff mutates the state in place by applying the diff and merging
// backfill data, then clears the backfill maps for the next block.
//
// Safety: the caller MUST hold LiveState.blockMu.Lock(). The RWMutex guarantees
// all readers (holding RLock) have finished before Lock() proceeds, so no
// goroutine is reading the maps while we mutate them.
//
// This is O(diff + backfill) — typically ~50-200 storage changes per block,
// vs the old CloneWithDiff which was O(total state) copying ~870K entries.
func (s *StateDB) ApplyBlockDiff(diff *BlockDiff) {
	im := s.immutable.Load()

	// Lock backfill so no concurrent cache-miss writes happen during merge.
	s.backfillMu.Lock()

	// Mutate state: merge backfill first (may be stale), then diff (authoritative).
	im.ApplyDiffInPlace(diff, s.bfStorage, s.bfCode, s.bfBalance, s.bfNonce)

	// Clear backfill — all data has been merged into the primary layer.
	s.bfStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.bfCode = make(map[common.Address][]byte)
	s.bfCodeHash = make(map[common.Address]common.Hash)
	s.bfBalance = make(map[common.Address]*uint256.Int)
	s.bfNonce = make(map[common.Address]uint64)

	s.backfillMu.Unlock()
}

// ─── Storage ─────────────────────────────────────────────────────────

func (s *StateDB) GetCommittedState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	return s.getStorage(addr, key)
}

func (s *StateDB) GetState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	return s.getStorage(addr, key)
}

func (s *StateDB) getStorage(addr common.Address, key common.Hash) common.Hash {
	val, _ := s.getStorageWithErr(addr, key)
	return val
}

func (s *StateDB) getStorageWithErr(addr common.Address, key common.Hash) (common.Hash, error) {
	// 1. Immutable (lock-free)
	im := s.immutable.Load()
	if val, ok := im.GetStorage(addr, key); ok {
		return val, nil
	}
	// 2. Backfill (RLock)
	s.backfillMu.RLock()
	if slots, ok := s.bfStorage[addr]; ok {
		if val, ok := slots[key]; ok {
			s.backfillMu.RUnlock()
			return val, nil
		}
	}
	s.backfillMu.RUnlock()
	// 3. Fetch from network
	if s.fetcher != nil {
		val, err := s.fetcher.FetchStorage(addr, key)
		if err != nil {
			return common.Hash{}, fmt.Errorf("FetchStorage(%s, %s): %w", addr.Hex()[:10], key.Hex()[:14], err)
		}
		s.backfillMu.Lock()
		if s.bfStorage[addr] == nil {
			s.bfStorage[addr] = make(map[common.Hash]common.Hash)
		}
		s.bfStorage[addr][key] = val
		s.backfillMu.Unlock()
		return val, nil
	}
	return common.Hash{}, nil
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash, _ ...stateconf.StateDBStateOption) {
	s.backfillMu.Lock()
	if s.bfStorage[addr] == nil {
		s.bfStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.bfStorage[addr][key] = value
	s.backfillMu.Unlock()
}

// SetStorageSlot sets a storage slot (used for initial dump loading and block diffs).
func (s *StateDB) SetStorageSlot(addr common.Address, slot, value common.Hash) {
	s.backfillMu.Lock()
	if s.bfStorage[addr] == nil {
		s.bfStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.bfStorage[addr][slot] = value
	s.backfillMu.Unlock()
}

// HasStorageSlot returns true if the slot is cached in either layer.
func (s *StateDB) HasStorageSlot(addr common.Address, slot common.Hash) bool {
	if s.immutable.Load().HasStorageSlot(addr, slot) {
		return true
	}
	s.backfillMu.RLock()
	defer s.backfillMu.RUnlock()
	if slots, ok := s.bfStorage[addr]; ok {
		_, ok = slots[slot]
		return ok
	}
	return false
}

// ─── Balance ─────────────────────────────────────────────────────────

func (s *StateDB) GetBalance(addr common.Address) *uint256.Int {
	bal, _ := s.getBalanceWithErr(addr)
	return bal
}

func (s *StateDB) getBalanceWithErr(addr common.Address) (*uint256.Int, error) {
	// 1. Immutable
	if bal, ok := s.immutable.Load().GetBalance(addr); ok {
		return new(uint256.Int).Set(bal), nil
	}
	// 2. Backfill
	s.backfillMu.RLock()
	if bal, ok := s.bfBalance[addr]; ok {
		s.backfillMu.RUnlock()
		return new(uint256.Int).Set(bal), nil
	}
	s.backfillMu.RUnlock()
	// 3. Fetch
	if s.fetcher != nil {
		bal, err := s.fetcher.FetchBalance(addr)
		if err != nil {
			return uint256.NewInt(0), err
		}
		s.backfillMu.Lock()
		s.bfBalance[addr] = bal
		s.backfillMu.Unlock()
		return new(uint256.Int).Set(bal), nil
	}
	return uint256.NewInt(0), nil
}

func (s *StateDB) SubBalance(addr common.Address, amount *uint256.Int) {
	bal := s.GetBalance(addr)
	newBal := new(uint256.Int).Sub(bal, amount)
	s.backfillMu.Lock()
	s.bfBalance[addr] = newBal
	s.backfillMu.Unlock()
}

func (s *StateDB) AddBalance(addr common.Address, amount *uint256.Int) {
	bal := s.GetBalance(addr)
	newBal := new(uint256.Int).Add(bal, amount)
	s.backfillMu.Lock()
	s.bfBalance[addr] = newBal
	s.backfillMu.Unlock()
}

func (s *StateDB) CreateAccount(addr common.Address) {}

// ─── Nonce ───────────────────────────────────────────────────────────

func (s *StateDB) GetNonce(addr common.Address) uint64 {
	// 1. Immutable
	if n, ok := s.immutable.Load().GetNonce(addr); ok {
		return n
	}
	// 2. Backfill
	s.backfillMu.RLock()
	if n, ok := s.bfNonce[addr]; ok {
		s.backfillMu.RUnlock()
		return n
	}
	s.backfillMu.RUnlock()
	// 3. Fetch
	if s.fetcher != nil {
		n, err := s.fetcher.FetchNonce(addr)
		if err != nil {
			return 0
		}
		s.backfillMu.Lock()
		s.bfNonce[addr] = n
		s.backfillMu.Unlock()
		return n
	}
	return 0
}

func (s *StateDB) SetNonce(addr common.Address, n uint64) {
	s.backfillMu.Lock()
	s.bfNonce[addr] = n
	s.backfillMu.Unlock()
}

// ─── Code ────────────────────────────────────────────────────────────

func (s *StateDB) GetCode(addr common.Address) []byte {
	code, _ := s.getCodeWithErr(addr)
	return code
}

func (s *StateDB) getCodeWithErr(addr common.Address) ([]byte, error) {
	// 1. Immutable
	if code, ok := s.immutable.Load().GetCode(addr); ok {
		return code, nil
	}
	// 2. Backfill
	s.backfillMu.RLock()
	if code, ok := s.bfCode[addr]; ok {
		s.backfillMu.RUnlock()
		return code, nil
	}
	s.backfillMu.RUnlock()
	// 3. Fetch
	if s.fetcher != nil {
		code, err := s.fetcher.FetchCode(addr)
		if err != nil {
			return nil, err
		}
		s.backfillMu.Lock()
		s.bfCode[addr] = code
		if len(code) > 0 {
			s.bfCodeHash[addr] = crypto.Keccak256Hash(code)
		}
		s.backfillMu.Unlock()
		return code, nil
	}
	return nil, nil
}

func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	// 1. Immutable
	if h, ok := s.immutable.Load().GetCodeHash(addr); ok {
		return h
	}
	// 2. Backfill
	s.backfillMu.RLock()
	if h, ok := s.bfCodeHash[addr]; ok {
		s.backfillMu.RUnlock()
		return h
	}
	s.backfillMu.RUnlock()
	// 3. Fetch code to compute hash
	code := s.GetCode(addr)
	if len(code) == 0 {
		return common.Hash{}
	}
	return crypto.Keccak256Hash(code)
}

func (s *StateDB) SetCode(addr common.Address, code []byte) {
	s.backfillMu.Lock()
	s.bfCode[addr] = code
	if len(code) > 0 {
		s.bfCodeHash[addr] = crypto.Keccak256Hash(code)
	} else {
		delete(s.bfCodeHash, addr)
	}
	s.backfillMu.Unlock()
}

func (s *StateDB) GetCodeSize(addr common.Address) int { return len(s.GetCode(addr)) }

// ─── Refund ──────────────────────────────────────────────────────────

func (s *StateDB) AddRefund(gas uint64) { s.refund += gas }
func (s *StateDB) SubRefund(gas uint64) { s.refund -= gas }
func (s *StateDB) GetRefund() uint64    { return s.refund }

// ─── Transient storage (EIP-1153) ────────────────────────────────────

func (s *StateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if m, ok := s.transientStorage[addr]; ok {
		return m[key]
	}
	return common.Hash{}
}

func (s *StateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	if _, ok := s.transientStorage[addr]; !ok {
		s.transientStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.transientStorage[addr][key] = value
}

// ─── Self-destruct ───────────────────────────────────────────────────

func (s *StateDB) SelfDestruct(addr common.Address)           {}
func (s *StateDB) HasSelfDestructed(addr common.Address) bool { return false }
func (s *StateDB) Selfdestruct6780(addr common.Address)       {}

// ─── Account queries ─────────────────────────────────────────────────

func (s *StateDB) Exist(addr common.Address) bool {
	if s.GetCodeSize(addr) > 0 {
		return true
	}
	if s.GetBalance(addr).Sign() > 0 {
		return true
	}
	return false
}

func (s *StateDB) Empty(addr common.Address) bool {
	return s.GetBalance(addr).IsZero() && s.GetNonce(addr) == 0 && s.GetCodeSize(addr) == 0
}

// ─── Access list ─────────────────────────────────────────────────────

func (s *StateDB) AddressInAccessList(addr common.Address) bool {
	_, ok := s.accessList[addr]
	return ok
}

func (s *StateDB) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	slots, addrOk := s.accessList[addr]
	if !addrOk {
		return false, false
	}
	_, slotOk := slots[slot]
	return true, slotOk
}

func (s *StateDB) AddAddressToAccessList(addr common.Address) {
	if _, ok := s.accessList[addr]; !ok {
		s.accessList[addr] = make(map[common.Hash]bool)
	}
}

func (s *StateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	s.AddAddressToAccessList(addr)
	s.accessList[addr][slot] = true
}

func (s *StateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	s.accessList = make(map[common.Address]map[common.Hash]bool)
	s.refund = 0
	s.logs = nil
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash)

	s.AddAddressToAccessList(sender)
	if dest != nil {
		s.AddAddressToAccessList(*dest)
	}
	s.AddAddressToAccessList(coinbase)
	for _, p := range precompiles {
		s.AddAddressToAccessList(p)
	}
	for _, el := range txAccesses {
		s.AddAddressToAccessList(el.Address)
		for _, key := range el.StorageKeys {
			s.AddSlotToAccessList(el.Address, key)
		}
	}
}

// ─── Snapshots ───────────────────────────────────────────────────────
// Note: For EVM execution, prefer CallState which has journal-based
// snapshots (O(mutations) not O(state)). These StateDB-level snapshots
// exist for the vm.StateDB interface but are rarely used in practice.

func (s *StateDB) Snapshot() int {
	s.snapID++
	snap := stateSnapshot{
		id:        s.snapID,
		immutable: s.immutable.Load(),
		refund:    s.refund,
	}
	s.snapshots = append(s.snapshots, snap)
	return s.snapID
}

func (s *StateDB) RevertToSnapshot(id int) {
	for i := len(s.snapshots) - 1; i >= 0; i-- {
		if s.snapshots[i].id == id {
			s.immutable.Store(s.snapshots[i].immutable)
			s.refund = s.snapshots[i].refund
			s.snapshots = s.snapshots[:i]
			return
		}
	}
}

// ─── Logs ────────────────────────────────────────────────────────────

func (s *StateDB) AddLog(log *types.Log) {
	s.logs = append(s.logs, log)
}
func (s *StateDB) AddPreimage(hash common.Hash, data []byte) {}

// ─── Block hash ──────────────────────────────────────────────────────

func (s *StateDB) GetBlockHash(num uint64) common.Hash {
	if s.fetcher != nil {
		h, _ := s.fetcher.FetchBlockHash(num)
		return h
	}
	return common.Hash{}
}

// ─── StateDB as Fetcher (for overlay pattern) ────────────────────────

func (s *StateDB) FetchStorage(addr common.Address, slot common.Hash) (common.Hash, error) {
	return s.getStorageWithErr(addr, slot)
}

func (s *StateDB) FetchBalance(addr common.Address) (*uint256.Int, error) {
	return s.getBalanceWithErr(addr)
}

func (s *StateDB) FetchNonce(addr common.Address) (uint64, error) {
	n := s.GetNonce(addr)
	return n, nil
}

func (s *StateDB) FetchCode(addr common.Address) ([]byte, error) {
	return s.getCodeWithErr(addr)
}

func (s *StateDB) FetchBlockHash(num uint64) (common.Hash, error) {
	if s.fetcher != nil {
		return s.fetcher.FetchBlockHash(num)
	}
	return common.Hash{}, nil
}

// ─── Overlay creation ────────────────────────────────────────────────

// NewOverlay creates a fresh StateDB backed by this one.
func (s *StateDB) NewOverlay() *StateDB {
	return NewStateDB(s)
}

// NewReusableOverlay creates an overlay that can be Reset() between calls.
func (s *StateDB) NewReusableOverlay() *StateDB {
	o := NewStateDB(s)
	return o
}

// Reset clears per-call state for reuse between EVM calls.
func (s *StateDB) Reset() {
	s.backfillMu.Lock()
	s.bfStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.bfCode = make(map[common.Address][]byte)
	s.bfCodeHash = make(map[common.Address]common.Hash)
	s.bfBalance = make(map[common.Address]*uint256.Int)
	s.bfNonce = make(map[common.Address]uint64)
	s.backfillMu.Unlock()
	s.accessList = make(map[common.Address]map[common.Hash]bool)
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash)
	s.refund = 0
	s.logs = s.logs[:0]
	s.snapshots = s.snapshots[:0]
	s.snapID = 0
}

// SetAccount sets account metadata (used for initial dump loading).
func (s *StateDB) SetAccount(addr common.Address, balance *uint256.Int, nonce uint64, code []byte) {
	s.backfillMu.Lock()
	if balance != nil {
		s.bfBalance[addr] = balance
	}
	s.bfNonce[addr] = nonce
	if len(code) > 0 {
		s.bfCode[addr] = code
		s.bfCodeHash[addr] = crypto.Keccak256Hash(code)
	}
	s.backfillMu.Unlock()
}

// ─── EVM execution ──────────────────────────────────────────────────

// EVMConfig holds configuration for EVM execution.
type EVMConfig struct {
	BlockNumber uint64
	Timestamp   uint64
	ChainID     int64
	BaseFee     uint64
	GasLimit    uint64
}

func ptr[T any](v T) *T { return &v }

// AvalancheCChainConfig returns the Avalanche C-Chain params matching the real node.
var AvalancheCChainConfig = &params.ChainConfig{
	ChainID:             big.NewInt(43114),
	HomesteadBlock:      big.NewInt(0),
	EIP150Block:         big.NewInt(0),
	EIP155Block:         big.NewInt(0),
	EIP158Block:         big.NewInt(0),
	ByzantiumBlock:      big.NewInt(0),
	ConstantinopleBlock: big.NewInt(0),
	PetersburgBlock:     big.NewInt(0),
	IstanbulBlock:       big.NewInt(0),
	MuirGlacierBlock:    big.NewInt(0),
	BerlinBlock:         big.NewInt(1640340),
	LondonBlock:         big.NewInt(3308552),
	ShanghaiTime:        ptr(uint64(1709740800)),
	CancunTime:          ptr(uint64(1734368400)),
}

// ExecuteCall runs an eth_call using the EVM.
func ExecuteCall(state *StateDB, cfg EVMConfig, from, to common.Address, data []byte) ([]byte, uint64, error) {
	ctx := GetCachedContext(cfg)
	return ctx.Execute(state, from, to, data)
}

// CachedContext holds pre-allocated EVM context for a given block config.
type CachedContext struct {
	blockCtx       vm.BlockContext
	rules          params.Rules
	chainCfg       *params.ChainConfig
	gasPrice       *big.Int
}

var cachedCtx *CachedContext

// GetCachedContext returns a cached EVM context, creating one if needed.
func GetCachedContext(cfg EVMConfig) *CachedContext {
	if cachedCtx != nil && cachedCtx.blockCtx.Time == cfg.Timestamp {
		return cachedCtx
	}

	baseFee := cfg.BaseFee
	if baseFee == 0 {
		baseFee = 25_000_000_000
	}
	blockGasLimit := cfg.GasLimit
	if blockGasLimit == 0 {
		blockGasLimit = 40_000_000
	}

	random := common.Hash{}
	coinbase := common.HexToAddress("0x0100000000000000000000000000000000000000")
	blockNumber := new(big.Int).SetUint64(cfg.BlockNumber)

	blockCtx := vm.BlockContext{
		CanTransfer: func(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
			return db.GetBalance(addr).Cmp(amount) >= 0
		},
		Transfer: func(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
			db.SubBalance(sender, amount)
			db.AddBalance(recipient, amount)
		},
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		BlockNumber: blockNumber,
		Time:        cfg.Timestamp,
		Difficulty:  big.NewInt(1),
		Random:      &random,
		GasLimit:    blockGasLimit,
		BaseFee:     new(big.Int).SetUint64(baseFee),
	}

	chainCfg := AvalancheCChainConfig
	rules := chainCfg.Rules(blockNumber, true, cfg.Timestamp)

	cachedCtx = &CachedContext{
		blockCtx: blockCtx,
		rules:    rules,
		chainCfg: chainCfg,
		gasPrice: new(big.Int).SetUint64(baseFee),
	}
	return cachedCtx
}

// Execute runs an EVM call using the pre-allocated context.
func (ctx *CachedContext) Execute(state *StateDB, from, to common.Address, data []byte) ([]byte, uint64, error) {
	txCtx := vm.TxContext{
		Origin:   from,
		GasPrice: ctx.gasPrice,
	}
	precompiles := vm.ActivePrecompiles(ctx.rules)
	state.Prepare(ctx.rules, from, ctx.blockCtx.Coinbase, &to, precompiles, nil)
	evm := vm.NewEVM(ctx.blockCtx, txCtx, state, ctx.chainCfg, vm.Config{})
	gasLimit := uint64(5_000_000)
	ret, gasLeft, err := evm.Call(vm.AccountRef(from), to, data, gasLimit, uint256.NewInt(0))
	return ret, gasLimit - gasLeft, err
}

// ExecuteWithCallState runs an EVM call on a CallState overlay.
// Uses CallerContract for JUMPDEST sharing across calls.
func (ctx *CachedContext) ExecuteWithCallState(cs *CallState, from, to common.Address, data []byte) ([]byte, uint64, error) {
	txCtx := vm.TxContext{
		Origin:   from,
		GasPrice: ctx.gasPrice,
	}
	precompiles := vm.ActivePrecompiles(ctx.rules)
	cs.Prepare(ctx.rules, from, ctx.blockCtx.Coinbase, &to, precompiles, nil)
	evm := vm.NewEVM(ctx.blockCtx, txCtx, cs, ctx.chainCfg, vm.Config{})
	gasLimit := uint64(5_000_000)
	ret, gasLeft, err := evm.Call(vm.AccountRef(from), to, data, gasLimit, uint256.NewInt(0))
	return ret, gasLimit - gasLeft, err
}
