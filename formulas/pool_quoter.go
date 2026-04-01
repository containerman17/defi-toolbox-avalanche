package formulas

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// PoolQuoter is implemented by each pool type's struct.
// Quote() is pure math — no state access, no keccak, no map lookups.
type PoolQuoter interface {
	// Quote returns the output amount for a given input. Zero = no output.
	Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int

	// Address returns the pool's contract address.
	Address() common.Address
}

// zeroQuoter is a PoolQuoter that always returns zero.
// Used for blacklisted/unknown pools so Get() never returns nil.
type zeroQuoter struct{ addr common.Address }

func (z *zeroQuoter) Address() common.Address                          { return z.addr }
func (z *zeroQuoter) Quote(_ *uint256.Int, _ bool) uint256.Int { return uint256.Int{} }

// EVMCaller executes a view call against the current EVM state.
// Returns (result, true) on success, (nil, false) on revert or error.
type EVMCaller func(to common.Address, data []byte) ([]byte, bool)

// quoteCacheKey packs (amountIn, direction) into a single lookup key.
// The last bit of the lowest word encodes direction (safe because real
// ERC20 amounts never use the full 256-bit range with the low bit mattering).
type quoteCacheKey = uint256.Int

func makeQuoteCacheKey(amountIn *uint256.Int, zeroForOne bool) quoteCacheKey {
	var k quoteCacheKey
	k.Lsh(amountIn, 1)
	if zeroForOne {
		k.Or(&k, uint256.NewInt(1))
	}
	return k
}

// QuoteCache is a map-based cache for quote results per pool.
// Invalidated on block updates (pool state change).
type QuoteCache struct {
	entries map[quoteCacheKey]uint256.Int
}

func (c *QuoteCache) Lookup(amountIn *uint256.Int, zeroForOne bool) (out uint256.Int, hit bool) {
	key := makeQuoteCacheKey(amountIn, zeroForOne)
	out, hit = c.entries[key]
	return
}

func (c *QuoteCache) Store(amountIn *uint256.Int, zeroForOne bool, out uint256.Int) {
	key := makeQuoteCacheKey(amountIn, zeroForOne)
	if c.entries == nil {
		c.entries = make(map[quoteCacheKey]uint256.Int)
	}
	c.entries[key] = out
}

// PoolManager holds pool structs and handles lazy construction + invalidation.
type PoolManager struct {
	pools          map[common.Address]PoolQuoter
	registry       *Registry
	reader         StorageReader
	evmCaller      EVMCaller // optional; used for rate provider calls (Balancer V3 WITH_RATE tokens)
	poolTokens     map[common.Address][2]common.Address // pool → [token0, token1]
	poolTypes      map[common.Address]int               // pool → poolType from pools.txt
	poolDex        map[common.Address]string            // pool → DEX provider name (e.g. "pangolin_v2")
	tokenModels    *TokenModelRegistry
	blockTimestamp uint64 // block.timestamp for volatility reference updates (LFJ V2)

	// depSlots: reverse map from (contractAddr, slot) → poolAddr for cache busting.
	depSlots map[common.Address]map[common.Hash]common.Address

	// Quote cache: 16-slot ring buffer per pool. Skipped for LFJ V2 (time-dependent).
	quoteCaches  map[common.Address]*QuoteCache
	noQuoteCache map[common.Address]bool // true for LFJ V2 pools

	// Balance cache: pool output token balances for the reserve cap check.
	// Key: pool address, value: [balance0, balance1]. Invalidated with pool.
	balanceCache map[common.Address][2]uint256.Int

	// Mutex for thread-safe cache access (quoteCaches + balanceCache).
	cacheMu sync.RWMutex
}

// NewPoolManager creates a PoolManager backed by the given registry and storage reader.
func NewPoolManager(registry *Registry, reader StorageReader) *PoolManager {
	return &PoolManager{
		pools:        make(map[common.Address]PoolQuoter),
		registry:     registry,
		reader:       reader,
		poolTokens:   make(map[common.Address][2]common.Address),
		poolTypes:    make(map[common.Address]int),
		poolDex:      make(map[common.Address]string),
		tokenModels:  NewTokenModelRegistry(reader),
		depSlots:     make(map[common.Address]map[common.Hash]common.Address),
		quoteCaches:  make(map[common.Address]*QuoteCache),
		noQuoteCache: make(map[common.Address]bool),
		balanceCache: make(map[common.Address][2]uint256.Int),
	}
}

