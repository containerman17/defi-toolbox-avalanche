package main

import (
	"math/big"
	"strconv"
	"strings"
	"sync"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// Fake address where MultiQuoter bytecode is injected.
var QUOTER_ADDR = common.HexToAddress("0x00000000000000000000000000000000DEADBEEF")
var DUMMY_CALLER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")


// Single selector: swap(address[],uint8[],address[],uint256)
var swapSelector = crypto.Keccak256([]byte("swap(address[],uint8[],address[],uint256)"))[:4]

// ---- Types ----

type BlockInfo struct {
	Number    uint64
	Timestamp uint64
	Basefee   uint64
	GasLimit  uint64
}

type ExtraData struct {
	Fee         uint32
	TickSpacing int32
	Hooks       common.Address
	WrappedIn   common.Address
	BufPool     common.Address
	WrappedOut  common.Address
}

func (ed *ExtraData) isEmpty() bool {
	return ed.Fee == 0 && ed.TickSpacing == 0 && ed.Hooks == (common.Address{}) && ed.WrappedIn == (common.Address{})
}

type PoolEdge struct {
	Pool      common.Address
	PoolType  uint8
	TokenIn   common.Address
	TokenOut  common.Address
	ExtraData ExtraData
	Rank      uint32
}

type QuoteJob struct {
	Pool      common.Address
	PoolType  uint8
	TokenIn   common.Address
	TokenOut  common.Address
	Amount    *big.Int
	ExtraData ExtraData
}

type QuoteResult struct {
	Pool      common.Address
	PoolType  uint8
	TokenIn   common.Address
	TokenOut  common.Address
	Amount    *big.Int
	AmountOut *big.Int
	GasUsed   uint64
	ExtraData ExtraData
}

type QuoteKey struct {
	Pool    common.Address
	TokenIn common.Address
	TokenOut common.Address
}

type RouteStep struct {
	Pool      common.Address
	PoolType  uint8
	TokenIn   common.Address
	TokenOut  common.Address
	AmountIn  *big.Int
	AmountOut *big.Int
	ExtraData ExtraData
}

// ---- Parsing ----

func parseExtraData(s string) ExtraData {
	var ed ExtraData
	for _, part := range strings.Split(s, ",") {
		if v, ok := strings.CutPrefix(part, "fee="); ok {
			n, _ := strconv.ParseUint(v, 10, 32)
			ed.Fee = uint32(n)
		} else if v, ok := strings.CutPrefix(part, "ts="); ok {
			n, _ := strconv.ParseInt(v, 10, 32)
			ed.TickSpacing = int32(n)
		} else if v, ok := strings.CutPrefix(part, "hooks="); ok {
			ed.Hooks = common.HexToAddress(v)
		} else if v, ok := strings.CutPrefix(part, "wi="); ok {
			ed.WrappedIn = common.HexToAddress(v)
		} else if v, ok := strings.CutPrefix(part, "bp="); ok {
			ed.BufPool = common.HexToAddress(v)
		} else if v, ok := strings.CutPrefix(part, "wo="); ok {
			ed.WrappedOut = common.HexToAddress(v)
		}
	}
	return ed
}

func parsePools(content string) []PoolEdge {
	var edges []PoolEdge
	var count uint32

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip lines that are just digits (count header)
		allDigits := true
		for _, c := range line {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) < 5 {
			continue
		}

		pool := common.HexToAddress(parts[0])
		if pool == (common.Address{}) {
			continue
		}
		poolType, err := strconv.ParseUint(parts[2], 10, 8)
		if err != nil {
			continue
		}

		var tokens []common.Address
		var extraData ExtraData
		for _, s := range parts[4:] {
			if strings.HasPrefix(s, "@") {
				extraData = parseExtraData(s[1:])
			} else if len(s) >= 42 && strings.HasPrefix(s, "0x") {
				tokens = append(tokens, common.HexToAddress(s))
			}
		}

		for i := 0; i < len(tokens); i++ {
			for j := 0; j < len(tokens); j++ {
				if i != j {
					edges = append(edges, PoolEdge{
						Pool:      pool,
						PoolType:  uint8(poolType),
						TokenIn:   tokens[i],
						TokenOut:  tokens[j],
						ExtraData: extraData,
						Rank:      count,
					})
				}
			}
		}
		count++
	}

	return edges
}

func buildPoolsByToken(edges []PoolEdge) map[common.Address][]PoolEdge {
	m := make(map[common.Address][]PoolEdge)
	for _, e := range edges {
		m[e.TokenIn] = append(m[e.TokenIn], e)
	}
	return m
}

// ---- ABI Encoding ----

func uint256Bytes(v uint64) [32]byte {
	var buf [32]byte
	b := new(big.Int).SetUint64(v).Bytes()
	copy(buf[32-len(b):], b)
	return buf
}

