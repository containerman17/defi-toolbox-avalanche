package formulas

import "github.com/holiman/uint256"

var mask128U256 = new(uint256.Int).Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 128), uint256.NewInt(1))
