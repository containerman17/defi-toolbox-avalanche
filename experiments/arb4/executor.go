package main

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

	pf "defi-toolbox/pathfinder"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/gorilla/websocket"
	"github.com/holiman/uint256"
)

var chainID = big.NewInt(43114)

// Executor signs and sends arb transactions.
type Executor struct {
	key        *ecdsa.PrivateKey
	addr       common.Address
	rpcURL     string
	routerAddr common.Address
	signer     types.Signer

	mu    sync.Mutex
	nonce uint64

	PriorityFee   uint64
	GasMultiplier float64
}

func NewExecutor(privKeyHex string, rpcURL string, routerAddr common.Address) (*Executor, error) {
	privKeyHex = strings.TrimPrefix(privKeyHex, "0x")
	key, err := crypto.HexToECDSA(privKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	fmt.Fprintf(os.Stderr, "[arb4/exec] wallet: %s\n", addr.Hex())

	return &Executor{
		key:           key,
		addr:          addr,
		rpcURL:        rpcURL,
		routerAddr:    routerAddr,
		signer:        types.NewLondonSigner(chainID),
		PriorityFee:   0,
		GasMultiplier: 1.3,
	}, nil
}

func (e *Executor) Address() common.Address { return e.addr }

func (e *Executor) SetNonce(n uint64) {
	e.mu.Lock()
	e.nonce = n
	e.mu.Unlock()
}

// Execute sends a swap transaction for a CycleResult.
func (e *Executor) Execute(result *CycleResult, baseFee uint64) (common.Hash, error) {
	// Re-encode with minOutput = amountIn (break-even protection)
	calldata := buildSwapCalldata(result.Steps, &result.AmountIn)

	gasLimit := uint64(float64(result.GasUsed) * e.GasMultiplier)
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
	profitAvax := float64FromU256(&result.Profit) / 1e18
	fmt.Fprintf(os.Stderr, "[arb4/exec] SENDING tx=%s %d-hop profit=%.6f_AVAX gas=%d nonce=%d\n",
		txHash.Hex()[:14], len(result.Steps), profitAvax, gasLimit, nonce)

	if err := e.sendRawTx(rawTx); err != nil {
		e.mu.Lock()
		if e.nonce == nonce+1 {
			e.nonce = nonce
		}
		e.mu.Unlock()
		return common.Hash{}, fmt.Errorf("send tx: %w", err)
	}

	return txHash, nil
}

// buildSwapCalldata encodes swap() with minOutput for on-chain execution.
func buildSwapCalldata(steps []pf.RouteStep, amountIn *uint256.Int) []byte {
	poolAddrs := make([]common.Address, len(steps))
	poolTypes := make([]int, len(steps))
	tokenPairs := make([]common.Address, 0, len(steps)*2)
	extraDatas := make([]string, len(steps))

	for i, s := range steps {
		poolAddrs[i] = s.Pool
		poolTypes[i] = s.PoolType
		tokenPairs = append(tokenPairs, s.TokenIn, s.TokenOut)
		extraDatas[i] = s.ExtraData
	}

	// minOutput = amountIn (break-even protection for cyclic arb)
	return pf.EncodeSwapMulti(poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, amountIn)
}

// Approve sends an ERC-20 approve transaction.
func (e *Executor) Approve(token common.Address, baseFee uint64) (common.Hash, error) {
	data := make([]byte, 68)
	data[0], data[1], data[2], data[3] = 0x09, 0x5e, 0xa7, 0xb3
	copy(data[4+12:4+32], e.routerAddr[:])
	approveAmt := new(big.Int).Mul(big.NewInt(1_000_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	amtBytes := approveAmt.Bytes()
	copy(data[68-len(amtBytes):68], amtBytes)

	maxPriorityFee := new(big.Int).SetUint64(e.PriorityFee)
	maxFee := new(big.Int).SetUint64(baseFee*2 + e.PriorityFee + 1_000_000_000)

	e.mu.Lock()
	nonce := e.nonce
	e.nonce++
	e.mu.Unlock()

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce,
		GasTipCap: maxPriorityFee, GasFeeCap: maxFee,
		Gas: 60_000, To: &token, Value: big.NewInt(0), Data: data,
	})

	signedTx, err := types.SignTx(tx, e.signer, e.key)
	if err != nil {
		return common.Hash{}, err
	}
	rawTx, err := signedTx.MarshalBinary()
	if err != nil {
		return common.Hash{}, err
	}

	if err := e.sendRawTx(rawTx); err != nil {
		e.mu.Lock()
		if e.nonce == nonce+1 {
			e.nonce = nonce
		}
		e.mu.Unlock()
		return common.Hash{}, err
	}
	return signedTx.Hash(), nil
}

func (e *Executor) FetchNonce() (uint64, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "eth_getTransactionCount",
		"params": []interface{}{e.addr.Hex(), "latest"},
	})
	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return 0, err
	}
	var rpcResp struct{ Result string }
	json.Unmarshal(resp, &rpcResp)
	n := new(big.Int)
	n.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return n.Uint64(), nil
}

func (e *Executor) FetchBalance() (*big.Int, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "eth_getBalance",
		"params": []interface{}{e.addr.Hex(), "latest"},
	})
	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return nil, err
	}
	var rpcResp struct{ Result string }
	json.Unmarshal(resp, &rpcResp)
	bal := new(big.Int)
	bal.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return bal, nil
}

func (e *Executor) FetchERC20Balance(token common.Address) (*big.Int, error) {
	data := "0x70a08231000000000000000000000000" + hex.EncodeToString(e.addr[:])
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "eth_call",
		"params": []interface{}{map[string]string{"to": token.Hex(), "data": data}, "latest"},
	})
	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return nil, err
	}
	var rpcResp struct{ Result string }
	json.Unmarshal(resp, &rpcResp)
	bal := new(big.Int)
	bal.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return bal, nil
}

func (e *Executor) CheckAllowance(token common.Address) (*big.Int, error) {
	data := "0xdd62ed3e" +
		"000000000000000000000000" + hex.EncodeToString(e.addr[:]) +
		"000000000000000000000000" + hex.EncodeToString(e.routerAddr[:])
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "eth_call",
		"params": []interface{}{map[string]string{"to": token.Hex(), "data": data}, "latest"},
	})
	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return nil, err
	}
	var rpcResp struct{ Result string }
	json.Unmarshal(resp, &rpcResp)
	val := new(big.Int)
	val.SetString(strings.TrimPrefix(rpcResp.Result, "0x"), 16)
	return val, nil
}

func (e *Executor) sendRawTx(rawTx []byte) error {
	rawHex := "0x" + hex.EncodeToString(rawTx)
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "eth_sendRawTransaction",
		"params": []string{rawHex},
	})
	resp, err := e.rpcCall(reqBody)
	if err != nil {
		return err
	}
	var rpcResp struct {
		Error *struct{ Message string } `json:"error"`
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
	return msg, err
}

func (e *Executor) rpcCallHTTP(reqBody []byte) ([]byte, error) {
	resp, err := http.Post(e.rpcURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
