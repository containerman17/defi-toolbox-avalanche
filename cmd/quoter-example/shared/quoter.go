package shared

import (
	"fmt"
	"math/big"
	"strings"

	router "defi-toolbox/contracts"
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"
	poolcollector "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Quoter wraps the formula engine + EVM verification behind a simple Quote API.
type Quoter struct {
	ls         *statedb.LiveState
	pm         *formulas.PoolManager
	adj        map[common.Address][]pf.PoolEdge
	pools      []pf.Pool
	registry   *formulas.Registry
	routerAddr common.Address
	overrides  []pf.ParsedOverride
	maxHops    int
	dexMap     map[common.Address]string
}

// NewQuoter creates a Quoter connected to the given LiveState.
// Loads pools, builds adjacency, warms up formula quoters.
func NewQuoter(ls *statedb.LiveState, poolLimit, maxHops int) *Quoter {
	if maxHops <= 0 || maxHops > 4 {
		maxHops = 4
	}

	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)
	state := ls.State()

	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)

	dexMap := make(map[common.Address]string, len(pools))
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
		dexMap[p.Address] = p.Dex
	}
	pm.SetBlockTimestamp(ls.Timestamp())

	pm.SetEVMCaller(func(to common.Address, data []byte) ([]byte, bool) {
		cs := statedb.NewCallState(state)
		cfg := statedb.EVMConfig{
			BlockNumber: ls.Block(), Timestamp: ls.Timestamp(),
			ChainID: 43114, BaseFee: ls.BaseFee(), GasLimit: ls.GasLimit(),
		}
		ctx := statedb.GetCachedContext(cfg)
		ret, _, err := ctx.ExecuteWithCallState(cs, common.Address{}, to, data)
		return ret, err == nil
	})

	adj := pf.BuildAdjacency(pools, registry)
	routerAddr := router.DeployedRouter
	overrides := router.BuildTokenOverrides(routerAddr, pools)

	// Warmup: build all pool quoters
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	ls.RUnlock()

	return &Quoter{
		ls:         ls,
		pm:         pm,
		adj:        adj,
		pools:      pools,
		registry:   registry,
		routerAddr: routerAddr,
		overrides:  overrides,
		maxHops:    maxHops,
		dexMap:     dexMap,
	}
}

// StartBlockLoop runs the block invalidation loop in a goroutine.
// Call this once after NewQuoter.
func (q *Quoter) StartBlockLoop() {
	type blockEvent struct {
		timestamp uint64
		entries   [][2]string
	}
	blockCh := make(chan blockEvent, 4)

	q.ls.SetOnBlock(func(ls *statedb.LiveState, entries [][2]string) {
		select {
		case blockCh <- blockEvent{ls.Timestamp(), entries}:
		default:
		}
	})

	go func() {
		for bi := range blockCh {
			q.pm.SetBlockTimestamp(bi.timestamp)
			for _, entry := range bi.entries {
				key := entry[0]
				if strings.HasPrefix(key, "s:") {
					parts := strings.SplitN(key, ":", 3)
					if len(parts) == 3 {
						addr := common.HexToAddress(parts[1])
						slot := common.HexToHash(parts[2])
						q.pm.InvalidateBySlot(addr, slot)
					}
				}
			}
		}
	}()
}

// Quote runs a two-way quote. Returns forward (tokenIn→tokenOut) and reverse
// (tokenOut→tokenIn) results. Reverse is nil when tokenIn == tokenOut.
func (q *Quoter) Quote(req QuoteRequest) (*QuoteResponse, error) {
	tokenIn := common.HexToAddress(req.TokenIn)
	tokenOut := common.HexToAddress(req.TokenOut)

	amountIn := new(uint256.Int)
	bi, ok := new(big.Int).SetString(req.AmountIn, 10)
	if !ok || bi.Sign() <= 0 {
		return nil, fmt.Errorf("invalid amountIn: %s", req.AmountIn)
	}
	if amountIn.SetFromBig(bi) {
		return nil, fmt.Errorf("amountIn overflow: %s", req.AmountIn)
	}

	q.ls.RLock()
	defer q.ls.RUnlock()

	cfg := q.ls.EVMConfig()
	state := q.ls.State()

	// Forward: tokenIn → tokenOut
	fwdRoute := pf.FindBestRoute(q.pm, q.adj, q.pools, state, cfg, q.routerAddr, q.overrides,
		tokenIn, tokenOut, amountIn, q.maxHops)

	resp := &QuoteResponse{
		Forward: q.routeToResult(fwdRoute, tokenIn, tokenOut, amountIn),
	}

	// Reverse: tokenOut → tokenIn (skip for cyclic)
	cyclic := tokenIn == tokenOut
	if !cyclic {
		revRoute := pf.FindBestRoute(q.pm, q.adj, q.pools, state, cfg, q.routerAddr, q.overrides,
			tokenOut, tokenIn, amountIn, q.maxHops)
		resp.Reverse = q.routeToResult(revRoute, tokenOut, tokenIn, amountIn)
	}

	return resp, nil
}

func (q *Quoter) routeToResult(route *pf.Route, tokenIn, tokenOut common.Address, amountIn *uint256.Int) *QuoteResult {
	if route == nil {
		return &QuoteResult{
			TokenIn:   tokenIn.Hex(),
			TokenOut:  tokenOut.Hex(),
			AmountIn:  amountIn.Dec(),
			AmountOut: "0",
		}
	}

	steps := make([]PathStep, len(route.Steps))
	for i, s := range route.Steps {
		steps[i] = PathStep{
			Pool:     s.Pool.Hex(),
			TokenIn:  s.TokenIn.Hex(),
			TokenOut: s.TokenOut.Hex(),
			Dex:      q.dexMap[s.Pool],
		}
	}

	return &QuoteResult{
		TokenIn:   tokenIn.Hex(),
		TokenOut:  tokenOut.Hex(),
		AmountIn:  amountIn.Dec(),
		AmountOut: route.AmountOut.Dec(),
		Path:      steps,
		GasUsed:   route.GasUsed,
	}
}
