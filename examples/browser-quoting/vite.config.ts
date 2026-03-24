import { defineConfig } from "vite";
import { resolve } from "path";

const repoRoot = resolve(__dirname, "../..");

export default defineConfig({
  resolve: {
    // Let Vite resolve .ts imports from the repo
    extensions: [".ts", ".js", ".mjs"],
  },
  server: {
    fs: {
      // Allow serving files from the repo root (pools.txt, bytecode.hex, wasm)
      allow: [repoRoot],
    },
    allowedHosts: true,
  },
});
