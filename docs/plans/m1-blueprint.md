# M1 Blueprint — tenancy & safety

**Status:** validated plan, awaiting approval · 2026-08-04
**Headline:** Make the v-min server safe to expose to a network and to multiple tenants: mandatory bearer-token auth with structural tenant scoping on every route, hard budget caps enforced at the ModelGateway, per-tenant run quotas, workspace hardening, the atomic-submit fix promised in M0 — proven by a hostile-tenant e2e demo.

## Problem statement

- **Goal (PRD §9 M1):** tenants + auth, quotas + hard budget caps, sandbox hardening; demo = a hostile prompt-injected project fails to reach another tenant, the host, or any credential.
- **Constraints:** contracts SL/RC untouched; API stays the only surface; stdlib-only (token gen `crypto/rand`, hashing `crypto/sha256`); existing M0 data (implicit `t_default`) keeps working.
- **Non-goals:** OIDC/web login, user objects distinct from tenants (see open question 1), storage/egress quotas (egress is moot at L0 — no exec, no network tools; storage quota deferred to M2 with real asset upload), Postgres, containers.
- **Success criteria:** (S1) every route except `/healthz` rejects missing/invalid tokens; (S2) a valid token cannot see or touch another tenant's projects/sessions/runs — structurally (scoped queries), observed as 404; (S3) a tenant over its daily token budget has new model calls refused at the gateway with a plain-language event, and the cap cannot be bypassed; (S4) per-tenant concurrent-run quota respected by Claim; (S5) the hostile script's every escape attempt fails and no cross-tenant bytes appear in its session; (S6) duplicate concurrent submits of one idempotency key produce exactly one run *and* one `user.message` event (closes the M0 race).

## Hypothesis (and its falsifier)

One migration + one new package + a middleware, no architectural change: `tenants` and `api_tokens` tables; `internal/tenant` (identity + budget/quota queries); auth middleware in `internal/api` resolving token→tenant and scoping every query; budget check inside `model.Metered` (the one choke point); quota check inside `run.Coordinator.Claim`; `AppendTx`/`RequestRunTx` extracted so submit is one transaction.

**Falsifier:** if any enforcement point turns out *not* to be a single choke point — e.g. model calls that bypass `Metered`, or a route reading the DB without going through the scoped helpers — the middleware-plus-chokepoint shape is wrong and enforcement would need to move into the storage layer. Checked below (A2, A6): both chokepoints are real.

## Ordered steps

