package router

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

//go:embed contracts/bytecode.hex
var bytecodeHex string

//go:embed data/token_overrides.json
var tokenOverridesJSON string

type tokenOverrideEntry struct {
	Address      string `json:"address"`
	Slot         int    `json:"slot"`
	ERC7201Base  string `json:"erc7201_base,omitempty"`
	Shift        int    `json:"shift,omitempty"`
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

// RouterBytecode returns the decoded router contract bytecode.
func RouterBytecode() []byte {
	code, _ := hex.DecodeString(strings.TrimSpace(bytecodeHex))
	return code
}

// computeBalanceSlot computes keccak256(abi.encode(holder, slot)) for standard ERC20 mapping.
func computeBalanceSlot(holder common.Address, entry *tokenOverrideEntry) common.Hash {
	var slotKey [64]byte
	copy(slotKey[12:32], holder[:]) // address left-padded to 32 bytes

	if entry.ERC7201Base != "" {
		base := common.HexToHash(entry.ERC7201Base)
		copy(slotKey[32:64], base[:])
	} else {
		slotHash := common.BigToHash(uint256.NewInt(uint64(entry.Slot)).ToBig())
		copy(slotKey[32:64], slotHash[:])
	}

	return crypto.Keccak256Hash(slotKey[:])
}

// BuildOverrides creates state overrides for the router + token balances for all tokens in pools.
func BuildOverrides(routerAddr common.Address, pools []pathfinder.Pool) []pathfinder.ParsedOverride {
	bytecode := RouterBytecode()

	// Router bytecode override
	overrides := []pathfinder.ParsedOverride{{
		Addr:    routerAddr,
		Balance: uint256.NewInt(0),
		Code:    bytecode,
	}}

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
	}

	return overrides
}
