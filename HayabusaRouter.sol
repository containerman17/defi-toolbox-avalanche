// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

interface IERC20 {
    function balanceOf(address) external view returns (uint256);
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
}

interface IWAVAX {
    function deposit() external payable;
    function withdraw(uint256) external;
}

// === POOL INTERFACES ===

interface IUniV3Pool {
    function swap(
        address recipient,
        bool zeroForOne,
        int256 amountSpecified,
        uint160 sqrtPriceLimitX96,
        bytes calldata data
    ) external returns (int256, int256);
    function token0() external view returns (address);
    function token1() external view returns (address);
}

interface IAlgebraPool {
    function swap(
        address recipient,
        bool zeroForOne,
        int256 amountSpecified,
        uint160 sqrtPriceLimitX96,
        bytes calldata data
    ) external returns (int256, int256);
    function token0() external view returns (address);
    function token1() external view returns (address);
}

interface ILFJV1Pair {
    function swap(
        uint amount0Out,
        uint amount1Out,
        address to,
        bytes calldata data
    ) external;
    function getReserves() external view returns (uint112, uint112, uint32);
    function token0() external view returns (address);
    function token1() external view returns (address);
}

interface ILBPair {
    function swap(
        bool swapForY,
        address to
    ) external returns (bytes32 amountsOut);
    function getTokenX() external view returns (address);
    function getTokenY() external view returns (address);
}

// LFJ V2.0 minimal-proxy pools use tokenX()/tokenY() instead of getTokenX()/getTokenY()
interface ILBPairV20 {
    function swap(
        bool swapForY,
        address to
    ) external returns (bytes32 amountsOut);
    function tokenX() external view returns (address);
    function tokenY() external view returns (address);
}

interface IDODOPool {
    function sellBase(address to) external returns (uint256);
    function sellQuote(address to) external returns (uint256);
    function _BASE_TOKEN_() external view returns (address);
    function _QUOTE_TOKEN_() external view returns (address);
}

interface IWooRouter {
    function swap(
        address fromToken,
        address toToken,
        uint256 fromAmount,
        uint256 minToAmount,
        address payable to,
        address rebateTo
    ) external payable returns (uint256);
}

interface IPharaohV1Pair {
    function swap(
        uint amount0Out,
        uint amount1Out,
        address to,
        bytes calldata data
    ) external;
    function metadata()
        external
        view
        returns (
            uint256 dec0,
            uint256 dec1,
            uint256 r0,
            uint256 r1,
            bool st,
            address t0,
            address t1
        );
    function getAmountOut(
        uint256 amountIn,
        address tokenIn
    ) external view returns (uint256);
}

interface IBalancerV3Vault {
    enum SwapKind {
        EXACT_IN,
        EXACT_OUT
    }
    struct VaultSwapParams {
        SwapKind kind;
        address pool;
        IERC20 tokenIn;
        IERC20 tokenOut;
        uint256 amountGivenRaw;
        uint256 limitRaw;
        bytes userData;
    }
    function unlock(bytes calldata data) external returns (bytes memory);
    function swap(
        VaultSwapParams calldata params
    )
        external
        returns (uint256 amountCalculated, uint256 amountIn, uint256 amountOut);
    function settle(
        IERC20 token,
        uint256 amountHint
    ) external returns (uint256 credit);
    function sendTo(IERC20 token, address to, uint256 amount) external;

    enum WrappingDirection { WRAP, UNWRAP }
    struct BufferWrapOrUnwrapParams {
        SwapKind kind;
        WrappingDirection direction;
        IERC4626 wrappedToken;
        uint256 amountGivenRaw;
        uint256 limitRaw;
    }
    function erc4626BufferWrapOrUnwrap(
        BufferWrapOrUnwrapParams memory params
    ) external returns (uint256 amountCalculatedRaw, uint256 amountInRaw, uint256 amountOutRaw);
}

interface IERC4626 {
    function asset() external view returns (address);
    function deposit(uint256 assets, address receiver) external returns (uint256 shares);
    function redeem(uint256 shares, address receiver, address owner) external returns (uint256 assets);
}

interface IWombatPool {
    function swap(
        address fromToken,
        address toToken,
        uint256 fromAmount,
        uint256 minimumToAmount,
        address to,
        uint256 deadline
    ) external returns (uint256 actualToAmount, uint256 haircut);
}

interface ICavalrePool {
    function swap(address payToken, address receiveToken, uint256 payAmount, uint256 minReceiveAmount) external returns (uint256 receiveAmount, uint256 feeAmount);
}

interface IKyberDMMPool {
    function swap(uint amount0Out, uint amount1Out, address to, bytes calldata data) external;
    function token0() external view returns (address);
    function token1() external view returns (address);
    function getTradeInfo() external view returns (uint112, uint112, uint112, uint112, uint256);
}

interface ISynapsePool {
    function swap(uint8 tokenIndexFrom, uint8 tokenIndexTo, uint256 dx, uint256 minDy, uint256 deadline) external returns (uint256);
    function getToken(uint8 index) external view returns (address);
}


interface IBentoBox {
    function deposit(address token_, address from, address to, uint256 amount, uint256 share) external payable returns (uint256 amountOut, uint256 shareOut);
    function withdraw(address token_, address from, address to, uint256 amount, uint256 share) external returns (uint256 amountOut, uint256 shareOut);
}

