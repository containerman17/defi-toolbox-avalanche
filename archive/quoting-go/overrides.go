package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// TokenOverride describes how to compute balance/allowance storage slots for a token.
type TokenOverride struct {
	Slot          int32
	AllowanceSlot int32
	Base          common.Hash
	AllowanceBase common.Hash
	Shift         uint32
}

func (o *TokenOverride) isERC7201() bool {
	return o.Base != (common.Hash{})
}

type jsonEntry struct {
	Address          string  `json:"address"`
	Slot             int32   `json:"slot"`
	AllowanceSlot    *int32  `json:"allowance_slot,omitempty"`
	ERC7201Base      *string `json:"erc7201_base,omitempty"`
	ERC7201Allowance *string `json:"erc7201_allowance,omitempty"`
	Shift            uint32  `json:"shift"`
}

func loadTokenOverrides(path string) map[common.Address]*TokenOverride {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("read %s: %v", path, err))
	}
	var entries []jsonEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		panic(fmt.Sprintf("parse %s: %v", path, err))
	}

	m := make(map[common.Address]*TokenOverride, len(entries))
	for _, e := range entries {
		addr := common.HexToAddress(e.Address)
		ovr := &TokenOverride{}

		if e.ERC7201Base != nil {
			ovr.Base = parseB256(*e.ERC7201Base)
			if e.ERC7201Allowance != nil {
				ovr.AllowanceBase = parseB256(*e.ERC7201Allowance)
			}
			ovr.Slot = 0
			ovr.AllowanceSlot = -1
			ovr.Shift = e.Shift
		} else {
			ovr.Slot = e.Slot
			if e.AllowanceSlot != nil {
				ovr.AllowanceSlot = *e.AllowanceSlot
			} else {
				ovr.AllowanceSlot = -1
			}
			ovr.Base = common.Hash{}
			ovr.AllowanceBase = common.Hash{}
			ovr.Shift = 0
		}

		m[addr] = ovr
	}

	fmt.Fprintf(os.Stderr, "loaded %d token overrides from %s\n", len(m), path)
	return m
}

// getOverride computes storage overrides (balance + allowance slots) for a token.
// Returns slice of (address, slot, value) triples.
func getOverride(token, account common.Address, balance *big.Int, spender common.Address, tokens map[common.Address]*TokenOverride) []struct {
	Addr  common.Address
	Slot  common.Hash
	Value common.Hash
} {
	ovr, ok := tokens[token]
	if !ok {
		return nil
	}

	var result []struct {
		Addr  common.Address
		Slot  common.Hash
		Value common.Hash
	}

	// Balance slot
	balanceSlot := computeBalanceSlot(account, ovr)
	balanceValue := new(big.Int).Set(balance)
	if ovr.Shift > 0 {
		balanceValue.Lsh(balanceValue, uint(ovr.Shift))
	}
	result = append(result, struct {
		Addr  common.Address
		Slot  common.Hash
		Value common.Hash
	}{token, balanceSlot, common.BigToHash(balanceValue)})

	// Allowance slot
	allowanceSlot := computeAllowanceSlot(account, spender, ovr)
	result = append(result, struct {
		Addr  common.Address
		Slot  common.Hash
		Value common.Hash
	}{token, allowanceSlot, common.BigToHash(balance)})

	return result
}

func computeBalanceSlot(account common.Address, ovr *TokenOverride) common.Hash {
	if ovr.isERC7201() {
		return keccak256AddressHash(account, ovr.Base)
	}
	return keccak256AddressUint(account, uint64(ovr.Slot))
}

func computeAllowanceSlot(owner, spender common.Address, ovr *TokenOverride) common.Hash {
	var innerHash common.Hash
	if ovr.isERC7201() {
		base := ovr.Base
		if ovr.AllowanceBase != (common.Hash{}) {
			base = ovr.AllowanceBase
		}
		innerHash = keccak256AddressHash(owner, base)
	} else {
		slot := uint64(ovr.Slot + 1)
		if ovr.AllowanceSlot >= 0 {
			slot = uint64(ovr.AllowanceSlot)
		}
		innerHash = keccak256AddressUint(owner, slot)
	}
	return keccak256AddressHash(spender, innerHash)
}

// keccak256(abi.encode(address, uint256))
func keccak256AddressUint(addr common.Address, slot uint64) common.Hash {
	var data [64]byte
	copy(data[12:32], addr.Bytes())
	slotBytes := new(big.Int).SetUint64(slot).Bytes()
	copy(data[64-len(slotBytes):64], slotBytes)
	return crypto.Keccak256Hash(data[:])
}

// keccak256(abi.encode(address, bytes32))
func keccak256AddressHash(addr common.Address, hash common.Hash) common.Hash {
	var data [64]byte
	copy(data[12:32], addr.Bytes())
	copy(data[32:64], hash.Bytes())
	return crypto.Keccak256Hash(data[:])
}

func parseB256(hexStr string) common.Hash {
	clean := strings.TrimPrefix(hexStr, "0x")
	b, err := hex.DecodeString(clean)
	if err != nil {
		panic(fmt.Sprintf("invalid hex: %s", hexStr))
	}
	var buf [32]byte
	start := 32 - len(b)
	copy(buf[start:], b)
	return common.Hash(buf)
}
