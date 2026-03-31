import { createV2Provider } from "./v2-swap.ts";
import { POOL_TYPE_KYBER_DMM } from "../types.ts";

export const kyberDMM = createV2Provider("kyber_dmm", POOL_TYPE_KYBER_DMM, new Set([
  "0x10908c875d865c66f271f5d3949848971c9595c9",
]));
