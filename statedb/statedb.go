package statedb

import (
	"math/big"

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
// Native backend implements this via WebSocket to state server.
// WASM backend implements this via JS callbacks.
type Fetcher interface {
	FetchStorage(addr common.Address, slot common.Hash) common.Hash
	FetchBalance(addr common.Address) *uint256.Int
	FetchNonce(addr common.Address) uint64
	FetchCode(addr common.Address) []byte
	FetchBlockHash(num uint64) common.Hash
}

type account struct {
	balance  *uint256.Int
	nonce    uint64
	code     []byte
	codeHash common.Hash
	storage  map[common.Hash]common.Hash
	exists   bool // true if we know this account exists (was fetched or created)
}

// StateDB implements vm.StateDB with on-demand fetching via a Fetcher.
// Lock-free: EVM execution is single-threaded per call.
type StateDB struct {
	accounts   map[common.Address]*account
	accessList map[common.Address]map[common.Hash]bool
	refund     uint64
	fetcher    Fetcher
	logs       []*types.Log

	// Snapshot support
	snapshots []snapshot
	snapID    int

	// Transient storage (EIP-1153)
	transientStorage map[common.Address]map[common.Hash]common.Hash
}

type snapshot struct {
	id       int
	accounts map[common.Address]*account // deep copy at snapshot time
	refund   uint64
}

func NewStateDB(fetcher Fetcher) *StateDB {
	return &StateDB{
		accounts:         make(map[common.Address]*account),
		accessList:       make(map[common.Address]map[common.Hash]bool),
		fetcher:          fetcher,
		transientStorage: make(map[common.Address]map[common.Hash]common.Hash),
	}
}

func (s *StateDB) getOrFetch(addr common.Address) *account {
	if a, ok := s.accounts[addr]; ok {
		// If account exists locally but hasn't been fully fetched yet
		// (created by SetStorageSlot with only storage overrides),
		// fetch the rest from the base/remote.
		if !a.exists && s.fetcher != nil {
			a.balance = s.fetcher.FetchBalance(addr)
			a.nonce = s.fetcher.FetchNonce(addr)
			a.code = s.fetcher.FetchCode(addr)
			if len(a.code) > 0 {
				a.codeHash = crypto.Keccak256Hash(a.code)
			}
			a.exists = true
		}
		return a
	}
	// Fetch from remote
	a := &account{
		storage: make(map[common.Hash]common.Hash),
	}
	if s.fetcher != nil {
		a.balance = s.fetcher.FetchBalance(addr)
		a.nonce = s.fetcher.FetchNonce(addr)
		a.code = s.fetcher.FetchCode(addr)
		if len(a.code) > 0 {
			a.codeHash = crypto.Keccak256Hash(a.code)
		}
		a.exists = a.balance.Sign() > 0 || a.nonce > 0 || len(a.code) > 0
	} else {
		a.balance = uint256.NewInt(0)
	}
	s.accounts[addr] = a
	return a
}

// SetAccount sets account data directly (used for prefilling from initial_dump).
func (s *StateDB) SetAccount(addr common.Address, balance *uint256.Int, nonce uint64, code []byte) {
	a := &account{
		balance: balance,
		nonce:   nonce,
		code:    code,
		storage: make(map[common.Hash]common.Hash),
		exists:  true,
	}
	if len(code) > 0 {
		a.codeHash = crypto.Keccak256Hash(code)
	}
	if existing, ok := s.accounts[addr]; ok {
		// Preserve existing storage
		a.storage = existing.storage
	}
	s.accounts[addr] = a
}

// SetStorageSlot sets a storage slot directly (used for prefilling).
func (s *StateDB) SetStorageSlot(addr common.Address, slot, value common.Hash) {
	a, ok := s.accounts[addr]
	if !ok {
		a = &account{
			balance: uint256.NewInt(0),
			storage: make(map[common.Hash]common.Hash),
		}
		s.accounts[addr] = a
	}
	a.storage[slot] = value
}

// Balance
func (s *StateDB) CreateAccount(addr common.Address) {
	a := s.getOrFetch(addr)
	a.exists = true
}

func (s *StateDB) SubBalance(addr common.Address, amount *uint256.Int) {
	a := s.getOrFetch(addr)
	a.balance = new(uint256.Int).Sub(a.balance, amount)
}

func (s *StateDB) AddBalance(addr common.Address, amount *uint256.Int) {
	a := s.getOrFetch(addr)
	a.balance = new(uint256.Int).Add(a.balance, amount)
}

func (s *StateDB) GetBalance(addr common.Address) *uint256.Int {
	return new(uint256.Int).Set(s.getOrFetch(addr).balance)
}

// Nonce
func (s *StateDB) GetNonce(addr common.Address) uint64    { return s.getOrFetch(addr).nonce }
func (s *StateDB) SetNonce(addr common.Address, n uint64) { s.getOrFetch(addr).nonce = n }

// Code
func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	a := s.getOrFetch(addr)
	if len(a.code) == 0 {
		return common.Hash{}
	}
	if a.codeHash == (common.Hash{}) {
		a.codeHash = crypto.Keccak256Hash(a.code)
	}
	return a.codeHash
}

