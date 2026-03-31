package contracts

import (
	_ "embed"
	"encoding/json"

	"defi-toolbox/pathfinder"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

//go:embed address.json
var deployedRouterJSON string

//go:embed token_overrides.json
var tokenOverridesJSON string

type tokenOverrideEntry struct {
	Address      string `json:"address"`
	Slot         int    `json:"slot"`
	ERC7201Base  string `json:"erc7201_base,omitempty"`
	Shift        int    `json:"shift,omitempty"`
	Vyper        bool   `json:"vyper,omitempty"` // Vyper uses keccak(slot, addr) instead of keccak(addr, slot)
	HookContract string `json:"hookContract,omitempty"`
	DisableSlots []int  `json:"disableSlots,omitempty"`
}

var overrideMap map[common.Address]*tokenOverrideEntry

func init() {
	var entries []tokenOverrideEntry
	json.Unmarshal([]byte(tokenOverridesJSON), &entries)
	overrideMap = make(map[common.Address]*tokenOverrideEntry, len(entries))
	for i := range entries {
		addr := common.HexToAddress(entries[i].Address)
		overrideMap[addr] = &entries[i]
	}
}

// config holds the deployment config from contracts/address.json.
var config = parseConfig()

// DeployedRouter is the on-chain address of the HayabusaRouter contract.
var DeployedRouter = config.Address

// DeployedBlock is the reference block number for benchmarking and state snapshots.
var DeployedBlock = config.Block

type deployConfig struct {
	Address common.Address
	Block   int
}

func parseConfig() deployConfig {
	var raw struct {
		Address string `json:"address"`
		Block   int    `json:"block"`
	}
	json.Unmarshal([]byte(deployedRouterJSON), &raw)
	return deployConfig{
		Address: common.HexToAddress(raw.Address),
		Block:   raw.Block,
	}
}

// computeBalanceSlot computes keccak256(abi.encode(holder, slot)) for standard ERC20 mapping.
// For Vyper contracts, the order is reversed: keccak256(abi.encode(slot, holder)).
func computeBalanceSlot(holder common.Address, entry *tokenOverrideEntry) common.Hash {
	var slotKey [64]byte

	if entry.Vyper {
		// Vyper: keccak256(slot || addr)
		slotHash := common.BigToHash(uint256.NewInt(uint64(entry.Slot)).ToBig())
		copy(slotKey[0:32], slotHash[:])
		copy(slotKey[44:64], holder[:])
	} else if entry.ERC7201Base != "" {
		copy(slotKey[12:32], holder[:])
		base := common.HexToHash(entry.ERC7201Base)
		copy(slotKey[32:64], base[:])
	} else {
		copy(slotKey[12:32], holder[:])
		slotHash := common.BigToHash(uint256.NewInt(uint64(entry.Slot)).ToBig())
		copy(slotKey[32:64], slotHash[:])
	}

	return crypto.Keccak256Hash(slotKey[:])
}

// BuildTokenOverrides creates token balance overrides only (no router bytecode).
// Use this when the router contract is already deployed on-chain and its code
// is available in the state dump.
func BuildTokenOverrides(routerAddr common.Address, pools []pathfinder.Pool) []pathfinder.ParsedOverride {
	return buildTokenOverrides(routerAddr, pools)
}

// BuildSingleTokenOverride creates a balance override for one token on the router.
// Returns nil if the token has no known balance slot in token_overrides.json.
func BuildSingleTokenOverride(routerAddr, token common.Address, amount *uint256.Int) *pathfinder.ParsedOverride {
	entry, ok := overrideMap[token]
	if !ok {
		return nil
	}

	slot := computeBalanceSlot(routerAddr, entry)

	var value common.Hash
	if entry.Shift > 0 {
		shifted := new(uint256.Int).Lsh(amount, uint(entry.Shift))
		value = common.Hash(shifted.Bytes32())
	} else {
		value = common.Hash(amount.Bytes32())
	}

	po := pathfinder.ParsedOverride{
		Addr: token,
		Slots: []struct {
			Slot  common.Hash
			Value common.Hash
		}{{Slot: slot, Value: value}},
	}

	for _, ds := range entry.DisableSlots {
		dsHash := common.BigToHash(uint256.NewInt(uint64(ds)).ToBig())
		po.Slots = append(po.Slots, struct {
			Slot  common.Hash
			Value common.Hash
		}{Slot: dsHash, Value: common.Hash{}})
	}

	if entry.HookContract != "" {
		// Hook neutralization needs a separate override — caller handles this
	}

	return &po
}

func buildTokenOverrides(routerAddr common.Address, pools []pathfinder.Pool) []pathfinder.ParsedOverride {
	// Collect all unique tokens
	tokenSet := make(map[common.Address]bool)
	for i := range pools {
		for _, t := range pools[i].Tokens {
			tokenSet[t] = true
		}
	}

	// For each token with a known balance slot, set a realistic balance on the router.
	// 1000 units (at 18 decimals) — enough for quoting but not so large it creates fake arb.
	largeBalance := new(uint256.Int).Mul(uint256.NewInt(1000), uint256.NewInt(1_000_000_000_000_000_000)) // 1000 * 1e18

	var overrides []pathfinder.ParsedOverride
	hookSet := make(map[common.Address]bool)
	for token := range tokenSet {
		entry, ok := overrideMap[token]
		if !ok {
			continue
		}

		slot := computeBalanceSlot(routerAddr, entry)

		var value common.Hash
		if entry.Shift > 0 {
			shifted := new(uint256.Int).Lsh(largeBalance, uint(entry.Shift))
			value = common.Hash(shifted.Bytes32())
		} else {
			value = common.Hash(largeBalance.Bytes32())
		}

		po := pathfinder.ParsedOverride{
			Addr: token,
			Slots: []struct {
				Slot  common.Hash
				Value common.Hash
			}{{Slot: slot, Value: value}},
		}

		// DisableSlots: zero out specific slots (e.g., maxWallet checks)
		for _, ds := range entry.DisableSlots {
			dsHash := common.BigToHash(uint256.NewInt(uint64(ds)).ToBig())
			po.Slots = append(po.Slots, struct {
				Slot  common.Hash
				Value common.Hash
			}{Slot: dsHash, Value: common.Hash{}})
		}

		overrides = append(overrides, po)

		// HookContract: replace hook contract code with a no-op so staking hooks
		// don't interfere with swap execution.
		if entry.HookContract != "" {
			hookAddr := common.HexToAddress(entry.HookContract)
			if !hookSet[hookAddr] {
				hookSet[hookAddr] = true
				// STOP opcode (0x00) — any call to this contract returns successfully with no data
				overrides = append(overrides, pathfinder.ParsedOverride{
					Addr:    hookAddr,
					Balance: uint256.NewInt(0),
					Code:    []byte{0x00},
				})
			}
		}
	}

	return overrides
}
