package main

import (
	"testing"

	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
)

func BenchmarkStateDBColdOverlay(b *testing.B) {
	base := statedb.NewStateDB(nil)

	var addrs []common.Address
	slot8 := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000008")

	for i := 0; i < 10000; i++ {
		var addr common.Address
		addr[19] = byte(i)
		addr[18] = byte(i >> 8)
		addrs = append(addrs, addr)
		for j := 0; j < 40; j++ {
			var slot common.Hash
			slot[31] = byte(j)
			var val common.Hash
			val[31] = byte(j + 1)
			base.SetStorageSlot(addr, slot, val)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh overlay each time — simulates BFS per-call pattern
		overlay := base.NewOverlay()
		addr := addrs[i%len(addrs)]
		overlay.GetState(addr, slot8)
	}
}
