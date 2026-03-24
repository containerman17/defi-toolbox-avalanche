package main

import (
	"testing"

	"github.com/ava-labs/libevm/common"
)

// Simulate two-level map lookup (current StateDB pattern)
func BenchmarkTwoLevelMap(b *testing.B) {
	// Build: outer map[Address] -> inner map[Hash]Hash
	outer := make(map[common.Address]map[common.Hash]common.Hash)
	for i := 0; i < 10000; i++ {
		addr := common.BigToAddress(common.Big1)
		addr[19] = byte(i)
		addr[18] = byte(i >> 8)
		inner := make(map[common.Hash]common.Hash)
		for j := 0; j < 40; j++ {
			var slot common.Hash
			slot[31] = byte(j)
			var val common.Hash
			val[31] = byte(j + 1)
			inner[slot] = val
		}
		outer[addr] = inner
	}

	// Random-ish access pattern
	var addrs []common.Address
	for addr := range outer {
		addrs = append(addrs, addr)
	}
	slot := common.Hash{}
	slot[31] = 8 // slot 8

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		addr := addrs[i%len(addrs)]
		inner := outer[addr]
		_ = inner[slot]
	}
}

// Simulate flat map lookup (concatenated key)
func BenchmarkFlatMap(b *testing.B) {
	type key52 [52]byte
	flat := make(map[key52]common.Hash)
	var addrs []common.Address
	for i := 0; i < 10000; i++ {
		var addr common.Address
		addr[19] = byte(i)
		addr[18] = byte(i >> 8)
		addrs = append(addrs, addr)
		for j := 0; j < 40; j++ {
			var k key52
			copy(k[:20], addr[:])
			k[51] = byte(j)
			var val common.Hash
			val[31] = byte(j + 1)
			flat[k] = val
		}
	}

	var slot common.Hash
	slot[31] = 8

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var k key52
		copy(k[:20], addrs[i%len(addrs)][:])
		copy(k[20:], slot[:])
		_ = flat[k]
	}
}
