import { readFileSync, writeFileSync } from 'fs'
import path from 'path'
import solc from 'solc'

console.log('Compiling HayabusaRouter.sol...')
const source = readFileSync(path.join(import.meta.dirname, '../../HayabusaRouter.sol'), 'utf-8')
const input = {
    language: 'Solidity',
    sources: { 'HayabusaRouter.sol': { content: source } },
    settings: {
        evmVersion: 'paris',
        viaIR: true,
        optimizer: { enabled: true, runs: 200 },
        outputSelection: { '*': { '*': ['evm.deployedBytecode.object'] } }
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
const deployedBytecode = contract.evm.deployedBytecode.object

writeFileSync(path.join(import.meta.dirname, 'bytecode.hex'), deployedBytecode)
console.log(`Wrote bytecode.hex: ${deployedBytecode.length / 2} bytes`)
