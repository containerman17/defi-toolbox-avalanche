// Parity test: compare local EVM swap against node's eth_call for single pools.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/holiman/uint256"
)

func parityTest() {
	rpcURL := "http://127.0.0.1:9650/ext/bc/C/rpc"
	wsURL := "ws://127.0.0.1:9650/ext/bc/C/ws"
	block := uint64(80390594)

	// Load quoter bytecode
	bcHex, err := os.ReadFile("../quoting/data/quoter_bytecode.hex")
	if err != nil {
		panic(err)
	}
	contractCode, _ := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(string(bcHex), "0x")))
	fmt.Printf("Quoter bytecode: %d bytes\n", len(contractCode))

	// Fetch block info
	blockInfo := fetchBlockHTTP(rpcURL, block)
	fmt.Printf("Block %d, basefee=%d, timestamp=%d\n", blockInfo.Number, blockInfo.Basefee, blockInfo.Timestamp)

	// Connect WS
	ws := NewWSClient(wsURL)
	defer ws.Close()

	// Create shared cache + EVM
	cache := NewSharedStateCache(ws, block)
	evm := NewLocalEVM(cache, blockInfo.Number, blockInfo.Timestamp, blockInfo.Basefee, blockInfo.GasLimit)

	quoterAddr := common.HexToAddress("0x00000000000000000000000000000000DEADBEEF")
	dummyCaller := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	wavax := common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
	usdc := common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E")

	// Load token overrides
	tokenOverrides := loadTokenOverrides("../quoting/data/token_overrides.json")

	// Inject quoter bytecode
	evm.SetCode(quoterAddr, contractCode)

	hugeBalance := new(big.Int)
	hugeBalance.SetString("115792089237316195423570985008687907853269984665640564039457584007913129639935", 10) // type(uint256).max

	type testCase struct {
		name     string
		pool     common.Address
		poolType uint8
		tokenIn  common.Address
		tokenOut common.Address
		amount   string
	}

	usdt := common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7")
	_ = usdt

	// Use UniV3 as the test pool (it gives non-zero output)
	pool := common.HexToAddress("0xfAe3f424a0a47706811521E3ee268f00cFb5c45E")
	poolType := uint8(0)
	amount := new(big.Int)
	amount.SetString("1000000000000000000", 10)

	calldata := encodeSwap(
		[]common.Address{pool},
		[]uint8{poolType},
		[]common.Address{wavax, usdc},
		amount,
	)
	overridesSlots := getOverride(wavax, quoterAddr, hugeBalance, quoterAddr, tokenOverrides)

	// Get node result (ground truth)
	nodeOut, nodeErr := nodeQuoteMulti(rpcURL, block, quoterAddr, dummyCaller, contractCode, calldata, overridesSlots)
	if nodeErr != nil {
		fmt.Printf("NODE ERROR: %v\n", nodeErr)
		return
	}
	fmt.Printf("\nNODE (ground truth): %s\n", nodeOut.String())

	// Test matrix: toggle each fix independently
	type blockCtxOverride struct {
		name       string
		coinbase   common.Address
		difficulty int64
		randomByte byte // value at position [31]
	}

	// Current (FIXED) values
	fixedCoinbase := common.HexToAddress("0x0100000000000000000000000000000000000000")
	// Old (BROKEN) values
	zeroCoinbase := common.Address{}

	configs := []blockCtxOverride{
		{"ALL FIXED (coinbase=blackhole, diff=1, random=0x01)", fixedCoinbase, 1, 0x01},
		{"OLD: coinbase=zero, diff=0, random=zero", zeroCoinbase, 0, 0x00},
		{"ONLY coinbase wrong (zero)", zeroCoinbase, 1, 0x01},
		{"ONLY difficulty wrong (0)", fixedCoinbase, 0, 0x00},
		{"ONLY random wrong (zero)", fixedCoinbase, 1, 0x00},
	}

	// ========== 2-HOP PARITY TEST ==========
	fmt.Println("\n========== 2-HOP PARITY TEST ==========")
	{
		tokenMid := common.HexToAddress("0x152b9d0FdC40C096757F570A51E494bd4b943E50")
		pool1 := common.HexToAddress("0x856b38Bf1e2E367F747DD4d3951DDA8a35F1bF60")
		pool2 := common.HexToAddress("0xa7548448f4C774E3C3005BCfe81cD21B5925E91a")

		// Hop 1 alone (type=3, LFJ V2)
		evm.ResetMutable()
		evm.SetCode(quoterAddr, contractCode)
		ovrs1 := getOverride(wavax, quoterAddr, hugeBalance, quoterAddr, tokenOverrides)
		for _, o := range ovrs1 {
			evm.SetStateOverride(o.Addr, o.Slot, o.Value)
		}
		cd1 := encodeSwap([]common.Address{pool1}, []uint8{3}, []common.Address{wavax, tokenMid}, amount)
		ret1, _, err1 := evm.Call(dummyCaller, quoterAddr, cd1, 2_000_000)
		hop1Local := big.NewInt(0)
		if err1 == nil && len(ret1) >= 32 {
			hop1Local = new(big.Int).SetBytes(ret1[:32])
		}
		fmt.Printf("  Hop1 local (WAVAX→MID, type=3): %s\n", hop1Local)

		// Hop 1 via RPC
		hop1Node, _ := nodeQuoteMulti(rpcURL, block, quoterAddr, dummyCaller, contractCode, cd1, ovrs1)
		fmt.Printf("  Hop1 RPC: %s\n", hop1Node)

		// Hop 2 alone (type=4, DODO) with hop1 output
		if hop1Local.Sign() > 0 {
			evm.ResetMutable()
			evm.SetCode(quoterAddr, contractCode)
			ovrs2 := getOverride(tokenMid, quoterAddr, hugeBalance, quoterAddr, tokenOverrides)
			for _, o := range ovrs2 {
				evm.SetStateOverride(o.Addr, o.Slot, o.Value)
			}
			cd2 := encodeSwap([]common.Address{pool2}, []uint8{4}, []common.Address{tokenMid, usdc}, hop1Local)
			ret2, _, err2 := evm.Call(dummyCaller, quoterAddr, cd2, 2_000_000)
			hop2Local := big.NewInt(0)
			if err2 == nil && len(ret2) >= 32 {
				hop2Local = new(big.Int).SetBytes(ret2[:32])
			}
			fmt.Printf("  Hop2 local (MID→USDC, type=4, input=%s): %s\n", hop1Local, hop2Local)

			hop2Node, _ := nodeQuoteMulti(rpcURL, block, quoterAddr, dummyCaller, contractCode, cd2, ovrs2)
			fmt.Printf("  Hop2 RPC: %s\n", hop2Node)
		}

		// Full 2-hop atomic
		evm.ResetMutable()
		evm.SetCode(quoterAddr, contractCode)
		ovrsAtom := getOverride(wavax, quoterAddr, hugeBalance, quoterAddr, tokenOverrides)
		for _, o := range ovrsAtom {
			evm.SetStateOverride(o.Addr, o.Slot, o.Value)
		}
		cdAtom := encodeSwap(
			[]common.Address{pool1, pool2},
			[]uint8{3, 4},
			[]common.Address{wavax, tokenMid, usdc},
			amount,
		)
		retAtom, _, errAtom := evm.Call(dummyCaller, quoterAddr, cdAtom, 2_000_000)
		atomicLocal := big.NewInt(0)
		if errAtom == nil && len(retAtom) >= 32 {
			atomicLocal = new(big.Int).SetBytes(retAtom[:32])
		}
		fmt.Printf("  Atomic 2-hop local: %s\n", atomicLocal)

		atomicNode, _ := nodeQuoteMulti(rpcURL, block, quoterAddr, dummyCaller, contractCode, cdAtom, ovrsAtom)
		fmt.Printf("  Atomic 2-hop RPC: %s\n", atomicNode)

		if atomicLocal.Sign() > 0 && atomicNode.Sign() > 0 {
			delta := new(big.Int).Sub(atomicLocal, atomicNode)
			fmt.Printf("  DELTA (local - RPC): %s wei\n", delta)
		}
	}

	fmt.Println("\n========== BLOCK CONTEXT A/B TEST ==========")
	for _, cfg := range configs {
		// Reset
		evm.ResetMutable()
		evm.SetCode(quoterAddr, contractCode)
		for _, o := range overridesSlots {
			evm.SetStateOverride(o.Addr, o.Slot, o.Value)
		}

		// Temporarily patch block context via a custom call
		ret, _, err := callWithContext(evm, dummyCaller, quoterAddr, calldata, 2_000_000,
			cfg.coinbase, cfg.difficulty, cfg.randomByte)
		if err != nil {
			fmt.Printf("  %-50s  ERROR: %v\n", cfg.name, err)
			continue
		}
		localOut := decodeSwapResult(ret)
		delta := new(big.Int).Sub(localOut, nodeOut)
		match := "MATCH"
		if delta.Sign() != 0 {
			match = fmt.Sprintf("DELTA=%s", delta.String())
		}
		fmt.Printf("  %-50s  %s  [%s]\n", cfg.name, localOut.String(), match)
	}
}

