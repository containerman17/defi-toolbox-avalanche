package formulas

// balancer_v2.go — Balancer V2 Vault storage slot helpers.
//
// The Balancer V2 Vault is a singleton at 0xBA12222222228d8Ba445958a75a0704d566BF2C8.
// It stores per-pool balances in three data structures depending on pool specialization:
//
//   Specialization 0 (GENERAL):             GeneralPoolsBalance
//   Specialization 1 (MINIMAL_SWAP_INFO):   MinimalSwapInfoPoolsBalance
//   Specialization 2 (TWO_TOKEN):           TwoTokenPoolsBalance
//
// The poolId encodes pool address + specialization + nonce:
//   | 20 bytes pool address | 2 bytes specialization | 10 bytes nonce |
//   MSB                                                             LSB
//
// Token balances are packed in a bytes32: lower 112 bits = cash, next 112 bits = managed,
// upper 32 bits = lastChangeBlock.  total = cash + managed.
//
// Vault storage layout (C3 linearization order):
//   slot 0:  ReentrancyGuard._status
//   slot 1:  TemporarilyPausable._paused
//   slot 2:  SignaturesValidator._nextNonce         (mapping address→uint256)
//   slot 3:  PoolRegistry._isPoolRegistered         (mapping bytes32→bool)
//   slot 4:  PoolRegistry._nextPoolNonce
//   slot 5:  MinimalSwapInfoPoolsBalance._minimalSwapInfoPoolsBalances
//              mapping(bytes32 poolId => mapping(IERC20 token => bytes32 balance))
//   slot 6:  MinimalSwapInfoPoolsBalance._minimalSwapInfoPoolsTokens
//              mapping(bytes32 poolId => EnumerableSet.AddressSet)
//   slot 7:  TwoTokenPoolsBalance._twoTokenPoolTokens
//              mapping(bytes32 poolId => TwoTokenPoolTokens)
//              where TwoTokenPoolTokens = { tokenA, tokenB, mapping(bytes32 pairHash => TwoTokenPoolBalances) }
//   slot 8:  GeneralPoolsBalance._generalPoolsBalances (not used here)
//   ...
//
// NOTE: slots were empirically verified using known pool data.

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// Balancer V2 Vault address (same on all chains).
var balV2VaultAddr = common.HexToAddress("0xBA12222222228d8Ba445958a75a0704d566BF2C8")

// Vault storage slot numbers.
const (
	balV2SlotMinimalSwapInfoBalances = 5
	balV2SlotTwoTokenPoolTokens      = 7
)

// Pool specialization encoded in poolId bytes 10-11 (from LSB).
const (
	balV2SpecGeneral        = 0
	balV2SpecMinimalSwapInfo = 1
	balV2SpecTwoToken       = 2
)

// balV2PoolSpecialization extracts the specialization from a poolId.
// poolId layout: | 20B pool addr | 2B specialization | 10B nonce |   (MSB to LSB)
// In bytes32 big-endian: bytes[0..19] = addr, bytes[20..21] = spec, bytes[22..31] = nonce.
func balV2PoolSpecialization(poolId [32]byte) int {
	// spec is at bytes 20-21 (big-endian), i.e., bits 80-95 from LSB.
	spec := (int(poolId[20]) << 8) | int(poolId[21])
	return spec
}

// balV2MappingSlot computes keccak256(key ++ slot32) for a simple mapping(bytes32 => ...).
// key is 32 bytes, slot is the mapping's storage slot number.
func balV2MappingSlot32(key [32]byte, slot int) common.Hash {
	var data [64]byte
	copy(data[0:32], key[:])
	big.NewInt(int64(slot)).FillBytes(data[32:64])
	return crypto.Keccak256Hash(data[:])
}

// balV2ExtractBalance extracts total balance (cash + managed) from a packed bytes32.
// Layout: [32b lastChangeBlock | 112b managed | 112b cash]  (MSB first)
func balV2ExtractBalance(packed common.Hash) *big.Int {
	val := new(big.Int).SetBytes(packed[:])
	mask112 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
	cash := new(big.Int).And(val, mask112)
	managed := new(big.Int).And(new(big.Int).Rsh(val, 112), mask112)
	return new(big.Int).Add(cash, managed)
}

