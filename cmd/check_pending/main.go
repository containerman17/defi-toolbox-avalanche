package main

import (
    "fmt"
    "math/big"
    "os"
    
    "defi-toolbox/statedb"
    "github.com/ava-labs/libevm/common"
)

func main() {
    pools := []string{
        "0x41100C6D2c6920B10d12Cd8D59c8A9AA2eF56fC7",
        "0x668Aa7AEfa8512416Fc6244afBe5129200277A69",
    }
    
    db, err := statedb.NewStateDB(os.Getenv("HOME") + "/chaindata")
    if err != nil {
        panic(err)
    }
    
    cfg, state, err := db.GetLatestState()
    if err != nil {
        panic(err)
    }
    _ = cfg
    
    mask104 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 104), big.NewInt(1))
    
    for _, pool := range pools {
        addr := common.HexToAddress(pool)
        
        // Read slot 4
        slot4 := state.GetState(addr, common.BigToHash(big.NewInt(4)))
        val4 := new(big.Int).SetBytes(slot4[:])
        feePending0 := new(big.Int).And(val4, mask104)
        feePending1 := new(big.Int).And(new(big.Int).Rsh(val4, 104), mask104)
        
        // Read globalState slot 2
        slot2 := state.GetState(addr, common.BigToHash(big.NewInt(2)))
        val2 := new(big.Int).SetBytes(slot2[:])
        pluginConfig := uint8(new(big.Int).Rsh(val2, 200).Int64() & 0xFF)
        communityFee := uint32(new(big.Int).Rsh(val2, 208).Int64() & 0xFFFF)
        
        fmt.Printf("Pool %s:\n", pool)
        fmt.Printf("  pluginConfig=0x%02x communityFee=%d\n", pluginConfig, communityFee)
        fmt.Printf("  feePending0=%s feePending1=%s\n", feePending0.String(), feePending1.String())
        fmt.Printf("  feePending0 > 1e12: %v\n", feePending0.Cmp(big.NewInt(1_000_000_000_000)) > 0)
        fmt.Println()
    }
}
