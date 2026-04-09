package splitter

import (
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Params bundles the shared dependencies for all split strategies.
type Params struct {
	PM         formulas.PoolQuoterSource
	BasePM     *formulas.PoolManager
	Adj        map[common.Address][]pf.PoolEdge
	Pools      []pf.Pool
	State      *statedb.StateDB
	EVMConfig  statedb.EVMConfig
	RouterAddr common.Address
	Sender     common.Address
	TokenIn    common.Address
	TokenOut   common.Address
	MaxHops    int
}

// Leg is one piece of a split route.
type Leg struct {
	Steps   []pf.RouteStep
	Volume  uint256.Int // input for this leg
	Output  uint256.Int // EVM-verified output
	GasUsed uint64
}

// Result is the output of a split strategy.
type Result struct {
	Legs      []Leg
	Total     uint256.Int // sum of all leg outputs
	TotalGas  uint64
	ElapsedUs int64 // wall time in microseconds
}

// buildCalldata constructs swap() calldata for a set of steps at a given volume.
func buildCalldata(steps []pf.RouteStep, volume *uint256.Int) []byte {
	poolAddrs := make([]common.Address, len(steps))
	poolTypes := make([]int, len(steps))
	tokenPairs := make([]common.Address, len(steps)*2)
	extraDatas := make([]string, len(steps))
	for i, s := range steps {
		poolAddrs[i] = s.Pool
		poolTypes[i] = s.PoolType
		tokenPairs[i*2] = s.TokenIn
		tokenPairs[i*2+1] = s.TokenOut
		extraDatas[i] = s.ExtraData
	}
	return pf.EncodeSwapMulti(poolAddrs, poolTypes, tokenPairs, volume, extraDatas, uint256.NewInt(0))
}
