import { type PoolProvider, POOL_TYPE_UNIV3, POOL_TYPE_ALGEBRA, POOL_TYPE_LFJ_V1, POOL_TYPE_V2 } from "../types.ts";
import { createV3Provider } from "./v3-swap.ts";
import { createV2Provider } from "./v2-swap.ts";
import { lfjV2 } from "./lfj-v2.ts";
import { dodo } from "./dodo.ts";
import { woofiV2, woofiPP } from "./woofi.ts";
import { balancerV2 } from "./balancer-v2.ts";
import { balancerV3 } from "./balancer-v3.ts";
import { pharaohV1 } from "./pharaoh-v1.ts";
import { uniswapV4 } from "./uniswap-v4.ts";
import { wombat, platypus } from "./wombat-platypus.ts";
import { cavalre } from "./cavalre.ts";
import { kyberDMM } from "./kyber-dmm.ts";
import { synapse } from "./synapse.ts";
import { trident } from "./trident.ts";

// --- V3-style providers ---

export const uniswapV3 = createV3Provider("uniswap_v3", POOL_TYPE_UNIV3, new Set([
  "0x740b1c1de25031c31ff4fc9a62f554a55cdc1bad",
  "0x1128f23d0bc0a8396e9fbc3c0c68f5ea228b8256",
  "0x3e603c14af37ebdad31709c4f848fc6ad5bec715",
]));

export const pharaohV3 = createV3Provider("pharaoh_v3", POOL_TYPE_UNIV3, new Set([
  "0xae6e5c62328ade73ceefd42228528b70c8157d0d",
  "0xaaa32926fce6be95ea2c51cb4fcb60836d320c42",
]));

export const algebra = createV3Provider("algebra", POOL_TYPE_ALGEBRA, new Set([
  "0x512eb749541b7cf294be882d636218c84a5e9e5f",
]));

// --- V2-style providers (factory() + token0() + token1() verification) ---

export const lfjV1 = createV2Provider("lfj_v1", POOL_TYPE_LFJ_V1, new Set([
  "0x9ad6c38be94206ca50bb0d90783181662f0cfa10",
]));

export const pangolinV2 = createV2Provider("pangolin_v2", POOL_TYPE_V2, new Set([
  "0xefa94de7a4656d787667c749f7e1223d71e9fd88",
]));

export const arenaV2 = createV2Provider("arena_v2", POOL_TYPE_V2, new Set([
  "0xf16784dcaf838a3e16bef7711a62d12413c39bd1",
]));

export const sushiswapV2 = createV2Provider("sushiswap_v2", POOL_TYPE_V2, new Set([
  "0xc35dadb65012ec5796536bd9864ed8773abc74c4",
]));

export const canary = createV2Provider("canary", POOL_TYPE_V2, new Set([
  "0xcfba329d49c24b70f3a8b9cc0853493d4645436b",
]));

export const complus = createV2Provider("complus", POOL_TYPE_V2, new Set([
  "0x5c02e78a3969d0e64aa2cfa765acc1d671914ac0",
]));

export const lydia = createV2Provider("lydia", POOL_TYPE_V2, new Set([
  "0xe0c1bb6df4851feeedc3e14bd509feaf428f7655",
]));

export const hurricane = createV2Provider("hurricane", POOL_TYPE_V2, new Set([
  "0x8e6f4af0b6c26d16febdd6f28fa7c694bd49c6bf",
]));

export const fraxswap = createV2Provider("fraxswap", POOL_TYPE_V2, new Set([
  "0xf77ca9b635898980fb219b4f4605c50e4ba58aff",
]));

export const swapsicle = createV2Provider("swapsicle", POOL_TYPE_V2, new Set([
  "0x9c60c867ce07a3c403e2598388673c10259ec768",
]));

export const uniswapV2 = createV2Provider("uniswap_v2", POOL_TYPE_V2, new Set([
  "0x9e5a52f57b3038f1b8eee45f28b3c1967e22799c",
]));

export const thorus = createV2Provider("thorus", POOL_TYPE_V2, new Set([
  "0xa98ea6356a316b44bf710d5f9b6b4ea0081409ef",
]));

export const radioshack = createV2Provider("radioshack", POOL_TYPE_V2, new Set([
  "0xa0fbfda09b8815dd42ddc70e4f9fe794257cd9b6",
]));

export const vapordex = createV2Provider("vapordex", POOL_TYPE_V2, new Set([
  "0xc009a670e2b02e21e7e75ae98e254f467f7ae257",
]));

export const elkdex = createV2Provider("elkdex", POOL_TYPE_V2, new Set([
  "0x091d35d7f63487909c863001ddca481c6de47091",
]));

export const yetiswap = createV2Provider("yetiswap", POOL_TYPE_V2, new Set([
  "0x58c8cd291fa36130119e6deb9e520fbb6aca1c3a",
]));

export const partyswap = createV2Provider("partyswap", POOL_TYPE_V2, new Set([
  "0x58a08bc28f3e8dab8fb2773d8f243bc740398b09",
]));

export const oliveswap = createV2Provider("oliveswap", POOL_TYPE_V2, new Set([
  "0x4fe4d8b01a56706bc6cad26e8c59d0c7169976b3",
]));

export const zeroex = createV2Provider("zeroex", POOL_TYPE_V2, new Set([
  "0x2ef422f30cdb7c5f1f7267ab5cf567a88974b308",
]));

export const hakuswap = createV2Provider("hakuswap", POOL_TYPE_V2, new Set([
  "0x2db46feb38c57a6621bca4d97820e1fc1de40f41",
]));

// --- All providers ---

export const providers: PoolProvider[] = [
  // V3-style
  uniswapV3,
  pharaohV3,
  algebra,
  // V2-style (LFJ V1 uses same swap event as V2)
  lfjV1,
  pangolinV2,
  arenaV2,
  sushiswapV2,
  canary,
  complus,
  lydia,
  hurricane,
  fraxswap,
  swapsicle,
  uniswapV2,
  thorus,
  radioshack,
  vapordex,
  elkdex,
  yetiswap,
  partyswap,
  oliveswap,
  zeroex,
  hakuswap,
  // LFJ V2
  lfjV2,
  // Special protocols
  dodo,
  woofiV2,
  woofiPP,
  balancerV2,
  balancerV3,
  pharaohV1,
  uniswapV4,
  // Wombat / Platypus stableswap
  wombat,
  platypus,
  // Cavalre multiswap
  cavalre,
  // KyberSwap DMM
  kyberDMM,
  // Synapse stableswap
  synapse,
  // BentoBox/Trident
  trident,
];

export { seedV4Pools, processV4InitLog } from "./uniswap-v4.ts";
