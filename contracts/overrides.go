package contracts

// Token balance/allowance overrides for EVM swap simulation.
// The router's debugSwapSingle does real token transfers, so the sender
// needs balance + allowance on every token. This computes the correct
// storage slots from token_overrides.json and writes them to a StateView.

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"

	lc "defi-toolbox/lightclient"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

//go:embed token_overrides.json
var tokenOverridesJSON string

type tokenOverrideEntry struct {
	Address        string            `json:"address"`
	Slot           int               `json:"slot"`
	AllowanceSlot  *int              `json:"allowance_slot,omitempty"`
	ERC7201Base    string            `json:"erc7201_base,omitempty"`
	Shift          int               `json:"shift,omitempty"`
	Vyper          bool              `json:"vyper,omitempty"`
	HookContracts  []string          `json:"hookContracts,omitempty"`
	DisableSlots   []int             `json:"disableSlots,omitempty"`
	WhitelistSlots []int             `json:"whitelistSlots,omitempty"`
	RouterAddressSlots []int         `json:"routerAddressSlots,omitempty"`
	CodeContracts  map[string]string `json:"codeContracts,omitempty"`
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

func computeBalanceSlot(holder common.Address, entry *tokenOverrideEntry) common.Hash {
	var slotKey [64]byte
	if entry.Vyper {
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

func computeAllowanceSlot(owner, spender common.Address, entry *tokenOverrideEntry) common.Hash {
	aSlot := entry.Slot + 1
	if entry.AllowanceSlot != nil {
		aSlot = *entry.AllowanceSlot
	}

	if entry.ERC7201Base != "" {
		baseHash := common.HexToHash(entry.ERC7201Base)
		baseInt := new(uint256.Int).SetBytes(baseHash[:])
		allowanceBase := common.Hash(new(uint256.Int).Add(baseInt, uint256.NewInt(1)).Bytes32())
		var inner [64]byte
		copy(inner[12:32], owner[:])
		copy(inner[32:64], allowanceBase[:])
		innerSlot := crypto.Keccak256Hash(inner[:])
		var outer [64]byte
		copy(outer[12:32], spender[:])
		copy(outer[32:64], innerSlot[:])
		return crypto.Keccak256Hash(outer[:])
	} else if entry.Vyper {
		allowanceBase := common.BigToHash(uint256.NewInt(uint64(aSlot)).ToBig())
		var inner [64]byte
		copy(inner[0:32], allowanceBase[:])
		copy(inner[44:64], owner[:])
		innerSlot := crypto.Keccak256Hash(inner[:])
		var outer [64]byte
		copy(outer[0:32], innerSlot[:])
		copy(outer[44:64], spender[:])
		return crypto.Keccak256Hash(outer[:])
	}

	allowanceBase := common.BigToHash(uint256.NewInt(uint64(aSlot)).ToBig())
	var inner [64]byte
	copy(inner[12:32], owner[:])
	copy(inner[32:64], allowanceBase[:])
	innerSlot := crypto.Keccak256Hash(inner[:])
	var outer [64]byte
	copy(outer[12:32], spender[:])
	copy(outer[32:64], innerSlot[:])
	return crypto.Keccak256Hash(outer[:])
}

// ApplyTokenOverrides sets up balance + allowance + hook/whitelist overrides
// on a StateView so that EVM swap calls (debugSwapSingle) work.
// The sender gets large balance + max allowance for every known token.
// The router gets implementation bytecode injected.
func ApplyTokenOverrides(sv *lc.StateView, sender, routerAddr common.Address, tokens []common.Address) {
	largeBalance := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(36))
	maxUint256 := new(uint256.Int).Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 256), uint256.NewInt(1))
	trueVal := common.Hash(uint256.NewInt(1).Bytes32())
	routerHash := common.BytesToHash(routerAddr[:])

	// Inject router implementation bytecode.
	sv.SetCode(routerAddr, RouterBytecode())

	hookSet := make(map[common.Address]bool)

	for _, token := range tokens {
		entry, ok := overrideMap[token]
		if !ok {
			continue
		}

		// Balance for sender.
		balSlot := computeBalanceSlot(sender, entry)
		balVal := largeBalance
		if entry.Shift > 0 {
			balVal = new(uint256.Int).Lsh(largeBalance, uint(entry.Shift))
		}
		sv.SetState(token, balSlot, common.Hash(balVal.Bytes32()))

		// Balance for router (debugSwapSingle needs router to hold tokens).
		routerBalSlot := computeBalanceSlot(routerAddr, entry)
		sv.SetState(token, routerBalSlot, common.Hash(balVal.Bytes32()))

		// Allowance: sender → router.
		allowSlot := computeAllowanceSlot(sender, routerAddr, entry)
		sv.SetState(token, allowSlot, common.Hash(maxUint256.Bytes32()))

		// DisableSlots.
		for _, ds := range entry.DisableSlots {
			dsHash := common.BigToHash(uint256.NewInt(uint64(ds)).ToBig())
			sv.SetState(token, dsHash, common.Hash{})
		}

		// WhitelistSlots for both sender and router.
		for _, ws := range entry.WhitelistSlots {
			wsHash := common.BigToHash(uint256.NewInt(uint64(ws)).ToBig())
			for _, addr := range []common.Address{sender, routerAddr} {
				var key [64]byte
				copy(key[12:32], addr[:])
				copy(key[32:64], wsHash[:])
				wlSlot := crypto.Keccak256Hash(key[:])
				sv.SetState(token, wlSlot, trueVal)
			}
		}

		// RouterAddressSlots.
		for _, rs := range entry.RouterAddressSlots {
			rsHash := common.BigToHash(uint256.NewInt(uint64(rs)).ToBig())
			sv.SetState(token, rsHash, routerHash)
		}

		// HookContracts: no-op bytecode.
		for _, hc := range entry.HookContracts {
			hookAddr := common.HexToAddress(hc)
			if !hookSet[hookAddr] {
				hookSet[hookAddr] = true
				sv.SetCode(hookAddr, []byte{0x60, 0x20, 0x5f, 0xf3})
			}
		}

		// CodeContracts: deploy real bytecode.
		for addr, hexCode := range entry.CodeContracts {
			codeAddr := common.HexToAddress(addr)
			if !hookSet[codeAddr] {
				hookSet[codeAddr] = true
				code, err := hex.DecodeString(strings.TrimPrefix(hexCode, "0x"))
				if err == nil && len(code) > 0 {
					sv.SetCode(codeAddr, code)
				}
			}
		}
	}
}
