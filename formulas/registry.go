package formulas

import (
	"bufio"
	_ "embed"
	"fmt"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

//go:embed registry.txt
var registryData string

//go:embed data/token_amounts.txt
var tokenAmountsData string

// LoadEmbeddedRegistry loads the formula registry from embedded data.
func LoadEmbeddedRegistry() *Registry {
	return parseRegistryContent(registryData)
}

// LoadEmbeddedTokenAmounts returns a map of token address → swap amount (~1 AVAX worth).
func LoadEmbeddedTokenAmounts() map[common.Address]*uint256.Int {
	result := make(map[common.Address]*uint256.Int)
	scanner := bufio.NewScanner(strings.NewReader(tokenAmountsData))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		addr := common.HexToAddress(parts[0])
		amt := new(uint256.Int)
		amt.SetFromHex(parts[1])
		if !amt.IsZero() {
			result[addr] = amt
		}
	}
	return result
}

// Formula IDs
const (
	FormulaV2_30bps  = 0  // V2 constant product, 0.3% fee
	FormulaPharaohV1 = 1  // Pharaoh V1 (stable/volatile with registry)
	FormulaV3        = 2  // Uniswap V3 / Pharaoh V3 tick-walking
	FormulaLFJV2     = 3  // LFJ V2 Liquidity Book (discrete bins)
	FormulaAlgebra   = 4  // Algebra V1 Integral (dynamic fee CL)
	FormulaDODO      = 5  // DODO PMM
	FormulaV4          = 6  // Uniswap V4 (singleton PoolManager)
	FormulaBalancerV3  = 7  // Balancer V3 (Weighted + Stable pools via Vault singleton)
	FormulaBalancerV2  = 8  // Balancer V2 (Weighted pools via Vault singleton)
	FormulaWombat      = 10 // Wombat DynamicPoolV2 (stableswap with yield-bearing tokens)
	FormulaPlatypus    = 11 // Platypus stableswap (price slippage curve)
	FormulaNoImpl      = 9  // Pool type is known but has no formula implementation.
	                        // Setting this prevents EVM fallback: PoolManager.Get() caches
	                        // a deadPoolQuoter (returns nil,false) instead of returning nil.
	FormulaInvalid     = -1 // Do not use formula (FoT, broken, custom fee)
)

// Registry maps pool addresses to formula IDs.
type Registry struct {
	pools map[common.Address]int
}

func parseRegistryContent(content string) *Registry {
	r := &Registry{pools: make(map[common.Address]int)}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		addr := common.HexToAddress(parts[0])
		var id int
		fmt.Sscanf(parts[1], "%d", &id)
		r.pools[addr] = id
	}

	return r
}

// IsInvalid returns true if a pool is known to not work with formulas (FoT, broken).
// The BFS can skip EVM calls for these pools entirely.
// GetFormulaID returns the formula ID for a pool, and whether it's known.
func (r *Registry) GetFormulaID(pool common.Address) (int, bool) {
	id, ok := r.pools[pool]
	return id, ok
}

// SetFormulaID sets the formula ID for a pool only if not already in the registry.
// Existing entries (including -1) are never overwritten — the registry is authoritative.
func (r *Registry) SetFormulaID(pool common.Address, id int) {
	if _, exists := r.pools[pool]; !exists {
		r.pools[pool] = id
	}
}


// RegistryStats returns the count of validated and invalid pools.
func (r *Registry) RegistryStats() (validated, invalid int) {
	for _, id := range r.pools {
		if id >= 0 {
			validated++
		} else {
			invalid++
		}
	}
	return
}