func bigIntBytes32(v *big.Int) [32]byte {
	var buf [32]byte
	b := v.Bytes()
	if len(b) > 32 {
		b = b[len(b)-32:]
	}
	copy(buf[32-len(b):], b)
	return buf
}

func addressBytes32(addr common.Address) [32]byte {
	var buf [32]byte
	copy(buf[12:], addr.Bytes())
	return buf
}

// encodeSwap encodes a call to swap(address[],uint8[],address[],uint256).
// Used for both single-hop (1-element arrays) and multi-hop (N-element arrays).
func encodeSwap(pools []common.Address, poolTypes []uint8, tokens []common.Address, amountIn *big.Int) []byte {
	nPools := len(pools)
	nTypes := len(poolTypes)
	nTokens := len(tokens)

	data := make([]byte, 0, 4+32*32)
	data = append(data, swapSelector...)

	// Head: 4 params → 3 dynamic offsets + 1 value
	// Layout: [offset_pools][offset_types][offset_tokens][amountIn]
	headSize := uint64(4 * 32)
	poolsBody := uint64(32 + nPools*32)
	typesBody := uint64(32 + nTypes*32)

	poolsOffset := headSize
	typesOffset := poolsOffset + poolsBody
	tokensOffset := typesOffset + typesBody

	buf := uint256Bytes(poolsOffset)
	data = append(data, buf[:]...)
	buf = uint256Bytes(typesOffset)
	data = append(data, buf[:]...)
	buf = uint256Bytes(tokensOffset)
	data = append(data, buf[:]...)
	amtBuf := bigIntBytes32(amountIn)
	data = append(data, amtBuf[:]...)

	// Pools array
	buf = uint256Bytes(uint64(nPools))
	data = append(data, buf[:]...)
	for _, p := range pools {
		ab := addressBytes32(p)
		data = append(data, ab[:]...)
	}

	// Pool types array
	buf = uint256Bytes(uint64(nTypes))
	data = append(data, buf[:]...)
	for _, t := range poolTypes {
		var tb [32]byte
		tb[31] = t
		data = append(data, tb[:]...)
	}

	// Tokens array
	buf = uint256Bytes(uint64(nTokens))
	data = append(data, buf[:]...)
	for _, t := range tokens {
		ab := addressBytes32(t)
		data = append(data, ab[:]...)
	}

	return data
}

// ---- Pool quoting ----

var hugeBalance = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
var whaleETH = new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))

func executeQuoteJob(evm *LocalEVM, quoterCode []byte, tokenOverrides map[common.Address]*TokenOverride, job *QuoteJob, blockInfo *BlockInfo, gasLimit uint64) *QuoteResult {
	// Reset mutable state but keep RPC cache
	evm.ResetMutable()

	// Inject quoter bytecode
	evm.SetCode(QUOTER_ADDR, quoterCode)

	// Set balance overrides
	evm.SetBalanceOverride(DUMMY_CALLER, whaleETH)
	ovrs := getOverride(job.TokenIn, QUOTER_ADDR, hugeBalance, QUOTER_ADDR, tokenOverrides)
	for _, o := range ovrs {
		evm.SetStateOverride(o.Addr, o.Slot, o.Value)
	}

	// Encode as single-hop swap call
	calldata := encodeSwap(
		[]common.Address{job.Pool},
		[]uint8{job.PoolType},
		[]common.Address{job.TokenIn, job.TokenOut},
		job.Amount,
	)

	// Execute — swap returns a single uint256
	output, gasUsed, err := evm.Call(DUMMY_CALLER, QUOTER_ADDR, calldata, gasLimit)

	amountOut := big.NewInt(0)
	if err == nil && len(output) >= 32 {
		amountOut = new(big.Int).SetBytes(output[:32])
	}

	return &QuoteResult{
		Pool:      job.Pool,
		PoolType:  job.PoolType,
		TokenIn:   job.TokenIn,
		TokenOut:  job.TokenOut,
		Amount:    job.Amount,
		AmountOut: amountOut,
		GasUsed:   gasUsed,
		ExtraData: job.ExtraData,
	}
}