1. **Migration `internal/store/migrations/0002_tenancy.sql`** — `tenants(id, name, daily_token_limit INTEGER NULL, max_concurrent_runs INTEGER NOT NULL DEFAULT 2, created_at)`; `api_tokens(id, tenant_id, name, token_sha256 UNIQUE, created_at, revoked_at NULL)`; seed `t_default` (unlimited budget) so M0 data remains valid.
2. **`internal/tenant/`** — `Create`, `SetLimits`, `CreateToken` (returns plaintext `creo_<40 hex>` exactly once; stores sha256), `Revoke`, `Authenticate(plaintext) → tenantID` (constant-time compare on hash), `SpentToday(tenantID)` (SUM over `usage` ⋈ `runs` ⋈ `projects` since midnight UTC), `CheckBudget`, `RunningRuns(tenantID)`. Unit tests incl. revoked-token rejection and budget window edges.
3. **Atomic submit (S6)** — `eventlog.AppendTx(tx, …) []Event` + exported `Publish` (post-commit); `run.RequestRunTx(tx, …)` + exported `Poke`; rewrite `api.submit` as one `db.Write` (key check → append → run insert), then publish+poke. Hammer test: N concurrent same-key submits ⇒ exactly 1 run, 1 `user.message`.
4. **Budget + quota enforcement** — `model.Metered` gains a `Budget` hook called before every `Complete` (resolves run→project→tenant, checks `CheckBudget`); on exceeded returns typed `ErrBudgetExceeded`; harness maps it to a `run.failed` with plain userText ("The AI budget for this account is used up for today…"). `run.Coordinator.Claim` adds the per-tenant concurrent-run predicate (join through projects). Contract-style tests for both.
5. **API auth + scoping** — middleware on all `/v1/*` routes: `Authorization: Bearer` → `tenant.Authenticate` → tenantID in request context; every query gains `tenant_id = ?` (projects) or an ownership pre-check (sessions/runs/versions via join); unknown-or-foreign ⇒ 404 (existence is not leaked). `createProject` writes the real tenant id. `serve --insecure` (open question 3) maps missing auth to `t_default` for local dev.
6. **CLI** — local admin subcommands operating directly on the data dir (bootstrap can't require a token): `creo tenant new NAME [--daily-tokens N] [--data]`, `creo token new TENANT_ID --name X`, `creo token revoke ID`; client commands (`project`, `say`, `watch`) gain `--token` / `CREO_TOKEN`.
7. **Workspace hardening** — `resolve` gains an `EvalSymlinks` containment check (defense against out-of-band symlinks in a workspace); unit test with a planted symlink pointing outside.
8. **Hostile e2e (`internal/e2e/hostile_test.go`)** — fake script `hostile` whose steps attempt: `read_file ../../<victim>/index.html`, `read_file /etc/passwd`, `read_file ../../../../.env`, `write_file ../evil.txt`, `delete_file ../x`; assertions: every `tool.result` is an error, nothing outside the workspace changed, the victim project's content never appears in any event, and tenant B's token gets 404 on all of tenant A's routes. Plus budget-exhausted e2e (tiny limit ⇒ refused run with plain event) and concurrent-quota e2e.
9. **Docs** — AGENTS.md (new subcommands, auth in quickstart), README quickstart update, `docs/components.md` IdentityService/§4 status notes, PRD M1 delivery mark.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | Only 6 routes + healthz exist; all unauthenticated today | Confirmed | `grep mux.HandleFunc internal/api/api.go` at HEAD `5168ab6` (7 lines, this session) |
| A2 | All model traffic flows through `model.Metered.Complete` — one budget chokepoint | Confirmed | `internal/harness/harness.go` calls only `h.Gateway.Complete` (a `*model.Metered`); no other `Gateway` consumer (`grep`) |
| A3 | `tenant_id` exists on `projects`+`sessions` with `t_default` default; `runs`/`usage` reach tenant via `project_id` join | Confirmed | `0001_init.sql:3,11`; `runs.project_id` (`0001_init.sql`), `usage.run_id` |
| A4 | SQLite (modernc) handles the new migration file pattern | Confirmed | migration runner globs `migrations/*.sql`, applies by index (`internal/store/store.go`), proven with 0001 |
| A5 | `Append` can be split into `AppendTx` + post-commit publish without breaking SL contracts | Confirmed | publish already happens after `db.Write` returns (`eventlog.go`); the split moves code, not semantics — SL tests stay as the guard |
| A6 | Claim's SQL can express per-tenant concurrency (join runs→projects on tenant) | Confirmed | same NOT EXISTS shape as the per-project predicate already in `coordinator.go` |
| A7 | No in-flight work / drift | Confirmed | `git status` clean at `5168ab6`; sole author this session |

## Spec cross-validation

- **R-TEN-1** (structural scoping) → step 5: scoped queries + context tenant, not per-handler if-checks; foreign = 404. **R-TEN-3** (quotas) → step 4 — *partial*: concurrent runs + daily tokens only; CPU/egress are moot at L0 (no exec, no network tools), storage quota deferred to M2 (flagged, not silent). **R-LLM-5** (hard caps at the gateway) → step 4. **PRD AC-6/AC-7** (cross-tenant + credential unreachability) → step 8. **IdentityService component** (`components.md` §11) → steps 2/6 implement exactly its minimal surface. Trust tier: this is the T1→T2 transition; the PreviewGateway origin concession is untouched (M2 concern).

## Project conventions to follow

- **[`AGENTS.md` "Stdlib-first"]** three deps, additions are architecture decisions → steps 2/5 use `crypto/rand`+`sha256`, no new deps.
- **[`AGENTS.md` "Contracts are tests"]** → steps 3/4 extend the eventlog/run test suites; SL/RC tests must stay green unmodified.
- **[`AGENTS.md` "Canonical commands"]** `go test ./...`, `go vet`, `gofmt` before commit → per-step commits as in M0.
- **[`AGENTS.md` "Commits"]** area-prefixed imperative subjects, one commit per step → same cadence.
- **[`AGENTS.md` "Ports"]** API on `127.0.0.1:8080` → unchanged; auth is header-based, no new ports.
- **Migration tooling** — numbered SQL files under `internal/store/migrations/` (`store.go` runner) → step 1 follows the 0001 pattern.
- **Package manager / TS rows** — don't constrain this plan (pure Go milestone).

## Codebase conflict sweep

In-flight: none (clean tree, single author). Shadow duplication: none — tenancy code has no prior art in-repo. Caller-side drift: step 3 changes `api.submit` internals only (HTTP shape unchanged); CLI e2e helpers gain a token header (step 8 updates them). Test infra: e2e harness from M0 is reused as-is; hostile test is a new file in the existing package.

## Counterfactual — **Confirmed.** S1→step 5(+8), S2→5(+8), S3→4(+8), S4→4(+8), S5→7/8, S6→3.

## Risks & mitigations

- **Scoping miss on a future route** → the e2e cross-tenant test iterates every registered route; adding a route without a scoping test fails review by convention (noted in AGENTS.md update).
- **Budget window edge cases (UTC midnight)** → unit test pins the boundary; limits are advisory-precision (a run in flight at rollover finishes) — documented, matches "hard stop for *new* calls".
- **`--insecure` misuse** → refuses to bind non-loopback addresses; logs a warning banner.
- **Token in shell history** → CLI prints once with a warning; `CREO_TOKEN` env is the documented path.

## Out of scope

OIDC/users, storage/egress quotas, per-project budgets, token rotation UX, rate limiting (RPM), TLS (reverse-proxy documented), audit-log surfacing (events already carry run/tenant attribution).

## Open questions (gate)

1. **Token = tenant principal for now?** No user objects until a human-login surface exists (M3/M4). PRD says tenant→users→projects; this defers the middle layer knowingly.
2. **Budget model:** per-tenant *daily* token limit (input+output summed, resets midnight UTC), null = unlimited, set via CLI. Good enough for M1?
3. **`serve --insecure`** (loopback-only, maps to `t_default`) to keep the M0 dev loop friction-free — yes/no?

## Test plan

Unit: tenant auth (valid/revoked/garbage), budget math + window edge, token hashing. Contract-adjacent: Claim tenant-quota predicate; Metered budget refusal; atomic-submit hammer (S6). E2E: hostile script (S5), cross-tenant 404 sweep (S2), no-token/bad-token sweep (S1), budget-exhausted run (S3), quota (S4). Manual: README quickstart with real token flow.