interface ITridentPool {
    function swap(bytes calldata data) external returns (uint256 amountOut);
    function token0() external view returns (address);
    function token1() external view returns (address);
}

interface IBalancerV2Vault {
    enum SwapKind { GIVEN_IN, GIVEN_OUT }
    struct SingleSwap {
        bytes32 poolId;
        SwapKind kind;
        address assetIn;
        address assetOut;
        uint256 amount;
        bytes userData;
    }
    struct FundManagement {
        address sender;
        bool fromInternalBalance;
        address payable recipient;
        bool toInternalBalance;
    }
    function swap(
        SingleSwap memory singleSwap,
        FundManagement memory funds,
        uint256 limit,
        uint256 deadline
    ) external payable returns (uint256);
}

interface IPoolManager {
    struct PoolKey {
        address currency0;
        address currency1;
        uint24 fee;
        int24 tickSpacing;
        address hooks;
    }
    struct SwapParams {
        bool zeroForOne;
        int256 amountSpecified;
        uint160 sqrtPriceLimitX96;
    }
    function unlock(bytes calldata data) external returns (bytes memory);
    function swap(PoolKey memory key, SwapParams memory params, bytes calldata hookData)
        external returns (int256);
    function settle() external payable returns (uint256);
    function take(address token, address to, uint256 amount) external;
    function sync(address currency) external;
}

