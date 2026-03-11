import { readFileSync, writeFileSync } from 'fs'
import path from 'path'
import solc from 'solc'
import { createPublicClient, createWalletClient, http } from 'viem'
import { avalanche } from 'viem/chains'
import { privateKeyToAccount } from 'viem/accounts'
import { config } from "dotenv"

const rootEnv = path.join(import.meta.dirname, '../../../../../../.env')
config({ path: rootEnv })

const RPC = 'https://api.avax.network/ext/bc/C/rpc'
const privateKey = process.env.PRIVATE_KEY
if (!privateKey) {
    console.error('Set PRIVATE_KEY in .env')
    process.exit(1)
}

// Compile
console.log('Compiling HayabusaRouter.sol...')
const source = readFileSync(path.join(import.meta.dirname, '../../HayabusaRouter.sol'), 'utf-8')
const input = {
    language: 'Solidity',
    sources: { 'HayabusaRouter.sol': { content: source } },
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
const contract = output.contracts['HayabusaRouter.sol']['HayabusaRouter']
const abi = contract.abi
const bytecode = `0x${contract.evm.bytecode.object}` as `0x${string}`
const deployedBytecode = contract.evm.deployedBytecode.object

writeFileSync(path.join(import.meta.dirname, 'bytecode.hex'), deployedBytecode)
console.log(`Bytecode: ${contract.evm.bytecode.object.length / 2} bytes (init), ${deployedBytecode.length / 2} bytes (runtime)`)

// Deploy
const transport = http(RPC)
const publicClient = createPublicClient({ chain: avalanche, transport })
const account = privateKeyToAccount(privateKey as `0x${string}`)
const walletClient = createWalletClient({ account, chain: avalanche, transport })

console.log(`\nDeploying from ${account.address}...`)

const nonce = await publicClient.getTransactionCount({ address: account.address, blockTag: 'pending' })
console.log(`Nonce: ${nonce}`)

const hash = await walletClient.deployContract({
    abi, bytecode, args: [],
    nonce,
    gas: 5_000_000n,
    maxFeePerGas: 50_000_000_000n,
    maxPriorityFeePerGas: 1_000_000_000n,
})
console.log(`TX: ${hash}`)

const receipt = await publicClient.waitForTransactionReceipt({
    hash,
    pollingInterval: 500,
    timeout: 60_000,
})
const routerAddress = receipt.contractAddress!
console.log(`\nDeployed at: ${routerAddress}`)
console.log(`\nUpdate ROUTER_ADDRESS in types.ts to: "${routerAddress}"`)
