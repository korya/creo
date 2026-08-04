# M0 Blueprint — the spine, in Go

**Status:** validated plan, awaiting approval · 2026-08-03
**Headline:** Implement the M0 spine as a single Go binary (SQLite storage, in-process workers, HTTP+SSE API, CLI client), building each component against the testable contracts in `docs/components.md`, with the kill-9-and-resume demo as the acceptance gate. TypeScript enters at M3 (web client); M0 is pure Go.

## Approach summary

- One Go module, one binary (`creo`), stdlib-first: `net/http` (Go 1.22+ mux), `flag` subcommands, `embed` for migrations. Three external deps only: `modernc.org/sqlite` (pure Go, CGO-free — **probe-validated on darwin/arm64**), `anthropic-sdk-go` (pinned), a ULID package.
- Components land in contract order — SessionLog → RunCoordinator → Workspace/ProjectStore → ModelGateway → Harness → API → CLI — each with its conformance tests (SL-1..5, RC-1..5) before the next starts, so the spine is proven bottom-up.
- A **fake model adapter** (scripted tool calls, controllable pacing) makes every test deterministic and API-key-free; the real Anthropic adapter is exercised by the manual demo script.
- Writes go through a single-writer connection discipline (probe showed 0 errors under concurrency, but we don't rely on luck); the event log runs `synchronous(FULL)` — no relaxed durability for the source of truth (SL-5).

## Ordered steps

1. **Foundation** — `go.mod` (module path: open question 1); `internal/store/`: SQLite open with `WAL`, `busy_timeout(5000)`, `synchronous(FULL)`, embedded migrations `internal/store/migrations/0001_init.sql` (tables: `projects, sessions, events, runs, idempotency_keys, versions, version_files, usage`); single-writer helper `store.Write(fn)`.
2. **SessionLog** — `internal/eventlog/`: event envelope per `architecture.md` §3.1 (ULID id, per-session gapless `seq`, type taxonomy, `userText`/`detail` payload, causation/run refs); `Append(events, lease)` with in-transaction generation check; `Read(after, filter)`; `Subscribe` via in-process broker fed post-commit. **Tests: SL-1..4 in-process; SL-5 as a crash test** (helper subprocess appends in a loop, parent SIGKILLs it, verifies every acknowledged append survived).
3. **RunCoordinator** — `internal/run/`: `RequestRun` (idempotency upsert), `Claim` (single-writer-per-project inside `BEGIN IMMEDIATE`), leases `{runID, workerID, gen, expiresAt}` with strictly increasing `gen`, `Renew`, `Complete`, `RecoverOrphans` (boot + ticker). **Tests: RC-1..5**, including takeover-after-expiry and stale-holder-cannot-commit (composes with SL-3).
4. **Workspace + ProjectStore** — `internal/workspace/local.go` (L0: List/Read/Write/Delete, path confinement via canonicalize-then-verify); `internal/project/`: `Commit` (SHA-256 per file into `data/cas/`, version row + manifest referencing the producing event), `Materialize`, `ListVersions`. **Tests:** byte-exact round-trip, confinement (traversal attempts fail), content-addressing (same content ⇒ same id).
5. **ModelGateway** — `internal/model/`: `Complete(req)` interface + capability struct; `anthropic.go` (pinned SDK, manual tool-loop shapes: `Messages.New`, `ToolParam`/`ToolUnionParam`, `resp.ToParam()`, `StopReasonToolUse`); `fake.go` scripted adapter (canned tool-call sequences, pause hooks for kill-tests); usage recorded per run even on failure.
6. **AgentHarness** — `internal/harness/`: the spike loop productionized — context reconstruction from the log, embedded default websites-ish profile (system prompt + file-tool palette), tool dispatch onto Workspace, events emitted via `Append(…, lease)`, `ProjectStore.Commit` on success, iteration cap, plain-language final text. **Tests:** full runs against the fake adapter, including mid-run lease loss (harness's late append rejected, no partial state visible).
7. **API + server assembly** — `internal/api/`: `POST /v1/projects`, `POST /v1/sessions/{id}/messages` (requires `Idempotency-Key`), `GET /v1/sessions/{id}/events?after=N` (SSE backfill + live tail), `GET /healthz`; `internal/server/`: wiring, worker pool (2), recovery scan on boot. Localhost bind by default (identity is an M1 concern).
8. **CLI** — `cmd/creo/`: `serve`, `project new`, `say`, `watch` — the client subcommands speak **only HTTP** to the server (headlessness proven in M0, not promised).
9. **M0 acceptance tests** — `internal/e2e/`: (a) duplicate submit ⇒ exactly one run (AC-3); (b) **kill-resume**: spawn `creo serve` with the fake slow model, SIGKILL mid-run, restart, assert the run completes and the log has no gaps (AC-1); (c) workspace loss: delete the workspace dir, next run materializes from the last version (AC-2). Plus `scripts/demo-m0.sh` running the same choreography against the real Anthropic adapter.
10. **Conventions + docs** — root `AGENTS.md` (layout, canonical commands: `go build ./cmd/creo`, `go test ./...`, `gofmt`/`go vet`, port conventions, contract-test policy), README stub, PRD M0 status note.

## Assumptions validated

| Assumption | Status | Evidence |
|---|---|---|
| `modernc.org/sqlite` (no CGO) supports WAL, `busy_timeout`, transactional fenced appends, concurrent writers on darwin/arm64 | **Confirmed** | Probe run 2026-08-03: `journal_mode: wal`; stale-generation append rejected with 0 partial writes; 400 concurrent inserts, 0 errors (scratchpad `sqlprobe`) |
| Go toolchain present on target machine | **Confirmed** | `go version go1.26.5 darwin/arm64` |
| `anthropic-sdk-go` supports Messages + manual tool loop | **Confirmed** | claude-api skill reference (loaded this session): `client.Messages.New`, `ToolParam`, `ToolUnionParam`, `resp.ToParam()`, `StopReasonToolUse`, worked example |
| `ANTHROPIC_API_KEY` available for the demo script | **Confirmed** | `.env` at repo root (verified this session); gitignored |
| SSE from stdlib `net/http` | **Confirmed** | `http.Flusher` pattern, stdlib-stable; localhost in M0 so no proxy-buffering concerns |
| Fencing/idempotency SQL is portable to Postgres later | **Confirmed by design** | Contracts SL-3/RC-1/RC-3 use `SELECT`+conditional-`INSERT` in a transaction and unique-key upserts — no SQLite-only constructs; conformance suite is store-agnostic by construction |
| Harness loop effort | **Confirmed** | spike-01: ~100-line loop worked first try (`spikes/01-harness/RESULTS.md`, H3) |

**Hypothesis falsifier (checked):** if M0 required cross-process coordination (multiple worker processes), the in-process broker + single-writer discipline would be the wrong shape — it doesn't: the v-min decision (architecture.md §1) fixes one process, and the contracts (not the mechanisms) are what carry to cluster.

## Spec cross-validation

- **PRD AC-1** (session survives worker failure) → step 9b. **AC-2** (project survives workspace loss) → step 9c. **AC-3** (no duplicate work) → steps 3 (RC-1) + 9a.
- **Contracts SL-1..5** → step 2 tests; **RC-1..5** → step 3 tests; ProjectStore contracts (immutability, traceability, round-trip) → step 4 tests; Gateway contracts (budget hook point, usage-on-failure, credentials only here) → step 5; Harness statelessness + error translation at emit → step 6.
- **Invariants:** event log is sole interaction truth (no component state outside it — steps 2–7 all route through `Append`); workspaces never authoritative (step 9c proves it); L0 palette has no exec (step 4 Workspace has no such method — capability by absence).

## Project conventions to follow

- **[repo root, verified 2026-08-03]** No `AGENTS.md`/`CLAUDE.md`/`README.md` exists — the repo is docs+spike only, zero commits. → **Reflected in plan:** step 10 creates `AGENTS.md`; this plan *sets* the conventions rather than inheriting them (flagged, not silent).
- **[`go.mod` — to be created]** Package manager: Go modules; three pinned deps. → Steps 1, 5.
- **[`spikes/01-harness/package-lock.json`]** TS tooling uses npm. → Doesn't constrain this plan (no TS in M0); recorded for M3.
- **[`docs/components.md` overview + §8]** Ports: API `:8080`, served sites `:8081`. → Step 7 binds `:8080`; `:8081` reserved for M2.
- **[`docs/components.md` §1, §2]** Contract tests are the testing policy for storage-adjacent components. → Steps 2–4 structure tests as store-agnostic conformance suites.
- **[`docs/architecture.md` §1 decisions]** v-min: single process, in-process dispatch, no outbox; SQLite fsync-on-commit for the log. → Steps 1, 2, 7.
- **Test framework / lint:** Go stdlib `testing`, `gofmt` + `go vet` — set by this plan (step 10), no framework deps.

## Codebase conflict sweep

- **In-flight work:** none — zero commits, working tree is docs/spike only (`git log`: no commits yet; `git status` checked 2026-08-03).
- **Shadow duplication:** the spike harness (`spikes/01-harness/`) is intentionally throwaway (its README says so); step 6 ports its *semantics*, not its code. No consolidation needed.
- **Caller-side drift:** no callers exist; the API surface created here becomes the contract.
- **Test infra gaps:** none to inherit; e2e harness (subprocess spawn/kill) is created in step 9.

## Counterfactual — plan vs. success criteria: **Confirmed**

Kill-9-resume → 9b; duplicate-submit-once → 3+9a; workspace-loss survival → 9c; contracts tested → 2/3/4; headless proof → 8. No criterion without a step.

## Risks & mitigations

- **SQLITE_BUSY under load** → single-writer connection + `busy_timeout`; probe showed 0 errors; contract tests hammer it deliberately.
- **Crash-test flakiness** (timing-dependent SIGKILL) → fake adapter's pause hooks make the kill point deterministic, not sleep-based.
- **Anthropic SDK drift** → version pinned; all tests run on the fake adapter; only the demo script touches the network.
- **modernc.org/sqlite is a translation, not upstream C** → acceptable at our write rates (probe evidence); the storage interface means swapping to the CGO driver is a one-file change if ever needed.

## Out of scope (M0)

Auth/tokens, web client, preview/publish, budgets beyond usage recording, OpenAI-compat adapter, checkpoint/compaction, export, multi-tenant surface (schema carries `tenant_id` with a constant default), container sandboxes, TypeScript anything.

## Open questions (gate)

1. **Go module path** — propose `github.com/korya/creo` (matches git user). Confirm or supply.
2. **Initial commit** — the repo has zero commits; I'd commit the current docs/spike state first (one commit), then land M0 in reviewable commits per step. Confirm you want me committing.

## Test plan

- **Unit/contract:** SL-1..5 (incl. subprocess crash test), RC-1..5, workspace confinement, ProjectStore round-trip/CAS, harness runs on fake adapter incl. mid-run lease loss.
- **e2e (fake adapter):** duplicate submit; kill-9-resume; workspace loss.
- **Manual:** `scripts/demo-m0.sh` against real Anthropic — the PRD M0 demo, performed live.
