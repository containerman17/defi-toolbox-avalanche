package pathfinder

import (
	"strconv"
	"strings"

	"github.com/ava-labs/libevm/common"
)

// Pool represents a DEX pool from pools.txt.
type Pool struct {
	Address   common.Address
	Dex       string // DEX provider name (e.g. "pangolin_v2", "sushiswap_v2")
	PoolType  int
	Tokens    []common.Address
	ExtraData string
}

// Edge represents a directed swap edge in the token graph.
type Edge struct {
	Pool     *Pool
	TokenOut common.Address
}

// Graph maps each token to its outgoing edges.
type Graph struct {
	Edges map[common.Address][]Edge
}

// ParsePools parses pools.txt content into a slice of pools.
// Format: line 1 = head block, lines 2+: address:provider:poolType:swapBlock:token0:token1[:@extraData]
func ParsePools(content string, limit int) []Pool {
	lines := strings.Split(content, "\n")
	var pools []Pool

	for i, line := range lines {
		if i == 0 {
			continue // skip head block line
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) < 6 {
			continue
		}

		addr := common.HexToAddress(parts[0])
		poolType, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}

		// Parse tokens (fields 4+), @ prefix means extraData
		var tokens []common.Address
		var extraData string
		for _, part := range parts[4:] {
			if strings.HasPrefix(part, "@") {
				extraData = part[1:]
			} else if len(part) == 42 && strings.HasPrefix(part, "0x") {
				tokens = append(tokens, common.HexToAddress(part))
			}
		}

		if len(tokens) < 2 {
			continue
		}

		pools = append(pools, Pool{
			Address:   addr,
			Dex:       parts[1],
			PoolType:  poolType,
			Tokens:    tokens,
			ExtraData: extraData,
		})

		if limit > 0 && len(pools) >= limit {
			break
		}
	}

	return pools
}

// BuildGraph creates a token adjacency graph from pools.
func BuildGraph(pools []Pool) *Graph {
	g := &Graph{Edges: make(map[common.Address][]Edge)}

	for i := range pools {
		pool := &pools[i]
		for ti := 0; ti < len(pool.Tokens); ti++ {
			for tj := 0; tj < len(pool.Tokens); tj++ {
				if ti != tj {
					g.Edges[pool.Tokens[ti]] = append(g.Edges[pool.Tokens[ti]], Edge{
						Pool:     pool,
						TokenOut: pool.Tokens[tj],
					})
				}
			}
		}
	}

	return g
}
