package contracts

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"

	"defi-toolbox/pathfinder"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

//go:embed address.json
var deployedRouterJSON string

//go:embed token_overrides.json
var tokenOverridesJSON string

//go:embed bytecode.hex
var routerBytecodeHex string

type tokenOverrideEntry struct {
	Address        string `json:"address"`
	Slot           int    `json:"slot"`
	AllowanceSlot  *int   `json:"allowance_slot,omitempty"` // nil = slot+1 (default)
	ERC7201Base    string `json:"erc7201_base,omitempty"`
	Shift          int    `json:"shift,omitempty"`
	Vyper          bool   `json:"vyper,omitempty"` // Vyper uses keccak(slot, addr) instead of keccak(addr, slot)
	HookContracts  []string `json:"hookContracts,omitempty"`
	DisableSlots   []int  `json:"disableSlots,omitempty"`
	WhitelistSlots     []int  `json:"whitelistSlots,omitempty"`     // mapping slots to set mapping[addr]=true for router (e.g., excludedFromLockPeriod)
	RouterAddressSlots []int  `json:"routerAddressSlots,omitempty"` // plain slots to overwrite with the router address (e.g., registered uniswapV2Router)
	CodeContracts  map[string]string `json:"codeContracts,omitempty"` // address → hex bytecode: deploy real code at these addresses (for proxy resolvers/implementations not in state dump)
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
	Address        common.Address
	Implementation common.Address
	Block          int
}