func callWithContext(e *LocalEVM, from, to common.Address, data []byte, gasLimit uint64,
	coinbase common.Address, difficulty int64, randomByte byte) ([]byte, uint64, error) {

	random := common.Hash{}
	random[31] = randomByte

	blockCtx := vm.BlockContext{
		CanTransfer: canTransfer,
		Transfer:    transfer,
		GetHash:     getHashFn(e.blockNum),
		Coinbase:    coinbase,
		BlockNumber: new(big.Int).SetUint64(e.blockNum),
		Time:        e.blockTimestamp,
		Difficulty:  big.NewInt(difficulty),
		Random:      &random,
		GasLimit:    e.gasLimit,
		BaseFee:     new(big.Int).SetUint64(e.baseFee),
	}
	txCtx := vm.TxContext{
		Origin:   from,
		GasPrice: big.NewInt(0),
	}
	rules := e.chainCfg.Rules(blockCtx.BlockNumber, blockCtx.Random != nil, blockCtx.Time)
	e.state.Prepare(rules, from, blockCtx.Coinbase, &to, vm.ActivePrecompiles(rules), nil)
	vmConfig := vm.Config{}
	evmInst := vm.NewEVM(blockCtx, txCtx, e.state, e.chainCfg, vmConfig)
	value := uint256.NewInt(0)
	caller := vm.AccountRef(from)
	ret, gasLeft, err := evmInst.Call(caller, to, data, gasLimit, value)
	gasUsed := gasLimit - gasLeft
	if err != nil {
		return ret, gasUsed, fmt.Errorf("evm call: %w", err)
	}
	return ret, gasUsed, nil
}

