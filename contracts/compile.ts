import { readFileSync, writeFileSync } from 'fs'
import path from 'path'
import solc from 'solc'

const newProxy = process.argv.includes('--new-proxy')
const deploy = process.argv.includes('--deploy') || newProxy

// Admin key derivation salt — public, deterministic.
// adminKey = keccak256(deployerKey || ADMIN_SALT)
const ADMIN_SALT = 'hayabusa-proxy-admin'

// ── Compile ─────────────────────────────────────────────────────────

console.log('Compiling contracts...')

const routerSource = readFileSync(path.join(import.meta.dirname!, 'HayabusaRouter.sol'), 'utf-8')
const proxySource = readFileSync(path.join(import.meta.dirname!, 'HayabusaProxy.sol'), 'utf-8')

const input = {
    language: 'Solidity',
    sources: {
        'HayabusaRouter.sol': { content: routerSource },
        'HayabusaProxy.sol': { content: proxySource },
    },
    settings: {
        evmVersion: 'paris',
        viaIR: true,
        optimizer: { enabled: true, runs: 200 },
        outputSelection: { '*': { '*': ['abi', 'evm.bytecode.object', 'evm.deployedBytecode.object'] } }
    }
}
const output = JSON.parse(solc.compile(JSON.stringify(input)))
if (output.errors?.some((e: any) => e.severity === 'error')) {
    console.error(output.errors)
    process.exit(1)
}
if (output.errors?.length) {
    output.errors.forEach((e: any) => console.warn(e.severity + ':', e.message))
}

const routerContract = output.contracts['HayabusaRouter.sol']['HayabusaRouter']
const proxyContract = output.contracts['HayabusaProxy.sol']['HayabusaProxy']

const deployedBytecode = routerContract.evm.deployedBytecode.object
writeFileSync(path.join(import.meta.dirname!, 'bytecode.hex'), deployedBytecode)
console.log(`Wrote bytecode.hex: ${deployedBytecode.length / 2} bytes (router runtime)`)

if (!deploy) process.exit(0)

// ── Deploy ──────────────────────────────────────────────────────────

const { createPublicClient, createWalletClient, http, keccak256, concat, toHex, encodeFunctionData } = await import('viem')
const { avalanche } = await import('viem/chains')
const { privateKeyToAccount } = await import('viem/accounts')
const { config } = await import('dotenv')

config({ path: path.join(import.meta.dirname!, '../.env') })

const RPC = 'https://api.avax.network/ext/bc/C/rpc'
let privateKey = process.env.ARB_PRIVATE_KEY
if (!privateKey) {
    console.error('Set ARB_PRIVATE_KEY in .env')
    process.exit(1)
}
if (!privateKey.startsWith('0x')) privateKey = '0x' + privateKey

// Derive admin key: keccak256(deployerKey || salt)
const adminKey = keccak256(concat([privateKey as `0x${string}`, toHex(ADMIN_SALT)])) as `0x${string}`

const transport = http(RPC)
const publicClient = createPublicClient({ chain: avalanche, transport })

const deployerAccount = privateKeyToAccount(privateKey as `0x${string}`)
const adminAccount = privateKeyToAccount(adminKey)
const deployerWallet = createWalletClient({ account: deployerAccount, chain: avalanche, transport })
const adminWallet = createWalletClient({ account: adminAccount, chain: avalanche, transport })

console.log(`\nDeployer (router owner): ${deployerAccount.address}`)
console.log(`Admin (proxy upgrades):  ${adminAccount.address}`)
console.log(`Salt: "${ADMIN_SALT}"`)

const baseFee = await publicClient.getGasPrice()

// Step 1: Deploy new implementation
const implNonce = await publicClient.getTransactionCount({ address: deployerAccount.address, blockTag: 'pending' })
console.log(`\nDeploying implementation... (nonce ${implNonce})`)

const implHash = await deployerWallet.deployContract({
    abi: routerContract.abi,
    bytecode: `0x${routerContract.evm.bytecode.object}`,
    args: [],
    nonce: implNonce,
    gas: 5_000_000n,
    maxFeePerGas: baseFee * 2n,
    maxPriorityFeePerGas: 0n,
})
const implReceipt = await publicClient.waitForTransactionReceipt({ hash: implHash, pollingInterval: 500, timeout: 60_000 })
const implAddress = implReceipt.contractAddress!
console.log(`Implementation: ${implAddress} (block ${implReceipt.blockNumber})`)

if (newProxy) {
    // Step 2a: Deploy new proxy (one-time operation)
    // Encode initialize(owner) calldata for the proxy constructor
    const initData = encodeFunctionData({
        abi: routerContract.abi,
        functionName: 'initialize',
        args: [deployerAccount.address],
    })

    const proxyNonce = await publicClient.getTransactionCount({ address: deployerAccount.address, blockTag: 'pending' })
    console.log(`\nDeploying NEW proxy... (nonce ${proxyNonce})`)

    const proxyHash = await deployerWallet.deployContract({
        abi: proxyContract.abi,
        bytecode: `0x${proxyContract.evm.bytecode.object}`,
        args: [implAddress, adminAccount.address, initData],
        nonce: proxyNonce,
        gas: 1_000_000n,
        maxFeePerGas: baseFee * 2n,
        maxPriorityFeePerGas: 0n,
    })
    const proxyReceipt = await publicClient.waitForTransactionReceipt({ hash: proxyHash, pollingInterval: 500, timeout: 60_000 })
    const proxyAddress = proxyReceipt.contractAddress!
    console.log(`Proxy: ${proxyAddress} (block ${proxyReceipt.blockNumber})`)

    const addressJson = JSON.stringify({
        proxy: proxyAddress,
        implementation: implAddress,
        block: Number(proxyReceipt.blockNumber),
        // Legacy field — points to proxy so existing code works unchanged
        address: proxyAddress,
    }, null, 2) + '\n'
    writeFileSync(path.join(import.meta.dirname!, 'address.json'), addressJson)
    console.log('\nUpdated address.json (new proxy)')
} else {
    // Step 2b: Upgrade existing proxy to new implementation
    const addressJson = JSON.parse(readFileSync(path.join(import.meta.dirname!, 'address.json'), 'utf-8'))
    const proxyAddress = addressJson.proxy || addressJson.address
    console.log(`\nUpgrading proxy ${proxyAddress} → ${implAddress}`)

    const upgradeNonce = await publicClient.getTransactionCount({ address: adminAccount.address, blockTag: 'pending' })
    const upgradeHash = await adminWallet.writeContract({
        address: proxyAddress as `0x${string}`,
        abi: proxyContract.abi,
        functionName: 'upgradeTo',
        args: [implAddress],
        nonce: upgradeNonce,
        gas: 100_000n,
        maxFeePerGas: baseFee * 2n,
        maxPriorityFeePerGas: 0n,
    })
    await publicClient.waitForTransactionReceipt({ hash: upgradeHash, pollingInterval: 500, timeout: 60_000 })
    console.log(`Upgrade TX: ${upgradeHash}`)

    const updatedJson = JSON.stringify({
        proxy: proxyAddress,
        implementation: implAddress,
        block: addressJson.block,
        address: proxyAddress,
    }, null, 2) + '\n'
    writeFileSync(path.join(import.meta.dirname!, 'address.json'), updatedJson)
    console.log('Updated address.json (new implementation)')
}
