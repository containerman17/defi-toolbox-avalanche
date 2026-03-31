package poolcollector

import (
	_ "embed"

	"defi-toolbox/pathfinder"
)

//go:embed data/pools.txt
var PoolsData string

// EmbeddedPools returns parsed pools from the embedded pools.txt.
func EmbeddedPools(limit int) []pathfinder.Pool {
	return pathfinder.ParsePools(PoolsData, limit)
}
