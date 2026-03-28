package arb

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/gorilla/websocket"
)

var chainID = big.NewInt(43114) // Avalanche C-Chain

// Executor signs and sends profitable arb transactions.
type Executor struct {
	key        *ecdsa.PrivateKey
	addr       common.Address
	rpcURL     string
	routerAddr common.Address
	pt         *PoolTable
	hub        common.Address
	signer     types.Signer

	mu    sync.Mutex
	nonce uint64

	// Config
	MinProfitWei  *big.Int // minimum EVM profit to execute (in wei)
	PriorityFee   uint64   // priority fee in wei (tip to validator)
	GasMultiplier float64  // multiplier on verified gasUsed (e.g. 1.3)
}

// NewExecutor creates an executor from a hex private key (no 0x prefix).
func NewExecutor(privKeyHex string, rpcURL string, routerAddr common.Address, pt *PoolTable, hub common.Address) (*Executor, error) {
	privKeyHex = strings.TrimPrefix(privKeyHex, "0x")
	key, err := crypto.HexToECDSA(privKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	addr := crypto.PubkeyToAddress(key.PublicKey)
	fmt.Fprintf(os.Stderr, "[arb/exec] wallet: %s\n", addr.Hex())

	signer := types.NewLondonSigner(chainID)

	return &Executor{
		key:           key,
		addr:          addr,
		rpcURL:        rpcURL,
		routerAddr:    routerAddr,
		pt:            pt,
		hub:           hub,
		signer:        signer,
		MinProfitWei:  big.NewInt(1_000_000_000_000), // 0.000001 AVAX default
		PriorityFee:   0,                             // no tip — low-competition arbs
		GasMultiplier: 1.3,
	}, nil
}

func (e *Executor) Address() common.Address { return e.addr }

// FetchBalance fetches the AVAX balance of the wallet.
func (e *Executor) FetchBalance() (*big.Int, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBalance",
		"params":  []interface{}{e.addr.Hex(), "latest"},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return nil, err
	}

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	bal := new(big.Int)
	bal.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return bal, nil
}

// SetNonce sets the initial nonce (fetch from chain at startup).
func (e *Executor) SetNonce(n uint64) {
	e.mu.Lock()
	e.nonce = n
	e.mu.Unlock()
}

// Approve sends an ERC-20 approve(router, maxUint256) transaction.
func (e *Executor) Approve(token common.Address, baseFee uint64) (common.Hash, error) {
	// approve(address,uint256) = 0x095ea7b3
	data := make([]byte, 68)
	data[0], data[1], data[2], data[3] = 0x09, 0x5e, 0xa7, 0xb3
	copy(data[4+12:4+32], e.routerAddr[:])
	// 1,000,000 AVAX (1e6 * 1e18 = 1e24)
	approveAmt := new(big.Int).Mul(big.NewInt(1_000_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	amtBytes := approveAmt.Bytes()
	copy(data[68-len(amtBytes):68], amtBytes)

	maxPriorityFee := new(big.Int).SetUint64(e.PriorityFee)
	maxFee := new(big.Int).SetUint64(baseFee*2 + e.PriorityFee + 1_000_000_000) // +1 gwei for approval to go through

	e.mu.Lock()
	nonce := e.nonce
	e.nonce++
	e.mu.Unlock()

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: maxPriorityFee,
		GasFeeCap: maxFee,
		Gas:       60_000,
		To:        &token,
		Value:     big.NewInt(0),
		Data:      data,
	})

	signedTx, err := types.SignTx(tx, e.signer, e.key)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign approve: %w", err)
	}

	rawTx, err := signedTx.MarshalBinary()
	if err != nil {
		return common.Hash{}, fmt.Errorf("marshal approve: %w", err)
	}

	txHash := signedTx.Hash()
	fmt.Fprintf(os.Stderr, "[arb/exec] APPROVE %s for router, nonce=%d tx=%s\n",
		token.Hex()[:10], nonce, txHash.Hex()[:14])

	if err := e.sendRawTx(rawTx); err != nil {
		e.mu.Lock()
		if e.nonce == nonce+1 {
			e.nonce = nonce
		}
		e.mu.Unlock()
		return common.Hash{}, err
	}

	return txHash, nil
}

