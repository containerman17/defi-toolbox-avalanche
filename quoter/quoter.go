package quoter

import (
	"fmt"
	"math/big"
	"strings"

	router "defi-toolbox/contracts"
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/pathfinder/splitter"
	"defi-toolbox/statedb"
	poolcollector "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Quoter wraps the formula engine + EVM verification behind a simple Quote API.
type Quoter struct {
	ls                 *statedb.LiveState
	pm                 *formulas.PoolManager
	adj                map[common.Address][]pf.PoolEdge
	pools              []pf.Pool
	registry           *formulas.Registry
	routerAddr         common.Address
	sender             common.Address
	stateWithOverrides *statedb.StateDB // persistent overlay with token overrides
	maxHops            int
	dexMap             map[common.Address]string
	onBlock            func(block, timestamp uint64) // called after each block is processed
}

// SetOnBlock registers a callback fired after each block_diff is applied
// and pool invalidation is complete. Safe to call Quote from the callback.
func (q *Quoter) SetOnBlock(fn func(block, timestamp uint64)) {
	q.onBlock = fn
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
			pm.SetPoolTokens(p.Address, p.Tokens...)
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
	sender := pf.DUMMY_SENDER

	// Build persistent overlay with sender overrides only.
	// swap() uses transferFrom (sender→router), so only the sender needs balance + allowance.
	// Router starts with zero balance — it gets tokens via transferFrom.
	// Created once, reused across Quote() calls so code hash caches persist.
	senderOverrides := router.BuildSenderOverrides(sender, routerAddr, pools)
	stateWithOverrides := pf.ApplyOverridesFlat(state, senderOverrides)

	// Warmup: build all pool quoters
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	ls.RUnlock()

	return &Quoter{
		ls:                 ls,
		pm:                 pm,
		adj:                adj,
		pools:              pools,
		registry:           registry,
		routerAddr:         routerAddr,
		sender:             sender,
		stateWithOverrides: stateWithOverrides,
		maxHops:            maxHops,
		dexMap:             dexMap,
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
			if q.onBlock != nil {
				q.onBlock(q.ls.Block(), bi.timestamp)
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

	// Forward: tokenIn → tokenOut
	fwdRoute := pf.FindBestRoute(q.pm, q.adj, q.pools, q.stateWithOverrides, cfg, q.routerAddr, q.sender,
		tokenIn, tokenOut, amountIn, q.maxHops)

	resp := &QuoteResponse{
		Forward: q.routeToResult(fwdRoute, tokenIn, tokenOut, amountIn),
	}

	// Reverse: tokenOut → tokenIn (skip for cyclic)
	cyclic := tokenIn == tokenOut
	if !cyclic {
		revRoute := pf.FindBestRoute(q.pm, q.adj, q.pools, q.stateWithOverrides, cfg, q.routerAddr, q.sender,
			tokenOut, tokenIn, amountIn, q.maxHops)
		resp.Reverse = q.routeToResult(revRoute, tokenOut, tokenIn, amountIn)
	}

	// Split routing (forward only)
	if req.Split {
		params := &splitter.Params{
			PM:         q.pm,
			BasePM:     q.pm,
			Adj:        q.adj,
			Pools:      q.pools,
			State:      q.stateWithOverrides,
			EVMConfig:  cfg,
			RouterAddr: q.routerAddr,
			Sender:     q.sender,
			TokenIn:    tokenIn,
			TokenOut:   tokenOut,
			MaxHops:    q.maxHops,
		}
		splitResult := splitter.Split(params, amountIn)
		if splitResult != nil {
			resp.Split = q.splitToResult(splitResult)
		}
	}

	return resp, nil
}

// ── Getters for split routing ────────────────────────────────────────

func (q *Quoter) PM() *formulas.PoolManager                   { return q.pm }
func (q *Quoter) Adj() map[common.Address][]pf.PoolEdge       { return q.adj }
func (q *Quoter) Pools() []pf.Pool                            { return q.pools }
func (q *Quoter) StateWithOverrides() *statedb.StateDB         { return q.stateWithOverrides }
func (q *Quoter) RouterAddr() common.Address                   { return q.routerAddr }
func (q *Quoter) Sender() common.Address                      { return q.sender }
func (q *Quoter) MaxHops() int                                 { return q.maxHops }
func (q *Quoter) LiveState() *statedb.LiveState                { return q.ls }

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

func (q *Quoter) splitToResult(r *splitter.Result) *SplitResult {
	legs := make([]SplitLeg, len(r.Legs))
	for i, leg := range r.Legs {
		path := make([]PathStep, len(leg.Steps))
		for j, s := range leg.Steps {
			path[j] = PathStep{
				Pool:     s.Pool.Hex(),
				TokenIn:  s.TokenIn.Hex(),
				TokenOut: s.TokenOut.Hex(),
				Dex:      q.dexMap[s.Pool],
			}
		}
		legs[i] = SplitLeg{
			AmountIn:  leg.Volume.Dec(),
			AmountOut: leg.Output.Dec(),
			Path:      path,
			GasUsed:   leg.GasUsed,
		}
	}
	return &SplitResult{
		AmountOut: r.Total.Dec(),
		Legs:      legs,
		TotalGas:  r.TotalGas,
		ElapsedUs: r.ElapsedUs,
	}
}
