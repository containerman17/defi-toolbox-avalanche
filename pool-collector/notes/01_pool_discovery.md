Use 56195375 as the starting block

# Pool Discovery Logic

This package maintains a `pools.txt` snapshot and incrementally catches it up from on-chain logs.

The core invariant is:

- Every persisted row in `pools.txt` must contain enough information to rebuild the in-memory state needed to classify future logs after a restart.

That matters most for Uniswap V4 because swap logs are keyed by a 32-byte `poolId`, while the persisted pool address is only a 20-byte pseudo-address derived from the first 20 bytes of that `poolId`.

## Runtime Flow

`discover()` does this:

1. Load `pools.txt` if it exists.
2. Rebuild any in-memory provider caches from persisted pool metadata.
3. Determine the scan start block.
4. Fetch logs in batches using the union of all provider topics.
5. Let each provider classify the batch and emit discovery events.
6. Merge those events into the persisted pool map.
7. Save periodically and again at the end.

The package does not maintain a second sidecar state file. `pools.txt` is the only durable snapshot.

## `pools.txt` Format

Line 1 is the last processed block.

Each later line is:

```text
address:providerName:poolType:latestSwapBlock:token0:token1[:tokenN...][:@extraData]
```

Notes:

- `address` is the real pool address for every protocol except Uniswap V4.
- For Uniswap V4, `address` is a pseudo-address equal to the first 20 bytes of the 32-byte `poolId`.
- `extraData` is provider-specific durable metadata.
- `extraData` must be treated as part of the persisted discovery state, not as cosmetic output.

## Event Model

Internally the discovery pipeline merges provider outputs into `StoredPool` rows.

Historically the code only persisted swap-derived events. That worked for protocols where swap logs plus RPC verification were enough to re-identify the pool after restart.

For Uniswap V4, that was not enough:

- `Initialize` creates the `poolId -> pool metadata` mapping.
- later `Swap` logs use the full `poolId`
- the persisted row only used the pseudo-address
- restart lost the exact `poolId`
- future swaps for previously initialized pools could be silently dropped

The fix is to persist V4 discovery on `Initialize`, not only on `Swap`.

## Provider Logic

### V3-style pools

Providers:

- `uniswap_v3`
- `pharaoh_v3`
- `algebra`

Detection:

- scan `Swap(...)` topic
- use `factory()`, `token0()`, `token1()` through the cached RPC client
- match the returned factory against the provider's allowed factory set

Persistence:

- pool address is the real contract address
- tokens are persisted directly
- no extra durable metadata is required

### V2-style pools

Providers:

- `lfj_v1`
- `pangolin_v2`
- `arena_v2`
- `sushiswap_v2`
- `canary`
- `complus`
- `lydia`
- `hurricane`
- `fraxswap`
- `swapsicle`
- `uniswap_v2`
- `thorus`
- `radioshack`
- `vapordex`
- `elkdex`
- `yetiswap`
- `partyswap`
- `oliveswap`
- `zeroex`

Detection:

- scan the V2-style `Swap(...)` topic
- use `factory()`, `token0()`, `token1()`
- match the factory against the provider's factory set

Persistence:

- pool address is the real contract address
- tokens are persisted directly
- no extra durable metadata is required

### LFJ V2

Detection:

- scan LFJ V2 swap topics
- use `getFactory()`, `getTokenX()`, `getTokenY()`
- match factory against LFJ V2 factory set

Persistence:

- pool address is the real contract address
- tokens are persisted directly

### Balancer V3

Detection:

- scan Balancer Vault swap topic
- read pool and token addresses directly from indexed topics

Persistence:

- pool address is the Balancer pool address extracted from the event
- tokens are persisted directly

### DODO

Detection:

- scan `DODOSwap(...)`
- decode tokens and amounts from log data

Persistence:

- pool address is the emitting pool contract
- tokens are persisted directly

### WooFi

Providers:

- `woofi_v2`
- `woofi_pp`

Detection:

- `woofi_v2` reads router swap logs
- `woofi_pp` reads WooPP direct swap logs

Persistence:

- pool address is the emitting router or pool contract, matching the current provider semantics
- tokens are persisted directly

### Pharaoh V1

Detection:

- scan V2-style swap topic
- probe `stable()` and `metadata()` via RPC

Persistence:

- pool address is the real contract address
- tokens are persisted directly

### Uniswap V4

Detection uses two event types:

- `Initialize(bytes32 id, address currency0, address currency1, uint24 fee, int24 tickSpacing, address hooks)`
- `Swap(bytes32 id, ...)`

Runtime state:

- an in-memory map `poolId -> { currency0, currency1, fee, tickSpacing, hooks }`

Persistence rules:

- the persisted pool address is the pseudo-address `poolId.slice(0, 20 bytes)`
- `extraData` must include the full `poolId`

The V4 `extraData` format is:

```text
id=0x...,fee=...,ts=...,hooks=0x...
```

Behavior:

- on `Initialize`, store the exact `poolId` in memory
- on `Initialize`, immediately emit a discovery event so the pool is persisted even if no swap has happened yet
- on `Swap`, look up the full `poolId` in the in-memory map
- on restart, rebuild the in-memory map from persisted V4 rows by parsing `id=...` from `extraData`

This is the key V4 invariant:

- if a V4 row exists in `pools.txt`, it must carry `id=...`

Without that field, exact restart reconstruction is impossible.

## Why There Is No `v4pools.txt`

The package intentionally keeps a single durable snapshot:

- easier atomic writes
- easier packaging
- no cross-file consistency bugs
- all provider-specific restart state lives with the pool row that depends on it

For V4, that means the right place for the full `poolId` is `extraData`, not a sidecar file.

## Legacy V4 Rows

Older `pools.txt` files may contain Uniswap V4 rows without `id=...`.

Those rows are legacy-invalid because:

- the pseudo-address is only 20 bytes
- swap lookup needs the full 32-byte `poolId`
- the missing 12 bytes cannot be reconstructed from the pseudo-address

Current behavior is to fail fast if any persisted V4 row lacks `id=...`.

Required recovery:

1. Delete or replace the old `pools.txt`.
2. Run discovery from historical blocks so V4 `Initialize` events are reprocessed.
3. Save the regenerated snapshot with `id=...` embedded in each V4 row.

## Bootstrap vs Catch-up

There are two valid operating modes:

### Catch-up from a valid `pools.txt`

- load existing pools
- seed in-memory V4 state from persisted V4 rows
- resume from `lastProcessedBlock + 1`

This is the normal mode.

### Full rebuild

- start from an explicit historical block
- process all discovery topics from that point forward
- let providers rebuild the full durable snapshot

This is required when the existing file contains legacy-invalid V4 rows.

`discover()` supports an explicit `startBlock` override, and the CLI also accepts a third positional argument or `DISCOVERY_START_BLOCK`.

## Design Constraint

Discovery should persist facts, not guesses.

That means:

- do not reconstruct the V4 `poolId` by padding the pseudo-address
- do not rely on a preload pass to repair missing durable state
- do not treat `extraData` as optional if runtime correctness depends on it

If a future provider needs restart-only metadata, that metadata should also be persisted in the same pool row.
