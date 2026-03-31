import { type Log } from "viem";

// Pool type constants — must match Solidity constants in Hayabusa.sol
export const POOL_TYPE_UNIV3 = 0 as const;
export const POOL_TYPE_ALGEBRA = 1 as const;
export const POOL_TYPE_LFJ_V1 = 2 as const;
export const POOL_TYPE_LFJ_V2 = 3 as const;
export const POOL_TYPE_DODO = 4 as const;
export const POOL_TYPE_WOOFI = 5 as const;
export const POOL_TYPE_BALANCER_V3 = 6 as const;
export const POOL_TYPE_PHARAOH_V1 = 7 as const;
export const POOL_TYPE_V2 = 8 as const;
export const POOL_TYPE_UNIV4 = 9 as const;
export const POOL_TYPE_ERC4626 = 10 as const;
export const POOL_TYPE_BALANCER_V3_BUFFERED = 11 as const;
export const POOL_TYPE_WOMBAT = 12 as const;
export const POOL_TYPE_PLATYPUS = 13 as const;
export const POOL_TYPE_WOOPP_V2 = 14 as const;
export const POOL_TYPE_TRANSFER_FROM = 15 as const;
export const POOL_TYPE_BALANCER_V2 = 16 as const;
export const POOL_TYPE_CAVALRE = 17 as const;
export const POOL_TYPE_KYBER_DMM = 18 as const;
export const POOL_TYPE_SYNAPSE = 19 as const;
export const POOL_TYPE_TRIDENT = 20 as const;

export type PoolType =
  | typeof POOL_TYPE_UNIV3
  | typeof POOL_TYPE_ALGEBRA
  | typeof POOL_TYPE_LFJ_V1
  | typeof POOL_TYPE_LFJ_V2
  | typeof POOL_TYPE_DODO
  | typeof POOL_TYPE_WOOFI
  | typeof POOL_TYPE_BALANCER_V3
  | typeof POOL_TYPE_PHARAOH_V1
  | typeof POOL_TYPE_V2
  | typeof POOL_TYPE_UNIV4
  | typeof POOL_TYPE_ERC4626
  | typeof POOL_TYPE_BALANCER_V3_BUFFERED
  | typeof POOL_TYPE_WOMBAT
  | typeof POOL_TYPE_PLATYPUS
  | typeof POOL_TYPE_WOOPP_V2
  | typeof POOL_TYPE_TRANSFER_FROM
  | typeof POOL_TYPE_BALANCER_V2
  | typeof POOL_TYPE_CAVALRE
  | typeof POOL_TYPE_KYBER_DMM
  | typeof POOL_TYPE_SYNAPSE
  | typeof POOL_TYPE_TRIDENT;

export const POOL_TYPES = {
  UNIV3: POOL_TYPE_UNIV3,
  ALGEBRA: POOL_TYPE_ALGEBRA,
  LFJ_V1: POOL_TYPE_LFJ_V1,
  LFJ_V2: POOL_TYPE_LFJ_V2,
  DODO: POOL_TYPE_DODO,
  WOOFI: POOL_TYPE_WOOFI,
  BALANCER_V3: POOL_TYPE_BALANCER_V3,
  PHARAOH_V1: POOL_TYPE_PHARAOH_V1,
  V2: POOL_TYPE_V2,
  UNIV4: POOL_TYPE_UNIV4,
  ERC4626: POOL_TYPE_ERC4626,
  BALANCER_V3_BUFFERED: POOL_TYPE_BALANCER_V3_BUFFERED,
  WOMBAT: POOL_TYPE_WOMBAT,
  PLATYPUS: POOL_TYPE_PLATYPUS,
  WOOPP_V2: POOL_TYPE_WOOPP_V2,
  TRANSFER_FROM: POOL_TYPE_TRANSFER_FROM,
  BALANCER_V2: POOL_TYPE_BALANCER_V2,
  CAVALRE: POOL_TYPE_CAVALRE,
  KYBER_DMM: POOL_TYPE_KYBER_DMM,
  SYNAPSE: POOL_TYPE_SYNAPSE,
  TRIDENT: POOL_TYPE_TRIDENT,
} as const;

export interface CachedRPC {
  getAddress(address: string, method: string): Promise<string>;
  ethCall(to: string, method: string): Promise<string>;
}

export interface PoolProvider {
  name: string;
  poolType: PoolType;
  topics: string[];
  processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]>;
}

export interface SwapEvent {
  pool: string;
  tokenIn: string;
  tokenOut: string;
  amountIn: bigint;
  amountOut: bigint;
  blockNumber: number;
  poolType: PoolType;
  providerName: string;
  extraData?: string;
}

export interface StoredPool {
  address: string;
  providerName: string;
  poolType: PoolType;
  tokens: string[];
  latestSwapBlock: number;
  extraData?: string;
}