// SetEVMCaller provides an EVM execution function used for calling rate providers
// in Balancer V3 WITH_RATE token pools. Must be called before Get() for those pools.
func (pm *PoolManager) SetEVMCaller(fn EVMCaller) {
	pm.evmCaller = fn
}

// SetBlockTimestamp sets the block timestamp used for LFJ V2 volatility reference
// updates. Updates both the PoolManager's default (for new pool construction) and
// all cached LFJ V2 pool structs (no rebuild needed — just updates the field).
func (pm *PoolManager) SetBlockTimestamp(ts uint64) {
	pm.blockTimestamp = ts
	for _, q := range pm.pools {
		// Unwrap fotPoolQuoter if present
		inner := q
		if fot, ok := inner.(*fotPoolQuoter); ok {
			inner = fot.inner
		}
		if lfj, ok := inner.(*LFJV2Pool); ok {
			lfj.SetBlockTimestamp(ts)
		}
	}
}

// SetPoolType registers the pool type and DEX provider for a pool (from pools.txt).
// DEX name is used to dispatch V2 variants (hurricane, fraxswap) to correct constructors.
func (pm *PoolManager) SetPoolType(pool common.Address, poolType int, dex string) {
	pm.poolTypes[pool] = poolType
	pm.poolDex[pool] = dex
}

// SetPoolTokens registers the token pair for a pool, enabling FoT adjustment.
func (pm *PoolManager) SetPoolTokens(pool common.Address, token0, token1 common.Address) {
	pm.poolTokens[pool] = [2]common.Address{token0, token1}
}

// Get returns a PoolQuoter for the given pool, building it if needed.
// Never returns nil — unknown/blacklisted pools get a zeroQuoter.
func (pm *PoolManager) Get(pool common.Address) PoolQuoter {
	if q, ok := pm.pools[pool]; ok {
		return q
	}

	formulaID, known := pm.registry.GetFormulaID(pool)
	if !known || formulaID < 0 {
		z := &zeroQuoter{addr: pool}
		pm.pools[pool] = z
		return z
	}

	result := pm.buildQuoter(pool, formulaID)
	if result == nil {
		z := &zeroQuoter{addr: pool}
		pm.pools[pool] = z
		return z
	}
	return result
}

// Quote returns the output for a pool swap, using both pool cache and quote cache.
// The pool struct is lazily built and cached. Quote results are cached in a map
// per pool (skipped for LFJ V2 which is time-dependent).
func (pm *PoolManager) Quote(pool common.Address, amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	// Check quote cache (read lock)
	if !pm.noQuoteCache[pool] {
		pm.cacheMu.RLock()
		if cache, ok := pm.quoteCaches[pool]; ok {
			if out, hit := cache.Lookup(amountIn, zeroForOne); hit {
				pm.cacheMu.RUnlock()
				return out
			}
		}
		pm.cacheMu.RUnlock()
	}

	// Get/build pool struct (pool cache)
	quoter := pm.Get(pool)
	out := quoter.Quote(amountIn, zeroForOne)

	// Balance cap: if output exceeds the pool's actual balance of the output token,
	// the on-chain transfer would revert. Return zero.
	if !out.IsZero() && pm.evmCaller != nil {
		if tokens, ok := pm.poolTokens[pool]; ok {
			pm.cacheMu.RLock()
			balances, cached := pm.balanceCache[pool]
			pm.cacheMu.RUnlock()
			if !cached {
				balances[0] = pm.readBalanceOf(tokens[0], pool)
				balances[1] = pm.readBalanceOf(tokens[1], pool)
				pm.cacheMu.Lock()
				pm.balanceCache[pool] = balances
				pm.cacheMu.Unlock()
			}
			balIdx := 1
			if !zeroForOne {
				balIdx = 0
			}
			if !balances[balIdx].IsZero() && out.Gt(&balances[balIdx]) {
				out = uint256.Int{}
			}
		}
	}

	// Store in quote cache (write lock)
	if !pm.noQuoteCache[pool] {
		pm.cacheMu.Lock()
		cache := pm.quoteCaches[pool]
		if cache == nil {
			cache = &QuoteCache{}
			pm.quoteCaches[pool] = cache
		}
		cache.Store(amountIn, zeroForOne, out)
		pm.cacheMu.Unlock()
	}

	return out
}