func (s *StateDB) GetCode(addr common.Address) []byte       { return s.getOrFetch(addr).code }
func (s *StateDB) SetCode(addr common.Address, code []byte) {
	a := s.getOrFetch(addr)
	a.code = code
	if len(code) > 0 {
		a.codeHash = crypto.Keccak256Hash(code)
	} else {
		a.codeHash = common.Hash{}
	}
}
func (s *StateDB) GetCodeSize(addr common.Address) int { return len(s.getOrFetch(addr).code) }

// Refund
func (s *StateDB) AddRefund(gas uint64) { s.refund += gas }
func (s *StateDB) SubRefund(gas uint64) { s.refund -= gas }
func (s *StateDB) GetRefund() uint64    { return s.refund }

// Storage
func (s *StateDB) GetCommittedState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	return s.getStorage(addr, key)
}

func (s *StateDB) GetState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	return s.getStorage(addr, key)
}

func (s *StateDB) getStorage(addr common.Address, key common.Hash) common.Hash {
	// Check local storage first (includes overlay overrides from SetStorageSlot)
	if a, ok := s.accounts[addr]; ok {
		if val, ok := a.storage[key]; ok {
			return val
		}
	}
	// Not in local storage — fetch from base/remote
	a := s.getOrFetch(addr)
	if val, ok := a.storage[key]; ok {
		return val
	}
	if s.fetcher != nil {
		val := s.fetcher.FetchStorage(addr, key)
		a.storage[key] = val
		return val
	}
	return common.Hash{}
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash, _ ...stateconf.StateDBStateOption) {
	a := s.getOrFetch(addr)
	a.storage[key] = value
}

// Transient storage
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

// Self-destruct
func (s *StateDB) SelfDestruct(addr common.Address)           {}
func (s *StateDB) HasSelfDestructed(addr common.Address) bool { return false }
func (s *StateDB) Selfdestruct6780(addr common.Address)       {}

// Account queries
func (s *StateDB) Exist(addr common.Address) bool {
	a := s.getOrFetch(addr)
	return a.exists || a.balance.Sign() > 0 || a.nonce > 0 || len(a.code) > 0
}

func (s *StateDB) Empty(addr common.Address) bool {
	a := s.getOrFetch(addr)
	return a.balance.IsZero() && a.nonce == 0 && len(a.code) == 0
}

// Access list
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

// Snapshots
func (s *StateDB) Snapshot() int {
	s.snapID++
	// Deep copy accounts for snapshot
	snap := snapshot{
		id:       s.snapID,
		accounts: s.deepCopyAccounts(),
		refund:   s.refund,
	}
	s.snapshots = append(s.snapshots, snap)
	return s.snapID
}

