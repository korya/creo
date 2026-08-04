# M3 Blueprint — websites vertical + reference web client

**Status:** validated plan, awaiting approval · 2026-08-04
**Headline:** Turn the embedded stand-in profile into a first-class ProductProfile (execution level enforced, artifact policy + site language as data) and ship a minimal embedded web client so a non-coder completes describe → preview → refine → publish in a browser — the R-WEB-3 loop, end to end.

## Problem statement

- **Goal (PRD §9 M3):** vertical profile + reference web client; the describe→preview→refine→publish loop works for a friendly non-coder with a human on call.
- **Constraints:** the client is a *thin* consumer of the existing API (P2 — no private endpoints); it is embedded in the Go binary (single-artifact self-host); the profile realizes `components.md` §10 (ProductProfile) and the spike-01 findings (siteLanguage explicit, execution-by-capability, static-only enforced by policy); TypeScript enters here per AGENTS.
- **Non-goals:** click-to-edit / node annotations (R-WEB-6 SHOULD, deferred), multi-vertical registry (one profile shipped), a framework SPA (a minimal vanilla-TS reference client suffices and is what "reference" means), real per-user auth for preview (T1 capability URL stands), asset upload UI (R-WEB-4 — API exists via blobs later; not M3).
- **Success criteria:** (S1) `ProductProfile` is data: system prompt, tool palette, `executionLevel`, `artifactPolicy` (CSP), `siteLanguage`; the platform refuses a palette exceeding the execution level. (S2) the served CSP comes from the profile's policy, not a hardcoded constant. (S3) site language is an explicit profile field surfaced to the model, never inferred (spike-01). (S4) the web client is served at `/` from the binary and drives only the public API. (S5) a browserless test proves the client's API layer performs the full loop (create → say → stream events → preview → publish). (S6) the existing suite stays green; the client bundle builds reproducibly.

## Hypothesis (and its falsifier)

Promote `harness.Profile` → `profile.Profile` (a package) carrying the new fields; a `profile.Websites()` constructor; enforcement (`ValidatePalette`) called at run start; `artifactPolicy.CSP` threaded from profile → serving (replacing the `serving.StaticCSP` constant). The client is a Vite-built vanilla-TS app in `web/`, output embedded via `go:embed` and served by the API at `/`; its API-calling logic is a testable module with vitest (jsdom) unit tests; a Go e2e drives the identical calls.

**Falsifier:** if the client needs anything the API doesn't already expose (a private endpoint, server-side rendering, a websocket), the "thin embedded consumer" shape is wrong and the API is under-built. Checked (A3): create/say/events(SSE)/preview/publish cover the whole loop — all exist since M2.

## Ordered steps

1. **`internal/profile` package** — move `Profile` out of harness. Fields: `ID`, `Version`, `System`, `Tools []model.ToolDef`, `MaxIterations`, `ExecutionLevel` (`"L0"|"L1"|"L2"`), `CSP string`, `SiteLanguage string`. `Websites()` constructor (ports the current DefaultProfile + a `SiteLanguage: "English"` default and the L0 level). `ValidatePalette() error` — refuses tools requiring exec (`bash`,`exec`,`run_*`) at L0/L1. Unit tests: palette validation, L2-tool-at-L0 rejected.
2. **Harness uses the profile** — `harness.DefaultProfile()` delegates to `profile.Websites()`; the system prompt gains an explicit "Write all site text in {{SiteLanguage}}" line (S3); run start calls `ValidatePalette` and fails the run with a plain-language event if violated. Existing harness tests updated for the import move.
3. **CSP from policy** — `serving.New` takes the CSP string; `server` passes `profile.Websites().CSP`; `serving.StaticCSP` becomes the profile's default value. Serving test asserts the header equals the profile CSP.
4. **Web client (`web/`)** — Vite + vanilla TS. `src/api.ts`: typed client (`createProject`, `sendMessage`, `streamEvents` via `EventSource`, `preview`, `publish`, `rollback`) — the sole API surface. `src/app.ts`: chat pane (message list from SSE `userText`), a text input, a live preview `<iframe>` refreshed on `preview.ready`/`publish.completed`, and Publish/Rollback buttons. `index.html` + minimal CSS. Vitest unit tests for `api.ts` against a mocked `fetch`/`EventSource`. Build → `web/dist`.
5. **Embed + serve** — `internal/webui` with `//go:embed dist` (built assets committed or built in CI; for dev, a `--web-dir` override serves from disk). API serves the SPA at `/` (and static assets), falling back to `index.html` for client routes; `/v1/*` and `/healthz` unaffected. In `--insecure` dev mode the client talks to the API tokenless; with auth, a token is entered in the UI and sent as a bearer header.
6. **CLI/build** — `web/package.json` scripts (`build`, `test`, `dev`); a top-level `make web` (or `scripts/build-web.sh`) that runs `npm ci && npm run build`; `go generate` hook optional. Document the two-step build (web then go) in AGENTS.
7. **e2e (`internal/e2e/webui_test.go`)** — assert `GET /` returns the SPA HTML referencing the bundle; assert the bundle asset is served; then run the full no-code loop through the API (the same calls `api.ts` makes) asserting a published live URL serves the built site. (Browser-driving is out of scope without a headless browser; the vitest suite covers client-side logic.)
8. **Docs** — README: browser quickstart (`serve` → open `http://127.0.0.1:8080`); AGENTS: `web/` conventions (npm, vite, vitest, build order); PRD M3 delivered; components.md ProductProfile status.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | `harness.Profile` is small and only used by harness+server | Confirmed | `internal/harness/profile.go` defines it; `grep` shows use in harness.go + server.go only |
| A2 | `serving` CSP is a single constant, trivially parameterized | Confirmed | `serving.StaticCSP` const used in one place (`serveFile`) |
| A3 | The full no-code loop needs no new API — create/say/events/preview/publish exist | Confirmed | `api.go` routes at HEAD `2efc584` (M2) |
| A4 | Node/npm present for the client build | Confirmed | `node v24.19.0`, `npm 11.17.0` |
| A5 | Go `//go:embed` serves an SPA + assets via `http.FileServer` | Confirmed | stdlib `embed` + `http.FileServerFS` (Go 1.22+); the binary already embeds SQL migrations |
| A6 | SSE from the API works cross-origin-free in a same-origin SPA (client served by the API) | Confirmed | client is same-origin (served at `/` by :8080); `EventSource` to `/v1/sessions/{id}/events` is same-origin |
| A7 | No in-flight work / drift | Confirmed | clean tree at `2efc584` |

