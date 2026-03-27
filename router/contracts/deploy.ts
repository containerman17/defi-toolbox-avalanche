import { readFileSync, writeFileSync } from 'fs'
import path from 'path'
import solc from 'solc'
import { createPublicClient, createWalletClient, http } from 'viem'
import { avalanche } from 'viem/chains'
import { privateKeyToAccount } from 'viem/accounts'
import { config } from "dotenv"

config({ path: path.join(import.meta.dirname, '../../.env') })

const RPC = 'https://api.avax.network/ext/bc/C/rpc'
let privateKey = process.env.ARB_PRIVATE_KEY
if (!privateKey) {
    console.error('Set ARB_PRIVATE_KEY in .env')
    process.exit(1)
}
if (!privateKey.startsWith('0x')) privateKey = '0x' + privateKey

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

const baseFee = await publicClient.getGasPrice()
console.log(`Base fee: ${baseFee} wei`)

const hash = await walletClient.deployContract({
    abi, bytecode, args: [],
    nonce,
    gas: 5_000_000n,
    maxFeePerGas: baseFee * 2n,
    maxPriorityFeePerGas: 0n,
})
console.log(`TX: ${hash}`)

const receipt = await publicClient.waitForTransactionReceipt({
    hash,
    pollingInterval: 500,
    timeout: 60_000,
})
const routerAddress = receipt.contractAddress!
const deployBlock = Number(receipt.blockNumber)
console.log(`\nDeployed at: ${routerAddress} (block ${deployBlock})`)

// Update address.json
const addressJson = JSON.stringify({ address: routerAddress, block: deployBlock }) + '\n'
writeFileSync(path.join(import.meta.dirname, 'address.json'), addressJson)
console.log('Updated router/contracts/address.json')