func (s *StateDB) RevertToSnapshot(id int) {
	for i := len(s.snapshots) - 1; i >= 0; i-- {
		if s.snapshots[i].id == id {
			s.accounts = s.snapshots[i].accounts
			s.refund = s.snapshots[i].refund
			s.snapshots = s.snapshots[:i]
			return
		}
	}
}

func (s *StateDB) deepCopyAccounts() map[common.Address]*account {
	cp := make(map[common.Address]*account, len(s.accounts))
	for addr, a := range s.accounts {
		na := &account{
			balance:  new(uint256.Int).Set(a.balance),
			nonce:    a.nonce,
			code:     a.code, // code is immutable, share the slice
			codeHash: a.codeHash,
			storage:  make(map[common.Hash]common.Hash, len(a.storage)),
			exists:   a.exists,
		}
		for k, v := range a.storage {
			na.storage[k] = v
		}
		cp[addr] = na
	}
	return cp
}

// Logs
func (s *StateDB) AddLog(log *types.Log) {
	s.logs = append(s.logs, log)
}
func (s *StateDB) AddPreimage(hash common.Hash, data []byte) {}

// GetBlockHash uses the fetcher to get block hashes.
func (s *StateDB) GetBlockHash(num uint64) common.Hash {
	if s.fetcher != nil {
		return s.fetcher.FetchBlockHash(num)
	}
	return common.Hash{}
}

// ─── StateDB as Fetcher (for overlay pattern) ─────────────────────
// A StateDB can back another StateDB: overlay checks itself first,
// then delegates to the base. Base delegates to the remote fetcher.

func (s *StateDB) FetchStorage(addr common.Address, slot common.Hash) common.Hash {
	return s.getStorage(addr, slot)
}

func (s *StateDB) FetchBalance(addr common.Address) *uint256.Int {
	return new(uint256.Int).Set(s.getOrFetch(addr).balance)
}

func (s *StateDB) FetchNonce(addr common.Address) uint64 {
	return s.getOrFetch(addr).nonce
}

func (s *StateDB) FetchCode(addr common.Address) []byte {
	return s.getOrFetch(addr).code
}

func (s *StateDB) FetchBlockHash(num uint64) common.Hash {
	if s.fetcher != nil {
		return s.fetcher.FetchBlockHash(num)
	}
	return common.Hash{}
}

// NewOverlay creates a fresh StateDB backed by this one.
// Writes go to the overlay; reads fall through to the base on miss.
func (s *StateDB) NewOverlay() *StateDB {
	return NewStateDB(s)
}

// NewReusableOverlay creates an overlay that can be Reset() between calls.
// Pre-allocates maps to avoid per-call allocation.
func (s *StateDB) NewReusableOverlay() *StateDB {
	return &StateDB{
		accounts:         make(map[common.Address]*account, 32),
		accessList:       make(map[common.Address]map[common.Hash]bool, 16),
		fetcher:          s,
		transientStorage: make(map[common.Address]map[common.Hash]common.Hash),
	}
}

// Reset clears all state in this overlay without allocating new maps.
// The fetcher (base) is preserved. Use between EVM calls to reuse the overlay.
func (s *StateDB) Reset() {
	for k := range s.accounts {
		delete(s.accounts, k)
	}
	for k := range s.accessList {
		delete(s.accessList, k)
	}
	for k := range s.transientStorage {
		delete(s.transientStorage, k)
	}
	s.refund = 0
	s.logs = s.logs[:0]
	s.snapshots = s.snapshots[:0]
	s.snapID = 0
}

// ─── EVM execution ─────────────────────────────────────────────────

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
// Reuse across calls to avoid per-call allocations.
type CachedContext struct {
	blockCtx vm.BlockContext
	rules    params.Rules
	chainCfg *params.ChainConfig
	gasPrice *big.Int
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