// readBalanceOf calls balanceOf(holder) on a token via evmCaller.
func (pm *PoolManager) readBalanceOf(token, holder common.Address) uint256.Int {
	// balanceOf(address) selector = 0x70a08231
	var calldata [36]byte
	calldata[0], calldata[1], calldata[2], calldata[3] = 0x70, 0xa0, 0x82, 0x31
	copy(calldata[16:36], holder[:])
	ret, ok := pm.evmCaller(token, calldata[:])
	if ok && len(ret) >= 32 {
		var bal uint256.Int
		bal.SetBytes(ret[:32])
		return bal
	}
	return uint256.Int{}
}

// QuoteBypassQuoteCache uses the pool cache but skips the quote cache.
// Use in benchmarks to measure actual formula speed.
func (pm *PoolManager) QuoteBypassQuoteCache(pool common.Address, amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	return pm.Get(pool).Quote(amountIn, zeroForOne)
}

// BuildQuoterForFormulaID builds a PoolQuoter for a pool using the given formula ID,
// bypassing the registry. Used by discover to test candidate formulas before they
// are registered. The quoter is NOT cached — each call builds a fresh quoter.
func (pm *PoolManager) BuildQuoterForFormulaID(pool common.Address, formulaID int) PoolQuoter {
	pm.Invalidate(pool) // clear any cached quoter
	return pm.buildQuoter(pool, formulaID)
}

