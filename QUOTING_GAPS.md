# Quoting Pipeline Gaps

The swap-replay benchmark (`benchmarks/swap-replay/`) handles several swap types that the formula quoting pipeline (BFS pathfinder + formula engine) cannot. These are routes the router executes correctly on-chain but that are invisible to our quoter — missed opportunities for better pricing.

**Current state**: 98.4% precision, 0 overquotes. These gaps affect route discovery, not correctness.

---

## 1. ERC4626 Vault Wrap/Unwrap (Pool Type 10)

### What it is

ERC4626 vaults convert between underlying tokens and yield-bearing share tokens. For example, USDC → waAvaUSDC (deposit) or waAvaUSDC → USDC (redeem). These are single-hop conversions, not AMM swaps.

### 8 known vaults

| Vault (shares) | Underlying | Address |
|----------------|-----------|---------|
| waAvaWAVAX | WAVAX | `0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b` |
| waAvaSAVAX | sAVAX | `0x7d0394f8898fba73836bf12bd606228887705895` |
| waAvaUSDC | USDC | `0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009` |
| waAvaUSDC_v2 | USDC | `0x1f0570a081fee0e4df6eac470f9d2d53cdeda1c5` |
| waAvaUSDT | USDt | `0x59933c571d200dc6a7fd1cda22495db442082e34` |
| waAvaAUSD | AUSD | `0x45cf39eeb437fa95bb9b52c0105254a6bd25d01e` |
| waAvaBTC.b | BTC.b | `0x2d324fd1ca86d90f61b0965d2db2f86d22ea4b74` |
| ggAVAX | WAVAX | `0xa25eaf2906fa1a3a13edac9b9657108af7b703e3` |

Source: `tools/pool-collector/erc4626.ts:7-88`

### What the router does

`HayabusaRouter.sol:787-801` — `_swapERC4626()`:
- **Deposit** (underlying → shares): calls `IERC4626(vault).deposit(amountIn, router)`
- **Redeem** (shares → underlying): calls `IERC4626(vault).redeem(amountIn, router, router)`
- Direction determined by `tokenIn == IERC4626(pool).asset()` (line 541-543)

### What the quoter does today

Nothing. These vaults are not in `pools.txt`. The formula engine has no quoter for type 10. If encountered, `buildQuoter()` returns nil → `zeroQuoter` → always returns 0.

### How to fix

**Step 1: Add formula ID** — `formulas/registry.go`
```go
FormulaERC4626 = 13
```

**Step 2: Create quoter** — `formulas/pool_erc4626.go` (new file)

ERC4626 math is simple:
- `deposit`: `shares = amountIn * totalSupply / totalAssets`
- `redeem`:  `assets = amountIn * totalAssets / totalSupply`

Read `totalAssets()` (selector `0x01e1d114`) and `totalSupply()` (selector `0x18160ddd`) via `EVMCaller` at pool construction time.

**Step 3: Wire into buildQuoter** — `formulas/pool_quoter.go:397`

Add case to the switch:
```go
case FormulaERC4626:
    if p := newERC4626Pool(pool, pm.evmCaller); p != nil {
        return wrapAndCache(p)
    }
```

**Step 4: Inject vault pools** — Create Go definitions of the 8 vaults as `[]pathfinder.Pool` with PoolType=10. Inject them into the pool list + registry at startup in `quoter/quoter.go` and `benchmarks/formula-accuracy/main.go`.

**Step 5: Encoding** — Pool type 10 needs no extraData. The router reads `IERC4626(pool).asset()` on-chain to determine direction. Verify `pathfinder/encode.go` handles type 10 (no special encoding needed, same as V2).

---

## 2. Balancer V3 Buffered Swaps (Pool Type 11)

### What it is

A 3-step atomic swap: wrap underlying → BalV3 pool swap between wrapped tokens → unwrap to underlying. For example:

```
USDC → [deposit waAvaUSDC] → waAvaUSDC → [BalV3 pool] → waAvaWAVAX → [redeem] → WAVAX
```

This appears to the user as a single USDC → WAVAX hop, but internally chains ERC4626 deposit + BalV3 swap + ERC4626 redeem through the Balancer vault's `erc4626BufferWrapOrUnwrap()`.