// CheckAllowance checks the ERC-20 allowance of the wallet for the router.
func (e *Executor) CheckAllowance(token common.Address) (*big.Int, error) {
	// allowance(owner, spender) = 0xdd62ed3e
	data := "0xdd62ed3e" +
		"000000000000000000000000" + hex.EncodeToString(e.addr[:]) +
		"000000000000000000000000" + hex.EncodeToString(e.routerAddr[:])

	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params":  []interface{}{map[string]string{"to": token.Hex(), "data": data}, "latest"},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return nil, err
	}
	var rpcResp struct {
		Result string `json:"result"`
	}
	json.Unmarshal(resp, &rpcResp)
	val := new(big.Int)
	val.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return val, nil
}

// SimulateViaRPCAtBlock runs swap() as eth_call at a specific block number.
// Returns (success, description, gasEstimate).
func (e *Executor) SimulateViaRPCAtBlock(opp *Opportunity, calldata []byte, block uint64) (bool, string, uint64) {
	calldataHex := "0x" + hex.EncodeToString(calldata)
	blockHex := fmt.Sprintf("0x%x", block)

	// eth_call at specific block
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []interface{}{
			map[string]string{
				"from": e.addr.Hex(),
				"to":   e.routerAddr.Hex(),
				"data": calldataHex,
			},
			blockHex,
		},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return false, fmt.Sprintf("rpc error: %v", err), 0
	}

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	json.Unmarshal(resp, &rpcResp)

	if rpcResp.Error != nil {
		return false, fmt.Sprintf("REVERT: %s", rpcResp.Error.Message), 0
	}
	if rpcResp.Result == "" || rpcResp.Result == "0x" {
		return false, "empty result", 0
	}

	// Decode amountOut from return data
	retHex := strings.TrimPrefix(rpcResp.Result, "0x")
	var outDesc string
	if len(retHex) >= 64 {
		outVal := new(big.Int)
		outVal.SetString(retHex[len(retHex)-64:], 16)
		outDesc = fmt.Sprintf("OK out=%s", outVal.String())
	} else {
		outDesc = fmt.Sprintf("OK ret_len=%d", len(retHex)/2)
	}

	// eth_estimateGas at same block
	gasReqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_estimateGas",
		"params": []interface{}{
			map[string]string{
				"from": e.addr.Hex(),
				"to":   e.routerAddr.Hex(),
				"data": calldataHex,
			},
			blockHex,
		},
	})
	var gasUsed uint64
	gasResp, err := e.rpcCall(gasReqBody)
	if err == nil {
		var gasRpcResp struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(gasResp, &gasRpcResp) == nil && gasRpcResp.Result != "" {
			gas := new(big.Int)
			gas.SetString(strings.TrimPrefix(gasRpcResp.Result, "0x"), 16)
			gasUsed = gas.Uint64()
		}
	}

	return true, fmt.Sprintf("%s gas=%d", outDesc, gasUsed), gasUsed
}

// SimulateViaRPCWithGas runs swap() as eth_call and returns the gas used.
func (e *Executor) SimulateViaRPCWithGas(opp *Opportunity) (uint64, error) {
	calldata := EncodeSwapCalldata(opp.Cycle, e.pt, e.hub, opp.AmountIn)
	calldataHex := "0x" + hex.EncodeToString(calldata)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_estimateGas",
		"params": []interface{}{
			map[string]string{
				"from": e.addr.Hex(),
				"to":   e.routerAddr.Hex(),
				"data": calldataHex,
			},
			"latest",
		},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return 0, fmt.Errorf("rpc error: %w", err)
	}

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(resp, &rpcResp)

	if rpcResp.Error != nil {
		return 0, fmt.Errorf("stage4 revert: %s", rpcResp.Error.Message)
	}

	gas := new(big.Int)
	gas.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return gas.Uint64(), nil
}

