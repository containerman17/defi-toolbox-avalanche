package lightclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	cparams "github.com/ava-labs/avalanchego/graft/coreth/params"
	cextras "github.com/ava-labs/avalanchego/graft/coreth/params/extras"
	ccustomtypes "github.com/ava-labs/avalanchego/graft/coreth/plugin/evm/customtypes"
	warpcontract "github.com/ava-labs/avalanchego/graft/coreth/precompile/contracts/warp"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/vms/evm/acp226"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
)

// ─── Config ─────────────────────────────────────────────────────────

// Config configures the light client. Zero values use sensible defaults.
type Config struct {
	// RPCURL is the WebSocket endpoint of the Avalanche C-Chain node.
	// Default: ws://127.0.0.1:9650/ext/bc/C/ws
	RPCURL string

	// DataDir is where snapshots are stored. Required.
	DataDir string

	// FixedBlock pins the client to a single block. Default: 0 (live mode).
	// When non-zero, Start loads the block-specific snapshot if present, otherwise
	// pins immediately and fills state lazily on demand during calls. It does not
	// subscribe to new heads.
	FixedBlock uint64

	// Concurrency is the number of RPC worker sockets.
	// Default: 2 * runtime.NumCPU()
	Concurrency int

	// SnapshotEveryBlocks controls how often the state is persisted.
	// Default: 25
	SnapshotEveryBlocks uint64

	// PruneKeepBlocks is how many blocks of history to retain.
	// Default: 20
	PruneKeepBlocks uint64

	// OnBlock is called after each block is processed.
	// Arguments: block number, list of changed (address, slot) pairs.
	OnBlock func(blockNum uint64)

}

func (cfg *Config) applyDefaults() {
	if cfg.RPCURL == "" {
		cfg.RPCURL = "ws://127.0.0.1:9650/ext/bc/C/ws"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2 * runtime.NumCPU()
	}
	if cfg.SnapshotEveryBlocks == 0 {
		cfg.SnapshotEveryBlocks = 25
	}
	if cfg.PruneKeepBlocks == 0 {
		cfg.PruneKeepBlocks = 20
	}
}

// ─── LightClient ────────────────────────────────────────────────────

// LightClient syncs Avalanche C-Chain state from a live node and provides
// local EVM execution against the current state.
type LightClient struct {
	cfg      Config
	pool     *RPCPool
	fetcher  *BlockFetcher
	state    *VersionedState
	chainCfg *params.ChainConfig

	mu                sync.RWMutex
	lastSnapshotBlock uint64

	// blockHashes caches recent block hashes for the BLOCKHASH opcode.
	blockHashes   map[uint64]common.Hash
	blockHashesMu sync.RWMutex

	// fetchStats tracks cumulative cache-miss RPC fetches. A background
	// reporter prints per-interval deltas when any occurred.
	fetchStats   FetchStats
	reporterStop chan struct{}
	reporterDone chan struct{}

	// DebugLogging, when true, enables informational log lines such as
	// snapshot loads/saves, catch-up progress, and per-block processing.
	// Default is false (silent). Errors are always logged.
	DebugLogging bool
}

func (c *LightClient) infof(format string, args ...interface{}) {
	if !c.DebugLogging {
		return
	}
	log.Printf(format, args...)
}

