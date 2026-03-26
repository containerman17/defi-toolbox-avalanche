import { spawn } from "node:child_process";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
const __dirname = dirname(fileURLToPath(import.meta.url));
const BIN_DIR = join(__dirname, "..", "bin");
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
export async function createQuoter(opts) {
    const args = ["--state-server", opts.stateServerUrl];
    const child = spawn(join(BIN_DIR, "harness-native"), args, {
        stdio: ["pipe", "pipe", "pipe"],
    });
    let nextId = 1;
    const pending = new Map();
    let buffer = "";
    child.stdout.on("data", (chunk) => {
        buffer += chunk.toString();
        let nl;
        while ((nl = buffer.indexOf("\n")) !== -1) {
            const line = buffer.slice(0, nl).trim();
            buffer = buffer.slice(nl + 1);
            if (!line)
                continue;
            try {
                const msg = JSON.parse(line);
                const p = pending.get(msg.id);
                if (p) {
                    pending.delete(msg.id);
                    if (msg.error)
                        p.reject(new Error(typeof msg.error === "string" ? msg.error : JSON.stringify(msg.error)));
                    else
                        p.resolve(msg.result);
                }
            }
            catch { }
        }
    });
    // Wait for the harness to connect to the state server
    await new Promise((resolve, reject) => {
        const timeout = setTimeout(() => reject(new Error("harness startup timeout (30s)")), 30_000);
        child.on("error", (err) => { clearTimeout(timeout); reject(err); });
        child.on("exit", (code) => {
            if (code !== null && code !== 0) {
                clearTimeout(timeout);
                reject(new Error(`harness exited with code ${code}`));
            }
        });
        // The harness prints "[native] connected to state server ..." to stderr when ready
        let stderrBuf = "";
        const onData = (chunk) => {
            stderrBuf += chunk.toString();
            process.stderr.write(chunk); // pass through to caller's stderr
            if (stderrBuf.includes("connected to state server")) {
                child.stderr.off("data", onData);
                clearTimeout(timeout);
                resolve();
            }
        };
        child.stderr.on("data", onData);
    });
    function request(method, params) {
        return new Promise((resolve, reject) => {
            const id = nextId++;
            pending.set(id, { resolve, reject });
            child.stdin.write(JSON.stringify({ id, method, params }) + "\n");
        });
    }
    async function findRoute(tokenIn, tokenOut, amountIn) {
        const result = await request("find_route", {
            tokenIn: tokenIn.toLowerCase(),
            tokenOut: tokenOut.toLowerCase(),
            amountIn: "0x" + amountIn.toString(16),
            maxHops: 2,
        });
        if (!result || result.route === null || !result.steps) {
            return null;
        }
        const native = result;
        return {
            steps: native.steps,
            amountOut: BigInt(native.amountOut),
            stats: native.stats,
        };
    }
    function close() {
        child.stdin.end();
        child.kill();
    }
    return { findRoute, request, close };
}
//# sourceMappingURL=quoter.js.map