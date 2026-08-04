// Vite+ (`vp`) is the single frontend toolchain: dev, build, test, lint, fmt,
// and type-check all read this one config. `defineConfig` must come from
// vite-plus — vite's own overload doesn't know the `test`/`lint` keys.
import { defineConfig } from "vite-plus";

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
  // `vp build` does NOT type-check on its own — without this, a type error
  // ships silently. typeAware turns on the tsgolint-backed rules; typeCheck
  // promotes compiler diagnostics (TS2322 &c.) to hard errors in `vp check`,
  // which `npm run build` runs first. Removing either re-opens that hole.
  lint: { options: { typeAware: true, typeCheck: true } },
});
