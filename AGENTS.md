# Creo — conventions for coding agents

## What this repo is

The Creo core platform (Go) plus product/architecture docs. Read in this order:
`VISION.md` → `PRD.md` → `docs/architecture.md` → `docs/components.md`. The
component catalog is normative: code implements its interfaces and contracts
(SL-1..5, RC-1..5), and contract tests are not optional.

## Layout

```
cmd/creo/            CLI + server entry point (stdlib flag, no framework)
internal/store/      SQLite open/migrations, single-writer discipline
internal/eventlog/   SessionLog (contracts SL-1..5 + crash test)
internal/run/        RunCoordinator (contracts RC-1..5)
internal/workspace/  L0 SandboxProvider (path-confined file tools, no exec)
internal/project/    ProjectStore (CAS versions, materialize, lineage)
internal/model/      ModelGateway: anthropic + fake adapters, usage metering, budget hook
internal/harness/    AgentHarness loop + embedded websites-v0 profile
internal/tenant/     tenants: tokens, budget, run + storage quotas
internal/identity/   human login: Authenticator seam, static driver, sessions
internal/publish/    live-version pointer + preview capability secret (atomic publish/rollback)
internal/serving/    PreviewGateway read side: origin-isolated site serving on :8081, CSP
internal/profile/    ProductProfile: websites vertical as data (palette, exec level, CSP, language)
internal/webui/      embeds web/dist; serves the SPA app shell at /
internal/api/        HTTP + SSE API with bearer auth + tenant scoping (the only client surface)
internal/server/     v-min process wiring: two http.Servers (API + serving), workers, renewal, recovery
web/                 reference web client (TypeScript + Vite); builds into internal/webui/dist
internal/e2e/        acceptance: kill-9-resume, dup submit, workspace loss, hostile containment, auth, budget
spikes/              throwaway experiment code; never imported by the core
scripts/             demo and operational scripts
```

## Canonical commands

`justfile` is the task runner — `just` on its own lists every recipe. It wraps
the raw commands below rather than replacing them, so a checkout without `just`
still works.

```sh
just run                     # serve locally on :8080 (fake model, no API key)
just run anthropic:claude-sonnet-5   # real model; sources .env for the API key
just build                   # web client, THEN the binary — Go embeds dist
just test                    # fast tests (Go -short + vitest) — the inner loop
just test-full               # everything, including e2e
just check                   # format + lint + tidy, FIXING in place
just check-ci                # the same checks, verify-only — never writes
just check-go / just check-ts        # one side only (same for build-/test-)
```

Short name = fast default; `-full` = comprehensive. CI
(`.github/workflows/ci.yml`) runs `just check-ci` then `just test-full` — the
same recipes you run locally, so the two cannot drift.

`check` fixes, `check-ci` reports. The verify-only recipes never write: the
module-graph probe is `go mod tidy -diff` (Go 1.23+), which prints what tidy
would change and exits non-zero without touching `go.mod`/`go.sum` — so
`check-ci` is safe on a dirty tree and independent of git state.

```sh
go build ./cmd/creo          # build the binary
go test ./...                # full suite, including e2e (spawns the binary)
go test ./... -short         # skips subprocess tests
go vet ./... && gofmt -l .   # both must be clean before committing
```

Run locally without `just`: `./creo serve --data ./data --model fake:site` (no
API key), or `--model anthropic:claude-sonnet-5` with `ANTHROPIC_API_KEY` set
(repo-root `.env` is gitignored; `set -a; source .env` before serving).

## Conventions

- **Stdlib-first.** Three external deps (sqlite driver, anthropic SDK, ulid).
  Adding a dependency is an architecture decision, not a convenience.
- **Contracts are tests.** A change to eventlog/run semantics must keep the
  conformance tests passing; new storage backends must pass the same suite.
- **Events are the only interaction state.** No component keeps durable state
  outside the log + versions + blobs. If a feature needs more, re-read
  `docs/components.md` §"one authority per component" before writing code.
- **Plain-language userText** is authored at emit time in the harness — never
  synthesized client-side. The same bar applies to API error bodies: anything a
  person can reach is a sentence, and unexpected failures go through
  `serverError` (logs the cause, returns a sentence) rather than returning
  `err.Error()`. `internal/e2e/language_test.go` enforces this — it reads what a
  user would see on every reachable failure path and rejects jargon.
- **Session state is named by the platform**, not inferred by clients:
  `session.state.changed` events plus `GET /v1/sessions/{id}` (R-SES-5). A client
  that decides for itself what `run.completed` means is a bug.
- **Two doors, one principal.** `api.auth` resolves a bearer token, a session
  cookie, or `--insecure` into one `identity.Principal`; handlers never learn
  which. Policy branches on `Assurance`, never on the driver name.
- **Ports:** API `127.0.0.1:8080`; `:8081` reserved for served sites (M2).
- **Every `/v1` route is tenant-scoped.** New routes MUST resolve the caller's
  tenant (via the `auth` middleware) and scope their queries; a foreign or
  missing resource returns 404, never 403. New routes MUST get a cross-tenant
  isolation case in `internal/e2e/hostile_test.go`.
- **Credentials never enter workspaces.** LLM keys live in the ModelGateway;
  nothing at L0 touches external credentials. The budget check is at the
  gateway — the one point no model call bypasses.
- **Web client (`web/`)**: TypeScript, npm (Node 24), **Vite+ (`vp`)** as the one
  toolchain — build, test, lint, format, and type-check all read `vite.config.ts`.
  It is a *thin* consumer of the public `/v1` API only (`src/api.ts` is the sole
  surface). Build order: `cd web && npm run build` outputs to
  `internal/webui/dist` (committed, so `go build` works on a fresh checkout),
  then `go build ./cmd/creo`. Dev: `npm run dev` (proxies to a running server),
  or `creo serve --web-dir web/... ` to serve a disk build without re-embedding.
  Tests: `cd web && npm test` (vitest, jsdom). The Go core never imports it.

  ```sh
  cd web
  npm run build   # vp check && vp build  — type-checks, then bundles
  npm test        # vp test    (vitest + jsdom)
  npm run check   # vp check   (oxfmt + oxlint + type-aware check)
  npm run fmt     # vp fmt     (oxfmt; also formats index.html)
  ```

  Two non-obvious rules, both load-bearing:
  - **`vp build` does not type-check on its own.** The gate lives in
    `vite.config.ts` (`lint.options.typeAware + typeCheck`) and in `npm run build`
    running `vp check` first. Drop either and type errors ship silently.
  - **`defineConfig` must be imported from `vite-plus`**, not `vite` — vite's
    overload rejects the `test`/`lint` keys. Lint/format scope themselves from
    `.gitignore`; without it they walk `node_modules`.

  `vite-plus` is pinned to an exact version (it is pre-1.0). `vite` and
  `typescript` are intentionally *not* direct deps — they come via `vite-plus`;
  `vitest` (global test types) and `jsdom` (test environment) must stay.
- **Commits:** one reviewable commit per component/step; imperative subject
  prefixed with the area (`core:`, `docs:`, `api:`).

## Testing policy

- Unit/contract tests co-located with the package (`*_test.go`).
- The fake model adapter (`model.FakeScript`) makes every test deterministic
  and key-free; only `scripts/demo-m0.sh` talks to a real provider.
- Crash/durability tests re-execute the test binary as a helper process — see
  `internal/eventlog/crash_test.go` for the pattern.
