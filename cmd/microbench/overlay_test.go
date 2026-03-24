package main

import (
	"testing"

	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func BenchmarkNewOverlay(b *testing.B) {
	base := statedb.NewStateDB(nil)
	// Add some data like a real state
	for i := 0; i < 1000; i++ {
		var addr common.Address
		addr[19] = byte(i)
		addr[18] = byte(i >> 8)
		base.SetAccount(addr, uint256.NewInt(0), 0, []byte{0x60, 0x00})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = base.NewOverlay()
	}
}

func BenchmarkNewOverlayAndGetState(b *testing.B) {
	base := statedb.NewStateDB(nil)
	var addr common.Address
	addr[19] = 1
	base.SetAccount(addr, uint256.NewInt(0), 0, []byte{0x60, 0x00})
	var slot common.Hash
	slot[31] = 8
	base.SetStorageSlot(addr, slot, common.HexToHash("0x1234"))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		overlay := base.NewOverlay()
		overlay.GetState(addr, slot)
	}
}
