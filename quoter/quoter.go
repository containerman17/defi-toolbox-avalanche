package quoter

import (
	"defi-toolbox/formulas"
	lc "defi-toolbox/lightclient"
	pf "defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/tools/pool-collector"

	router "defi-toolbox/contracts"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

const (
	basePoolCount      = 4000
	extraPoolsPerToken = 5
)

// Quoter finds optimal swap routes using formula-based BFS over DEX pools.
// State is provided per-call via lightclient.StateView — the quoter itself
// holds only the static pool graph and registry.
type Quoter struct {
	pools      []pf.Pool
	adj        map[common.Address][]pf.PoolEdge
	extraPools map[common.Address][]uint16 // token → pool indices beyond base cut
	activated  map[uint16]bool             // extra pools already added to adj
	registry   *formulas.Registry
	Router     common.Address
	MaxHops    int
}

// Quote is the result of a pathfinding search.
type Quote struct {
	Route    *pf.Route
	Split    *pf.SplitResult // non-nil when volume is split across multiple routes
	Block    uint64
	BaseFee  uint64
	AmountIn *uint256.Int
	TokenIn  common.Address
	TokenOut common.Address
}

// New creates a quoter with the embedded pool list and formula registry.
// The base adjacency graph uses the top 4000 most active pools.
// Per-query, up to 5 additional pools are activated for each query token
// so that low-activity tokens still have BFS edges.
func New(maxHops int) *Quoter {
	if maxHops <= 0 || maxHops > 4 {
		maxHops = 4
	}
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(0) // all pools

	baseCut := basePoolCount
	if baseCut > len(pools) {
		baseCut = len(pools)
	}
	adj := pf.BuildAdjacency(pools[:baseCut], registry)

	// Index extra pools (beyond base cut) by token, filtered to those with formulas
	extraPools := make(map[common.Address][]uint16)
	for i := baseCut; i < len(pools); i++ {
		p := &pools[i]
		if _, known := registry.GetFormulaID(p.Address); !known {
			continue
		}
		idx := uint16(i)
		for _, tok := range p.Tokens {
			extraPools[tok] = append(extraPools[tok], idx)
		}
	}

	return &Quoter{
		pools:      pools,
		adj:        adj,
		extraPools: extraPools,
		activated:  make(map[uint16]bool),
		registry:   registry,
		Router:     router.DeployedRouter,
		MaxHops:    maxHops,
	}
}

// ensureTokenPools activates up to extraPoolsPerToken additional pools
// for the given tokens from outside the base top-4000 set.
func (q *Quoter) ensureTokenPools(tokens ...common.Address) {
	for _, tok := range tokens {
		extras := q.extraPools[tok]
		if len(extras) == 0 {
			continue
		}
		limit := extraPoolsPerToken
		if limit > len(extras) {
			limit = len(extras)
		}
		for _, idx := range extras[:limit] {
			if q.activated[idx] {
				continue
			}
			q.activated[idx] = true
			p := &q.pools[idx]
			for ti := range p.Tokens {
				for tj := range p.Tokens {
					if ti != tj {
						q.adj[p.Tokens[ti]] = append(q.adj[p.Tokens[ti]], pf.PoolEdge{
							PoolIdx:  idx,
							TokenOut: p.Tokens[tj],
							TokenIn:  p.Tokens[ti],
						})
					}
				}
			}
		}
	}
}

// Quote finds the best cyclic route: tokenIn → ... → tokenIn (arbitrage).
// The StateView is read-only — the quoter reads storage slots via the
// light client's VersionedState, transparently fetching from RPC on miss.
func (q *Quoter) Quote(sv *lc.StateView, blockTimestamp uint64, tokenIn common.Address, amountIn *uint256.Int) *Quote {
	q.ensureTokenPools(tokenIn)
	pm := q.buildPM(sv, blockTimestamp)

	route := pf.FindBestFormulaRoute(
		pm, q.adj, q.pools,
		tokenIn, tokenIn, // cyclic: same token in and out
		amountIn,
		q.MaxHops,
		pf.FormulaSearchOptions{},
	)

	return &Quote{
		Route:    route,
		Block:    sv.Block(),
		AmountIn: amountIn,
		TokenIn:  tokenIn,
		TokenOut: tokenIn,
	}
}

// QuotePair finds the best route from tokenIn to tokenOut (non-cyclic).
// Uses elimination search to discover up to 4 diverse routes, then splits
// volume across them to maximize total output.
func (q *Quoter) QuotePair(sv *lc.StateView, blockTimestamp uint64, tokenIn, tokenOut common.Address, amountIn *uint256.Int) *Quote {
	q.ensureTokenPools(tokenIn, tokenOut)
	pm := q.buildPM(sv, blockTimestamp)

	routes := pf.FindRoutesElimination(
		pm, q.adj, q.pools,
		tokenIn, tokenOut,
		amountIn,
		q.MaxHops,
		8,
	)
	if len(routes) == 0 {
		return &Quote{
			Block:    sv.Block(),
			AmountIn: amountIn,
			TokenIn:  tokenIn,
			TokenOut: tokenOut,
		}
	}

	split := pf.OptimalSplit(pm, routes, amountIn)

	return &Quote{
		Route:    routes[0],
		Split:    split,
		Block:    sv.Block(),
		AmountIn: amountIn,
		TokenIn:  tokenIn,
		TokenOut: tokenOut,
	}
}

// Pools returns the pool list.
func (q *Quoter) Pools() []pf.Pool { return q.pools }

// Adj returns the adjacency graph.
func (q *Quoter) Adj() map[common.Address][]pf.PoolEdge { return q.adj }

func (q *Quoter) buildPM(sv *lc.StateView, blockTimestamp uint64) *formulas.PoolManager {
	reader := func(addr common.Address, slot common.Hash) common.Hash {
		return sv.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(q.registry, reader)
	for i := range q.pools {
		p := &q.pools[i]
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens...)
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	pm.SetBlockTimestamp(blockTimestamp)
	return pm
}