// balV2ReadMinimalSwapInfoBalance reads a token's total balance from the
// MinimalSwapInfoPoolsBalance mapping.
//   slot: keccak256(token_padded ++ keccak256(poolId ++ slot5))
func balV2ReadMinimalSwapInfoBalance(reader StorageReader, poolId [32]byte, token common.Address) *big.Int {
	// outer key = poolId, slot = 5 → keccak256(poolId ++ 5)
	outerSlot := balV2MappingSlot32(poolId, balV2SlotMinimalSwapInfoBalances)
	// inner key = token address → keccak256(token_padded ++ outerSlot)
	innerSlot := balV2MappingSlotAddrHash(token, outerSlot)
	packed := reader(balV2VaultAddr, innerSlot)
	return balV2ExtractBalance(packed)
}

// balV2MappingSlotAddrHash computes keccak256(addr_padded_to_32 ++ hashKey) where hashKey is a bytes32.
func balV2MappingSlotAddrHash(key common.Address, slot common.Hash) common.Hash {
	var data [64]byte
	copy(data[12:32], key.Bytes())
	copy(data[32:64], slot[:])
	return crypto.Keccak256Hash(data[:])
}

// balV2MappingSlot32Hash computes keccak256(key32 ++ hashKey) where both are bytes32.
func balV2MappingSlot32Hash(key [32]byte, slot common.Hash) common.Hash {
	var data [64]byte
	copy(data[0:32], key[:])
	copy(data[32:64], slot[:])
	return crypto.Keccak256Hash(data[:])
}

// balV2ReadTwoTokenBalances reads the tokenA and tokenB balances from the
// TwoTokenPoolsBalance structure for a given poolId.
//
// Storage layout of TwoTokenPoolTokens struct at slot keccak256(poolId ++ slot7):
//   - sub-slot 0: tokenA  (address, lower 20 bytes)
//   - sub-slot 1: tokenB  (address, lower 20 bytes)
//   - sub-slot 2+: balances mapping (pairHash => TwoTokenPoolBalances)
//     TwoTokenPoolBalances = { sharedCash (bytes32), sharedManaged (bytes32) }
//
// The struct base slot is keccak256(poolId ++ 7).
// tokenA is at baseSlot + 0, tokenB at baseSlot + 1.
// The balances mapping is at baseSlot + 2.
//
// pairHash = keccak256(abi.encodePacked(tokenA, tokenB))
// sharedCash slot = keccak256(pairHash ++ (baseSlot+2))
//   sharedCash packs [cash_A (112b) | cash_B (112b) | lastChangeBlock (32b)]  LSB first:
//   bits 0-111 = cashA, bits 112-223 = cashB
//
// totalA = cashA + managedA, where managedA = sharedManaged bits 0-111
// totalB = cashB + managedB, where managedB = sharedManaged bits 112-223
func balV2ReadTwoTokenBalances(reader StorageReader, poolId [32]byte, tokenA, tokenB common.Address) (balA, balB *big.Int) {
	// struct base = keccak256(poolId ++ 7)
	baseSlot := balV2MappingSlot32(poolId, balV2SlotTwoTokenPoolTokens)

	// The balances sub-mapping is at baseSlot+2 (after tokenA, tokenB fields)
	balancesMappingSlot := addToHash(baseSlot, 2)

	// pairHash = keccak256(abi.encodePacked(tokenA, tokenB))
	// NOTE: tokens must be sorted (tokenA < tokenB)
	pairHash := crypto.Keccak256Hash(tokenA.Bytes(), tokenB.Bytes())

	// sharedCash slot = keccak256(pairHash ++ balancesMappingSlot)
	sharedCashSlot := balV2MappingSlot32Hash([32]byte(pairHash), balancesMappingSlot)
	sharedManagedSlot := addToHash(sharedCashSlot, 1)

	sharedCash := reader(balV2VaultAddr, sharedCashSlot)
	sharedManaged := reader(balV2VaultAddr, sharedManagedSlot)

	mask112 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))

	cashVal := new(big.Int).SetBytes(sharedCash[:])
	managedVal := new(big.Int).SetBytes(sharedManaged[:])

	cashA := new(big.Int).And(cashVal, mask112)
	cashB := new(big.Int).And(new(big.Int).Rsh(cashVal, 112), mask112)
	managedA := new(big.Int).And(managedVal, mask112)
	managedB := new(big.Int).And(new(big.Int).Rsh(managedVal, 112), mask112)

	balA = new(big.Int).Add(cashA, managedA)
	balB = new(big.Int).Add(cashB, managedB)
	return
}

// addToHash adds an integer offset to a storage slot hash (for struct field access).
func addToHash(h common.Hash, offset int64) common.Hash {
	n := new(big.Int).SetBytes(h[:])
	n.Add(n, big.NewInt(offset))
	var result common.Hash
	n.FillBytes(result[:])
	return result
}
