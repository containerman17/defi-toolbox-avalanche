// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title HayabusaProxy — Minimal EIP-1967 transparent proxy
/// @notice Delegates all non-admin calls to the implementation contract.
///         Admin (separate address from router owner) can only upgrade.
///         Router owner interacts through the proxy like any other user.
///
/// Storage layout uses EIP-1967 standard slots so block explorers
/// can automatically detect the proxy pattern.
contract HayabusaProxy {
    /// @dev EIP-1967 implementation slot: keccak256("eip1967.proxy.implementation") - 1
    bytes32 internal constant IMPL_SLOT =
        0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc;

    /// @dev EIP-1967 admin slot: keccak256("eip1967.proxy.admin") - 1
    bytes32 internal constant ADMIN_SLOT =
        0xb53127684a568b3173ae13b9f8a6016e243e63b6e8ee1178d6a717850b5d6103;

    /// @param implementation Address of the initial HayabusaRouter implementation
    /// @param admin Address that can call upgradeTo (derived from deployer key)
    /// @param data Calldata for initialization (typically initialize(owner) encoded)
    constructor(address implementation, address admin, bytes memory data) {
        assembly {
            sstore(IMPL_SLOT, implementation)
            sstore(ADMIN_SLOT, admin)
        }
        if (data.length > 0) {
            (bool ok, ) = implementation.delegatecall(data);
            require(ok, "proxy: init failed");
        }
    }

    /// @notice Upgrade to a new implementation. Only callable by admin.
    /// @dev Uses a fixed selector check so admin calls never reach the implementation.
    ///      Admin key is derived deterministically: keccak256(deployerKey || salt).
    function upgradeTo(address newImplementation) external {
        assembly {
            if iszero(eq(caller(), sload(ADMIN_SLOT))) { revert(0, 0) }
            sstore(IMPL_SLOT, newImplementation)
        }
    }

    /// @notice Returns the current admin address. Only callable by admin.
    function proxyAdmin() external view returns (address admin) {
        assembly {
            if iszero(eq(caller(), sload(ADMIN_SLOT))) { revert(0, 0) }
            admin := sload(ADMIN_SLOT)
        }
    }

    /// @notice Returns the current implementation address. Only callable by admin.
    function implementation() external view returns (address impl) {
        assembly {
            if iszero(eq(caller(), sload(ADMIN_SLOT))) { revert(0, 0) }
            impl := sload(IMPL_SLOT)
        }
    }

    /// @dev Transparent proxy: if caller is admin, only proxy admin functions
    ///      are accessible (handled above). All other callers are delegated
    ///      to the implementation. This prevents admin from accidentally
    ///      calling implementation functions (e.g., swap, withdraw).
    fallback() external payable {
        assembly {
            // Admin calls that reach fallback are rejected — admin must use
            // the explicit upgradeTo/proxyAdmin/implementation functions.
            if eq(caller(), sload(ADMIN_SLOT)) { revert(0, 0) }

            let impl := sload(IMPL_SLOT)
            calldatacopy(0, 0, calldatasize())
            let ok := delegatecall(gas(), impl, 0, calldatasize(), 0, 0)
            returndatacopy(0, 0, returndatasize())
            switch ok
            case 0 { revert(0, returndatasize()) }
            default { return(0, returndatasize()) }
        }
    }

    receive() external payable {}
}
