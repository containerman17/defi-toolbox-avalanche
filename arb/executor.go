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
		PriorityFee:   1_500_000_000,                 // 1.5 gwei
		GasMultiplier: 1.3,
	}, nil
}

func (e *Executor) Address() common.Address { return e.addr }

// SetNonce sets the initial nonce (fetch from chain at startup).
func (e *Executor) SetNonce(n uint64) {
	e.mu.Lock()
	e.nonce = n
	e.mu.Unlock()
}

// Execute builds, signs, and sends an arb transaction for a verified opportunity.
func (e *Executor) Execute(opp *Opportunity, baseFee uint64) (common.Hash, error) {
	if !opp.EVMVerified {
		return common.Hash{}, fmt.Errorf("opportunity not EVM-verified")
	}

	calldata := encodeMultiHopSwap(opp.Cycle, e.pt, e.hub, opp.AmountIn)

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