## Spec cross-validation

- **R-WEB-1/3** (blank → published site through conversation; describe→preview→refine→publish) → steps 4,7. **R-WEB-2** (static bundle, no server code — enforced by policy) → step 1 `ExecutionLevel L0` + `ValidatePalette` (S1). **ProductProfile component** (`components.md` §10: prompts/tools/policy as data, platform refuses palette beyond level) → steps 1,2. **P2 headless** (client uses only public API) → step 4 `api.ts` is the sole surface; step 7 drives the same calls. **spike-01 findings** (siteLanguage explicit; static-only by policy not prompt) → steps 2,3. Trust tier unchanged (T1). **Deferred, flagged:** click-to-edit (R-WEB-6), asset upload (R-WEB-4).

## Project conventions to follow

- **[`AGENTS.md` "TypeScript enters at M3 for the web client"]** → this is that step; `web/` uses npm (Node 24), Vite, vitest — recorded in step 8.
- **[`AGENTS.md` "the only client surface is the API"]** → step 4 client calls only documented `/v1` routes; step 7 asserts no private endpoint.
- **[`AGENTS.md` "Every /v1 route tenant-scoped"]** → no new routes; serving `/` is unauthenticated static assets (the app shell), which carry no tenant data — data arrives via authenticated `/v1` calls the client makes.
- **[`AGENTS.md` "one authority per component"]** → ProductProfile becomes its own package (step 1); webui serving is its own package (step 5).
- **[`AGENTS.md` "Stdlib-first" (Go)]** → no new Go deps; the client's deps live in `web/` and don't affect the Go module.
- **[`AGENTS.md` migrations / ports]** → no schema change; client served on the existing API port :8080, sites on :8081 (unchanged).
- **Package manager (web):** npm + `package-lock.json` (Node default; matches the spike's `web` usage). Framework: **none** (vanilla TS) — a reference client, smallest embeddable bundle, testable without a browser runtime.

## Codebase conflict sweep

In-flight: none (clean tree). Shadow duplication: the harness `DefaultProfile` is the only profile source — step 1 consolidates, not branches. Caller-side drift: moving `Profile` out of harness touches harness.go + server.go imports (same steps); `serving.New` signature gains the CSP arg (server updated in step 3). Test infra: Go e2e reused; vitest is new tooling scoped to `web/` (its own config, no effect on `go test`).

## Counterfactual — **Confirmed.** S1→1/2, S2→3, S3→2, S4→5/7, S5→4/7, S6→build order (step 6) + existing suite.

## Risks & mitigations

- **Committed `web/dist` drifts from source** → CI/`make` rebuilds; a `//go:build ignore` dev path (`--web-dir web/dist`) serves fresh builds without re-embedding; e2e asserts the served HTML references the current bundle.
- **Browserless client testing leaves a gap** → mitigated by (a) vitest unit tests of `api.ts` (the logic that matters), (b) the Go e2e proving the exact API loop, (c) the served-HTML smoke test. A full Playwright pass is a follow-up when a headless browser is wired in; noted, not hidden.
- **Embed requires a build artifact to exist** → provide a tiny placeholder `web/dist/index.html` committed so `go build` never fails on a fresh checkout; real build overwrites it.

## Out of scope

Click-to-edit / node annotations, asset upload UI, multi-vertical registry, framework SPA, real preview auth, i18n of the client UI itself, mobile-specific client, offline.

## Open questions

None blocking — vanilla-TS reference client and the browserless test strategy are defensible v-min calls, documented above. Proceeding.

## Test plan

Go unit: profile palette validation (L0 refuses exec tools), CSP-from-profile in serving. Vitest: `api.ts` create/say/stream/preview/publish against mocked fetch+EventSource. Go e2e: SPA served at `/`, bundle asset served, full describe→publish loop via the API, published site serves. Manual: `creo serve --insecure`, open `http://127.0.0.1:8080`, build and publish a site in the browser.
