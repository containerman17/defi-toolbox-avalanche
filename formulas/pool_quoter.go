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
	// tokenIn/tokenOut identify the swap direction (supports N-token pools).
	Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int

	// Address returns the pool's contract address.
	Address() common.Address
}

// PoolQuoterSource is the interface consumed by pathfinder BFS.
// Both PoolManager and PoolManagerOverlay implement it.
type PoolQuoterSource interface {
	Quote(pool common.Address, amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int
}

// zeroQuoter is a PoolQuoter that always returns zero.
// Used for blacklisted/unknown pools so Get() never returns nil.
type zeroQuoter struct{ addr common.Address }

func (z *zeroQuoter) Address() common.Address { return z.addr }
func (z *zeroQuoter) Quote(_ *uint256.Int, _, _ common.Address) uint256.Int {
	return uint256.Int{}
}

// EVMCaller executes a view call against the current EVM state.
// Returns (result, true) on success, (nil, false) on revert or error.
type EVMCaller func(to common.Address, data []byte) ([]byte, bool)

// quoteCacheKey identifies a unique (amountIn, tokenIn, tokenOut) triple.
type quoteCacheKey struct {
	amountIn uint256.Int
	tokenIn  common.Address
	tokenOut common.Address
}

// QuoteCache is a map-based cache for quote results per pool.
// Invalidated on block updates (pool state change).
type QuoteCache struct {
	entries map[quoteCacheKey]uint256.Int
}

func (c *QuoteCache) Lookup(amountIn *uint256.Int, tokenIn, tokenOut common.Address) (out uint256.Int, hit bool) {
	key := quoteCacheKey{amountIn: *amountIn, tokenIn: tokenIn, tokenOut: tokenOut}
	out, hit = c.entries[key]
	return
}

func (c *QuoteCache) Store(amountIn *uint256.Int, tokenIn, tokenOut common.Address, out uint256.Int) {
	key := quoteCacheKey{amountIn: *amountIn, tokenIn: tokenIn, tokenOut: tokenOut}
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
	poolTokens     map[common.Address][]common.Address // pool → tokens (sorted by address)
	poolTypes      map[common.Address]int               // pool → poolType from pools.txt
	poolDex        map[common.Address]string            // pool → DEX provider name (e.g. "pangolin_v2")
	tokenModels    *TokenModelRegistry
	blockTimestamp uint64 // block.timestamp for volatility reference updates (LFJ V2)

	// depSlots: reverse map from (contractAddr, slot) → pool addresses for cache busting.
	// Multiple pools may read the same slot (e.g. Balancer vault).
	depSlots map[common.Address]map[common.Hash][]common.Address

	// Quote cache: 16-slot ring buffer per pool. Skipped for LFJ V2 (time-dependent).
	quoteCaches  map[common.Address]*QuoteCache
	noQuoteCache map[common.Address]bool // true for LFJ V2 pools

	// Balance cache: pool token balances for the reserve cap check.
	// Key: pool address, value: balances (one per token). Invalidated with pool.
	balanceCache map[common.Address][]uint256.Int

	// Mutex for thread-safe cache access (quoteCaches + balanceCache).
	cacheMu sync.RWMutex
}

// NewPoolManager creates a PoolManager backed by the given registry and storage reader.
func NewPoolManager(registry *Registry, reader StorageReader) *PoolManager {
	return &PoolManager{
		pools:        make(map[common.Address]PoolQuoter),
		registry:     registry,
		reader:       reader,
		poolTokens:   make(map[common.Address][]common.Address),
		poolTypes:    make(map[common.Address]int),
		poolDex:      make(map[common.Address]string),
		tokenModels:  NewTokenModelRegistry(reader),
		depSlots:     make(map[common.Address]map[common.Hash][]common.Address),
		quoteCaches:  make(map[common.Address]*QuoteCache),
		noQuoteCache: make(map[common.Address]bool),
		balanceCache: make(map[common.Address][]uint256.Int),
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
	if ts == pm.blockTimestamp {
		return
	}
	pm.blockTimestamp = ts
	for addr, q := range pm.pools {
		// Unwrap fotPoolQuoter if present
		inner := q
		if fot, ok := inner.(*fotPoolQuoter); ok {
			inner = fot.inner
		}
		if lfj, ok := inner.(*LFJV2Pool); ok {
			lfj.SetBlockTimestamp(ts)
			// Timestamp changed → flush quote cache for this pool.
			// The pool struct is updated in-place (no rebuild needed),
			// but cached quote results are stale at the new timestamp.
			pm.cacheMu.Lock()
			delete(pm.quoteCaches, addr)
			pm.cacheMu.Unlock()
		}
	}
}

// SetPoolType registers the pool type and DEX provider for a pool (from pools.txt).
// DEX name is used to dispatch V2 variants (hurricane, fraxswap) to correct constructors.
func (pm *PoolManager) SetPoolType(pool common.Address, poolType int, dex string) {
	pm.poolTypes[pool] = poolType
	pm.poolDex[pool] = dex
}

// SetPoolTokens registers the tokens for a pool, enabling FoT adjustment and balance cap.
func (pm *PoolManager) SetPoolTokens(pool common.Address, tokens ...common.Address) {
	pm.poolTokens[pool] = tokens
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
func (pm *PoolManager) Quote(pool common.Address, amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	// Check quote cache (read lock)
	if !pm.noQuoteCache[pool] {
		pm.cacheMu.RLock()
		if cache, ok := pm.quoteCaches[pool]; ok {
			if out, hit := cache.Lookup(amountIn, tokenIn, tokenOut); hit {
				pm.cacheMu.RUnlock()
				return out
			}
		}
		pm.cacheMu.RUnlock()
	}

	// Get/build pool struct (pool cache)
	quoter := pm.Get(pool)
	out := quoter.Quote(amountIn, tokenIn, tokenOut)

	// Balance cap: if output exceeds the pool's actual balance of the output token,
	// the on-chain transfer would revert. Return zero.
	if !out.IsZero() && pm.evmCaller != nil {
		if tokens, ok := pm.poolTokens[pool]; ok {
			pm.cacheMu.RLock()
			balances, cached := pm.balanceCache[pool]
			pm.cacheMu.RUnlock()
			if !cached {
				balances = make([]uint256.Int, len(tokens))
				for i, tok := range tokens {
					balances[i] = pm.readBalanceOf(tok, pool)
				}
				pm.cacheMu.Lock()
				pm.balanceCache[pool] = balances
				pm.cacheMu.Unlock()
			}
			for i, tok := range tokens {
				if tok == tokenOut {
					if !balances[i].IsZero() && out.Gt(&balances[i]) {
						out = uint256.Int{}
					}
					break
				}
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
		cache.Store(amountIn, tokenIn, tokenOut, out)
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
func (pm *PoolManager) QuoteBypassQuoteCache(pool common.Address, amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	return pm.Get(pool).Quote(amountIn, tokenIn, tokenOut)
}

// BuildQuoterForFormulaID builds a PoolQuoter for a pool using the given formula ID,
// bypassing the registry. Used by discover to test candidate formulas before they
// are registered. The quoter is NOT cached — each call builds a fresh quoter.
func (pm *PoolManager) BuildQuoterForFormulaID(pool common.Address, formulaID int) PoolQuoter {
	pm.Invalidate(pool) // clear any cached quoter
	return pm.buildQuoter(pool, formulaID)
}

func (pm *PoolManager) buildQuoter(pool common.Address, formulaID int) (pq PoolQuoter) {
	tokens := pm.poolTokens[pool]
	hasTokens := len(tokens) >= 2

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
	var fotModels map[common.Address]TokenModel
	wantFot := false
	if hasTokens && !poolExempt {
		fotModels = make(map[common.Address]TokenModel, len(tokens))
		anyFoT := false
		allHaveModel := true
		poolHexLower := strings.ToLower(pool.Hex())
		for _, tok := range tokens {
			tokHexLower := strings.ToLower(tok.Hex())
			// Check per-pool token override first
			if overrideCalc, ok := FotPoolTokenOverrides[fotPoolTokenKey{Pool: poolHexLower, Token: tokHexLower}]; ok {
				m := &fotTokenModel{calcFee: overrideCalc.calcFee, calcReceived: overrideCalc.calcReceived}
				fotModels[tok] = m
				anyFoT = true
				continue
			}
			m := pm.tokenModels.GetModel(tokHexLower)
			if m == nil {
				allHaveModel = false
				break
			}
			fotModels[tok] = m
			if m.IsFoT() {
				anyFoT = true
			}
		}
		wantFot = allHaveModel && anyFoT
	}

	registerSlots := func() {
		for _, a := range accessed {
			m := pm.depSlots[a.addr]
			if m == nil {
				m = make(map[common.Hash][]common.Address)
				pm.depSlots[a.addr] = m
			}
			// Append pool if not already present
			pools := m[a.slot]
			found := false
			for _, p := range pools {
				if p == pool {
					found = true
					break
				}
			}
			if !found {
				m[a.slot] = append(pools, pool)
			}
		}
	}

	wrapAndCache := func(inner PoolQuoter) PoolQuoter {
		registerSlots()
		// Wrap with dead direction check for broken tokens (generic, all pool types)
		var deadTokens map[common.Address]bool
		if hasTokens {
			for _, tok := range tokens {
				if brokenTokens[tok] {
					if deadTokens == nil {
						deadTokens = make(map[common.Address]bool)
					}
					deadTokens[tok] = true
				}
			}
		}
		// Pool-specific dead directions override token-level checks
		if deadTok, ok := deadPoolDirs[pool]; ok {
			if deadTokens == nil {
				deadTokens = make(map[common.Address]bool)
			}
			deadTokens[deadTok] = true
		}
		if len(deadTokens) > 0 {
			inner = &deadDirQuoter{inner: inner, deadTokens: deadTokens}
		}
		if wantFot {
			poolHex := strings.ToLower(pool.Hex())
			wrapped := &fotPoolQuoter{
				inner:        inner,
				models:       fotModels,
				inputExempt:  IsFotExemptInputPool(poolHex),
				outputExempt: IsFotExemptOutputPool(poolHex),
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
		// Quote cache is now safe: SetBlockTimestamp flushes LFJ V2 caches
		// when the timestamp changes. Within a block, quotes are deterministic.
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
	case FormulaWombat:
		if hasTokens {
			if p := newWombatPool(pool, trackedReader, tokens, pm.evmCaller); p != nil { return wrapAndCache(p) }
		}
	case FormulaPlatypus:
		if hasTokens {
			if p := newPlatypusPool(pool, trackedReader, tokens, pm.evmCaller); p != nil { return wrapAndCache(p) }
		}
	case FormulaWooFi:
		if p := newWooFiPool(pool, trackedReader); p != nil { return wrapAndCache(p) }
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
	// Clean up depSlots: remove this pool from all slot slices.
	// On next Get(), buildQuoter will re-record the slots.
	for contract, slots := range pm.depSlots {
		for slot, poolAddrs := range slots {
			filtered := poolAddrs[:0]
			for _, pa := range poolAddrs {
				if pa != addr {
					filtered = append(filtered, pa)
				}
			}
			if len(filtered) == 0 {
				delete(slots, slot)
			} else {
				slots[slot] = filtered
			}
		}
		if len(slots) == 0 {
			delete(pm.depSlots, contract)
		}
	}
}

// InvalidateBySlot invalidates all pools that depend on a specific (contract, slot).
// Returns the invalidated pool addresses.
func (pm *PoolManager) InvalidateBySlot(contractAddr common.Address, slot common.Hash) []common.Address {
	slots, ok := pm.depSlots[contractAddr]
	if !ok {
		return nil
	}
	poolAddrs, ok := slots[slot]
	if !ok || len(poolAddrs) == 0 {
		return nil
	}
	// Copy slice — Invalidate modifies depSlots
	addrs := make([]common.Address, len(poolAddrs))
	copy(addrs, poolAddrs)
	for _, addr := range addrs {
		pm.Invalidate(addr)
	}
	return addrs
}

// poolHex returns the lowercase hex string for a pool address.
func poolHex(addr common.Address) string {
	return strings.ToLower(addr.Hex())
}

// deadPoolDirs lists pools where swapping with a specific input token reverts on-chain
// but the formula computes a value. Key = pool address, value = dead input token address.
var deadPoolDirs = map[common.Address]common.Address{
	common.HexToAddress("0x4e0364a85f084b65a61a0e7d2d217fcbe958f9a1"): common.HexToAddress("0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b"), // BIFI/waAvaWAVAX BalancerV3: waAvaWAVAX as input, extreme imbalance
	common.HexToAddress("0x9ba9c677d19347abfba1d6b6d6ceb61942071561"): common.HexToAddress("0x38f9bf9dce51833ec7f03c9dc218197999999999"), // NYA/WAVAX UniV3: NYA as input reverts (paused)
	common.HexToAddress("0x4c79e30bc8eb6d83620b6166e49a27615eeed221"): common.HexToAddress("0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"), // Gladiator/USDC: USDC→Gladiator hits max_wallet
	common.HexToAddress("0xdf9db5a5f3a00e0e27def12af95b4528ec23cf86"): common.HexToAddress("0xc139aa91399600f6b72975ac3317b6d49cb30a69"), // Gladiator/Arena: Arena→Gladiator hits max_wallet
	common.HexToAddress("0x54ba397425a60361c684aeae22f2c7c78b24cf90"): common.HexToAddress("0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"), // V4: USDt as input reverts in this pool
	common.HexToAddress("0x5c2e48c07f27f6250e7a1709d12d01b6b92205ba"): common.HexToAddress("0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab"), // PharaohV1: WETH.e as input reverts (one-sided)
	common.HexToAddress("0x04d479a32d5941f2329c307ee906e36753f0e9f0"): common.HexToAddress("0xd698aeab9286a38d14894b6ccf5129660fffc6f3"), // V4 ArenaHook: hook reverts for this token
	common.HexToAddress("0x70201236b99f79392b877e760898061917796aeb"): common.HexToAddress("0xf84be5e3f534e6d4b60d104b299e33ecb03ce7fd"), // SOCK/WAVAX lfj_v1: SOCK._transfer triggers attemptFeeSwap through same pair, stale reserves cause K failure
}

// deadDirQuoter wraps a PoolQuoter to block directions where a broken token
// causes on-chain reverts. Checks both input and output: a token whose transfer()
// always reverts will cause reverts whether it's being sent in or sent out.
type deadDirQuoter struct {
	inner      PoolQuoter
	deadTokens map[common.Address]bool // tokens that revert when used as input or output
}

func (d *deadDirQuoter) Address() common.Address { return d.inner.Address() }
func (d *deadDirQuoter) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if d.deadTokens[tokenIn] || d.deadTokens[tokenOut] {
		return uint256.Int{}
	}
	return d.inner.Quote(amountIn, tokenIn, tokenOut)
}

// fotPoolQuoter wraps a PoolQuoter to apply FoT tax adjustments on input/output.
type fotPoolQuoter struct {
	inner        PoolQuoter
	models       map[common.Address]TokenModel // token address → model
	inputExempt  bool                          // true if pool is in FotExemptInputPools
	outputExempt bool                          // true if pool is in FotExemptOutputPools
}

func (f *fotPoolQuoter) Address() common.Address {
	return f.inner.Address()
}

func (f *fotPoolQuoter) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	modelIn := f.models[tokenIn]
	modelOut := f.models[tokenOut]
	if modelIn == nil || modelOut == nil {
		return f.inner.Quote(amountIn, tokenIn, tokenOut)
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

	out := f.inner.Quote(&effectiveIn, tokenIn, tokenOut)
	if out.IsZero() {
		return uint256.Int{}
	}

	// Adjust output: if tokenOut is FoT, user receives less.
	// Skip if this pool is exempt on the output side (token skips fee when pool is sender).
	if !f.outputExempt {
		// For reflection tokens, the pool (sender) being excluded affects the
		// post-transfer rate. Use SenderAwareOutputAdjuster when available.
		if sa, ok := modelOut.(SenderAwareOutputAdjuster); ok {
			out = sa.AdjustOutputFromSender(&out, f.inner.Address())
		} else {
			out = modelOut.AdjustOutput(&out)
		}
	}

	return out
}

// ── Getters for overlay construction ────────────────────────────────

// DepSlots returns the reverse dependency map: (contractAddr, slot) → pool addresses.
func (pm *PoolManager) DepSlots() map[common.Address]map[common.Hash][]common.Address {
	return pm.depSlots
}

// Reader returns the storage reader function.
func (pm *PoolManager) Reader() StorageReader { return pm.reader }

// GetRegistry returns the formula registry.
func (pm *PoolManager) GetRegistry() *Registry { return pm.registry }

// EVMCallerFn returns the EVM caller function (may be nil).
func (pm *PoolManager) EVMCallerFn() EVMCaller { return pm.evmCaller }

// GetBlockTimestamp returns the current block timestamp.
func (pm *PoolManager) GetBlockTimestamp() uint64 { return pm.blockTimestamp }

// GetPoolTokens returns the registered tokens for a pool.
func (pm *PoolManager) GetPoolTokens(pool common.Address) []common.Address {
	return pm.poolTokens[pool]
}

// GetPoolTypeInfo returns (poolType, dex) for a pool.
func (pm *PoolManager) GetPoolTypeInfo(pool common.Address) (int, string) {
	return pm.poolTypes[pool], pm.poolDex[pool]
}

// IsNoQuoteCache returns true if the pool skips quote caching (e.g. LFJ V2).
func (pm *PoolManager) IsNoQuoteCache(pool common.Address) bool {
	return pm.noQuoteCache[pool]
}

