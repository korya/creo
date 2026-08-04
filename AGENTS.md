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
internal/model/      ModelGateway: anthropic + fake adapters, usage metering
internal/harness/    AgentHarness loop + embedded websites-v0 profile
internal/api/        HTTP + SSE API (the only client surface)
internal/server/     v-min process wiring: workers, renewal, recovery
internal/e2e/        M0 acceptance: kill-9-resume, dup submit, workspace loss
spikes/              throwaway experiment code; never imported by the core
scripts/             demo and operational scripts
```

## Canonical commands

```sh
go build ./cmd/creo          # build the binary
go test ./...                # full suite, including e2e (spawns the binary)
go test ./... -short         # skips subprocess tests
go vet ./... && gofmt -l .   # both must be clean before committing
```

Run locally: `./creo serve --data ./data --model fake:site` (no API key), or
`--model anthropic:claude-sonnet-5` with `ANTHROPIC_API_KEY` set (repo-root
`.env` is gitignored; `set -a; source .env` before serving).

## Conventions

- **Stdlib-first.** Three external deps (sqlite driver, anthropic SDK, ulid).
  Adding a dependency is an architecture decision, not a convenience.
- **Contracts are tests.** A change to eventlog/run semantics must keep the
  conformance tests passing; new storage backends must pass the same suite.
- **Events are the only interaction state.** No component keeps durable state
  outside the log + versions + blobs. If a feature needs more, re-read
  `docs/components.md` §"one authority per component" before writing code.
- **Plain-language userText** is authored at emit time in the harness — never
  synthesized client-side.
- **Ports:** API `127.0.0.1:8080`; `:8081` reserved for served sites (M2).
- **TypeScript** (npm) enters at M3 for the web client and vertical tooling;
  the Go core does not import it.
- **Commits:** one reviewable commit per component/step; imperative subject
  prefixed with the area (`core:`, `docs:`, `api:`).

## Testing policy

- Unit/contract tests co-located with the package (`*_test.go`).
- The fake model adapter (`model.FakeScript`) makes every test deterministic
  and key-free; only `scripts/demo-m0.sh` talks to a real provider.
- Crash/durability tests re-execute the test binary as a helper process — see
  `internal/eventlog/crash_test.go` for the pattern.
