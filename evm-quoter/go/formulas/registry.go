package formulas

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Formula IDs
const (
	FormulaV2_30bps  = 0  // V2 constant product, 0.3% fee
	FormulaPharaohV1 = 1  // Pharaoh V1 (stable/volatile with registry)
	FormulaV3        = 2  // Uniswap V3 / Pharaoh V3 tick-walking
	FormulaInvalid   = -1 // Do not use formula (FoT, broken, custom fee)
)

// Registry maps pool addresses to formula IDs.
type Registry struct {
	pools map[common.Address]int
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{pools: make(map[common.Address]int)}
}

// LoadRegistry loads a registry file. Format: address:formula_id (one per line).
// Lines starting with # are comments. Missing file returns an empty registry.
func LoadRegistry(path string) *Registry {
	r := &Registry{pools: make(map[common.Address]int)}

	f, err := os.Open(path)
	if err != nil {
		return r // Empty registry — all pools use EVM
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		addr := common.HexToAddress(parts[0])
		var id int
		fmt.Sscanf(parts[1], "%d", &id)
		r.pools[addr] = id
	}

	return r
}

// executeSwap selector: keccak256("executeSwap(address[],uint8[],address[],uint256[],bytes[])")[:4]
var executeSwapSelector = []byte{0x32, 0x39, 0x33, 0x4d}

// TryQuote attempts to quote a call using formulas instead of EVM execution.
// Returns (returnData, true) if a formula was used, or (nil, false) to fall through to EVM.
func (r *Registry) TryQuote(readStorage StorageReader, data []byte) ([]byte, bool) {
	if len(r.pools) == 0 {
		return nil, false
	}

	// Check selector
	if len(data) < 4 || !bytes.Equal(data[:4], executeSwapSelector) {
		return nil, false
	}

	// ABI-decode executeSwap(address[], uint8[], address[], uint256[], bytes[])
	// For single-pool calls: each array has exactly 1 element (or 2 for tokens).
	pool, tokenIn, tokenOut, amountIn, ok := decodeExecuteSwapSingle(data[4:])
	if !ok {
		return nil, false
	}

	// Look up in registry
	formulaID, known := r.pools[pool]
	if !known || formulaID < 0 {
		return nil, false
	}

	// Build adapter readers for experiments-style formula APIs
	stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
		addr := common.HexToAddress(contractAddr)
		slotHash := common.BigToHash(slot)
		val := readStorage(addr, slotHash)
		return val, nil
	}
	bytesReader := func(addr [20]byte, slot [32]byte) ([32]byte, error) {
		val := readStorage(common.Address(addr), common.Hash(slot))
		return val, nil
	}

	// Dispatch by formula ID
	switch formulaID {
	case FormulaV2_30bps:
		return QuoteV2(readStorage, pool, tokenIn, tokenOut, amountIn)

	case FormulaPharaohV1:
		state, err := FetchPharaohV1StateStorage(stateReader, strings.ToLower(pool.Hex()))
		if err != nil || state == nil {
			return nil, false
		}
		zeroForOne := tokenIn.Cmp(tokenOut) < 0
		amtIn := amountIn.ToBig()
		out := QuotePharaohV1(state, amtIn, zeroForOne)
		if out == nil || out.Sign() <= 0 {
			return nil, false
		}
		outU256, overflow := uint256.FromBig(out)
		if overflow {
			return nil, false
		}
		var ret [32]byte
		outU256.WriteToSlice(ret[:])
		return ret[:], true

	case FormulaV3:
		poolAddrStr := strings.ToLower(pool.Hex())
		zeroForOne := tokenIn.Cmp(tokenOut) < 0
		result, err := QuoteV3U256(stateReader, bytesReader, [20]byte(pool), poolAddrStr, amountIn, zeroForOne)
		if err != nil {
			return nil, false
		}
		if result.IsZero() {
			return nil, false
		}
		var ret [32]byte
		result.WriteToSlice(ret[:])
		return ret[:], true

	default:
		return nil, false
	}
}

// decodeExecuteSwapSingle decodes executeSwap calldata for single-pool calls.
// Layout (after selector):
//   offset 0:   offset to pools[]      (= 0xa0 = 160)
//   offset 32:  offset to poolTypes[]   (= 0xe0 = 224)
//   offset 64:  offset to tokens[]      (= 0x120 = 288)
//   offset 96:  offset to amountsIn[]   (= 0x180 = 384)
//   offset 128: offset to extraDatas[]  (= 0x1c0 = 448)
//   then dynamic arrays...
//
// For single-pool: pools has 1 element, tokens has 2, amountsIn has 1.
func decodeExecuteSwapSingle(data []byte) (pool, tokenIn, tokenOut common.Address, amountIn *uint256.Int, ok bool) {
	if len(data) < 384 {
		return common.Address{}, common.Address{}, common.Address{}, nil, false
	}

	// Read offsets (first 5 words)
	poolsOffset := readUint256Word(data, 0)
	tokensOffset := readUint256Word(data, 64)
	amountsOffset := readUint256Word(data, 96)

	// pools array: [length, pool0]
	poolsStart := int(poolsOffset)
	if poolsStart+64 > len(data) {
		return common.Address{}, common.Address{}, common.Address{}, nil, false
	}
	poolsLen := readUint256Word(data, poolsStart)
	if poolsLen != 1 {
		return common.Address{}, common.Address{}, common.Address{}, nil, false // Multi-pool not supported
	}
	pool = common.BytesToAddress(data[poolsStart+44 : poolsStart+64])

	// tokens array: [length, tokenIn, tokenOut]
	tokensStart := int(tokensOffset)
	if tokensStart+96 > len(data) {
		return common.Address{}, common.Address{}, common.Address{}, nil, false
	}
	tokensLen := readUint256Word(data, tokensStart)
	if tokensLen != 2 {
		return common.Address{}, common.Address{}, common.Address{}, nil, false
	}
	tokenIn = common.BytesToAddress(data[tokensStart+44 : tokensStart+64])
	tokenOut = common.BytesToAddress(data[tokensStart+76 : tokensStart+96])

	// amountsIn array: [length, amount0]
	amountsStart := int(amountsOffset)
	if amountsStart+64 > len(data) {
		return common.Address{}, common.Address{}, common.Address{}, nil, false
	}
	amountsLen := readUint256Word(data, amountsStart)
	if amountsLen != 1 {
		return common.Address{}, common.Address{}, common.Address{}, nil, false
	}
	amountIn = new(uint256.Int).SetBytes(data[amountsStart+32 : amountsStart+64])

	return pool, tokenIn, tokenOut, amountIn, true
}

func readUint256Word(data []byte, offset int) uint64 {
	if offset+32 > len(data) {
		return 0
	}
	// Read last 8 bytes of 32-byte word (sufficient for offsets and small values)
	b := data[offset+24 : offset+32]
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// RegistryStats returns the count of validated and invalid pools.
func (r *Registry) RegistryStats() (validated, invalid int) {
	for _, id := range r.pools {
		if id >= 0 {
			validated++
		} else {
			invalid++
		}
	}
	return
}

// WriteRegistry writes the registry to a file, sorted by address.
func WriteRegistry(path string, pools map[common.Address]int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintln(f, "# Formula registry — auto-generated by discover_formulas")
	fmt.Fprintln(f, "# format: pool_address:formula_id")
	fmt.Fprintln(f, "# 0 = V2 constant product 30bps, -1 = no formula (FoT/broken)")

	for addr, id := range pools {
		fmt.Fprintf(f, "%s:%d\n", strings.ToLower("0x"+hex.EncodeToString(addr[:])), id)
	}
	return nil
}