// New creates a new LightClient. Call Start to begin syncing.
func New(cfg Config) (*LightClient, error) {
	cfg.applyDefaults()

	if cfg.DataDir == "" {
		return nil, fmt.Errorf("lightclient: DataDir is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("lightclient: create data dir: %w", err)
	}

	pool, err := NewRPCPool(cfg.RPCURL, cfg.Concurrency)
	if err != nil {
		return nil, fmt.Errorf("lightclient: rpc pool: %w", err)
	}

	chainCfg, err := FetchChainConfig(pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("lightclient: chain config: %w", err)
	}

	c := &LightClient{
		cfg:          cfg,
		pool:         pool,
		fetcher:      NewBlockFetcher(pool),
		state:        NewVersionedState(),
		chainCfg:     chainCfg,
		blockHashes:  make(map[uint64]common.Hash),
		reporterStop: make(chan struct{}),
		reporterDone: make(chan struct{}),
	}
	go c.runFetchReporter()
	return c, nil
}

// runFetchReporter prints a line every 5 seconds when any cache-miss RPC
// fetches occurred in that interval. A final line is emitted on shutdown if
// there are pending fetches since the last tick.
func (c *LightClient) runFetchReporter() {
	defer close(c.reporterDone)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastStorage, lastBalance, lastNonce, lastCode, lastSaved int64
	tick := func(isFinal bool) {
		s := c.fetchStats.Storage.Load()
		b := c.fetchStats.Balance.Load()
		n := c.fetchStats.Nonce.Load()
		co := c.fetchStats.Code.Load()
		ds := s - lastStorage
		db := b - lastBalance
		dn := n - lastNonce
		dc := co - lastCode
		total := ds + db + dn + dc
		if total > 0 {
			log.Printf("lightclient: fetched %d slots (storage=%d balance=%d nonce=%d code=%d)",
				total, ds, db, dn, dc)
		}
		lastStorage, lastBalance, lastNonce, lastCode = s, b, n, co

		// Fixed-block mode: snapshot on every tick that saw new fetches, and
		// unconditionally on the final tick (Close always persists).
		if c.cfg.FixedBlock != 0 {
			cur := s + b + n + co
			if isFinal || cur > lastSaved {
				c.saveSnapshotIfNeeded(true)
				lastSaved = cur
			}
		}
	}

	for {
		select {
		case <-c.reporterStop:
			tick(true)
			return
		case <-ticker.C:
			tick(false)
		}
	}
}

// LatestBlock returns the latest fully-processed block number.
func (c *LightClient) LatestBlock() uint64 {
	return c.state.LatestBlock()
}

// Call executes a simulated call at the given block. If block is 0,
// it resolves to the latest block atomically.
func (c *LightClient) Call(msg CallMsg, block uint64) ([]byte, uint64, error) {
	return c.CallWithState(msg, block, nil)
}

// StateView returns a fresh pinned StateView for the requested block. In fixed
// block mode, block 0 resolves to FixedBlock and any other block must match it.
func (c *LightClient) StateView(block uint64) (*StateView, error) {
	if c.cfg.FixedBlock != 0 {
		if block == 0 {
			block = c.cfg.FixedBlock
		} else if block != c.cfg.FixedBlock {
			return nil, fmt.Errorf("lightclient: fixed block mode is pinned to block %d (got %d)", c.cfg.FixedBlock, block)
		}
	} else if block == 0 {
		block = c.state.LatestBlock()
	}
	if block == 0 {
		return nil, fmt.Errorf("lightclient: no blocks processed yet")
	}

	miss := c.fetcher.MissCallbacks(c.state, &c.fetchStats)
	return NewStateView(c.state, block, miss), nil
}

// BlockTimestamp returns the timestamp of the requested block.
func (c *LightClient) BlockTimestamp(block uint64) (uint64, error) {
	if c.cfg.FixedBlock != 0 {
		if block == 0 {
			block = c.cfg.FixedBlock
		} else if block != c.cfg.FixedBlock {
			return 0, fmt.Errorf("lightclient: fixed block mode is pinned to block %d (got %d)", c.cfg.FixedBlock, block)
		}
	} else if block == 0 {
		block = c.state.LatestBlock()
	}
	if block == 0 {
		return 0, fmt.Errorf("lightclient: no blocks processed yet")
	}

	bd, err := c.fetcher.GetBlock(block)
	if err != nil {
		return 0, err
	}
	c.cacheBlockHash(block, bd.Hash)
	return bd.Timestamp, nil
}

// CallWithState executes a simulated call at the given block after first letting
// the caller mutate the fresh pinned StateView (for example to inject code or
// token balance/allowance overrides). If block is 0, it resolves to the latest
// block atomically, or to FixedBlock when pinned mode is enabled.
func (c *LightClient) CallWithState(msg CallMsg, block uint64, prepare func(*StateView)) ([]byte, uint64, error) {
	if c.cfg.FixedBlock != 0 {
		if block == 0 {
			block = c.cfg.FixedBlock
		} else if block != c.cfg.FixedBlock {
			return nil, 0, fmt.Errorf("lightclient: fixed block mode is pinned to block %d (got %d)", c.cfg.FixedBlock, block)
		}
	} else if block == 0 {
		block = c.state.LatestBlock()
	}
	if block == 0 {
		return nil, 0, fmt.Errorf("lightclient: no blocks processed yet")
	}

	miss := c.fetcher.MissCallbacks(c.state, &c.fetchStats)
	sv := NewStateView(c.state, block, miss)
	if prepare != nil {
		prepare(sv)
	}

	header := c.buildHeaderForBlock(block)
	getHash := c.getHashFunc()

	ret, gas, err := Call(msg, sv, header, c.chainCfg, getHash)
	return ret, gas, err
}

// DirectCallWithState executes an eth_call-style EVM call at the given block
// after preparing a fresh pinned StateView with any caller-supplied overrides.
// Unlike CallWithState, this bypasses ApplyMessage and goes straight through
// EVM.Call, which is closer to the legacy replay benchmark execution model.
func (c *LightClient) DirectCallWithState(msg CallMsg, block uint64, prepare func(*StateView)) ([]byte, uint64, error) {
	if c.cfg.FixedBlock != 0 {
		if block == 0 {
			block = c.cfg.FixedBlock
		} else if block != c.cfg.FixedBlock {
			return nil, 0, fmt.Errorf("lightclient: fixed block mode is pinned to block %d (got %d)", c.cfg.FixedBlock, block)
		}
	} else if block == 0 {
		block = c.state.LatestBlock()
	}
	if block == 0 {
		return nil, 0, fmt.Errorf("lightclient: no blocks processed yet")
	}

	miss := c.fetcher.MissCallbacks(c.state, &c.fetchStats)
	sv := NewStateView(c.state, block, miss)
	if prepare != nil {
		prepare(sv)
	}

	header := c.buildHeaderForBlock(block)
	getHash := c.getHashFunc()

	ret, gas, err := DirectCall(msg, sv, header, c.chainCfg, getHash)
	return ret, gas, err
}

// Start begins the block sync loop.
//
// Live mode (FixedBlock == 0): loads snapshot, catches up to head, then
// subscribes to new heads until the context is cancelled.
//
// Fixed-block mode (FixedBlock != 0): loads the fixed-block snapshot if present,
// otherwise pins immediately and fills state lazily on demand during calls.
// It does not subscribe to new heads and returns immediately.
func (c *LightClient) Start(ctx context.Context) error {
	// Load snapshot if available.
	snapPath := c.snapshotPath()
	if snap, err := LoadSnapshot(snapPath); err == nil {
		ApplySnapshot(c.state, snap)
		c.lastSnapshotBlock = snap.BlockNumber
		c.infof("lightclient: loaded snapshot at block %d", snap.BlockNumber)
	}

	if c.cfg.FixedBlock != 0 {
		targetBlock := c.cfg.FixedBlock
		loadedBlock := c.state.LatestBlock()
		if loadedBlock > targetBlock {
			return fmt.Errorf("lightclient: loaded snapshot at block %d, ahead of fixed block %d", loadedBlock, targetBlock)
		}
		if loadedBlock == 0 {
			c.state.SetLatestBlock(targetBlock)
			c.infof("lightclient: fixed block pinned at %d (lazy state fetch)", targetBlock)
			return nil
		}
		if loadedBlock != targetBlock {
			return fmt.Errorf("lightclient: fixed snapshot %s contains block %d, expected %d", snapPath, loadedBlock, targetBlock)
		}
		c.infof("lightclient: fixed block ready at %d (snapshot)", targetBlock)
		return nil
	}

	// Get current head from node.
	headNum, err := c.fetchHeadBlock()
	if err != nil {
		return fmt.Errorf("lightclient: fetch head: %w", err)
	}

	// Catch up from snapshot to head.
	startBlock := c.state.LatestBlock() + 1
	if startBlock <= headNum {
		c.infof("lightclient: catching up blocks %d → %d", startBlock, headNum)
		for blockNum := startBlock; blockNum <= headNum; blockNum++ {
			if err := c.processBlock(blockNum); err != nil {
				return fmt.Errorf("lightclient: catchup block %d: %w", blockNum, err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		c.infof("lightclient: caught up to block %d", headNum)
	}

	// Subscribe to new blocks.
	headsCh, err := c.pool.SubscribeNewHeads(ctx)
	if err != nil {
		return fmt.Errorf("lightclient: subscribe: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			c.saveSnapshotIfNeeded(true)
			c.pool.Close()
			return ctx.Err()

		case raw, ok := <-headsCh:
			if !ok {
				c.saveSnapshotIfNeeded(true)
				c.pool.Close()
				return fmt.Errorf("lightclient: subscription closed")
			}

			blockNum, err := parseNewHeadBlockNum(raw)
			if err != nil {
				log.Printf("lightclient: parse head notification: %v", err)
				continue
			}

			// Process all blocks from our latest to the new head
			// (handles gaps from dropped notifications).
			from := c.state.LatestBlock() + 1
			for n := from; n <= blockNum; n++ {
				if err := c.processBlock(n); err != nil {
					log.Printf("lightclient: block %d error: %v", n, err)
					break
				}
			}
		}
	}
}

// ─── Block processing ───────────────────────────────────────────────

func (c *LightClient) processBlock(blockNum uint64) error {
	start := time.Now()

	bd, err := c.fetcher.GetBlock(blockNum)
	if err != nil {
		return fmt.Errorf("fetch block: %w", err)
	}

	// Cache block hash for BLOCKHASH opcode.
	c.cacheBlockHash(blockNum, bd.Hash)

	// Convert BlockData → types.Block for the executor.
	block := BlockDataToTypesBlock(bd)

	// Create StateView at parent block.
	miss := c.fetcher.MissCallbacks(c.state, &c.fetchStats)
	sv := NewStateView(c.state, blockNum-1, miss)

	// Execute.
	diff, err := ExecuteBlock(block, sv, c.chainCfg, c.getHashFunc(), nil)
	if err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	// Apply diffs to versioned state.
	c.applyDiff(diff, blockNum)

	// Advance latest block pointer.
	c.state.SetLatestBlock(blockNum)

	elapsed := time.Since(start)
	c.infof("lightclient: block %d  txs=%d  elapsed=%v", blockNum, len(bd.Transactions), elapsed)

	// Periodic snapshot and prune.
	c.saveSnapshotIfNeeded(false)
	c.state.Prune(c.cfg.PruneKeepBlocks)

	// Notify callback.
	if c.cfg.OnBlock != nil {
		c.cfg.OnBlock(blockNum)
	}

	return nil
}

func (c *LightClient) applyDiff(diff *BlockDiff, block uint64) {
	for addr, slots := range diff.Storage {
		for slot, val := range slots {
			c.state.SetStorage(addr, slot, val, block)
		}
	}
	for addr, bal := range diff.Balances {
		c.state.SetBalance(addr, bal, block)
	}
	for addr, nonce := range diff.Nonces {
		c.state.SetNonce(addr, nonce, block)
	}
	for addr, code := range diff.Code {
		c.state.SetCode(addr, code, block)
	}
}

// ─── Block hash cache ───────────────────────────────────────────────

func (c *LightClient) cacheBlockHash(blockNum uint64, hash common.Hash) {
	c.blockHashesMu.Lock()
	c.blockHashes[blockNum] = hash
	// Prune old entries (keep last 256 + buffer).
	if len(c.blockHashes) > 300 {
		cutoff := blockNum - 256
		for k := range c.blockHashes {
			if k < cutoff {
				delete(c.blockHashes, k)
			}
		}
	}
	c.blockHashesMu.Unlock()
}

func (c *LightClient) getHashFunc() GetHashFunc {
	return func(n uint64) common.Hash {
		c.blockHashesMu.RLock()
		h, ok := c.blockHashes[n]
		c.blockHashesMu.RUnlock()
		if ok {
			return h
		}
		// Cache miss — fetch from node.
		h, err := c.fetcher.GetBlockHash(n)
		if err != nil {
			return common.Hash{}
		}
		c.cacheBlockHash(n, h)
		return h
	}
}

// ─── Snapshots ──────────────────────────────────────────────────────

func (c *LightClient) snapshotPath() string {
	if c.cfg.FixedBlock != 0 {
		return filepath.Join(c.cfg.DataDir, fmt.Sprintf("%d.snapshot", c.cfg.FixedBlock))
	}
	return filepath.Join(c.cfg.DataDir, "state.snapshot")
}

// Close persists the current snapshot and closes the RPC pool.
func (c *LightClient) Close() {
	if c.reporterStop != nil {
		close(c.reporterStop)
		<-c.reporterDone
		c.reporterStop = nil
	}
	c.saveSnapshotIfNeeded(true)
	if c.pool != nil {
		c.pool.Close()
	}
}

func (c *LightClient) saveSnapshotIfNeeded(force bool) {
	latest := c.state.LatestBlock()
	if c.cfg.FixedBlock != 0 && latest != c.cfg.FixedBlock {
		return
	}
	if !force && latest-c.lastSnapshotBlock < c.cfg.SnapshotEveryBlocks {
		return
	}
	if latest == 0 {
		return
	}
	if err := SaveSnapshot(c.state, c.snapshotPath()); err != nil {
		log.Printf("lightclient: snapshot save error: %v", err)
		return
	}
	c.lastSnapshotBlock = latest
	c.infof("lightclient: saved snapshot at block %d", latest)
}

// ─── Helpers ────────────────────────────────────────────────────────

func (c *LightClient) fetchHeadBlock() (uint64, error) {
	raw, err := c.pool.Call("eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}
	var hex string
	if err := json.Unmarshal(raw, &hex); err != nil {
		return 0, err
	}
	var n uint64
	fmt.Sscanf(hex, "0x%x", &n)
	return n, nil
}

func parseNewHeadBlockNum(raw json.RawMessage) (uint64, error) {
	var head struct {
		Number string `json:"number"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0, err
	}
	var n uint64
	fmt.Sscanf(head.Number, "0x%x", &n)
	return n, nil
}

// blockDataToTypesBlock converts our parsed BlockData into a types.Block
// that the executor expects, including Avalanche header extensions.
func BlockDataToTypesBlock(bd *BlockData) *types.Block {
	difficulty := bd.Difficulty
	if difficulty == nil {
		difficulty = new(big.Int)
	}
	baseFee := bd.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}

	header := &types.Header{
		ParentHash: bd.ParentHash,
		Number:     new(big.Int).SetUint64(bd.Number),
		Time:       bd.Timestamp,
		Difficulty: difficulty,
		GasLimit:   bd.GasLimit,
		BaseFee:    baseFee,
		Extra:      bytes.Clone(bd.Extra),
		MixDigest:  bd.MixDigest,
		Coinbase:   bd.Coinbase,
	}

	// Set Avalanche-specific header extensions.
	headerExtra := &ccustomtypes.HeaderExtra{}
	if bd.TimestampMilliseconds != nil {
		headerExtra.TimeMilliseconds = bd.TimestampMilliseconds
	}
	if bd.MinDelayExcess != nil {
		delay := acp226.DelayExcess(*bd.MinDelayExcess)
		headerExtra.MinDelayExcess = &delay
	}
	ccustomtypes.SetHeaderExtra(header, headerExtra)

	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: bd.Transactions})
}

// buildHeaderForBlock constructs a minimal header for Call execution.
// Uses cached block data where available.
func (c *LightClient) buildHeaderForBlock(block uint64) *types.Header {
	// For Call, we need a header with the basics. Fetch from node if needed.
	bd, err := c.fetcher.GetBlock(block)
	if err != nil {
		// Fallback: minimal header with just the block number.
		return &types.Header{
			Number: new(big.Int).SetUint64(block),
			Time:   uint64(time.Now().Unix()),
		}
	}
	return BlockDataToTypesBlock(bd).Header()
}

// ─── Chain config ───────────────────────────────────────────────────

func FetchChainConfig(pool *RPCPool) (*params.ChainConfig, error) {
	raw, err := pool.Call("eth_getChainConfig", []interface{}{})
	if err != nil {
		return nil, err
	}
	var cfg cparams.ChainConfigWithUpgradesJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	extra := cparams.GetExtra(&cfg.ChainConfig)
	extra.UpgradeConfig = cfg.UpgradeConfig
	if extra.DurangoBlockTimestamp != nil && !hasPrecompileUpgrade(extra.UpgradeConfig, warpcontract.ConfigKey) {
		extra.PrecompileUpgrades = append(extra.PrecompileUpgrades, cextras.PrecompileUpgrade{
			Config: warpcontract.NewDefaultConfig(extra.DurangoBlockTimestamp),
		})
	}
	if err := cparams.SetEthUpgrades(&cfg.ChainConfig); err != nil {
		return nil, err
	}

	// Fetch snow context (network ID, chain ID) and wire it into the chain config.
	// This is required for Avalanche precompiles (warp, etc.) to function.
	snowCtx, err := fetchSnowContext(pool)
	if err != nil {
		return nil, fmt.Errorf("snow context: %w", err)
	}
	extra.AvalancheContext = cextras.AvalancheContext{SnowCtx: snowCtx}

	return &cfg.ChainConfig, nil
}

func fetchSnowContext(pool *RPCPool) (*snow.Context, error) {
	// Fetch network ID via the C-Chain's net_version RPC.
	raw, err := pool.Call("net_version", []interface{}{})
	if err != nil {
		return nil, fmt.Errorf("net_version: %w", err)
	}
	var netVersion string
	if err := json.Unmarshal(raw, &netVersion); err != nil {
		return nil, fmt.Errorf("parse net_version: %w", err)
	}
	var networkID uint64
	fmt.Sscanf(netVersion, "%d", &networkID)

	// For Avalanche mainnet, the C-Chain blockchain ID is well-known.
	// On other networks we'd need the info API, but mainnet is the primary target.
	// Mainnet C-Chain ID: 2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5
	chainID, err := ids.FromString("2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5")
	if err != nil {
		return nil, fmt.Errorf("parse chain id: %w", err)
	}

	avaxAssetID, err := ids.FromString("FvwEAhmxKfeiG8SnEvq42hc6whRyY3EFYAvebMqDNDGCgxN5Z")
	if err != nil {
		return nil, fmt.Errorf("parse avax asset id: %w", err)
	}

	return &snow.Context{
		NetworkID:   uint32(networkID),
		ChainID:     chainID,
		CChainID:    chainID,
		AVAXAssetID: avaxAssetID,
	}, nil
}

func hasPrecompileUpgrade(cfg cextras.UpgradeConfig, key string) bool {
	for _, upgrade := range cfg.PrecompileUpgrades {
		if upgrade.Config != nil && upgrade.Key() == key {
			return true
		}
	}
	return false
}
