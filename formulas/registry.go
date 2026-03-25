package formulas

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

//go:embed registry.txt
var registryData string

// LoadEmbeddedRegistry loads the formula registry from embedded data.
func LoadEmbeddedRegistry() *Registry {
	return parseRegistryContent(registryData)
}

// Formula IDs
const (
	FormulaV2_30bps  = 0  // V2 constant product, 0.3% fee
	FormulaPharaohV1 = 1  // Pharaoh V1 (stable/volatile with registry)
	FormulaV3        = 2  // Uniswap V3 / Pharaoh V3 tick-walking
	FormulaLFJV2     = 3  // LFJ V2 Liquidity Book (discrete bins)
	FormulaAlgebra   = 4  // Algebra V1 Integral (dynamic fee CL)
	FormulaDODO      = 5  // DODO PMM
	FormulaV4        = 6  // Uniswap V4 (singleton PoolManager)
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

// LoadRegistry loads a registry file from disk (fallback for testing).
func LoadRegistry(path string) *Registry {
	f, err := os.Open(path)
	if err != nil {
		return NewRegistry()
	}
	defer f.Close()
	content, _ := os.ReadFile(path)
	return parseRegistryContent(string(content))
}

func parseRegistryContent(content string) *Registry {
	r := &Registry{pools: make(map[common.Address]int)}
	scanner := bufio.NewScanner(strings.NewReader(content))
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

// TryQuoteDirect attempts to quote using formulas with raw pool fields (no ABI encode/decode).
// Returns (amountOut, true) if a formula was used, or (nil, false) to fall through to EVM.
func (r *Registry) TryQuoteDirect(readStorage StorageReader, pool common.Address, tokenIn, tokenOut common.Address, amountIn *uint256.Int) (*uint256.Int, bool) {
	formulaID, known := r.pools[pool]
	if !known || formulaID < 0 {
		return nil, false
	}

	// FoT adjustment is handled by PoolManager wrapper, not here.
	// TryQuoteDirect is a legacy path used by function-based formulas (LFJ V2, Algebra).
	ret, ok := r.dispatchFormula(readStorage, formulaID, pool, tokenIn, tokenOut, amountIn)
	if !ok {
		return nil, false
	}

	var out uint256.Int
	out.SetBytes(ret)
	if out.IsZero() {
		return nil, false
	}

	return &out, true
}

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
	pool, tokenIn, tokenOut, amountIn, ok := decodeExecuteSwapSingle(data[4:])
	if !ok {
		return nil, false
	}

	// Look up in registry
	formulaID, known := r.pools[pool]
	if !known || formulaID < 0 {
		return nil, false
	}

	// FoT adjustment is handled by PoolManager wrapper, not here.
	// TryQuote is a legacy path used by function-based formulas (LFJ V2, Algebra).
	return r.dispatchFormula(readStorage, formulaID, pool, tokenIn, tokenOut, amountIn)
}

func (r *Registry) dispatchFormula(readStorage StorageReader, formulaID int, pool, tokenIn, tokenOut common.Address, amountIn *uint256.Int) (ret []byte, ok bool) {
	// Recover from panics in formula code (e.g. DODO nil pointer on bad state)
	defer func() {
		if r := recover(); r != nil {
			ret = nil
			ok = false
		}
	}()
	zeroForOne := tokenIn.Cmp(tokenOut) < 0

	switch formulaID {
	case FormulaV2_30bps:
		return QuoteV2(readStorage, pool, tokenIn, tokenOut, amountIn)

	case FormulaPharaohV1:
		// String-based reader for Pharaoh V1 (uses big.Int slots)
		stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
			addr := common.HexToAddress(contractAddr)
			slotHash := common.BigToHash(slot)
			return readStorage(addr, slotHash), nil
		}
		poolHex := strings.ToLower(pool.Hex())
		state, err := FetchPharaohV1StateStorage(stateReader, poolHex)
		if err != nil || state == nil {
			return nil, false
		}
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
		bytesReader := func(addr [20]byte, slot [32]byte) ([32]byte, error) {
			return readStorage(common.Address(addr), common.Hash(slot)), nil
		}
		stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
			addr := common.HexToAddress(contractAddr)
			slotHash := common.BigToHash(slot)
			return readStorage(addr, slotHash), nil
		}
		poolHex := strings.ToLower(pool.Hex())
		result, err := QuoteV3U256(stateReader, bytesReader, [20]byte(pool), poolHex, amountIn, zeroForOne)
		if err != nil {
			return nil, false
		}
		if result.IsZero() {
			return nil, false
		}
		var ret [32]byte
		result.WriteToSlice(ret[:])
		return ret[:], true

	case FormulaAlgebra:
		stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
			addr := common.HexToAddress(contractAddr)
			slotHash := common.BigToHash(slot)
			return readStorage(addr, slotHash), nil
		}
		poolHex := strings.ToLower(pool.Hex())
		out, err := QuoteAlgebraStorage(stateReader, poolHex, amountIn.ToBig(), zeroForOne)
		if err != nil || out == nil || out.Sign() <= 0 {
			return nil, false
		}
		outU256, overflow := uint256.FromBig(out)
		if overflow {
			return nil, false
		}
		var ret [32]byte
		outU256.WriteToSlice(ret[:])
		return ret[:], true

	case FormulaDODO:
		stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
			addr := common.HexToAddress(contractAddr)
			slotHash := common.BigToHash(slot)
			return readStorage(addr, slotHash), nil
		}
		poolHex := strings.ToLower(pool.Hex())
		dodoState, err := FetchDODOState(stateReader, poolHex, "", "")
		if err != nil || dodoState == nil {
			return nil, false
		}
		out := QuoteDODO(dodoState, amountIn.ToBig(), zeroForOne)
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

	case FormulaLFJV2:
		stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
			addr := common.HexToAddress(contractAddr)
			slotHash := common.BigToHash(slot)
			return readStorage(addr, slotHash), nil
		}
		poolHex := strings.ToLower(pool.Hex())
		token0Hex := strings.ToLower(tokenIn.Hex())
		token1Hex := strings.ToLower(tokenOut.Hex())
		// token0 < token1 for LFJ V2
		if token0Hex > token1Hex {
			token0Hex, token1Hex = token1Hex, token0Hex
		}
		lfjState, layout, err := FetchLFJV2StateFast(stateReader, poolHex, token0Hex, token1Hex)
		if err != nil || lfjState == nil {
			return nil, false
		}
		amtIn := amountIn.ToBig()
		out := QuoteLFJV2Fast(stateReader, lfjState, layout, amtIn, zeroForOne, 0)
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

// IsInvalid returns true if a pool is known to not work with formulas (FoT, broken).
// The BFS can skip EVM calls for these pools entirely.
// GetFormulaID returns the formula ID for a pool, and whether it's known.
func (r *Registry) GetFormulaID(pool common.Address) (int, bool) {
	id, ok := r.pools[pool]
	return id, ok
}

func (r *Registry) IsInvalid(pool common.Address) bool {
	id, known := r.pools[pool]
	return known && id < 0
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
