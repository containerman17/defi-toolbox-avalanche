package contracts

import (
	_ "embed"
	"encoding/json"

	"github.com/ava-labs/libevm/common"
)

//go:embed address.json
var deployedRouterJSON string

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
