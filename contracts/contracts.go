package contracts

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/ava-labs/libevm/common"
)

//go:embed address.json
var deployedRouterJSON string

//go:embed bytecode.hex
var routerBytecodeHex string

type deployConfig struct {
	Address common.Address `json:"address"`
	Block   uint64         `json:"block"`
}

var config = func() deployConfig {
	var c deployConfig
	json.Unmarshal([]byte(deployedRouterJSON), &c)
	return c
}()

// DeployedRouter is the on-chain address of the HayabusaRouter contract.
var DeployedRouter = config.Address

// DeployedBlock is the reference block number for benchmarking and state snapshots.
var DeployedBlock = config.Block

// RouterBytecode returns the raw router implementation bytecode.
// Use this to inject code into a StateView for debugSwapSingle calls
// (the on-chain router is a proxy and debugSwapSingle needs the
// implementation bytecode directly).
func RouterBytecode() []byte {
	b, _ := hex.DecodeString(strings.TrimSpace(routerBytecodeHex))
	return b
}