func parseConfig() deployConfig {
	var raw struct {
		Address        string `json:"address"`
		Implementation string `json:"implementation"`
		Block          int    `json:"block"`
	}
	json.Unmarshal([]byte(deployedRouterJSON), &raw)
	return deployConfig{
		Address:        common.HexToAddress(raw.Address),
		Implementation: common.HexToAddress(raw.Implementation),
		Block:          raw.Block,
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

	// WhitelistSlots: set mapping[routerAddr] = true
	trueVal := common.Hash(uint256.NewInt(1).Bytes32())
	for _, ws := range entry.WhitelistSlots {
		var key [64]byte
		copy(key[12:32], routerAddr[:])
		wsHash := common.BigToHash(uint256.NewInt(uint64(ws)).ToBig())
		copy(key[32:64], wsHash[:])
		wlSlot := crypto.Keccak256Hash(key[:])
		po.Slots = append(po.Slots, struct {
			Slot  common.Hash
			Value common.Hash
		}{Slot: wlSlot, Value: trueVal})
	}

	// RouterAddressSlots: overwrite plain slots with the router address
	routerHash := common.BytesToHash(routerAddr[:])
	for _, rs := range entry.RouterAddressSlots {
		rsHash := common.BigToHash(uint256.NewInt(uint64(rs)).ToBig())
		po.Slots = append(po.Slots, struct {
			Slot  common.Hash
			Value common.Hash
		}{Slot: rsHash, Value: routerHash})
	}

	return &po
}

func buildTokenOverrides(routerAddr common.Address, pools []pathfinder.Pool) []pathfinder.ParsedOverride {
	// Collect all unique tokens and V4 tokens (for PoolManager balance overrides)
	tokenSet := make(map[common.Address]bool)
	v4TokenSet := make(map[common.Address]bool) // ERC20 tokens used by V4 pools
	for i := range pools {
		for _, t := range pools[i].Tokens {
			tokenSet[t] = true
		}
		if pools[i].PoolType == 9 { // uniswap_v4
			for _, t := range pools[i].Tokens {
				if (t != common.Address{}) { // skip native AVAX
					v4TokenSet[t] = true
				}
			}
		}
	}

	// For each token with a known balance slot, set a realistic balance on the router.
	// 1000 units (at 18 decimals) — enough for quoting but not so large it creates fake arb.
	largeBalance := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(36)) // 1e36 — covers reverse swaps of low-value tokens

	var overrides []pathfinder.ParsedOverride

	// Native AVAX balance for the router: V4 pools with currency0=address(0) require
	// the router to hold native AVAX for settle{value:...}() during swap execution.
	if tokenSet[common.Address{}] {
		overrides = append(overrides, pathfinder.ParsedOverride{
			Addr:    routerAddr,
			Balance: new(uint256.Int).Set(largeBalance),
		})
	}

	// Override the router implementation bytecode so debugSwapSingle handles
	// native AVAX output (address(0) as tokenOut). Without this, calling
	// IERC20(address(0)).balanceOf() reverts because there's no contract at address(0).
	if implCode, err := hex.DecodeString(strings.TrimSpace(routerBytecodeHex)); err == nil && len(implCode) > 0 {
		overrides = append(overrides, pathfinder.ParsedOverride{
			Addr: config.Implementation,
			Code: implCode,
		})
	}

	// Deploy a shim at address(0) so IERC20(address(0)).balanceOf(addr) returns
	// the native AVAX balance of addr. Without code at address(0), Solidity 0.8+
	// reverts on any external call to it (EXTCODESIZE check).
	// This makes debugSwapSingle's balance delta measurement work for native AVAX.
	if tokenSet[common.Address{}] {
		// EVM bytecode: reads address from calldata[4..36], returns its BALANCE.
		//   PUSH1 0x04       // [4]
		//   CALLDATALOAD     // [calldata[4:36]] = left-padded address
		//   PUSH1 0x60       // [96, addr_padded]
		//   SHR              // [address] (shift right 96 bits to extract 160-bit address)
		//   BALANCE          // [balance]
		//   PUSH0            // [0, balance]
		//   MSTORE           // [] (store balance at memory[0:32])
		//   PUSH1 0x20       // [32]
		//   PUSH0            // [0, 32]
		//   RETURN           // return memory[0:32]
		nativeShim := []byte{
			0x60, 0x04, // PUSH1 4
			0x35,       // CALLDATALOAD
			0x60, 0x60, // PUSH1 96
			0x1c,       // SHR
			0x31,       // BALANCE
			0x5f,       // PUSH0
			0x52,       // MSTORE
			0x60, 0x20, // PUSH1 32
			0x5f,       // PUSH0
			0xf3,       // RETURN
		}
		overrides = append(overrides, pathfinder.ParsedOverride{
			Addr: common.Address{}, // address(0)
			Code: nativeShim,
		})
	}
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

		// WhitelistSlots: set mapping[routerAddr] = true for each whitelist mapping slot.
		// This bypasses transfer restrictions (e.g., excludedFromLockPeriod, isExcludedFromFee)
		// so the router can send/receive tokens during swap simulation.
		trueVal := common.Hash(uint256.NewInt(1).Bytes32())
		for _, ws := range entry.WhitelistSlots {
			var key [64]byte
			copy(key[12:32], routerAddr[:])
			wsHash := common.BigToHash(uint256.NewInt(uint64(ws)).ToBig())
			copy(key[32:64], wsHash[:])
			wlSlot := crypto.Keccak256Hash(key[:])
			po.Slots = append(po.Slots, struct {
				Slot  common.Hash
				Value common.Hash
			}{Slot: wlSlot, Value: trueVal})
		}

		// RouterAddressSlots: overwrite plain slots with the router address
		routerHash := common.BytesToHash(routerAddr[:])
		for _, rs := range entry.RouterAddressSlots {
			rsHash := common.BigToHash(uint256.NewInt(uint64(rs)).ToBig())
			po.Slots = append(po.Slots, struct {
				Slot  common.Hash
				Value common.Hash
			}{Slot: rsHash, Value: routerHash})
		}

		// V4 PoolManager balance: V4 pools store tokens in the singleton PoolManager.
		// The PM's take() transfers ERC20 tokens from PM to the router, so the PM
		// needs a balance override for every ERC20 token used by V4 pools.
		if v4TokenSet[token] {
			pmSlot := computeBalanceSlot(pathfinder.V4PoolManager, entry)
			po.Slots = append(po.Slots, struct {
				Slot  common.Hash
				Value common.Hash
			}{Slot: pmSlot, Value: value})
		}

		overrides = append(overrides, po)

		// HookContracts: replace hook contract code with a no-op so external hooks
		// (staking, antiBot, antiWhale) don't interfere with swap execution.
		// Bytecode: PUSH1 0x20, PUSH0, RETURN — returns 32 zero bytes.
		// This makes any high-level Solidity call succeed and decode the return as
		// false/0, which is the safe default for guard functions like isBotDetected().
		for _, hc := range entry.HookContracts {
			hookAddr := common.HexToAddress(hc)
			if !hookSet[hookAddr] {
				hookSet[hookAddr] = true
				overrides = append(overrides, pathfinder.ParsedOverride{
					Addr:    hookAddr,
					Balance: uint256.NewInt(0),
					Code:    []byte{0x60, 0x20, 0x5f, 0xf3},
				})
			}
		}

		// CodeContracts: deploy real bytecode at specific addresses so proxy tokens
		// whose resolver/implementation contracts are not in the state dump can
		// execute transfers. Unlike hookContracts (which deploy a no-op), these
		// deploy the actual code needed for the proxy chain to work.
		for addr, hexCode := range entry.CodeContracts {
			codeAddr := common.HexToAddress(addr)
			if !hookSet[codeAddr] {
				hookSet[codeAddr] = true
				code, err := hex.DecodeString(strings.TrimPrefix(hexCode, "0x"))
				if err == nil && len(code) > 0 {
					overrides = append(overrides, pathfinder.ParsedOverride{
						Addr: codeAddr,
						Code: code,
					})
				}
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

	largeBalance := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(36)) // 1e36 — covers reverse swaps of low-value tokens
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

		// WhitelistSlots: set mapping[addr] = true for both sender and router
		trueVal := common.Hash(uint256.NewInt(1).Bytes32())
		for _, ws := range entry.WhitelistSlots {
			wsHash := common.BigToHash(uint256.NewInt(uint64(ws)).ToBig())
			for _, addr := range []common.Address{sender, routerAddr} {
				var key [64]byte
				copy(key[12:32], addr[:])
				copy(key[32:64], wsHash[:])
				wlSlot := crypto.Keccak256Hash(key[:])
				po.Slots = append(po.Slots, struct {
					Slot  common.Hash
					Value common.Hash
				}{Slot: wlSlot, Value: trueVal})
			}
		}

		// RouterAddressSlots: overwrite plain slots with the router address
		routerHash := common.BytesToHash(routerAddr[:])
		for _, rs := range entry.RouterAddressSlots {
			rsHash := common.BigToHash(uint256.NewInt(uint64(rs)).ToBig())
			po.Slots = append(po.Slots, struct {
				Slot  common.Hash
				Value common.Hash
			}{Slot: rsHash, Value: routerHash})
		}

		overrides = append(overrides, po)
	}

	// HookContracts overrides: replace hook contract code with return-false no-op
	hookSet := make(map[common.Address]bool)
	for token := range tokenSet {
		entry, ok := overrideMap[token]
		if !ok {
			continue
		}
		for _, hc := range entry.HookContracts {
			hookAddr := common.HexToAddress(hc)
			if !hookSet[hookAddr] {
				hookSet[hookAddr] = true
				overrides = append(overrides, pathfinder.ParsedOverride{
					Addr:    hookAddr,
					Balance: uint256.NewInt(0),
					Code:    []byte{0x60, 0x20, 0x5f, 0xf3},
				})
			}
		}
		for addr, hexCode := range entry.CodeContracts {
			codeAddr := common.HexToAddress(addr)
			if !hookSet[codeAddr] {
				hookSet[codeAddr] = true
				code, err := hex.DecodeString(strings.TrimPrefix(hexCode, "0x"))
				if err == nil && len(code) > 0 {
					overrides = append(overrides, pathfinder.ParsedOverride{
						Addr: codeAddr,
						Code: code,
					})
				}
			}
		}
	}

	return overrides
}