func (pm *PoolManager) buildQuoter(pool common.Address, formulaID int) (pq PoolQuoter) {
	tokens, hasTokens := pm.poolTokens[pool]

	// Recover from panics during construction
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "V3DEBUG: PANIC building pool %s fid=%d: %v\n", pool.Hex(), formulaID, r)
			pq = nil
		}
	}()

	// Slot-tracking reader: records (addr, slot) accessed during construction
	type slotAccess struct {
		addr common.Address
		slot common.Hash
	}
	var accessed []slotAccess
	trackedReader := func(addr common.Address, slot common.Hash) common.Hash {
		accessed = append(accessed, slotAccess{addr, slot})
		return pm.reader(addr, slot)
	}

	// Build inner pool struct (concrete type checks to avoid nil-interface issue)
	poolExempt := hasTokens && IsFotExemptPool(strings.ToLower(pool.Hex()))
	var model0, model1 TokenModel
	if hasTokens && !poolExempt {
		t0Hex := strings.ToLower(tokens[0].Hex())
		t1Hex := strings.ToLower(tokens[1].Hex())
		model0 = pm.tokenModels.GetModel(t0Hex)
		model1 = pm.tokenModels.GetModel(t1Hex)
	}
	wantFot := model0 != nil && model1 != nil && (model0.IsFoT() || model1.IsFoT())

	registerSlots := func() {
		for _, a := range accessed {
			m := pm.depSlots[a.addr]
			if m == nil {
				m = make(map[common.Hash]common.Address)
				pm.depSlots[a.addr] = m
			}
			m[a.slot] = pool
		}
	}

	wrapAndCache := func(inner PoolQuoter) PoolQuoter {
		registerSlots()
		// Wrap with dead direction check for broken tokens (generic, all pool types)
		dead0, dead1 := false, false
		if hasTokens {
			dead0 = brokenTokens[tokens[0]]
			dead1 = brokenTokens[tokens[1]]
		}
		// Pool-specific dead directions override token-level checks
		if dir, ok := deadPoolDirs[pool]; ok {
			if dir == 0 {
				dead0 = true
			} else {
				dead1 = true
			}
		}
		if dead0 || dead1 {
			inner = &deadDirQuoter{inner: inner, deadDir0: dead0, deadDir1: dead1}
		}
		if wantFot {
			poolHex := strings.ToLower(pool.Hex())
			wrapped := &fotPoolQuoter{
				inner:         inner,
				model0:        model0,
				model1:        model1,
				inputExempt:   IsFotExemptInputPool(poolHex),
				outputExempt:  IsFotExemptOutputPool(poolHex),
			}
			pm.pools[pool] = wrapped
			return wrapped
		}
		pm.pools[pool] = inner
		return inner
	}

	switch formulaID {
	case FormulaV2_30bps:
		var p *V2Pool
		switch pm.poolDex[pool] {
		case "hurricane":
			p = newHurricanePool(pool, trackedReader)
		case "fraxswap":
			p = newFraxswapPool(pool, trackedReader)
		default:
			p = newV2Pool(pool, trackedReader)
		}
		if p != nil {
			if hasTokens {
				p.SetTokenBalances(pm.evmCaller, tokens[0], tokens[1])
				p.SetDeadDirs(tokens[0], tokens[1])
			}
			return wrapAndCache(p)
		}
	case FormulaPharaohV1:
		if p := newPharaohV1Pool(pool, trackedReader); p != nil { return wrapAndCache(p) }
	case FormulaV3:
		if p := newV3Pool(pool, trackedReader); p != nil { return wrapAndCache(p) }
	case FormulaDODO:
		var token0 common.Address
		if hasTokens {
			token0 = tokens[0]
		}
		if p := newDODOPool(pool, trackedReader, token0); p != nil { return wrapAndCache(p) }
	case FormulaLFJV2:
		pm.noQuoteCache[pool] = true // time-dependent: skip quote cache
		if hasTokens {
			return wrapAndCache(newLFJV2Pool(pool, trackedReader, tokens[0], tokens[1], pm.blockTimestamp, pm.evmCaller))
		}
	case FormulaV4:
		if p := newV4Pool(pool, trackedReader); p != nil { return wrapAndCache(p) }
	case FormulaBalancerV3:
		if p := newBalancerV3Pool(pool, trackedReader, pm.evmCaller); p != nil { return wrapAndCache(p) }
	case FormulaBalancerV2:
		if p := newBalancerV2Pool(pool, trackedReader); p != nil { return wrapAndCache(p) }
	case FormulaAlgebra:
		if p := newAlgebraPool(pool, trackedReader); p != nil { return wrapAndCache(p) }
	}
	return nil
}

// Invalidate drops the cached pool struct for a given address.
// Also cleans up depSlots entries that point to this pool.
func (pm *PoolManager) Invalidate(addr common.Address) {
	delete(pm.pools, addr)
	pm.cacheMu.Lock()
	delete(pm.quoteCaches, addr)
	delete(pm.balanceCache, addr)
	pm.cacheMu.Unlock()
	// Clean up depSlots: remove entries pointing to this pool.
	// On next Get(), buildQuoter will re-record the slots.
	for contract, slots := range pm.depSlots {
		for slot, poolAddr := range slots {
			if poolAddr == addr {
				delete(slots, slot)
			}
		}
		if len(slots) == 0 {
			delete(pm.depSlots, contract)
		}
	}
}

// InvalidateBySlot invalidates the pool that depends on a specific (contract, slot).
// Returns the invalidated pool address, or zero if no pool was affected.
func (pm *PoolManager) InvalidateBySlot(contractAddr common.Address, slot common.Hash) common.Address {
	slots, ok := pm.depSlots[contractAddr]
	if !ok {
		return common.Address{}
	}
	poolAddr, ok := slots[slot]
	if !ok {
		return common.Address{}
	}
	delete(pm.pools, poolAddr)
	pm.cacheMu.Lock()
	delete(pm.quoteCaches, poolAddr)
	delete(pm.balanceCache, poolAddr)
	pm.cacheMu.Unlock()
	// Remove all depSlots entries for this pool so they get re-recorded on rebuild
	for c, s := range pm.depSlots {
		for sl, pa := range s {
			if pa == poolAddr {
				delete(s, sl)
			}
		}
		if len(s) == 0 {
			delete(pm.depSlots, c)
		}
	}
	return poolAddr
}

