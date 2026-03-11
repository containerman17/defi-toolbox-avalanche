// Check what WooPP oracle returns in simulation vs at block
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const ORACLE = "0xd92e3c8f60a91fa6dfc59a43f7e1f7e43ee56be4";
const WOOPOOL = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Try calling queryPrice on oracle
// state(address base) returns (uint128 price, uint128 spread, uint64 coeff, uint64 woFeasible)
// selector: 0xb5e4b813 for some function, 0x893ddf87 for another

// Try to call the oracle to get WAVAX price
// Oracle functions: state(address) = ?
const state_call = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: "0x5ec1b05a000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" }, blockHex] as any
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("state(WAVAX):", state_call);

// Try getPrice
const getprice = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: "0x41976e09000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" }, blockHex] as any
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("getPrice(WAVAX):", getprice);

// Try c1701b67 function (the one WooPP calls)
const oracle_result = await client.request({
  method: "eth_call" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" },
    blockHex
  ] as any
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("0xc1701b67(WAVAX):", oracle_result);

// Try b5e4b813
const b5 = await client.request({
  method: "eth_call" as any,
  params: [
    { to: "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7", data: "0xb5e4b813000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" },
    blockHex
  ] as any
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("b5e4b813(WAVAX) on 0x5ecf662a:", b5);

// Check WooPP pool's tryQuery function
// tryQuery(fromToken, toToken, fromAmount) -> toAmount
// selector: 0x7dc2038a?
const try_query = await client.request({
  method: "eth_call" as any,
  params: [
    { to: WOOPOOL, data: "0x7dc2038a" + 
      "000000000000000000000000b97ef9ef8734c71904d8002f8b6bc66dd9c48a6e" + // fromToken = USDC
      "000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" + // toToken = WAVAX
      "000000000000000000000000000000000000000000000000000000012742c380"  // amount = 4950000000
    },
    blockHex
  ] as any
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("tryQuery(USDC, WAVAX, 4950000000):", try_query);

// Try "query" function
const query_q = await client.request({
  method: "eth_call" as any,
  params: [
    { to: WOOPOOL, data: "0x2c3b8964" + 
      "000000000000000000000000b97ef9ef8734c71904d8002f8b6bc66dd9c48a6e" + 
      "000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" + 
      "000000000000000000000000000000000000000000000000000000012742c380"
    },
    blockHex
  ] as any
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("query (0x2c3b8964) USDC->WAVAX:", query_q);