// Execute builds, signs, and sends an arb transaction for a verified opportunity.
func (e *Executor) Execute(opp *Opportunity, baseFee uint64) (common.Hash, error) {

	calldata := EncodeSwapCalldata(opp.Cycle, e.pt, e.hub, opp.AmountIn)

	gasLimit := uint64(float64(opp.EVMGasUsed) * e.GasMultiplier)
	if gasLimit < 100_000 {
		gasLimit = 100_000
	}

	maxPriorityFee := new(big.Int).SetUint64(e.PriorityFee)
	maxFee := new(big.Int).SetUint64(baseFee*2 + e.PriorityFee)

	e.mu.Lock()
	nonce := e.nonce
	e.nonce++
	e.mu.Unlock()

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: maxPriorityFee,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &e.routerAddr,
		Value:     big.NewInt(0),
		Data:      calldata,
	})

	signedTx, err := types.SignTx(tx, e.signer, e.key)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign tx: %w", err)
	}

	rawTx, err := signedTx.MarshalBinary()
	if err != nil {
		return common.Hash{}, fmt.Errorf("marshal tx: %w", err)
	}

	txHash := signedTx.Hash()
	profitAvax := opp.EVMProfit / 1e18

	fmt.Fprintf(os.Stderr, "[arb/exec] SENDING tx=%s %d-hop in=%.4f profit=%.6f_AVAX gas=%d nonce=%d\n",
		txHash.Hex()[:14], opp.Cycle.Hops, float64FromU256(opp.AmountIn)/1e18,
		profitAvax, gasLimit, nonce)

	err = e.sendRawTx(rawTx)
	if err != nil {
		e.mu.Lock()
		if e.nonce == nonce+1 {
			e.nonce = nonce
		}
		e.mu.Unlock()
		return common.Hash{}, fmt.Errorf("send tx: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[arb/exec] SENT tx=%s\n", txHash.Hex())
	return txHash, nil
}

// FetchERC20Balance fetches the balance of an ERC-20 token for the wallet.
func (e *Executor) FetchERC20Balance(token common.Address) (*big.Int, error) {
	// balanceOf(address) selector = 0x70a08231
	data := "0x70a08231000000000000000000000000" + hex.EncodeToString(e.addr[:])

	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []interface{}{
			map[string]string{"to": token.Hex(), "data": data},
			"latest",
		},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return nil, err
	}

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	bal := new(big.Int)
	bal.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return bal, nil
}

// FetchNonce fetches the current nonce from the RPC endpoint.
func (e *Executor) FetchNonce() (uint64, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getTransactionCount",
		"params":  []interface{}{e.addr.Hex(), "latest"},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return 0, err
	}

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpcResp); err != nil {
		return 0, err
	}
	if rpcResp.Error != nil {
		return 0, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	n := new(big.Int)
	n.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return n.Uint64(), nil
}

func (e *Executor) sendRawTx(rawTx []byte) error {
	rawHex := "0x" + hex.EncodeToString(rawTx)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_sendRawTransaction",
		"params":  []string{rawHex},
	})

	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return err
	}

	var rpcResp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &rpcResp) == nil && rpcResp.Error != nil {
		return fmt.Errorf("rpc: %s", rpcResp.Error.Message)
	}
	return nil
}

func (e *Executor) rpcCall(reqBody []byte) ([]byte, error) {
	if strings.HasPrefix(e.rpcURL, "ws://") || strings.HasPrefix(e.rpcURL, "wss://") {
		return e.rpcCallWS(reqBody)
	}
	return e.rpcCallHTTP(reqBody)
}

func (e *Executor) rpcCallWS(reqBody []byte) ([]byte, error) {
	conn, _, err := websocket.DefaultDialer.Dial(e.rpcURL, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, reqBody); err != nil {
		return nil, err
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func (e *Executor) rpcCallHTTP(reqBody []byte) ([]byte, error) {
	resp, err := http.Post(e.rpcURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
