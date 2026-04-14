package pathfinder

import "github.com/holiman/uint256"

// Split-route merging has been archived. For now, degrade split requests to the
// first simple route without any refinement or merging.
func MergeRoutes(routes []SquishRoute) ([]RouteStep, []*uint256.Int) {
	return fallbackSingleRoute(routes)
}

// Split-route merging with first-hop quoting is also degraded to the first
// simple route. The quoter is ignored.
func MergeRoutesWithQuoter(routes []SquishRoute, quoter func(RouteStep, *uint256.Int) uint256.Int) ([]RouteStep, []*uint256.Int) {
	return fallbackSingleRoute(routes)
}

func fallbackSingleRoute(routes []SquishRoute) ([]RouteStep, []*uint256.Int) {
	if len(routes) == 0 || len(routes[0].Steps) == 0 {
		return nil, nil
	}
	steps := make([]RouteStep, len(routes[0].Steps))
	amounts := make([]*uint256.Int, len(routes[0].Steps))
	copy(steps, routes[0].Steps)
	amounts[0] = new(uint256.Int).Set(routes[0].Volume)
	for i := 1; i < len(amounts); i++ {
		amounts[i] = uint256.NewInt(0)
	}
	return steps, amounts
}
