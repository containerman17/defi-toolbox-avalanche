# Live Spread Table

Continuously quotes 10 tokens against USDC ($100 buy + sell-back), showing real-time spreads. All pathfinding runs locally via **WebAssembly EVM** — no RPC calls, no backend.

## Browser version

Open `index.html` (or via GitHub Pages). Enter a state server URL and click Connect. The WASM quoter (~19MB) loads, syncs chain state over WebSocket, then quotes continuously.

## Node.js server version

Same WASM quoter, same tokens, runs in the terminal. Useful for testing without a browser or when the browser demo takes too long to cold-start.

```bash
# Requires: state server running on localhost:7449
node server.mjs

# Custom state server URL:
node server.mjs ws://your-server:7449/live

# Fewer pools for faster cold start:
node server.mjs ws://localhost:7449/live 200

# Full pool set:
node server.mjs ws://localhost:7449/live 2000
```

Builds WASM from source on first run (`make build-wasm`), then connects and prints a spread table in a loop. Fewer pools = faster initial sync but may miss some routes.

Both versions pass `split=true` to include split routing results in the response.