/// @notice HayabusaRouter - Flat-list swap router for Avalanche
/// @dev swap() takes a flat list of steps with interleaved tokens array.
///      Each step with amountIn > 0 uses that exact amount; amountIn == 0 uses the
///      contract's balanceOf(tokenIn), enabling multi-hop and split routes in a single call.
///      Owner exists only for recovering tokens accidentally sent to the contract.
///      Also exposes quoteMulti() for off-chain quoting (revert trick pattern).
contract HayabusaRouter {
    // Pool types
    uint8 constant UNIV3 = 0;
    uint8 constant ALGEBRA = 1;
    uint8 constant LFJ_V1 = 2;
    uint8 constant LFJ_V2 = 3;
    uint8 constant DODO = 4;
    uint8 constant WOOFI = 5;
    uint8 constant BALANCER_V3 = 6;
    uint8 constant PHARAOH_V1 = 7;
    uint8 constant PANGOLIN_V2 = 8;
    uint8 constant UNIV4 = 9;
    uint8 constant ERC4626_WRAP = 10;
    uint8 constant BALANCER_V3_BUFFERED = 11;
    uint8 constant WOMBAT = 12;
    uint8 constant PLATYPUS = 13;
    uint8 constant WOOPP_V2 = 14;
    uint8 constant TRANSFER_FROM = 15;
    uint8 constant BALANCER_V2 = 16;
    uint8 constant CAVALRE = 17;
    uint8 constant KYBER_DMM = 18;
    uint8 constant SYNAPSE = 19;
    uint8 constant TRIDENT = 20;

    // WooPP V2 pool on Avalanche (custom oracle-based pool with callback pattern)
    address constant WOOPP_V2_POOL = 0xABa7eD514217D51630053d73D358aC2502d3f9BB;

    // Balancer V2 vault on Avalanche
    IBalancerV2Vault constant BALANCER_V2_VAULT = IBalancerV2Vault(0xBA12222222228d8Ba445958a75a0704d566BF2C8);

    // V3 sqrt price limits
    uint160 constant MIN_SQRT_RATIO = 4295128739;
    uint160 constant MAX_SQRT_RATIO =
        1461446703485210103287273052203988822378723970342;

    // WOOFi router on Avalanche
    address constant WOOFI_ROUTER = 0x4c4AF8DBc524681930a27b2F1Af5bcC8062E6fB7;

    // Balancer V3 vault
    IBalancerV3Vault constant BALANCER_VAULT =
        IBalancerV3Vault(0xbA1333333333a1BA1108E8412f11850A5C319bA9);

    // Uniswap V4 PoolManager on Avalanche
    IPoolManager constant V4_POOL_MANAGER =
        IPoolManager(0x06380C0e0912312B5150364B9DC4542BA0DbBc85);
    address constant NATIVE = address(0);
    IWAVAX constant WAVAX = IWAVAX(0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7);

    // Transient storage for callbacks
    address private _currentTokenIn;
    address private _currentPool;
    uint256 private _currentAmountIn;

    // Balancer V3 callback params
    address private _balPool;
    address private _balTokenIn;
    address private _balTokenOut;
    uint256 private _balAmountIn;

    // Balancer V3 buffered callback params (wrap+swap+unwrap in one unlock)
    address private _bufUnderlyingIn;   // e.g. WAVAX
    address private _bufWrappedIn;      // e.g. waAvaWAVAX
    address private _bufWrappedOut;     // e.g. waAvaSAVAX
    address private _bufUnderlyingOut;  // e.g. sAVAX
    address private _bufPool;           // Balancer pool for the swap step
    uint256 private _bufAmountIn;

    // V4 callback params
    address private _v4TokenIn;
    address private _v4TokenOut;
    uint256 private _v4AmountIn;
    bytes private _v4ExtraData;

    // Owner — only for recovering stuck tokens
    address public owner;

    constructor() {
        owner = msg.sender;
    }

    // === PUBLIC SWAP ===

    /// @notice Flat-list swap: execute N independent steps in sequence.
    ///         tokens has 2*N entries: [tokenIn0, tokenOut0, tokenIn1, tokenOut1, ...]
    ///         amountsIn[i] > 0 uses that amount; amountsIn[i] == 0 uses balanceOf(tokenIn).
    /// @return The contract's balance of the last step's tokenOut
    function swap(
        address[] calldata pools,
        uint8[] calldata poolTypes,
        address[] calldata tokens,
        uint256[] calldata amountsIn,
        bytes[] calldata extraDatas
    ) external payable returns (uint256) {
        for (uint256 i = 0; i < pools.length;) {
            uint256 j = i * 2;
            uint256 amt = amountsIn[i];
            if (amt == 0) amt = IERC20(tokens[j]).balanceOf(address(this));
            _swapLeg(pools[i], poolTypes[i], tokens[j], tokens[j + 1], amt, extraDatas[i]);
            unchecked { ++i; }
        }
        return IERC20(tokens[pools.length * 2 - 1]).balanceOf(address(this));
    }

    /// @notice Quote a single pool+direction at multiple amounts.
    ///         Uses revert trick — each amount sees pristine reserves.
    function quoteMulti(
        address pool,
        uint8 poolType,
        address tokenIn,
        address tokenOut,
        uint256[] calldata amounts,
        bytes calldata extraData
    ) external returns (uint256[] memory amountsOut) {
        amountsOut = new uint256[](amounts.length);

        for (uint256 i = 0; i < amounts.length; i++) {
            try this._executeSingleRevert(pool, poolType, tokenIn, tokenOut, amounts[i], extraData) {
                amountsOut[i] = 0;
            } catch (bytes memory returnData) {
                if (returnData.length == 32) {
                    amountsOut[i] = abi.decode(returnData, (uint256));
                } else {
                    amountsOut[i] = 0;
                }
            }
        }
    }

    /// @dev External wrapper for try/catch — executes a single-hop swap
    function _executeSingle(
        address pool,
        uint8 poolType,
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        bytes calldata extraData
    ) external returns (uint256) {
        if (poolType == WOOPP_V2) {
            _balTokenOut = tokenOut;
            return _swapWooPPV2(tokenIn, amountIn);
        }
        if (poolType == BALANCER_V2) {
            _balTokenOut = tokenOut;
            return _swapBalancerV2(tokenIn, amountIn, extraData);
        }
        if (poolType == CAVALRE) {
            _balTokenOut = tokenOut;
            return _swapCavalre(tokenIn, amountIn, extraData);
        }
        if (poolType == SYNAPSE) {
            return _swapSynapse(pool, tokenIn, tokenOut, amountIn, extraData);
        }
        if (poolType == TRIDENT) {
            return _swapTrident(pool, tokenIn, tokenOut, amountIn, extraData);
        }
        bool zeroForOne = _getDirection(pool, poolType, tokenIn);
        if (poolType == BALANCER_V3 || poolType == WOOFI || poolType == WOMBAT || poolType == PLATYPUS) {
            _balTokenOut = tokenOut;
        }
        if (poolType == UNIV4) {
            return _swapUniV4(tokenIn, tokenOut, amountIn, extraData);
        }
        if (poolType == BALANCER_V3_BUFFERED) {
            return _swapBalancerV3Buffered(tokenIn, tokenOut, amountIn, extraData);
        }
        return _swap(pool, poolType, tokenIn, zeroForOne, amountIn);
    }

    /// @dev Revert trick wrapper: calls _executeSingle, then reverts with the result.
    function _executeSingleRevert(
        address pool,
        uint8 poolType,
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        bytes calldata extraData
    ) external {
        uint256 out = this._executeSingle(pool, poolType, tokenIn, tokenOut, amountIn, extraData);
        assembly { mstore(0x00, out) revert(0x00, 32) }
    }

    // === OWNER RECOVERY ===

    /// @notice Recover ERC20 tokens accidentally sent to this contract
    function withdraw(address token) external {
        require(msg.sender == owner, "not owner");
        uint256 bal = IERC20(token).balanceOf(address(this));
        if (bal > 0) {
            IERC20(token).transfer(owner, bal);
        }
    }

    /// @notice Recover AVAX accidentally sent to this contract
    function withdrawAVAX() external {
        require(msg.sender == owner, "not owner");
        uint256 bal = address(this).balance;
        if (bal > 0) {
            (bool ok, ) = payable(owner).call{value: bal}("");
            require(ok, "eth transfer failed");
        }
    }

    // === INTERNAL ROUTING ===

    function _swapLeg(
        address pool,
        uint8 poolType,
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        if (poolType == UNIV4) {
            return _swapUniV4(tokenIn, tokenOut, amountIn, extraData);
        }
        if (poolType == BALANCER_V3_BUFFERED) {
            return _swapBalancerV3Buffered(tokenIn, tokenOut, amountIn, extraData);
        }
        if (poolType == WOOPP_V2) {
            _balTokenOut = tokenOut;
            return _swapWooPPV2(tokenIn, amountIn);
        }
        if (poolType == TRANSFER_FROM) {
            // RFQ simulation: pull tokenOut from pool (vault EOA) via transferFrom.
            // Requires state overrides to set vault's tokenOut balance + approval.
            uint256 outAmount = abi.decode(extraData, (uint256));
            IERC20(tokenOut).transferFrom(pool, address(this), outAmount);
            return outAmount;
        }
        if (poolType == BALANCER_V2) {
            _balTokenOut = tokenOut;
            return _swapBalancerV2(tokenIn, amountIn, extraData);
        }
        if (poolType == CAVALRE) {
            _balTokenOut = tokenOut;
            return _swapCavalre(tokenIn, amountIn, extraData);
        }
        if (poolType == SYNAPSE) {
            return _swapSynapse(pool, tokenIn, tokenOut, amountIn, extraData);
        }
        if (poolType == TRIDENT) {
            return _swapTrident(pool, tokenIn, tokenOut, amountIn, extraData);
        }
        bool zeroForOne = _getDirection(pool, poolType, tokenIn);
        if (poolType == BALANCER_V3 || poolType == WOOFI || poolType == WOMBAT || poolType == PLATYPUS) {
            _balTokenOut = tokenOut;
        }
        // Custom fee for UniV2-style pools (oliveswap, vapordex): fee encoded in extraData
        if ((poolType == LFJ_V1 || poolType == PANGOLIN_V2) && extraData.length >= 32) {
            uint16 feeBps = uint16(abi.decode(extraData, (uint256)));
            return _swapLFJV1WithFee(pool, tokenIn, zeroForOne, amountIn, feeBps);
        }
        return _swap(pool, poolType, tokenIn, zeroForOne, amountIn);
    }

    function _getDirection(
        address pool,
        uint8 poolType,
        address tokenIn
    ) internal view returns (bool) {
        if (poolType == UNIV3) {
            return tokenIn == IUniV3Pool(pool).token0();
        } else if (poolType == ALGEBRA) {
            return tokenIn == IAlgebraPool(pool).token0();
        } else if (poolType == LFJ_V1 || poolType == PANGOLIN_V2 || poolType == KYBER_DMM) {
            return tokenIn == ILFJV1Pair(pool).token0();
        } else if (poolType == LFJ_V2) {
            return tokenIn == _lbjTokenX(pool);
        } else if (poolType == DODO) {
            return tokenIn == IDODOPool(pool)._BASE_TOKEN_();
        } else if (poolType == PHARAOH_V1) {
            (, , , , , address t0, ) = IPharaohV1Pair(pool).metadata();
            return tokenIn == t0;
        } else if (poolType == ERC4626_WRAP) {
            // zeroForOne = true means deposit (underlying→shares)
            return tokenIn == IERC4626(pool).asset();
        }
        return true;
    }

    function _swap(
        address pool,
        uint8 poolType,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        if (poolType == UNIV3) {
            return _swapUniV3(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == ALGEBRA) {
            return _swapAlgebra(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == LFJ_V1 || poolType == PANGOLIN_V2) {
            return _swapLFJV1(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == LFJ_V2) {
            return _swapLFJV2(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == DODO) {
            return _swapDODO(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == WOOFI) {
            return _swapWooFi(tokenIn, amountIn);
        } else if (poolType == BALANCER_V3) {
            return _swapBalancerV3(pool, tokenIn, amountIn);
        } else if (poolType == PHARAOH_V1) {
            return _swapPharaohV1(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == ERC4626_WRAP) {
            return _swapERC4626(pool, tokenIn, zeroForOne, amountIn);
        } else if (poolType == WOMBAT || poolType == PLATYPUS) {
            return _swapWombat(pool, tokenIn, amountIn);
        } else if (poolType == KYBER_DMM) {
            return _swapKyberDMM(pool, tokenIn, zeroForOne, amountIn);
        }
        revert("bad type");
    }

    // === V3-STYLE (CALLBACK) ===

    function _swapUniV3(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? IUniV3Pool(pool).token1()
            : IUniV3Pool(pool).token0();
        _currentTokenIn = tokenIn;
        _currentPool = pool;
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        IUniV3Pool(pool).swap(
            address(this),
            zeroForOne,
            int256(amountIn),
            zeroForOne ? MIN_SQRT_RATIO + 1 : MAX_SQRT_RATIO - 1,
            ""
        );
        _currentPool = address(0);
        _currentTokenIn = address(0);
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    function _swapAlgebra(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? IAlgebraPool(pool).token1()
            : IAlgebraPool(pool).token0();
        _currentTokenIn = tokenIn;
        _currentPool = pool;
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        IAlgebraPool(pool).swap(
            address(this),
            zeroForOne,
            int256(amountIn),
            zeroForOne ? MIN_SQRT_RATIO + 1 : MAX_SQRT_RATIO - 1,
            ""
        );
        _currentPool = address(0);
        _currentTokenIn = address(0);
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    // === TRANSFER-FIRST STYLE ===

    function _swapLFJV1(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? ILFJV1Pair(pool).token1()
            : ILFJV1Pair(pool).token0();
        (uint112 r0, uint112 r1, ) = ILFJV1Pair(pool).getReserves();
        uint256 rIn = zeroForOne ? r0 : r1;
        uint256 rOut = zeroForOne ? r1 : r0;

        // Transfer tokens to pool; use actual received amount for fee-on-transfer tokens
        uint256 poolBalBefore = IERC20(tokenIn).balanceOf(pool);
        IERC20(tokenIn).transfer(pool, amountIn);
        uint256 actualIn = IERC20(tokenIn).balanceOf(pool) - poolBalBefore;

        uint256 amountOut = (actualIn * 997 * rOut) /
            (rIn * 1000 + actualIn * 997);

        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        if (zeroForOne) {
            ILFJV1Pair(pool).swap(0, amountOut, address(this), "");
        } else {
            ILFJV1Pair(pool).swap(amountOut, 0, address(this), "");
        }
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    function _swapLFJV1WithFee(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn,
        uint16 feeBps
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? ILFJV1Pair(pool).token1()
            : ILFJV1Pair(pool).token0();
        (uint112 r0, uint112 r1, ) = ILFJV1Pair(pool).getReserves();
        uint256 rIn = zeroForOne ? r0 : r1;
        uint256 rOut = zeroForOne ? r1 : r0;

        IERC20(tokenIn).transfer(pool, amountIn);

        uint256 amountOut = (amountIn * feeBps * rOut) /
            (rIn * 10000 + amountIn * feeBps);

        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        if (zeroForOne) {
            ILFJV1Pair(pool).swap(0, amountOut, address(this), "");
        } else {
            ILFJV1Pair(pool).swap(amountOut, 0, address(this), "");
        }
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    /// @dev Returns tokenX for an LFJ V2 pair, supporting both getTokenX() (V2.1+)
    ///      and tokenX() (V2.0 minimal-proxy pools).
    function _lbjTokenX(address pool) internal view returns (address) {
        try ILBPair(pool).getTokenX() returns (address t) { return t; }
        catch { return ILBPairV20(pool).tokenX(); }
    }

    /// @dev Returns tokenY for an LFJ V2 pair, supporting both getTokenY() (V2.1+)
    ///      and tokenY() (V2.0 minimal-proxy pools).
    function _lbjTokenY(address pool) internal view returns (address) {
        try ILBPair(pool).getTokenY() returns (address t) { return t; }
        catch { return ILBPairV20(pool).tokenY(); }
    }

    function _swapLFJV2(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? _lbjTokenY(pool)
            : _lbjTokenX(pool);
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        IERC20(tokenIn).transfer(pool, amountIn);
        ILBPair(pool).swap(zeroForOne, address(this));
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    function _swapDODO(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? IDODOPool(pool)._QUOTE_TOKEN_()
            : IDODOPool(pool)._BASE_TOKEN_();
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        IERC20(tokenIn).transfer(pool, amountIn);
        if (zeroForOne) {
            IDODOPool(pool).sellBase(address(this));
        } else {
            IDODOPool(pool).sellQuote(address(this));
        }
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    function _swapPharaohV1(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        (, , , , , address t0, address t1) = IPharaohV1Pair(pool).metadata();
        address tokenOut = zeroForOne ? t1 : t0;

        // Transfer tokens to pool; use actual received amount for fee-on-transfer tokens
        uint256 poolBalBefore = IERC20(tokenIn).balanceOf(pool);
        IERC20(tokenIn).transfer(pool, amountIn);
        uint256 actualIn = IERC20(tokenIn).balanceOf(pool) - poolBalBefore;

        uint256 amountOut = IPharaohV1Pair(pool).getAmountOut(
            actualIn,
            tokenIn
        );

        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        if (zeroForOne) {
            IPharaohV1Pair(pool).swap(0, amountOut, address(this), "");
        } else {
            IPharaohV1Pair(pool).swap(amountOut, 0, address(this), "");
        }
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    function _swapWooFi(
        address tokenIn,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = _balTokenOut;
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        IERC20(tokenIn).approve(WOOFI_ROUTER, amountIn);
        IWooRouter(WOOFI_ROUTER).swap(
            tokenIn,
            tokenOut,
            amountIn,
            0,
            payable(address(this)),
            address(0)
        );
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    // === ERC-4626 WRAP/UNWRAP ===

    function _swapERC4626(
        address vault,
        address tokenIn,
        bool zeroForOne, // true = deposit (underlying→shares), false = redeem (shares→underlying)
        uint256 amountIn
    ) internal returns (uint256) {
        if (zeroForOne) {
            // Deposit: underlying → shares
            IERC20(tokenIn).approve(vault, amountIn);
            return IERC4626(vault).deposit(amountIn, address(this));
        } else {
            // Redeem: shares → underlying
            return IERC4626(vault).redeem(amountIn, address(this), address(this));
        }
    }

    // === WOOPP V2 (oracle-based pool with callback) ===

    function _swapWooPPV2(
        address tokenIn,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = _balTokenOut;
        _currentTokenIn = tokenIn;
        _currentPool = WOOPP_V2_POOL;
        _currentAmountIn = amountIn;
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));

        // Call WooPP V2's swap function (selector 0xac8bb7d9)
        // swap(address broker, uint256 direction, uint256 amount, uint256 minOutput, bytes data)
        // direction: 1 = sell base (non-USDC → USDC), 0 = sell quote (USDC → non-USDC)
        // data: abi.encode(tokenIn) — always the sold token (USDC when selling quote, baseToken when selling base)
        // minOutput: type(uint128).max signals flash-swap mode (WooPP sends tokenOut before callback)
        bool isSellingBase = tokenIn != 0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E; // USDC

        bytes memory callData = abi.encodePacked(
            bytes4(0xac8bb7d9),
            abi.encode(
                address(this),              // broker
                isSellingBase ? uint256(1) : uint256(0),  // direction
                amountIn,                   // amount
                isSellingBase ? uint256(0) : uint256(type(uint128).max)  // sell-base: 0, sell-quote: uint128.max (flash-swap)
            ),
            abi.encode(uint256(0xa0)),     // bytes offset (5th param offset)
            abi.encode(uint256(0x20)),     // bytes length
            abi.encode(tokenIn)            // bytes data = tokenIn (sold token)
        );

        (bool success, ) = WOOPP_V2_POOL.call(callData);
        require(success, "WooPP V2 swap failed");

        _currentPool = address(0);
        _currentTokenIn = address(0);
        _currentAmountIn = 0;
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    /// @dev WooPP V2 callback (selector 0xc3251075)
    /// Called by WooPP V2 during swap to pull tokenIn from this contract.
    /// Callback signature: 0xc3251075(int256 toAmount, uint256 fromAmount, bytes data)
    /// word1 (offset 4)  = toAmount (negative int256, tokens sent out by WooPP)
    /// word2 (offset 36) = fromAmount (uint256, tokens we need to send IN)
    fallback() external payable {
        if (msg.sender == WOOPP_V2_POOL && msg.data.length >= 4) {
            bytes4 sel;
            assembly { sel := calldataload(0) }
            if (sel == bytes4(0xc3251075)) {
                IERC20(_currentTokenIn).transfer(WOOPP_V2_POOL, _currentAmountIn);
                return;
            }
        }
        revert("unknown callback");
    }

    // === KYBER DMM ===

    function _swapKyberDMM(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = zeroForOne
            ? IKyberDMMPool(pool).token1()
            : IKyberDMMPool(pool).token0();

        (, , uint112 vReserve0, uint112 vReserve1, uint256 feeInPrecision) =
            IKyberDMMPool(pool).getTradeInfo();

        uint256 vReserveIn = zeroForOne ? vReserve0 : vReserve1;
        uint256 vReserveOut = zeroForOne ? vReserve1 : vReserve0;

        // Transfer tokens to pool; use actual received amount for fee-on-transfer tokens
        uint256 poolBalBefore = IERC20(tokenIn).balanceOf(pool);
        IERC20(tokenIn).transfer(pool, amountIn);
        uint256 actualIn = IERC20(tokenIn).balanceOf(pool) - poolBalBefore;

        uint256 amountInWithFee = actualIn * (1e18 - feeInPrecision) / 1e18;
        uint256 amountOut = vReserveOut * amountInWithFee / (vReserveIn + amountInWithFee);

        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        if (zeroForOne) {
            IKyberDMMPool(pool).swap(0, amountOut, address(this), "");
        } else {
            IKyberDMMPool(pool).swap(amountOut, 0, address(this), "");
        }
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    // === SYNAPSE ===

    function _swapSynapse(
        address pool,
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        (uint8 tokenIndexFrom, uint8 tokenIndexTo) = abi.decode(extraData, (uint8, uint8));
        IERC20(tokenIn).approve(pool, amountIn);
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        ISynapsePool(pool).swap(tokenIndexFrom, tokenIndexTo, amountIn, 0, block.timestamp);
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    // === TRIDENT ===

    function _swapTrident(
        address pool,
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        address bentoBox = abi.decode(extraData, (address));
        uint256 balBefore = IERC20(tokenOut).balanceOf(address(this));
        IERC20(tokenIn).approve(bentoBox, amountIn);
        IBentoBox(bentoBox).deposit(tokenIn, address(this), pool, amountIn, 0);
        ITridentPool(pool).swap(abi.encode(tokenIn, address(this), true));
        return IERC20(tokenOut).balanceOf(address(this)) - balBefore;
    }

    // === WOMBAT ===

    function _swapWombat(
        address pool,
        address tokenIn,
        uint256 amountIn
    ) internal returns (uint256) {
        address tokenOut = _balTokenOut;
        IERC20(tokenIn).approve(pool, amountIn);
        (uint256 actualToAmount, ) = IWombatPool(pool).swap(
            tokenIn,
            tokenOut,
            amountIn,
            0,
            address(this),
            block.timestamp
        );
        return actualToAmount;
    }

    // === BALANCER V2 ===

    function _swapBalancerV2(
        address tokenIn,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        address tokenOut = _balTokenOut;
        bytes32 poolId = abi.decode(extraData, (bytes32));
        IERC20(tokenIn).approve(address(BALANCER_V2_VAULT), amountIn);
        return BALANCER_V2_VAULT.swap(
            IBalancerV2Vault.SingleSwap({
                poolId: poolId,
                kind: IBalancerV2Vault.SwapKind.GIVEN_IN,
                assetIn: tokenIn,
                assetOut: tokenOut,
                amount: amountIn,
                userData: ""
            }),
            IBalancerV2Vault.FundManagement({
                sender: address(this),
                fromInternalBalance: false,
                recipient: payable(address(this)),
                toInternalBalance: false
            }),
            0,
            block.timestamp
        );
    }

    // === CAVALRE MULTISWAP ===

    function _swapCavalre(
        address tokenIn,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        address tokenOut = _balTokenOut;
        address pool = abi.decode(extraData, (address));
        IERC20(tokenIn).approve(pool, amountIn);
        (uint256 receiveAmount, ) = ICavalrePool(pool).swap(tokenIn, tokenOut, amountIn, 0);
        return receiveAmount;
    }

    // === BALANCER V3 ===

    function _swapBalancerV3(
        address pool,
        address tokenIn,
        uint256 amountIn
    ) internal returns (uint256) {
        _balPool = pool;
        _balTokenIn = tokenIn;
        _balAmountIn = amountIn;

        uint256 balBefore = IERC20(_balTokenOut).balanceOf(address(this));
        BALANCER_VAULT.unlock(abi.encodeCall(this.balancerUnlockCallback, ()));
        return IERC20(_balTokenOut).balanceOf(address(this)) - balBefore;
    }

    function balancerUnlockCallback() external returns (uint256 amountOut) {
        require(msg.sender == address(BALANCER_VAULT), "only vault");
        IERC20(_balTokenIn).transfer(address(BALANCER_VAULT), _balAmountIn);
        BALANCER_VAULT.settle(IERC20(_balTokenIn), _balAmountIn);
        (, , amountOut) = BALANCER_VAULT.swap(
            IBalancerV3Vault.VaultSwapParams({
                kind: IBalancerV3Vault.SwapKind.EXACT_IN,
                pool: _balPool,
                tokenIn: IERC20(_balTokenIn),
                tokenOut: IERC20(_balTokenOut),
                amountGivenRaw: _balAmountIn,
                limitRaw: 0,
                userData: ""
            })
        );
        BALANCER_VAULT.sendTo(IERC20(_balTokenOut), address(this), amountOut);
    }

    // === BALANCER V3 BUFFERED (wrap + swap + unwrap in one unlock) ===

    function _swapBalancerV3Buffered(
        address underlyingIn,
        address underlyingOut,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        // extraData: abi.encode(wrappedIn, pool, wrappedOut)
        (address wrappedIn, address pool, address wrappedOut) = abi.decode(extraData, (address, address, address));

        _bufUnderlyingIn = underlyingIn;
        _bufWrappedIn = wrappedIn;
        _bufPool = pool;
        _bufWrappedOut = wrappedOut;
        _bufUnderlyingOut = underlyingOut;
        _bufAmountIn = amountIn;

        uint256 balBefore = IERC20(underlyingOut).balanceOf(address(this));
        BALANCER_VAULT.unlock(abi.encodeCall(this.balancerBufferedUnlockCallback, ()));
        return IERC20(underlyingOut).balanceOf(address(this)) - balBefore;
    }

    function balancerBufferedUnlockCallback() external {
        require(msg.sender == address(BALANCER_VAULT), "only vault");

        // 1. Send underlying_in to vault and settle
        IERC20(_bufUnderlyingIn).transfer(address(BALANCER_VAULT), _bufAmountIn);
        BALANCER_VAULT.settle(IERC20(_bufUnderlyingIn), _bufAmountIn);

        // 2. Wrap: underlying_in → wrapped_in (via vault's internal buffer)
        (, , uint256 wrappedAmount) = BALANCER_VAULT.erc4626BufferWrapOrUnwrap(
            IBalancerV3Vault.BufferWrapOrUnwrapParams({
                kind: IBalancerV3Vault.SwapKind.EXACT_IN,
                direction: IBalancerV3Vault.WrappingDirection.WRAP,
                wrappedToken: IERC4626(_bufWrappedIn),
                amountGivenRaw: _bufAmountIn,
                limitRaw: 0
            })
        );

        // 3. Swap: wrapped_in → wrapped_out (in the Balancer pool)
        (, , uint256 swappedAmount) = BALANCER_VAULT.swap(
            IBalancerV3Vault.VaultSwapParams({
                kind: IBalancerV3Vault.SwapKind.EXACT_IN,
                pool: _bufPool,
                tokenIn: IERC20(_bufWrappedIn),
                tokenOut: IERC20(_bufWrappedOut),
                amountGivenRaw: wrappedAmount,
                limitRaw: 0,
                userData: ""
            })
        );

        // 4. Unwrap: wrapped_out → underlying_out (via vault's internal buffer)
        (, , uint256 underlyingOut) = BALANCER_VAULT.erc4626BufferWrapOrUnwrap(
            IBalancerV3Vault.BufferWrapOrUnwrapParams({
                kind: IBalancerV3Vault.SwapKind.EXACT_IN,
                direction: IBalancerV3Vault.WrappingDirection.UNWRAP,
                wrappedToken: IERC4626(_bufWrappedOut),
                amountGivenRaw: swappedAmount,
                limitRaw: 0
            })
        );

        // 5. Send underlying_out back to us
        BALANCER_VAULT.sendTo(IERC20(_bufUnderlyingOut), address(this), underlyingOut);
    }

    // === UNISWAP V4 ===

    function _swapUniV4(
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        bytes calldata extraData
    ) internal returns (uint256) {
        _v4TokenIn = tokenIn;
        _v4TokenOut = tokenOut;
        _v4AmountIn = amountIn;
        _v4ExtraData = extraData;

        uint256 balBefore = tokenOut == NATIVE
            ? address(this).balance
            : IERC20(tokenOut).balanceOf(address(this));
        V4_POOL_MANAGER.unlock(abi.encodeCall(this.v4UnlockCallback, ()));
        uint256 balAfter = tokenOut == NATIVE
            ? address(this).balance
            : IERC20(tokenOut).balanceOf(address(this));
        return balAfter - balBefore;
    }

    function v4UnlockCallback() external returns (bytes memory) {
        require(msg.sender == address(V4_POOL_MANAGER), "not pm");

        (uint256 fee, int256 tickSpacing, address hooks, uint256 wrapNative) =
            abi.decode(_v4ExtraData, (uint256, int256, address, uint256));

        // When wrapNative=1, the step uses WAVAX but the pool uses native AVAX.
        // Unwrap WAVAX input and wrap AVAX output.
        if (wrapNative == 1) {
            if (_v4TokenIn == address(WAVAX)) {
                WAVAX.withdraw(_v4AmountIn);
                _v4TokenIn = NATIVE;
            }
            if (_v4TokenOut == address(WAVAX)) {
                _v4TokenOut = NATIVE;
            }
        }

        address currency0;
        address currency1;
        bool zeroForOne;
        if (_v4TokenIn < _v4TokenOut) {
            currency0 = _v4TokenIn;
            currency1 = _v4TokenOut;
            zeroForOne = true;
        } else {
            currency0 = _v4TokenOut;
            currency1 = _v4TokenIn;
            zeroForOne = false;
        }

        IPoolManager.PoolKey memory key = IPoolManager.PoolKey({
            currency0: currency0,
            currency1: currency1,
            fee: uint24(fee),
            tickSpacing: int24(tickSpacing),
            hooks: hooks
        });

        int256 balanceDelta = V4_POOL_MANAGER.swap(
            key,
            IPoolManager.SwapParams({
                zeroForOne: zeroForOne,
                amountSpecified: -int256(_v4AmountIn),
                sqrtPriceLimitX96: zeroForOne ? MIN_SQRT_RATIO + 1 : MAX_SQRT_RATIO - 1
            }),
            ""
        );

        int128 delta0 = int128(balanceDelta >> 128);
        int128 delta1 = int128(balanceDelta);

        // Settle input
        if (_v4TokenIn == NATIVE) {
            V4_POOL_MANAGER.sync(NATIVE);
            V4_POOL_MANAGER.settle{value: _v4AmountIn}();
        } else {
            V4_POOL_MANAGER.sync(_v4TokenIn);
            IERC20(_v4TokenIn).transfer(address(V4_POOL_MANAGER), _v4AmountIn);
            V4_POOL_MANAGER.settle();
        }

        // Take output (positive delta = amount we receive)
        uint256 amountOut;
        if (zeroForOne) {
            amountOut = uint256(uint128(delta1));
        } else {
            amountOut = uint256(uint128(delta0));
        }
        V4_POOL_MANAGER.take(_v4TokenOut, address(this), amountOut);

        // Wrap native AVAX output back to WAVAX for downstream pools
        if (wrapNative == 1 && _v4TokenOut == NATIVE && amountOut > 0) {
            WAVAX.deposit{value: amountOut}();
        }

        return "";
    }

    /// @dev Standard V4 PoolManager callback - delegates to v4UnlockCallback via delegatecall
    function unlockCallback(bytes calldata) external returns (bytes memory) {
        require(msg.sender == address(V4_POOL_MANAGER), "not pm");
        // Use delegatecall so msg.sender remains PoolManager inside v4UnlockCallback
        (bool ok, bytes memory ret) = address(this).delegatecall(abi.encodeCall(this.v4UnlockCallback, ()));
        require(ok, "v4 callback failed");
        return ret;
    }

    // === CALLBACKS ===

    function uniswapV3SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata
    ) external {
        require(msg.sender == _currentPool, "not pool");
        uint256 amountToPay = amount0Delta > 0
            ? uint256(amount0Delta)
            : uint256(amount1Delta);
        IERC20(_currentTokenIn).transfer(msg.sender, amountToPay);
    }

    function algebraSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata
    ) external {
        require(msg.sender == _currentPool, "not pool");
        uint256 amountToPay = amount0Delta > 0
            ? uint256(amount0Delta)
            : uint256(amount1Delta);
        IERC20(_currentTokenIn).transfer(msg.sender, amountToPay);
    }

    function pangolinv3SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata
    ) external {
        require(msg.sender == _currentPool, "not pool");
        uint256 amountToPay = amount0Delta > 0
            ? uint256(amount0Delta)
            : uint256(amount1Delta);
        IERC20(_currentTokenIn).transfer(msg.sender, amountToPay);
    }

    function ramsesV2SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata
    ) external {
        require(msg.sender == _currentPool, "not pool");
        uint256 amountToPay = amount0Delta > 0
            ? uint256(amount0Delta)
            : uint256(amount1Delta);
        IERC20(_currentTokenIn).transfer(msg.sender, amountToPay);
    }

    receive() external payable {}
}