func quoteAll(evm *LocalEVM, quoterCode []byte, tokenOverrides map[common.Address]*TokenOverride, jobs []QuoteJob, blockInfo *BlockInfo, gasLimit uint64) (map[QuoteKey]*QuoteResult, int) {
	if len(jobs) == 0 {
		return nil, 0
	}

	numWorkers := wsPoolSize
	if numWorkers > len(jobs) {
		numWorkers = len(jobs)
	}

	type indexedResult struct {
		key QuoteKey
		r   *QuoteResult
	}

	resultsCh := make(chan indexedResult, len(jobs))
	jobsCh := make(chan *QuoteJob, len(jobs))

	for i := range jobs {
		jobsCh <- &jobs[i]
	}
	close(jobsCh)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		// Each worker gets its own EVM clone (shares WSClient pool for RPC)
		workerEvm := evm.CloneForWorker()
		go func() {
			defer wg.Done()
			for job := range jobsCh {
				r := executeQuoteJob(workerEvm, quoterCode, tokenOverrides, job, blockInfo, gasLimit)
				if r.AmountOut.Sign() > 0 {
					resultsCh <- indexedResult{
						key: QuoteKey{Pool: r.Pool, TokenIn: r.TokenIn, TokenOut: r.TokenOut},
						r:   r,
					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	results := make(map[QuoteKey]*QuoteResult)
	for ir := range resultsCh {
		results[ir.key] = ir.r
	}
	return results, len(jobs)
}

// ---- Gas-aware comparison ----

func isBetterNet(newAmount *big.Int, newGas uint64, oldAmount *big.Int, oldGas uint64, inputAvax *big.Int, basefee uint64) bool {
	maxAmt := new(big.Int).Set(newAmount)
	if oldAmount.Cmp(maxAmt) > 0 {
		maxAmt.Set(oldAmount)
	}
	basefeeBI := new(big.Int).SetUint64(basefee)

	valNew := new(big.Int).Mul(newAmount, inputAvax)
	costNew := new(big.Int).Mul(new(big.Int).SetUint64(newGas), basefeeBI)
	costNew.Mul(costNew, maxAmt)

	valOld := new(big.Int).Mul(oldAmount, inputAvax)
	costOld := new(big.Int).Mul(new(big.Int).SetUint64(oldGas), basefeeBI)
	costOld.Mul(costOld, maxAmt)

	netNew := new(big.Int).Sub(valNew, costNew)
	netOld := new(big.Int).Sub(valOld, costOld)
	return netNew.Cmp(netOld) > 0
}

// ---- Full route execution ----

func executeRoute(evm *LocalEVM, quoterCode []byte, tokenOverrides map[common.Address]*TokenOverride, route []RouteStep, amountIn *big.Int, blockInfo *BlockInfo, gasLimit uint64) (*big.Int, uint64) {
	if len(route) == 0 {
		return big.NewInt(0), 0
	}

	evm.ResetMutable()
	evm.SetCode(QUOTER_ADDR, quoterCode)
	evm.SetBalanceOverride(DUMMY_CALLER, whaleETH)

	tokenIn := route[0].TokenIn
	ovrs := getOverride(tokenIn, QUOTER_ADDR, hugeBalance, QUOTER_ADDR, tokenOverrides)
	for _, o := range ovrs {
		evm.SetStateOverride(o.Addr, o.Slot, o.Value)
	}

	pools := make([]common.Address, len(route))
	poolTypesVec := make([]uint8, len(route))
	tokens := make([]common.Address, len(route)+1)
	for i, s := range route {
		pools[i] = s.Pool
		poolTypesVec[i] = s.PoolType
		tokens[i] = s.TokenIn
	}
	tokens[len(route)] = route[len(route)-1].TokenOut

	calldata := encodeSwap(pools, poolTypesVec, tokens, amountIn)
	output, gasUsed, err := evm.Call(DUMMY_CALLER, QUOTER_ADDR, calldata, gasLimit)

	if err == nil && len(output) >= 32 {
		return new(big.Int).SetBytes(output[:32]), gasUsed
	}
	return big.NewInt(0), gasUsed
}

// executeRoutesParallel runs multiple route executions in parallel using worker goroutines.
// Each worker gets its own EVM clone. Matches Rust's quote_routes_parallel approach.
type RouteJob struct {
	Route []RouteStep
}
type RouteResult struct {
	AmountOut *big.Int
	GasUsed   uint64
}

func executeRoutesParallel(evm *LocalEVM, quoterCode []byte, tokenOverrides map[common.Address]*TokenOverride, jobs []RouteJob, amountIn *big.Int, blockInfo *BlockInfo, gasLimit uint64) []RouteResult {
	n := len(jobs)
	if n == 0 {
		return nil
	}

	results := make([]RouteResult, n)
	numWorkers := wsPoolSize
	if numWorkers > n {
		numWorkers = n
	}

	type indexedJob struct {
		idx   int
		route []RouteStep
	}

	jobCh := make(chan indexedJob, n)
	for i, j := range jobs {
		jobCh <- indexedJob{idx: i, route: j.Route}
	}
	close(jobCh)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerEvm := evm.CloneForWorker()
		go func() {
			defer wg.Done()
			for job := range jobCh {
				amt, gas := executeRoute(workerEvm, quoterCode, tokenOverrides, job.route, amountIn, blockInfo, gasLimit)
				results[job.idx] = RouteResult{AmountOut: amt, GasUsed: gas}
			}
		}()
	}
	wg.Wait()
	return results
}
