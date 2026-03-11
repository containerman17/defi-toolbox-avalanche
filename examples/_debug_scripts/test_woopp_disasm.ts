import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;

const code = await client.getBytecode({ address: WOOPP as `0x${string}`, blockNumber: block });
const hex = (code as string).slice(2);
const bytes = Buffer.from(hex, "hex");

// Simple EVM disassembler
const opcodes: Record<number, string> = {
  0x00: "STOP", 0x01: "ADD", 0x02: "MUL", 0x03: "SUB", 0x04: "DIV", 0x05: "SDIV",
  0x06: "MOD", 0x10: "LT", 0x11: "GT", 0x14: "EQ", 0x15: "ISZERO", 0x16: "AND",
  0x17: "OR", 0x19: "NOT", 0x1a: "BYTE", 0x20: "SHA3",
  0x33: "CALLER", 0x35: "CALLDATALOAD", 0x36: "CALLDATASIZE", 0x38: "CODESIZE",
  0x39: "CODECOPY", 0x3b: "EXTCODESIZE", 0x40: "BLOCKHASH", 0x42: "TIMESTAMP",
  0x43: "NUMBER", 0x50: "POP", 0x51: "MLOAD", 0x52: "MSTORE", 0x54: "SLOAD",
  0x55: "SSTORE", 0x56: "JUMP", 0x57: "JUMPI", 0x58: "PC", 0x5a: "GAS",
  0x5b: "JUMPDEST", 0xf0: "CREATE", 0xf1: "CALL", 0xf3: "RETURN", 0xf4: "DELEGATECALL",
  0xfa: "STATICCALL", 0xfd: "REVERT", 0xfe: "INVALID",
};
for (let i = 0x60; i <= 0x7f; i++) opcodes[i] = `PUSH${i - 0x5f}`;
for (let i = 0x80; i <= 0x8f; i++) opcodes[i] = `DUP${i - 0x7f}`;
for (let i = 0x90; i <= 0x9f; i++) opcodes[i] = `SWAP${i - 0x8f}`;

// Disassemble from offset 0x04cf
const start = 0x04cf;
const end = Math.min(start + 300, bytes.length);
let i = start;
while (i < end) {
  const op = bytes[i];
  const name = opcodes[op] || `0x${op.toString(16).padStart(2,"0")}`;
  if (op >= 0x60 && op <= 0x7f) {
    const size = op - 0x5f;
    const data = bytes.slice(i+1, i+1+size).toString("hex");
    console.log(`${i.toString(16).padStart(4,"0")}: ${name} 0x${data}`);
    i += 1 + size;
  } else {
    console.log(`${i.toString(16).padStart(4,"0")}: ${name}`);
    i++;
  }
}
