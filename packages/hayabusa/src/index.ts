export { createQuoter, type Quoter, type QuoterOptions, type FindRouteResult, type BlockInfo } from "./quoter.js";
export { loadPools, parsePools } from "./pools.js";
export { buildStateOverrides, getBalanceOverride } from "./overrides.js";
export { POOL_TYPE, type StoredPool, type RouteStep, type RouteResult, type PoolType } from "./types.js";

import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));

/** Deployed HayabusaRouter address on Avalanche C-Chain */
const addressJson = JSON.parse(
  readFileSync(join(__dirname, "..", "data", "address.json"), "utf-8"),
);
export const ROUTER_ADDRESS: string = addressJson.address;
export const DEPLOYMENT_BLOCK: number = addressJson.block;

/** Common token addresses on Avalanche C-Chain */
export const TOKENS = {
  WAVAX: "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7",
  USDC: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e",
  USDT: "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7",
  "WETH.e": "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab",
  "USDT.e": "0xc7198437980c041c805a1edcba50c1ce5db95118",
  "USDC.e": "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664",
  "WBTC.e": "0x50b7545627a5162f82a992c33b87adc75187b218",
  "DAI.e": "0xd586e7f844cea2f87f50152665bcbc2c279d8d70",
} as const;