// poolHex returns the lowercase hex string for a pool address.
func poolHex(addr common.Address) string {
	return strings.ToLower(addr.Hex())
}

// deadPoolDirs lists pools where one direction reverts on-chain but the formula computes
// a value. Key = pool address, value = direction to block (0 = block zeroForOne, 1 = block !zeroForOne).
var deadPoolDirs = map[common.Address]int{
	common.HexToAddress("0xd446eb1660f766d533beceef890df7a69d26f7d1"): 1, // WAVAX/USDC LFJ V2: dir=1 (USDC→WAVAX) reverts on-chain
	common.HexToAddress("0x55c211bbe9f63059a4a5a5e0c558c7e410412d98"): 0, // BTC.b/SolvBTC LFJ V2: dir=0 (BTC.b→SolvBTC) reverts on-chain
	common.HexToAddress("0x41100c6d2c6920b10d12cd8d59c8a9aa2ef56fc7"): 1, // WAVAX/USDC Algebra: dir=1 exceeds gas limit on-chain
	common.HexToAddress("0x668aa7aefa8512416fc6244afbe5129200277a69"): 1, // WAVAX/USDC Algebra: dir=1 exceeds gas limit on-chain
	common.HexToAddress("0x4e0364a85f084b65a61a0e7d2d217fcbe958f9a1"): 1, // BIFI/waAvaWAVAX BalancerV3: dir=1 extreme imbalance causes EVM revert
}

// deadDirQuoter wraps a PoolQuoter to block directions where a broken input token
// causes on-chain reverts. Generic version of V2Pool's deadDir flags — works for all pool types.
type deadDirQuoter struct {
	inner    PoolQuoter
	deadDir0 bool // true = zeroForOne always returns 0 (token0 as input reverts)
	deadDir1 bool // true = !zeroForOne always returns 0 (token1 as input reverts)
}

func (d *deadDirQuoter) Address() common.Address { return d.inner.Address() }
func (d *deadDirQuoter) Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	if zeroForOne && d.deadDir0 {
		return uint256.Int{}
	}
	if !zeroForOne && d.deadDir1 {
		return uint256.Int{}
	}
	return d.inner.Quote(amountIn, zeroForOne)
}

// fotPoolQuoter wraps a PoolQuoter to apply FoT tax adjustments on input/output.
type fotPoolQuoter struct {
	inner          PoolQuoter
	model0         TokenModel // token0's model
	model1         TokenModel // token1's model
	inputExempt    bool       // true if pool is in FotExemptInputPools (fee skipped when pool is recipient)
	outputExempt   bool       // true if pool is in FotExemptOutputPools (fee skipped when pool is sender)
}

func (f *fotPoolQuoter) Address() common.Address {
	return f.inner.Address()
}

func (f *fotPoolQuoter) Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	var modelIn, modelOut TokenModel
	if zeroForOne {
		modelIn = f.model0
		modelOut = f.model1
	} else {
		modelIn = f.model1
		modelOut = f.model0
	}

	// Adjust input: if tokenIn is FoT, pool receives less.
	// Skip if this pool is exempt on the input side (token's transfer skips fee when to==pool).
	var effectiveIn uint256.Int
	if f.inputExempt {
		effectiveIn = *amountIn
	} else {
		effectiveIn = modelIn.AdjustInput(amountIn)
		if effectiveIn.IsZero() {
			return uint256.Int{}
		}
	}

	out := f.inner.Quote(&effectiveIn, zeroForOne)
	if out.IsZero() {
		return uint256.Int{}
	}

	// Adjust output: if tokenOut is FoT, user receives less.
	// Skip if this pool is exempt on the output side (token skips fee when pool is sender).
	if !f.outputExempt {
		out = modelOut.AdjustOutput(&out)
	}

	return out
}

