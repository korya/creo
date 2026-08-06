# M4 Blueprint — multi-device & polish

**Status:** validated plan, awaiting approval · 2026-08-05
**Headline:** Give the platform its human-login surface (the pluggable `Authenticator` seam with the `static` T1 driver), the interaction primitives multi-device actually needs (waiting-for-input runs, answer-from-any-device, cancel), honest LAN/Tailscale reachability, the OpenAI-compat adapter, and the two owed deferrals (D1 users, D2 storage quota) — closing AC-4, AC-5, AC-12, AC-14.

**Scope decisions (Dmitri, 2026-08-05):** `oidc` driver deferred to M5 (new ledger entry D6 — go-oidc/oauth2 deps are an architecture decision, and T1 doesn't need the driver); static accounts are CLI-administered DB rows (matching `creo tenant/token`), config file is an M5 packaging concern; run cancel (R-RUN-4, previously unowned by any milestone) is in.

## Problem statement

- **Goal (PRD §9 M4):** second-device resume UX, cross-device approvals, idle-park/wake tuning, error-language pass (R-AGT-2/3), OpenAI-compat adapter against ≥1 local model. Plus, per session decisions of 2026-08-04: D1 (users/login — the `Authenticator`/IdentityService design, `components.md` §11), D2 (storage quota, missed at M2), and the OQ#4 networking direction (LAN + Tailscale).
- **Constraints:** stdlib-first (no new Go deps — the static driver needs none); every new `/v1` route tenant-scoped with a hostile-test case; plain-language `userText` authored at emit time; the web client stays a thin consumer of the public API; `static` fenced by tripwires per the §11 enforcement contract; local user rows canonical (`user_identities` linking table).
- **Non-goals:** `oidc` driver (D6, M5); asset upload (R-WEB-4 — still unowned, stays in ledger); per-site origins / preview auth rework (D4/D5, T2); checkpoint/compaction (R-SES-6 watched); a long-lived per-session harness ("idle-park" is inherent to v-min — workers exist only during runs; the M4 deliverable is *measuring* wake, not building parking).
- **Success criteria:**
  (S1) A human logs into the web client by tapping an account (no password, no token paste); every event they cause is attributed to their user ID; sessions survive browser restart (cookie).
  (S2) The same project opened on a second device shows identical state within seconds, and a question asked by the agent can be answered from either device (AC-4, AC-5).
  (S3) A run can wait for input indefinitely (releasing its worker), resume on answer, and be cancelled from any device; all transitions are events; no run is ever lost or duplicated (RC contracts hold).
  (S4) Clients render session state from explicit state events (R-SES-5), never inferred from event patterns.
  (S5) `serve` on a LAN address works end-to-end from a phone (login, chat, preview links resolve to a reachable host); binding a public address with static auth refuses to start without `--allow-unsecured`.
  (S6) `--model openai:<model>@<base-url>` completes the build loop against a local server (Ollama/LM Studio-class) with no client-visible behavior change (AC-14).
  (S7) A tenant at its storage limit gets a plain-language refusal, not a crash (D2, R-TEN-3).
  (S8) Every failure path a non-coder can hit ends in silent auto-recovery or one plain-language decision (AC-12) — audited against this week's non-coder transcripts.

## Hypothesis (and its falsifier)

One migration (`0004_identity.sql`: `users`, `user_identities`, `web_sessions`, `events.actor`, `tenants.max_storage_bytes`) plus one new package (`internal/identity`: `Authenticator` seam, static driver, cookie-session mint/verify, `Principal{UserID,TenantID,Method,Assurance}`) slots under the existing `api.auth` middleware — the single auth chokepoint — without touching any component below the API layer. Interaction machinery is two new run statuses (`waiting_for_input`, `cancelled`) in the existing coordinator + an `ask_user` tool in the profile palette; cancel rides the existing lease-renewal mechanics (flipping status kills the next `Renew`, which already aborts the worker). The web client gains a picker screen and drops token-in-URL (same-origin `EventSource` sends the session cookie automatically).

**Falsifier for the identity shape:** if authentication cannot be expressed as "middleware resolves a credential to a Principal" — i.e. if any component below the API layer needs to know *how* the human logged in — the seam is wrong. Checked: the only consumers of identity today are the middleware (`api.go:207`) and event attribution; both consume the resolved result, not the mechanism.
**Falsifier for the interaction shape:** if a waiting run must hold its lease (worker) to preserve RC-2, the "park and release" design is wrong and waiting runs would pin workers. Checked: RC-2 is enforced at `Claim` time by a per-project predicate (`coordinator.go:119-144`) — extending the predicate to also skip projects with a waiting run preserves single-writer without any lease held.

## Ordered steps

**Workstream A — identity (D1)**

1. **Migration `internal/store/migrations/0004_identity.sql`** — `users(id, tenant_id, name, color, created_at, disabled_at)`; `user_identities(issuer, subject, user_id, created_at, PRIMARY KEY(issuer, subject))`; `web_sessions(id, user_id, token_sha256 UNIQUE, created_at, expires_at, revoked_at)`; `ALTER TABLE events ADD COLUMN actor TEXT NOT NULL DEFAULT ''`; `ALTER TABLE tenants ADD COLUMN max_storage_bytes INTEGER` (NULL = unlimited). Static accounts get `user_identities(issuer='static', subject=<user id>)` rows at creation — the linking table exists from day one so an IdP migration never orphans attribution.
2. **`internal/identity` package** — `Authenticator` interface (`Describe/BeginLogin/CompleteLogin` per `components.md` §11), `Assurance` (`attributed|proven`), `VerifiedIdentity`, the `static` driver (choices from the `users` table of its bound tenant), and the fixed `Service`: `CompleteLogin → user row → mint web session` (random token, SHA-256 at rest — same discipline as `api_tokens`), `Authenticate(credential) → Principal`, `RevokeSession`. Login-flow state is in-memory with TTL (v-min single process; an interrupted login just restarts — nothing durable is lost). Unit tests: mint/verify/revoke, expiry, disabled user, unknown flow.
3. **Startup fence (`internal/server`)** — `checkExposure(bindAddr, hasStaticAccounts, allowUnsecured)`: loopback and private ranges (RFC1918, CGNAT 100.64/10 for Tailscale, ULA, link-local) pass; a global address (or `0.0.0.0`/`::` with any global interface IP) with static accounts **refuses to start** with a plain-language error naming `--allow-unsecured`; the override is logged at startup and stamped into `/healthz` output. Multi-tenant + static picker on a non-loopback bind → same refusal (a second untrusted human is T2 by definition). Unit tests over the address matrix.
4. **API auth + routes** — middleware accepts the `creo_session` cookie (→ user Principal) *or* bearer token (→ tenant principal, `Method: "api-token"`) at the existing single chokepoint; context carries `Principal`; API-written events set `actor`. New routes: `POST /v1/auth/login/begin` (unauthenticated; returns picker choices for the static tenant — name/color only), `POST /v1/auth/login/complete` (sets HttpOnly + SameSite=Lax cookie; no `Secure` flag at T1 — the documented plain-HTTP concession), `POST /v1/auth/logout`, `GET /v1/auth/me`. Hostile tests: cookie from tenant A on tenant B's resources → 404; revoked session → 401; `begin` leaks nothing beyond the bound tenant's display names.
5. **CLI** — `creo account new NAME [--tenant ID] [--color C]`, `account ls`, `account disable ID` (local, data-dir pattern like `tenant`/`token`). `serve` gains `--allow-unsecured`.
6. **Web client login** — the `key` screen becomes the account picker (`begin` → tap → `complete`); token paste remains behind an "operator" link (unchanged `Api` bearer path, used by tests/ops). Header shows current account + switch (re-runs picker, re-mints). The family-mode banner ("anyone who can reach this server can use these accounts") renders whenever `me.assurance == "attributed"`; dismissal lives in page memory only — no storage — so it reappears on every reload and new tab (decided 2026-08-05). Cookie mode drops the SSE `?token=` query param: same-origin `EventSource` sends cookies natively — strictly better than M3's token-in-URL.

**Workstream B — interaction primitives**

7. **Coordinator states** — add `waiting_for_input`, `cancelled`. `Await(lease)`: running → waiting, lease cleared (mirror of `Relinquish`). `Resume(runID)`: waiting → queued + `Poke`. `Cancel(runID)`: queued|waiting → cancelled directly; running → cancelled, which mechanically kills the worker on its next `Renew` (renew requires `status='running'`, `coordinator.go:170-175`) — `executeRun` gains a `cancelled` classification alongside the existing shutdown/lease-loss cases so cancellation is not reported as failure. `Claim`'s per-project predicate extends to skip projects with a waiting run (single-writer includes waiting — a second run must not mutate under a parked conversation). `RecoverOrphans` ignores waiting runs (no lease to expire). Contract tests: RC-2/RC-4 extended cases; waiting run pins no worker; cancel from every non-terminal state.
8. **`ask_user` tool** — added to the websites palette (`internal/profile`): `{question, choices?}`. Harness handles it by emitting `input.requested` (`userText` = the question verbatim, `detail.choices`) and returning `ErrAwaitingInput`; `executeRun` calls `coord.Await`. `reconstruct()` pairs the pending `tool_use` with the later `input.provided` event as its `tool_result`, so the resumed model sees a normal conversation. Approvals note: R-AGT-5 approvals are this same machinery with consequence-phrased `userText`; nothing at L0 requires a platform-initiated approval yet, so no separate approval type ships — the vocabulary is reserved, the mechanism is proven.
9. **API + routing** — `POST /v1/runs/{id}/input {text}` (validates waiting + tenant scope, appends `input.provided` with `actor`, `Resume`), `POST /v1/runs/{id}/cancel` (appends `run.cancelled`). `postMessage` re-routes: if the session's project has a `waiting_for_input` run, the message *is* the answer (append `input.provided`, resume same run) — a typed reply and a tapped choice are the same thing; idempotency keys cover both paths. Web + CLI (`creo answer`, `creo cancel`; watch renders choices): choices render as buttons, free text always allowed; Stop button while building.
10. **Session state events (R-SES-5)** — `session.state.changed` (`idle·queued·working·waiting-for-input·failed`) emitted at the transitions that already append run lifecycle events (submit, harness start, await, complete/fail/cancel); `GET /v1/sessions/{id}` returns current state for initial render. Client renders state *only* from these — deleting the current inference-from-event-patterns in `app.ts`.

**Workstream C — multi-device & networking**

11. **Reachable preview/publish URLs** — when `--public-url` is unset, derive the serving base per-request from the API request's `Host` (swap to the serving port) instead of the boot-time `127.0.0.1:8081` default, so a phone on the LAN gets links it can open. Explicit `--public-url` still wins (the Tailscale/ts.net case).
12. **Deploy doc `docs/deploy/lan-and-tailscale.md`** — the OQ#4 direction as operator steps: LAN bind (`--addr 192.168.x.x:8080`, no override needed — private range), Tailscale (`tailscale up`, bind the tailnet IP or use `tailscale serve` for ts.net HTTPS, `--public-url` for links), what the fence refuses and why, the secure-context caveat of plain-HTTP LAN.
13. **Second-device e2e + wake measurement** — e2e: device A (cookie client) starts a build that asks a question; device B logs in as the same account, sees identical state via cursor backfill, answers; A sees the resumed run (AC-4+5). Wake: measure time-to-first-event on reconnect after idle in e2e and log it against R-NFR-2's p95 < 3s — v-min has no parked harness to tune (workers are per-run), so the deliverable is the measurement proving the log-first design meets the target, or the finding that it doesn't.

**Workstream D — providers**

14. **OpenAI-compat adapter (`internal/model/openai.go`)** — chat-completions with tool calling, mapped to the existing `Gateway` shape (`Complete/Name` — the anthropic adapter is 102 lines, this lands similar); spec `openai:<model>@<base-url>`, key from `OPENAI_API_KEY`/`CREO_OPENAI_KEY` (optional — local servers don't need one). Unit tests against `httptest` fixtures for request/response/tool-call mapping; `scripts/demo-local-model.sh` runs the M0 demo loop against a local endpoint (manual gate for AC-14 — CI stays key-free and fake-only).

**Workstream E — quotas (D2)**

15. **Storage quota** — `CheckStorage` in `internal/tenant` (sum of distinct blob sizes per tenant — CAS-aware, not naive `version_files` sum); enforced in `project.Store.Commit`: over-limit → the run fails with plain-language `userText` ("This site's storage is full — delete old versions or raise the limit."). `creo tenant new --max-storage-mb N`. Hostile/e2e: tenant at limit gets refusal-not-crash; sibling tenant unaffected.

**Workstream F — language & closeout**

16. **Error-language pass (R-AGT-2/3)** — sweep every `userText` emission (`EmitFailure`, budget refusal, quota refusal, publish/rollback errors, cancel/waiting copy) against the escalation ladder: silent retry first, then "I fixed something," then one decision max; no error codes on the primary surface. **Input: this week's non-coder transcripts — this step runs last and its checklist is derived from where real users actually stalled.** Acceptance sweep for AC-12.
17. **Docs & ledger** — PRD §9 M4 delivered note; `docs/deferrals.md`: close D1 + D2 (link commits), add **D6** (`oidc` driver, owed M5, decided 2026-08-05); `components.md` §11 status → implemented (M4) with the final interface; AGENTS.md: new CLI commands, `identity` package, the cookie/bearer duality, new run statuses.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | Auth has a single chokepoint the cookie path can join | Confirmed | `api.go:207-237` — one `auth` middleware wraps every `/v1` route |
| A2 | No waiting/approval/cancel machinery exists (greenfield, no drift) | Confirmed | `coordinator.go:26-30` statuses `queued…failed` only; no `input`/`approval` grep hits outside docs |
| A3 | `RequestRun` always creates a run; serialization happens at `Claim` — so `postMessage` must re-route to a waiting run or it would queue a deadlocked second run | Confirmed | `coordinator.go:90-113` (unconditional INSERT), `coordinator.go:119-144` (RC-2 at claim) → step 9's re-routing is load-bearing, not polish |
| A4 | Cancel can ride lease renewal: `Renew` demands `status='running'`, and a failed renew already aborts the worker | Confirmed | `coordinator.go:170-175`; `server.go:267-282` (renewal goroutine cancels run context) |
| A5 | Same-origin cookies reach SSE — the SPA is served by the API process itself | Confirmed | `api.go:69-73` (web at `/` on :8080); browser-standard: same-origin `EventSource` sends cookies |
| A6 | Migrations pattern extends (0001–0003 exist, `ALTER TABLE ADD COLUMN` precedent in 0003) | Confirmed | `internal/store/migrations/` |
| A7 | `Gateway` is two methods; the anthropic adapter is the size precedent for openai-compat | Confirmed | `model.go:68-72`; `anthropic.go` = 102 lines |
| A8 | The fake adapter can script `ask_user` for deterministic e2e | Confirmed | `fake.go:19-26` — `FakeStep.Tools` carries arbitrary `{Name, Input}`; a new script suffices |
| A9 | Private-range classification is stdlib | Confirmed | `net/netip`/`net.IP.IsPrivate` (RFC1918, Go ≥1.17) + `IsLoopback`/`IsLinkLocal*`; CGNAT 100.64/10 is one manual prefix |
| A10 | No in-flight work conflicts | Confirmed | clean tree at `997a3c2`; last non-doc commits are CI/tooling |
| A11 | Web toolchain: npm + vite-plus (`vp`), vitest/jsdom, oxfmt/oxlint | Confirmed | AGENTS.md web section; `web/package.json` |

## Spec cross-validation

- **AC-4** (continue from another device) → steps 6, 13. **AC-5** (questions/approvals from any device) → 8, 9, 13. **AC-12** (every failure → auto-recovery or one decision) → 16. **AC-14** (two providers incl. one private) → 14.
- **R-SES-5** (explicit state machine, clients never infer) → 10 — this *removes* an existing violation in `app.ts`. **R-RUN-4** (cancel) → 7, 9 — previously unowned by any milestone. **R-RUN-2/5** preserved → 7's contract tests. **R-AGT-5** approvals → mechanism in 8/9, vocabulary reserved (noted, not silently skipped). **R-API-3** (sessions for humans, tokens for programs) → 2, 4. **R-TEN-3** storage → 15. **R-LLM-1** → 14. **R-SEC-2** attribution → `events.actor` (1, 4, 9). **OQ#4/#5 resolutions** → 3, 6, 11, 12 implement the recorded directions verbatim, including the §5.3 T1 honesty caveat (banner, step 6).
- **Invariants:** tenant scoping on all new routes (4, 9 + hostile tests); no credentials below the gateway (openai key lives in the adapter, 14); workspace/L0 untouched; event log remains sole interaction truth (waiting state is a run-row status *derived from* logged events, and rebuildable).

## Project conventions to follow

- **[AGENTS.md "Stdlib-first — adding a dependency is an architecture decision"]** → zero new Go deps in M4; the `oidc` deps are exactly why D6 defers to M5. Reflected in steps 2, 14 (stdlib HTTP client).
- **[AGENTS.md "Every /v1 route tenant-scoped… MUST get a cross-tenant case in hostile_test.go"]** → steps 4, 9, 15 each name their hostile cases.
- **[AGENTS.md "Plain-language userText authored at emit time in the harness"]** → steps 8, 15, 16; no client-side translation added anywhere.
- **[AGENTS.md "justfile is the task runner; CI runs the same recipes"]** → no new entry points; new tests live under `just test` / `test-full` unchanged.
- **[AGENTS.md web rules: npm, `vp` toolchain, `vp build` doesn't type-check alone, `defineConfig` from `vite-plus`]** → step 6 touches `web/src` only; build pipeline untouched.
- **[AGENTS.md "fake adapter makes every test deterministic and key-free"]** → steps 8, 13 use a new fake script; only `scripts/demo-local-model.sh` (step 14) touches a real endpoint, mirroring `demo-m0.sh` precedent.
- **[AGENTS.md commits: "one reviewable commit per component/step; area-prefixed imperative subject"]** → the 17 steps map to ~area commits (`identity:`, `run:`, `api:`, `web:`, `model:`, `docs:`).
- **[AGENTS.md ports 8080/8081]** → unchanged; step 11 only changes how the :8081 base is *named* to clients.
- **Migrations** → numbered SQL files in `internal/store/migrations/` (step 1 = `0004_identity.sql`); no separate tool exists, none invented.

## Codebase conflict sweep

In-flight: none (`997a3c2`, docs/CI only above the M3 code). Shadow duplication: token hashing/minting discipline already exists in `tenant.CreateToken` — `identity` reuses the pattern (SHA-256 at rest, plaintext once), deliberately parallel not shared (different lifecycles: API tokens vs browser sessions). Caller-side drift: `executeRun`'s error classification gains two cases (waiting, cancelled) — the existing shutdown/stale-lease/failure cases keep their semantics (step 7 keeps their tests green); `postMessage` behavior changes for sessions with a waiting run (step 9; new e2e covers old path too); `publicURL` moves from constructor value to per-request derivation (step 11; `serving` untouched, only the API's URL *strings* change). Test infra: e2e harness spawns the binary and drives HTTP — cookie support needs a `http.CookieJar` client in `helpers_test.go`, a small addition, no new layer.

## Risks & mitigations

- **Waiting-state edge cases (the RC contracts are the crown jewels).** Mitigation: every new transition gets contract tests in the same suite that guards RC-1..5; the fence property (SL-3) is untouched — waiting runs hold no lease by design.
- **Non-coder transcripts arrive late or thin** → step 16 degrades to auditing against the PRD ladder alone; flagged in the delivered-note if so (honesty over schedule).
- **Local-model tool-calling quality varies wildly** (qwen-class OK, smaller models flaky) → AC-14 gates on *one* validated model; the adapter declares nothing it can't do; degradation messaging is R-LLM-4 (SHOULD) and explicitly not promised in M4.
- **Cookie-on-plain-HTTP is interceptable on a hostile LAN** → that's precisely T1's documented threat model (§5.3 row 1 + banner); the fence stops the *public-internet* misconfiguration, the docs state the LAN residual.
- **Per-request Host derivation (step 11) trusts the Host header** → used only to *format links returned to that same client*, never for auth or routing; a lying Host header hurts only the liar.

## Out of scope (explicit)

`oidc` driver (D6→M5) · asset upload (R-WEB-4, still in ledger) · per-site origins / preview auth (D4/D5→T2) · egress/CPU quotas (D3, trigger-based) · session branching (R-SES-7) · compaction (R-SES-6 watched) · TS SDK (R-API-4) · approval-specific event type (mechanism ships via `ask_user`; vocabulary reserved until a platform-initiated approval exists).

## Open questions — resolved (Dmitri, 2026-08-05)

1. **Session cookie lifetime:** 90 days rolling; revocation via `account disable` + `RevokeSession`.
2. **`creo answer` CLI:** `creo answer RUN_ID "text"`.

## Test plan

- **Unit/contract:** identity mint/verify/expiry/revoke/disabled; fence address matrix (loopback/RFC1918/CGNAT/ULA/global/0.0.0.0, ± accounts, ± override); coordinator new-state transitions incl. RC-2-with-waiting, cancel-from-each-state, recover-ignores-waiting; openai adapter request/tool-call/response mapping via httptest; storage-sum CAS-awareness.
- **Hostile (`internal/e2e/hostile_test.go`):** foreign cookie → 404 on projects/runs/input/cancel; revoked session → 401; `login/begin` cross-tenant leak check; quota-exhausted tenant isolated from siblings.
- **e2e:** two-device question/answer choreography (A8's fake script); cancel mid-build leaves last committed version intact; state-endpoint + `session.state.changed` sequence assertions; kill -9 while `waiting_for_input` → wait survives reboot (extends the M0 crash choreography); reconnect wake-time measurement logged vs R-NFR-2.
- **Web (vitest):** picker flow, banner render/dismiss-in-memory semantics, choice buttons, state rendering from explicit events only.
- **Manual gates:** phone-on-LAN full loop (S5); `scripts/demo-local-model.sh` against Ollama/LM Studio (S6); transcript-driven language audit (S8).
