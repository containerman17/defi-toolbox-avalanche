package arb

import (
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"runtime"
	"sync"

	"github.com/ava-labs/libevm/common"
)

// PoolTable maps pool indices ↔ addresses and stores metadata for lookup.
// Built once at startup, shared read-only.
type PoolTable struct {
	addr      []common.Address // index → address
	tokens    [][2]common.Address
	poolTypes []int
	extras    []string
	index     map[common.Address]uint16 // address → index
}

func NewPoolTable(pools []pf.Pool) *PoolTable {
	pt := &PoolTable{
		addr:      make([]common.Address, len(pools)),
		tokens:    make([][2]common.Address, len(pools)),
		poolTypes: make([]int, len(pools)),
		extras:    make([]string, len(pools)),
		index:     make(map[common.Address]uint16, len(pools)),
	}
	for i := range pools {
		pt.addr[i] = pools[i].Address
		pt.poolTypes[i] = pools[i].PoolType
		pt.extras[i] = pools[i].ExtraData
		pt.index[pools[i].Address] = uint16(i)
		if len(pools[i].Tokens) >= 2 {
			pt.tokens[i] = [2]common.Address{pools[i].Tokens[0], pools[i].Tokens[1]}
		}
	}
	return pt
}

func (pt *PoolTable) Addr(i uint16) common.Address         { return pt.addr[i] }
func (pt *PoolTable) Token0(i uint16) common.Address        { return pt.tokens[i][0] }
func (pt *PoolTable) Token1(i uint16) common.Address        { return pt.tokens[i][1] }
func (pt *PoolTable) PoolType(i uint16) int                 { return pt.poolTypes[i] }
func (pt *PoolTable) ExtraData(i uint16) string             { return pt.extras[i] }
func (pt *PoolTable) Lookup(addr common.Address) (uint16, bool) {
	i, ok := pt.index[addr]
	return i, ok
}
func (pt *PoolTable) Len() int { return len(pt.addr) }

// Cycle is a compact representation: fixed-size arrays, pool indices instead of addresses.
// 18 bytes per cycle (vs ~300+ with slices).
type Cycle struct {
	Hops  uint8       // number of hops (2–4)
	Pools [4]uint16   // pool indices (only [0..Hops-1] valid)
	Dirs  [4]bool     // zeroForOne per hop
}

// PoolAddr returns the pool address at hop i.
func (c *Cycle) PoolAddr(pt *PoolTable, i int) common.Address { return pt.Addr(c.Pools[i]) }

// TokenAt returns the token at position i in the path (0 = hub, Hops = hub).
func (c *Cycle) TokenAt(pt *PoolTable, hub common.Address, i int) common.Address {
	if i == 0 || i == int(c.Hops) {
		return hub
	}
	// Token at position i = output of hop i-1
	p := c.Pools[i-1]
	if c.Dirs[i-1] {
		return pt.Token1(p) // zeroForOne: output is token1
	}
	return pt.Token0(p) // oneForZero: output is token0
}

// ExpandTokens returns the full token path [hub, ..., hub] for EVM encoding.
func (c *Cycle) ExpandTokens(pt *PoolTable, hub common.Address) []common.Address {
	tokens := make([]common.Address, int(c.Hops)+1)
	for i := 0; i <= int(c.Hops); i++ {
		tokens[i] = c.TokenAt(pt, hub, i)
	}
	return tokens
}

// ExpandPoolAddrs returns pool addresses for the cycle.
func (c *Cycle) ExpandPoolAddrs(pt *PoolTable) []common.Address {
	addrs := make([]common.Address, c.Hops)
	for i := 0; i < int(c.Hops); i++ {
		addrs[i] = pt.Addr(c.Pools[i])
	}
	return addrs
}

// ExpandPoolTypes returns pool types for the cycle.
func (c *Cycle) ExpandPoolTypes(pt *PoolTable) []int {
	types := make([]int, c.Hops)
	for i := 0; i < int(c.Hops); i++ {
		types[i] = pt.PoolType(c.Pools[i])
	}
	return types
}

// ExpandExtraDatas returns extra data strings for the cycle.
func (c *Cycle) ExpandExtraDatas(pt *PoolTable) []string {
	eds := make([]string, c.Hops)
	for i := 0; i < int(c.Hops); i++ {
		eds[i] = pt.ExtraData(c.Pools[i])
	}
	return eds
}

// ExpandDirs returns a slice of dirs.
func (c *Cycle) ExpandDirs() []bool {
	dirs := make([]bool, c.Hops)
	copy(dirs, c.Dirs[:c.Hops])
	return dirs
}

// ─── Edge table for formula-only graph ─────────────────────────────

// compactEdge is a formula-only edge in the token graph.
type compactEdge struct {
	poolIdx  uint16
	tokenOut common.Address
	dir      bool // zeroForOne
}