### How edges are generated

`tools/pool-collector/erc4626.ts:104-129` — `generateBufferedEdges()`:

For each Balancer V3 pool (type 6) with 2+ wrapped tokens that are ERC4626 vaults, generate synthetic type 11 edges between their underlying tokens:
```
extraData = "wi=<wrappedIn>,bp=<balV3Pool>,wo=<wrappedOut>"
```

### What the router does

`HayabusaRouter.sol:1029-1094` — `_swapBalancerV3Buffered()`:

Decodes extraData as `abi.encode(wrappedIn, pool, wrappedOut)`, then uses the Balancer vault's `unlock()` callback to:
1. Transfer underlying_in to vault, settle
2. Wrap underlying_in → wrapped_in via `erc4626BufferWrapOrUnwrap(WRAP)`
3. Swap wrapped_in → wrapped_out in the BalV3 pool
4. Unwrap wrapped_out → underlying_out via `erc4626BufferWrapOrUnwrap(UNWRAP)`

### What the quoter does today

Nothing. Type 11 pools don't exist in `pools.txt` (they're synthetic). No formula ID, no quoter. The BFS can't discover these routes.

### How to fix

**Step 1: Add formula ID** — `formulas/registry.go`
```go
FormulaBufferedBalV3 = 14
```

**Step 2: Create quoter** — add to `formulas/pool_erc4626.go`

Chain three sub-quoters:
1. ERC4626 deposit quoter (underlying_in → wrapped_in)
2. BalV3 quoter for the pool (wrapped_in → wrapped_out) — reuse existing `pm.Get(balV3Pool)`
3. ERC4626 redeem quoter (wrapped_out → underlying_out)

Use a global registry (`RegisterBufferedBalV3Pool()`) same pattern as `formulas/pool_balancer_v3.go`.

**Step 3: Generate + inject synthetic edges** at startup

Same logic as `generateBufferedEdges()` in TypeScript:
- For each BalV3 pool with 2+ wrapped tokens in the ERC4626 vault list
- Generate synthetic pool with deterministic address: `keccak256(balV3Pool || wrappedIn || wrappedOut)[:20]`
- PoolType=11, Tokens=[underlyingIn, underlyingOut], ExtraData=`"wi=...,bp=...,wo=..."`
- Register with `FormulaBufferedBalV3` formula ID

**Step 4: Calldata encoding** — `pathfinder/encode.go`

Add `encodeBufferedExtraData()` that parses `"wi=...,bp=...,wo=..."` KV format and returns ABI-encoded `(address, address, address)` (96 bytes). Wire into `EncodeSwapSingleWithExtra` and `EncodeSwapMulti` for poolType 11.

---

## 3. V4 Native AVAX Wrapping

### What it is

Some Uniswap V4 pools use native AVAX (address(0)) instead of WAVAX. The router handles this by unwrapping WAVAX→AVAX before the V4 swap and wrapping AVAX→WAVAX after, so downstream pools see WAVAX.

### What the router does

`HayabusaRouter.sol:1122-1192` — `v4UnlockCallback()`:

When `wrapNative=1` in the V4 extraData:
```solidity
if (wrapNative == 1) {
    if (_v4TokenIn == address(WAVAX)) {
        WAVAX.withdraw(_v4AmountIn);  // WAVAX → native AVAX
        _v4TokenIn = NATIVE;          // address(0)
    }
    if (_v4TokenOut == address(WAVAX)) {
        _v4TokenOut = NATIVE;
    }
}
// ... execute swap with native AVAX ...
// After swap:
if (wrapNative == 1 && _v4TokenOut == NATIVE && amountOut > 0) {
    WAVAX.deposit{value: amountOut}();  // native AVAX → WAVAX
}
```

### What the quoter does today

`pathfinder/encode.go:59-72` — word 3 of V4 extraData is hardcoded to 0:
```go
// word 3: wrapNative = 0 (already zeroed)
```

The encoder already parses the `extraData` KV string and has the infrastructure. It just doesn't read a `wrapNative` key.

### How to fix

**Step 1**: In `encodeV4ExtraData()` (`pathfinder/encode.go:38-75`), parse `wrapNative` from the KV string and write to word 3:

```go
if wnStr, ok := parts["wrapNative"]; ok {
    var wn big.Int
    fmt.Sscan(wnStr, &wn)
    writeWord(buf, 96, &wn)
}
```

**Step 2**: When registering V4 pools that use native AVAX, include `wrapNative=1` in their ExtraData. The pool collector or V4 pool registration would need to detect pools with `currency0` or `currency1` == address(0) and normalize to WAVAX + set the flag.

**Impact**: Currently, V4 pools using native AVAX either aren't in the pool list or have the wrong token address. This fix enables the BFS to route through them correctly.

---

## 4. Token Override Fields Not Used by Go

### What it is

`contracts/token_overrides.json` has fields for reflection token storage slots that the Go override builder (`contracts/overrides.go`) doesn't read. These fields are used by the TypeScript benchmark but ignored by the Go formula engine.

### Missing fields

| JSON Field | Count | Purpose | Used in Go? |
|-----------|-------|---------|-------------|
| `rOwnedSlot` | 5 entries | Reflection token `_rOwned` mapping slot | No |
| `rTotalSlot` | 5 entries | Reflection token `_rTotal` storage slot | No |
| `tTotalSlot` | 2 entries | Reflection token `_tTotal` storage slot | No |
| `erc7201_allowance` | ~27 entries | ERC-7201 allowance base slot | No (derived from `erc7201_base + 1`) |

### Current Go struct (`contracts/overrides.go:25-37`)

```go
type tokenOverrideEntry struct {
    Address            string            `json:"address"`
    Slot               int               `json:"slot"`
    AllowanceSlot      *int              `json:"allowance_slot,omitempty"`
    ERC7201Base        string            `json:"erc7201_base,omitempty"`
    Shift              int               `json:"shift,omitempty"`
    Vyper              bool              `json:"vyper,omitempty"`
    HookContracts      []string          `json:"hookContracts,omitempty"`
    DisableSlots       []int             `json:"disableSlots,omitempty"`
    WhitelistSlots     []int             `json:"whitelistSlots,omitempty"`
    RouterAddressSlots []int             `json:"routerAddressSlots,omitempty"`
    CodeContracts      map[string]string `json:"codeContracts,omitempty"`
}
```

Missing: `rOwnedSlot`, `rTotalSlot`, `tTotalSlot`, `erc7201_allowance`.

### Where these should be used

The reflection token formulas in `formulas/fot.go` and `formulas/token_model.go` currently hardcode these slots in `reflectionTokenConfigs`. Instead, they could read from `token_overrides.json` for a single source of truth.

### How to fix

**Step 1**: Add fields to `tokenOverrideEntry` struct:
```go
ROwnedSlot *int `json:"rOwnedSlot,omitempty"`
RTotalSlot *int `json:"rTotalSlot,omitempty"`
TTotalSlot *int `json:"tTotalSlot,omitempty"`
```

**Step 2**: When building overrides for reflection tokens, use the stored `rOwnedSlot` to set the router's `_rOwned[router]` storage slot to a large value. This would fix input-direction overrides for reflection tokens where `_rOwned` is at a non-standard slot.

**Step 3**: Consider consolidating `reflectionTokenConfigs` (in `formulas/fot.go`) with `token_overrides.json` so slot information isn't duplicated.

**Impact**: 5 reflection tokens could have more accurate overrides. Low priority — these are mostly low-liquidity tokens.

---

## Priority Order

| # | Gap | Routes Unlocked | Effort | Priority |
|---|-----|-----------------|--------|----------|
| 1 | ERC4626 wrap/unwrap | 8 vault hops, enables BalV3 boosted pool access | Medium | **High** |
| 2 | BalV3 buffered swaps | Direct underlying→underlying routes through boosted pools | Medium (depends on #1) | **High** |
| 3 | V4 native AVAX wrapping | V4 pools using native AVAX | Small | Medium |
| 4 | Reflection token override fields | 5 reflection tokens | Small | Low |

Items 1 and 2 are the biggest wins — they unlock Balancer V3 boosted pool routes (USDC ↔ WAVAX ↔ sAVAX ↔ USDt etc.) which are high-liquidity paths currently invisible to the quoter.
