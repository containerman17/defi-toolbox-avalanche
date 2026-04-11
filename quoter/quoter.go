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

// Quoter finds optimal swap routes using formula-based BFS over DEX pools.
// State is provided per-call via lightclient.StateView — the quoter itself
// holds only the static pool graph and registry.
type Quoter struct {
	pools    []pf.Pool
	adj      map[common.Address][]pf.PoolEdge
	registry *formulas.Registry
	Router   common.Address
	MaxHops  int
}

// Quote is the result of a pathfinding search.
type Quote struct {
	Route    *pf.Route
	Block    uint64
	BaseFee  uint64
	AmountIn *uint256.Int
	TokenIn  common.Address
	TokenOut common.Address
}

// New creates a quoter with the embedded pool list and formula registry.
func New(maxHops int) *Quoter {
	if maxHops <= 0 || maxHops > 4 {
		maxHops = 4
	}
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(0) // 0 = all pools
	adj := pf.BuildAdjacency(pools, registry)

	return &Quoter{
		pools:    pools,
		adj:      adj,
		registry: registry,
		Router:   router.DeployedRouter,
		MaxHops:  maxHops,
	}
}

// Quote finds the best cyclic route: tokenIn → ... → tokenIn (arbitrage).
// The StateView is read-only — the quoter reads storage slots via the
// light client's VersionedState, transparently fetching from RPC on miss.
func (q *Quoter) Quote(sv *lc.StateView, blockTimestamp uint64, tokenIn common.Address, amountIn *uint256.Int) *Quote {
	pm := q.buildPM(sv, blockTimestamp)

	route := pf.FindBestFormulaRoute(
		pm, q.adj, q.pools,
		tokenIn, tokenIn, // cyclic: same token in and out
		amountIn,
		q.MaxHops,
		pf.FormulaSearchOptions{BeamWidth: 3},
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
func (q *Quoter) QuotePair(sv *lc.StateView, blockTimestamp uint64, tokenIn, tokenOut common.Address, amountIn *uint256.Int) *Quote {
	pm := q.buildPM(sv, blockTimestamp)

	route := pf.FindBestFormulaRoute(
		pm, q.adj, q.pools,
		tokenIn, tokenOut,
		amountIn,
		q.MaxHops,
		pf.FormulaSearchOptions{BeamWidth: 3},
	)

	return &Quote{
		Route:    route,
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
