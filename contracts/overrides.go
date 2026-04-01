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
	Address       string `json:"address"`
	Slot          int    `json:"slot"`
	AllowanceSlot *int   `json:"allowance_slot,omitempty"` // nil = slot+1 (default)
	ERC7201Base   string `json:"erc7201_base,omitempty"`
	Shift         int    `json:"shift,omitempty"`
	Vyper         bool   `json:"vyper,omitempty"` // Vyper uses keccak(slot, addr) instead of keccak(addr, slot)
	HookContract  string `json:"hookContract,omitempty"`
	DisableSlots  []int  `json:"disableSlots,omitempty"`
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
	largeBalance := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(30)) // 1e30 — enough for low-value meme tokens

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

// BuildSenderOverrides creates token balance + allowance overrides for a sender address
// so that swap() (which does transferFrom(msg.sender, router, amount)) works in EVM simulation.
func BuildSenderOverrides(sender, routerAddr common.Address, pools []pathfinder.Pool) []pathfinder.ParsedOverride {
	// Collect all unique tokens
	tokenSet := make(map[common.Address]bool)
	for i := range pools {
		for _, t := range pools[i].Tokens {
			tokenSet[t] = true
		}
	}

	largeBalance := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(30)) // 1e30 — enough for low-value meme tokens
	maxUint256 := new(uint256.Int).Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 256), uint256.NewInt(1))   // 2^256 - 1

	var overrides []pathfinder.ParsedOverride
	for token := range tokenSet {
		entry, ok := overrideMap[token]
		if !ok {
			continue
		}

		// --- Balance slot for sender ---
		balanceSlot := computeBalanceSlot(sender, entry)

		var balanceValue common.Hash
		if entry.Shift > 0 {
			shifted := new(uint256.Int).Lsh(largeBalance, uint(entry.Shift))
			balanceValue = common.Hash(shifted.Bytes32())
		} else {
			balanceValue = common.Hash(largeBalance.Bytes32())
		}

		// --- Allowance slot for allowance[sender][router] ---
		// Determine raw allowance mapping slot
		aSlot := entry.Slot + 1 // default: balance slot + 1
		if entry.AllowanceSlot != nil {
			aSlot = *entry.AllowanceSlot
		}

		var allowanceSlot common.Hash
		if entry.ERC7201Base != "" {
			// ERC7201: use erc7201_base hash + 1 as allowance base slot
			baseHash := common.HexToHash(entry.ERC7201Base)
			baseInt := new(uint256.Int).SetBytes(baseHash[:])
			allowanceBase := common.Hash(new(uint256.Int).Add(baseInt, uint256.NewInt(1)).Bytes32())

			var inner [64]byte
			copy(inner[12:32], sender[:])
			copy(inner[32:64], allowanceBase[:])
			innerSlot := crypto.Keccak256Hash(inner[:])

			var outer [64]byte
			copy(outer[12:32], routerAddr[:])
			copy(outer[32:64], innerSlot[:])
			allowanceSlot = crypto.Keccak256Hash(outer[:])
		} else if entry.Vyper {
			// Vyper: reversed key order at each level
			allowanceBase := common.BigToHash(uint256.NewInt(uint64(aSlot)).ToBig())

			var inner [64]byte
			copy(inner[0:32], allowanceBase[:])
			copy(inner[44:64], sender[:])
			innerSlot := crypto.Keccak256Hash(inner[:])

			var outer [64]byte
			copy(outer[0:32], innerSlot[:])
			copy(outer[44:64], routerAddr[:])
			allowanceSlot = crypto.Keccak256Hash(outer[:])
		} else {
			// Standard Solidity: mapping(address owner => mapping(address spender => uint256))
			allowanceBase := common.BigToHash(uint256.NewInt(uint64(aSlot)).ToBig())

			var inner [64]byte
			copy(inner[12:32], sender[:])
			copy(inner[32:64], allowanceBase[:])
			innerSlot := crypto.Keccak256Hash(inner[:])

			var outer [64]byte
			copy(outer[12:32], routerAddr[:])
			copy(outer[32:64], innerSlot[:])
			allowanceSlot = crypto.Keccak256Hash(outer[:])
		}

		allowanceValue := common.Hash(maxUint256.Bytes32())

		po := pathfinder.ParsedOverride{
			Addr: token,
			Slots: []struct {
				Slot  common.Hash
				Value common.Hash
			}{
				{Slot: balanceSlot, Value: balanceValue},
				{Slot: allowanceSlot, Value: allowanceValue},
			},
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
	}

	// HookContract overrides: replace hook contract code with no-op
	hookSet := make(map[common.Address]bool)
	for token := range tokenSet {
		entry, ok := overrideMap[token]
		if !ok || entry.HookContract == "" {
			continue
		}
		hookAddr := common.HexToAddress(entry.HookContract)
		if !hookSet[hookAddr] {
			hookSet[hookAddr] = true
			overrides = append(overrides, pathfinder.ParsedOverride{
				Addr:    hookAddr,
				Balance: uint256.NewInt(0),
				Code:    []byte{0x00},
			})
		}
	}

	return overrides
}