// buildFormulaGraph builds an adjacency list using only formula-supported pools.
func buildFormulaGraph(graph *pf.Graph, pt *PoolTable, registry *formulas.Registry) map[common.Address][]compactEdge {
	edges := make(map[common.Address][]compactEdge)
	seen := make(map[uint32]bool) // (poolIdx<<1 | dirBit) dedup

	for tokenIn, graphEdges := range graph.Edges {
		for _, e := range graphEdges {
			idx, ok := pt.Lookup(e.Pool.Address)
			if !ok {
				continue
			}
			_, known := registry.GetFormulaID(e.Pool.Address)
			if !known {
				continue
			}
			dir := e.Pool.Tokens[0] == tokenIn
			key := uint32(idx)<<1
			if dir {
				key |= 1
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			edges[tokenIn] = append(edges[tokenIn], compactEdge{
				poolIdx:  idx,
				tokenOut: e.TokenOut,
				dir:      dir,
			})
		}
	}
	return edges
}

// ─── Cycle enumeration ─────────────────────────────────────────────

// EnumerateCycles finds all hub→...→hub cycles of 2–maxHops hops.
// Only pools with formula support (registry ID >= 0) are included.
// Parallelized by first edge from hub.
func EnumerateCycles(graph *pf.Graph, pools []pf.Pool, hub common.Address, maxHops int, registry *formulas.Registry, pt *PoolTable) []Cycle {
	if maxHops < 2 {
		maxHops = 2
	}
	if maxHops > 4 {
		maxHops = 4
	}

	edges := buildFormulaGraph(graph, pt, registry)
	reachable := buildReachability(edges, hub, maxHops)

	// Collect first edges from hub
	firstEdges := edges[hub]
	if len(firstEdges) == 0 {
		return nil
	}

	// Partition first edges among goroutines
	nWorkers := runtime.NumCPU()
	if nWorkers > len(firstEdges) {
		nWorkers = len(firstEdges)
	}

	type workerResult struct {
		cycles []Cycle
	}
	results := make([]workerResult, nWorkers)

	var wg sync.WaitGroup
	chunkSize := (len(firstEdges) + nWorkers - 1) / nWorkers

	for w := 0; w < nWorkers; w++ {
		start := w * chunkSize
		end := start + chunkSize
		if end > len(firstEdges) {
			end = len(firstEdges)
		}
		if start >= end {
			continue
		}

		wg.Add(1)
		go func(workerID int, myEdges []compactEdge) {
			defer wg.Done()
			var local []Cycle
			for _, fe := range myEdges {
				enumerateFromEdge(hub, fe, edges, reachable, maxHops, &local)
			}
			results[workerID].cycles = local
		}(w, firstEdges[start:end])
	}
	wg.Wait()

	// Merge and dedup
	total := 0
	for _, r := range results {
		total += len(r.cycles)
	}

	seen := make(map[uint64]bool, total)
	cycles := make([]Cycle, 0, total)
	for _, r := range results {
		for _, c := range r.cycles {
			key := cycleKey(c)
			if !seen[key] {
				seen[key] = true
				cycles = append(cycles, c)
			}
		}
	}

	return cycles
}

// dfsFrame is a zero-allocation stack frame for the DFS.
type dfsFrame struct {
	token common.Address
	depth uint8
	pools [4]uint16
	dirs  [4]bool
}

// enumerateFromEdge runs DFS from a single first edge off hub.
func enumerateFromEdge(
	hub common.Address,
	firstEdge compactEdge,
	edges map[common.Address][]compactEdge,
	reachable map[common.Address]int,
	maxHops int,
	out *[]Cycle,
) {
	// Pre-allocate stack (max branching is bounded by maxHops)
	stack := make([]dfsFrame, 0, 256)
	stack = append(stack, dfsFrame{
		token: firstEdge.tokenOut,
		depth: 1,
		pools: [4]uint16{firstEdge.poolIdx},
		dirs:  [4]bool{firstEdge.dir},
	})

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for _, e := range edges[f.token] {
			// Check visited (linear scan of ≤3 entries)
			dup := false
			for j := uint8(0); j < f.depth; j++ {
				if f.pools[j] == e.poolIdx {
					dup = true
					break
				}
			}
			if dup {
				continue
			}

			if e.tokenOut == hub {
				// Found a cycle (need ≥ 2 hops)
				if f.depth+1 >= 2 {
					c := Cycle{Hops: f.depth + 1}
					copy(c.Pools[:], f.pools[:])
					c.Pools[f.depth] = e.poolIdx
					copy(c.Dirs[:], f.dirs[:])
					c.Dirs[f.depth] = e.dir
					*out = append(*out, c)
				}
			} else if f.depth+1 < uint8(maxHops) {
				remaining := maxHops - int(f.depth) - 1
				if dist, ok := reachable[e.tokenOut]; ok && dist <= remaining {
					nf := dfsFrame{
						token: e.tokenOut,
						depth: f.depth + 1,
					}
					copy(nf.pools[:], f.pools[:])
					nf.pools[f.depth] = e.poolIdx
					copy(nf.dirs[:], f.dirs[:])
					nf.dirs[f.depth] = e.dir
					stack = append(stack, nf)
				}
			}
		}
	}
}

// buildReachability does a reverse BFS from hub: for each token, the minimum
// number of hops to reach hub (using only formula-supported pools).
func buildReachability(edges map[common.Address][]compactEdge, hub common.Address, maxDist int) map[common.Address]int {
	reach := map[common.Address]int{hub: 0}
	frontier := []common.Address{hub}

	for dist := 1; dist <= maxDist && len(frontier) > 0; dist++ {
		var next []common.Address
		for _, token := range frontier {
			for _, e := range edges[token] {
				if _, ok := reach[e.tokenOut]; !ok {
					reach[e.tokenOut] = dist
					next = append(next, e.tokenOut)
				}
			}
		}
		frontier = next
	}

	return reach
}

// cycleKey creates a uint64 dedup key from pool indices.
// Canonical rotation: start at the smallest pool index.
func cycleKey(c Cycle) uint64 {
	n := c.Hops
	// Find rotation starting at smallest pool index
	minIdx := uint8(0)
	for i := uint8(1); i < n; i++ {
		if c.Pools[i] < c.Pools[minIdx] {
			minIdx = i
		}
	}
	// Pack into uint64: 4 x 16-bit pool indices in canonical rotation
	var key uint64
	for i := uint8(0); i < n; i++ {
		key |= uint64(c.Pools[(minIdx+i)%n]) << (i * 16)
	}
	return key
}
