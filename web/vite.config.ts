import { defineConfig } from "vite";

export default defineConfig({
  // Emit straight into the Go embed location — single source of truth, no copy
  // step. Relative base so the SPA works at / or behind a path prefix.
  base: "./",
  build: { outDir: "../internal/webui/dist", emptyOutDir: true },
  server: {
    // `npm run dev` proxies API calls to a locally-running creo server.
    proxy: {
      "/v1": "http://127.0.0.1:8080",
      "/healthz": "http://127.0.0.1:8080",
    },
  },
  test: { environment: "jsdom", globals: true },
});
