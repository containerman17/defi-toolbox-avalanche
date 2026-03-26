export interface QuoterOptions {
    /** WebSocket URL of the state server (e.g. "ws://localhost:7449/live") */
    stateServerUrl: string;
}
export interface FindRouteResult {
    steps: {
        pool: string;
        poolType: number;
        tokenIn: string;
        tokenOut: string;
    }[];
    amountOut: bigint;
    stats: {
        formulaQuotes: number;
        evmQuotes: number;
        totalQuotes: number;
        formulaMs: number;
        evmMs: number;
    };
}
export interface Quoter {
    /** Find the best route from tokenIn to tokenOut */
    findRoute(tokenIn: string, tokenOut: string, amountIn: bigint): Promise<FindRouteResult | null>;
    /** Send a raw JSON-RPC request to the native harness */
    request(method: string, params: any): Promise<any>;
    /** Close the harness process */
    close(): void;
}
/**
 * Create a quoter backed by the native Go harness.
 *
 * The harness is a child process that communicates via stdin/stdout JSON-RPC.
 * It connects to a state server for live chain state and runs EVM + formula
 * quoting locally.
 *
 * @example
 * ```ts
 * const quoter = await createQuoter({ stateServerUrl: "ws://localhost:7449/live" });
 * const result = await quoter.findRoute(WAVAX, USDC, 10n ** 18n);
 * if (result) {
 *   console.log(`${result.amountOut} out via ${result.steps.length} hops`);
 * }
 * quoter.close();
 * ```
 */
export declare function createQuoter(opts: QuoterOptions): Promise<Quoter>;
//# sourceMappingURL=quoter.d.ts.map