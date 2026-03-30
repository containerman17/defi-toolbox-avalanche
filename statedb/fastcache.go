package statedb

import "github.com/ava-labs/libevm/common"

// StorageKey is a flat key for fast storage lookups (address + slot).
type StorageKey [52]byte

func MakeStorageKey(addr common.Address, slot common.Hash) StorageKey {
	var k StorageKey
	copy(k[:20], addr[:])
	copy(k[20:], slot[:])
	return k
}

// FastCache provides O(1) flat-map storage reads built from a StateDB.
// Used by formula quoting to avoid the two-level map lookup overhead.
type FastCache struct {
	storage map[StorageKey]common.Hash
}

// BuildFastCache creates a FastCache from an ImmutableState's storage.
func BuildFastCache(im *ImmutableState) *FastCache {
	fc := &FastCache{storage: make(map[StorageKey]common.Hash)}
	for addr, slots := range im.Storage {
		for slot, val := range slots {
			fc.storage[MakeStorageKey(addr, slot)] = val
		}
	}
	return fc
}

// GetState reads a storage slot from the flat cache.
// Returns (value, true) if found, (empty, false) if not cached.
func (fc *FastCache) GetState(addr common.Address, slot common.Hash) (common.Hash, bool) {
	val, ok := fc.storage[MakeStorageKey(addr, slot)]
	return val, ok
}

// Len returns the number of cached entries.
func (fc *FastCache) Len() int {
	return len(fc.storage)
}