func decodeSwapResult(data []byte) *big.Int {
	if len(data) < 32 {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(data[0:32])
}

func padL(b []byte, size int) []byte {
	if len(b) >= size {
		return b[len(b)-size:]
	}
	r := make([]byte, size)
	copy(r[size-len(b):], b)
	return r
}

func nodeQuoteMulti(rpcURL string, block uint64, quoterAddr, caller common.Address, bytecode, calldata []byte, stateOverrides []struct {
	Addr  common.Address
	Slot  common.Hash
	Value common.Hash
}) (*big.Int, error) {
	// Build state override map
	override := map[string]interface{}{
		quoterAddr.Hex(): map[string]interface{}{
			"code": "0x" + hex.EncodeToString(bytecode),
		},
	}
	// Add token balance/allowance overrides
	for _, o := range stateOverrides {
		addrHex := o.Addr.Hex()
		if _, ok := override[addrHex]; !ok {
			override[addrHex] = map[string]interface{}{
				"stateDiff": map[string]string{},
			}
		}
		entry := override[addrHex].(map[string]interface{})
		if _, ok := entry["stateDiff"]; !ok {
			entry["stateDiff"] = map[string]string{}
		}
		entry["stateDiff"].(map[string]string)[o.Slot.Hex()] = o.Value.Hex()
	}
	params := []interface{}{
		map[string]interface{}{
			"from": caller.Hex(),
			"to":   quoterAddr.Hex(),
			"data": "0x" + hex.EncodeToString(calldata),
			"gas":  "0x1e8480",
		},
		fmt.Sprintf("0x%x", block),
		override,
	}
	body := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "eth_call", "params": params}
	data, _ := json.Marshal(body)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(rpcURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(b, &rpcResp)
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc: %s", rpcResp.Error.Message)
	}
	retData, _ := hex.DecodeString(strings.TrimPrefix(rpcResp.Result, "0x"))
	return decodeSwapResult(retData), nil
}

func fetchBlockHTTP(rpcURL string, num uint64) BlockInfo {
	body := map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "eth_getBlockByNumber",
		"params": []interface{}{fmt.Sprintf("0x%x", num), false},
	}
	data, _ := json.Marshal(body)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, _ := client.Post(rpcURL, "application/json", bytes.NewReader(data))
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var rpcResp struct {
		Result struct {
			Number        string `json:"number"`
			Timestamp     string `json:"timestamp"`
			BaseFeePerGas string `json:"baseFeePerGas"`
			GasLimit      string `json:"gasLimit"`
		} `json:"result"`
	}
	json.Unmarshal(b, &rpcResp)
	parseH := func(s string) uint64 {
		var v uint64
		fmt.Sscanf(strings.TrimPrefix(s, "0x"), "%x", &v)
		return v
	}
	return BlockInfo{
		Number:    parseH(rpcResp.Result.Number),
		Timestamp: parseH(rpcResp.Result.Timestamp),
		Basefee:   parseH(rpcResp.Result.BaseFeePerGas),
		GasLimit:  parseH(rpcResp.Result.GasLimit),
	}
}
