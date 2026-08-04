# Blueprint — Fix 4: a graceful restart must resume the in-flight build, not fail it

**Status:** validated plan, awaiting approval · 2026-08-04
**Headline:** A SIGTERM (deploy, config change, `Ctrl-C`) during a build cancels the run context; `executeRun` misreads that cancellation as a genuine failure and writes `run.failed` + `Complete(failed)`, destroying recoverable work and showing the user "Something went wrong." Fix it at the classifier: distinguish **context cancellation** (shutdown or lease-loss) from a **genuine harness error**. On shutdown, leave the run recoverable — proactively transition it to `recovering` so the next boot claims it immediately — reusing the exact machinery the SIGKILL path already proves. Genuine errors still emit `run.failed`.

## Problem statement

- **Goal:** a SIGTERM mid-build leaves the run **resumable** — on restart it resumes from the SessionLog and completes, exactly like the SIGKILL path — the user sees **no** false "something went wrong", and genuine failures still produce `run.failed`.
- **Root cause (verified at HEAD):** the CLI wires `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` (`cmd/creo/main.go:207`). On SIGTERM the parent ctx cancels; it flows `Run → worker → executeRun`, which derives `runCtx` from it (`server.go:248`) and passes `runCtx` into `h.Execute` (`server.go:269`). The model call `h.Gateway.Complete(ctx, …)` returns `ctx.Err()` on cancel (`fake.go:34–37`), which the harness wraps as `"model call: %w"` (`harness.go:144–146`). Back in `executeRun`, the only special-cased error is `eventlog.ErrStaleLease` (`server.go:272`); a wrapped `context.Canceled` is **not** matched, so it falls through to `EmitFailure` + `Complete(…, StatusFailed, …)` (`server.go:273–274`). A shutdown-cancel is thus misclassified as a real failure.
- **Constraints / must-not-break:**
  - The kill-resume e2e `TestKillDashNineAndResume` (`e2e_test.go:194–236`) and the fencing/zombie tests (`coordinator_test.go` `TestFencingAfterTakeover`, `harness_test.go` `TestStaleHarnessIsFenced`) must stay green.
  - The genuine-failure guard `TestBudgetExhausted` (`hostile_test.go:170–199`) must stay green — a real failure still emits a plain-language `run.failed`.
  - RC-4 "no limbo" (`docs/components.md:70`): a shutdown-canceled run must deterministically reach a terminal **or waiting** state, never limbo.
  - Zero committed-event loss (R-NFR-1, `PRD.md:267`; R-SES-1, `PRD.md:155`): no path may drop or corrupt logged work.
  - Stdlib-first (`AGENTS.md`): the fix uses `sync`/`context`/`errors` only — no new dependency.
- **Non-goals:** drain-to-completion of in-flight builds on shutdown (rejected below — a build is 60–140 s, far past any acceptable graceful window); changing lease TTL semantics; reworking `RecoverOrphans`; the other findings (1–3); any client change.
- **Success criteria:**
  - **S1** SIGTERM mid-run does **not** write `run.failed` and does **not** mark the run `failed`.
  - **S2** After restart on the same data dir, the run resumes from the log and reaches `run.completed` exactly once, with a gapless sequence and one `run.resumed` marker (SIGKILL-parity).
  - **S3** A genuine harness/model failure still emits `run.failed` + `Complete(failed)`.
  - **S4** RC-4 holds: the shutdown-canceled run is left in `running` (lease-expiry → `RecoverOrphans`) or `recovering` (claimable at once) — never a terminal-failed or limbo state.
  - **S5** No committed-event loss; the lease-loss zombie path (b) still writes nothing.
  - **S6** `go build ./...`, `go vet ./...`, `gofmt -l .`, and `go test ./...` all clean.

## Hypothesis (and its falsifier)

**Chosen shape: leave-recoverable, made immediately claimable via `recovering`.** Reclassify the `executeRun` error branch by inspecting the two contexts in scope, which cleanly separate the three cancel origins the current code conflates:

| Case | Origin | `ctx.Err()` (parent/worker) | `runCtx.Err()` (child) | Correct handling |
|---|---|---|---|---|
| (a) graceful shutdown | parent ctx canceled by SIGTERM, propagates to `runCtx` | **non-nil** | non-nil | leave recoverable; proactively mark `recovering` |
| (b) lease lost to takeover | renewal goroutine calls `cancel()` on `runCtx` only (`server.go:258–261`) | nil | **non-nil** | do nothing — new holder owns the narrative; writes are fenced anyway |
| (c) genuine harness/model error | no cancellation | nil | nil | `EmitFailure` + `Complete(failed)` — unchanged |

The discriminator is `ctx.Err()` for (a) vs. `runCtx.Err()` for (b) vs. neither for (c). The classifier becomes: **only `EmitFailure`+`Complete(failed)` when neither context is canceled and the error isn't a stale lease.**

For (a), "immediately claimable" is the reason to do more than nothing. If we merely *skip* the failure and let the run stay `running` (the passive variant, = exactly the SIGKILL state), the run carries a **future** `lease_expires_at`; on reboot `RecoverOrphans` skips it (its `WHERE … lease_expires_at <= now` requires an *expired* lease, `coordinator.go:220`) and `Claim` skips it (`WHERE r.status IN (queued, recovering)`, `coordinator.go:131`) — so it is reclaimable only after the lease TTL elapses in real time, then the next periodic recover tick flips it to `recovering`. Delay ≈ up to `TTL + TTL/2` (~22 s at the 15 s default). To avoid that wait, on shutdown the holder calls a new `coord.Relinquish(lease)` that sets the run to `StatusRecovering` (lease-fenced, like `Complete`). A `recovering` run is directly claimable by `Claim` on the next boot — resume is near-instant. The passive leave-`running` behavior remains the **safety net**: if the relinquish write doesn't land (drain times out, DB already closing), the run is still recovered via lease-expiry, exactly as SIGKILL is today.

**Enabling change — bounded worker drain.** `Run` does **not** currently wait for worker goroutines before `s.db.Close()` (`server.go:216–221`): on `ctx.Done()` it shuts the HTTP servers and closes the DB immediately. A relinquish write issued from `executeRun` during shutdown would race `db.Close()`. So the fix adds a `sync.WaitGroup` over the workers and a **short, bounded** wait (proposed 2 s) before `db.Close()` — long enough for `executeRun` to persist one `recovering` update (sub-second; the harness returns on cancel near-instantly per `fake.go:34`), not long enough to wait for a build. On timeout, proceed to close anyway → passive safety net applies.

**Why not drain-to-completion (option ii):** it would hold the process open until the in-flight build finishes. A real model build is 60–140 s (per the task's field data) — longer than any acceptable graceful-shutdown / deploy window (SIGTERM→SIGKILL is typically 10–30 s). It would either block deploys or get SIGKILLed mid-drain anyway, reducing to the SIGKILL path with extra latency. Note our chosen drain waits only for the *relinquish write*, **not** the build — the 60–140 s problem never applies.

**Why proactively `recovering` rather than clear the lease:** setting `lease_expires_at=''` on a `running` run would make it **invisible** to `RecoverOrphans` (its guard is `lease_expires_at != ''`, `coordinator.go:220`) — the opposite of claimable. The claimable state is `recovering`, which `Claim` selects directly (`coordinator.go:131`). Cluster-safe too: a node relinquishing to `recovering` is a graceful hand-off another live node claims immediately, still lease-fenced (RC-3).

**Falsifier for the shape:** if `ctx.Err()` were **not** reliably non-nil in (a) at the classification point, the discriminator collapses — refuted: on SIGTERM the parent ctx is canceled *before* `runCtx` (child) unblocks the model call that returns the error, so by the time `Execute` returns, `ctx.Err()` is set. Second falsifier: if the renewal goroutine also canceled the **parent** on lease-loss, (a) and (b) would be indistinguishable — refuted: it calls the local `cancel()` bound to `runCtx` only (`server.go:248,260`), never the parent. Third falsifier: if a genuine error could carry a canceled `runCtx` (e.g. harness cancels internally on a real error), (c) would be misrouted to "leave recoverable" and never fail — inspected: the harness never cancels its own context; `runCtx` is canceled only by shutdown (parent) or the renewal goroutine (`server.go:260`). Holds.

## Ordered steps

1. **`internal/run/coordinator.go` — add `Relinquish` (satisfies S1, S4).** A lease-fenced transition to `recovering`, symmetric with `Complete`:
   ```go
   // Relinquish yields a held run back to the recoverable pool without failing
   // it — used on graceful shutdown so the next boot claims it immediately.
   // Lease-fenced (RC-3): only the current holder can, and a superseded holder
   // is a no-op. Leaves the run in `recovering` (RC-4 waiting state), claimable.
   func (c *Coordinator) Relinquish(ctx context.Context, lease eventlog.Lease) error
   ```
   `UPDATE runs SET status=?, lease_expires_at='', updated_at=? WHERE id=? AND lease_worker=? AND lease_gen=? AND status=?` with `StatusRecovering` / `StatusRunning`; `n==0` → `ErrLeaseLost` (benign here). Call `c.Poke()` on success (mirrors `RecoverOrphans`, `coordinator.go:229–231`) so a co-resident worker could pick it up even without a restart.
2. **`internal/server/server.go` `executeRun` (~269–277) — reclassify the error branch (satisfies S1, S3, S5).** Replace the single `ErrStaleLease` guard with the three-way classifier:
   ```go
   text, err := s.h.Execute(runCtx, r)
   if err != nil {
       switch {
       case errors.Is(err, eventlog.ErrStaleLease):
           // (b') zombie after takeover — already fenced; write nothing.
       case ctx.Err() != nil:
           // (a) graceful shutdown — leave recoverable, never fail.
           slog.Info("run left recoverable (shutdown)", "run", r.ID)
           if e := s.coord.Relinquish(context.WithoutCancel(ctx), r.Lease); e != nil {
               slog.Warn("relinquish failed", "run", r.ID, "err", e)
           }
       case runCtx.Err() != nil:
           // (b) lease lost to a takeover — new holder owns the narrative.
           slog.Info("run yielded (lease lost)", "run", r.ID)
       default:
           // (c) genuine failure.
           slog.Warn("run failed", "run", r.ID, "err", err)
           s.h.EmitFailure(context.WithoutCancel(ctx), r, err)
           s.coord.Complete(context.WithoutCancel(ctx), r.Lease, run.StatusFailed, err.Error())
       }
       return
   }
   ```
   Note: `context.WithoutCancel(ctx)` is required because the parent `ctx` is canceled during shutdown — mirrors the existing failure path (`server.go:273`) and `model.go:95`.
3. **`internal/server/server.go` `Server` + `Run` + `worker` — bounded worker drain (satisfies S2's write, S4 safety net).** Add `wg sync.WaitGroup` to `Server`. In `Run`, before the worker loop, `s.wg.Add(1)` per worker and pass so `worker` does `defer s.wg.Done()`. In the `case <-ctx.Done()` arm, before `s.http.Shutdown`, wait on the group with a bound:
   ```go
   drained := make(chan struct{})
   go func() { s.wg.Wait(); close(drained) }()
   select {
   case <-drained:
   case <-time.After(2 * time.Second): // relinquish window; NOT the build
       slog.Warn("worker drain timed out; in-flight runs recovered via lease expiry")
   }
   ```
   then the existing `http.Shutdown` / `serving.Shutdown` / `db.Close()`. Add `"sync"` to imports. (The drain window is a small constant, not user-facing config; see open question if it should be a flag.)
4. **`internal/run/coordinator_test.go` — unit-test `Relinquish` (satisfies S1, S4, RC-3).** (a) a held lease → run becomes `recovering`, `lease_expires_at` cleared, and a subsequent `Claim` returns it with an incremented gen; (b) a **superseded** lease (after another `Claim`) → `Relinquish` is a no-op returning `ErrLeaseLost`, and the run keeps the new holder's state (fencing preserved).
5. **`internal/e2e/e2e_test.go` — SIGTERM-resume e2e (satisfies S1, S2), + a `sigterm()` helper.** Add `func (e *env) sigterm()` mirroring `sigkill()` (`e2e_test.go:97–104`) but sending `syscall.SIGTERM` (and `e.cmd.Wait()`). Add `TestSigtermMidRunResumes`, structurally the twin of `TestKillDashNineAndResume` (`e2e_test.go:194–236`): `fake:slow-site`, wait for `tool.result >= 3`, `e.sigterm()` instead of `e.sigkill()`, restart same data dir, assert `run.completed == 1`, exactly one `run.resumed`, gapless seq, run status `completed`, 1 committed version, all 8 pages present. Crucially assert **`count(evs, "run.failed") == 0`** across the whole session — the guard that shutdown produced no false failure (S1). This is the executable reproduction of the bug.
6. **Regression guard for genuine failure (satisfies S3).** `TestBudgetExhausted` (`hostile_test.go:170–199`) already proves a genuine failure emits a plain-language `run.failed` and no `run.completed`. It exercises case (c) (no context canceled — budget `Deny` is a plain error), so step 2 leaves it green. No new test needed; cite it as the S3 guard. Optionally note it in the plan's test matrix.
7. **Build + full suite (satisfies S6).** `go build ./cmd/creo`, `go vet ./...`, `gofmt -l .`, `go test ./...` (unit + e2e; e2e spawns the real binary per `TestMain`, `e2e_test.go:21–35`). No `web/` change → no re-embed.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | SIGTERM cancels the run ctx, surfacing as a wrapped `context.Canceled` classified as a genuine failure today | Confirmed | `cmd/creo/main.go:207` `NotifyContext(…SIGTERM)` → `server.go:248` `runCtx` from parent → `server.go:269` `Execute(runCtx,…)` → `fake.go:34–37` returns `ctx.Err()` → `harness.go:144–146` wraps `"model call: %w"` → `server.go:272–274` only skips `ErrStaleLease`, else `EmitFailure`+`Complete(failed)`. |
| A2 | A run left `running` with a **future** lease is **not** immediately claimable after restart | Confirmed | `RecoverOrphans` requires `lease_expires_at != '' AND lease_expires_at <= now` (`coordinator.go:220`) — a future lease is skipped; `Claim` selects only `status IN (queued, recovering)` (`coordinator.go:131`). So it waits ~`TTL + TTL/2`. Motivates proactive `recovering`. |
| A3 | `recovering` runs are directly claimable, no lease-expiry wait | Confirmed | `Claim` `WHERE r.status IN (?, ?)` = `StatusQueued, StatusRecovering` (`coordinator.go:131,144`); RC-4 (`docs/components.md:70`) names `recovering` the claimable waiting state. |
| A4 | Lease-loss (b) cancels only `runCtx`, not the parent — so `ctx.Err()` distinguishes (a) from (b) | Confirmed | renewal goroutine closes over the local `cancel` bound to `runCtx` (`server.go:248,252–261`); parent `ctx` untouched. |
| A5 | `Run` does **not** wait for workers before `db.Close()` — a shutdown write races the close | Confirmed | `server.go:216–221`: `case <-ctx.Done()` → `http.Shutdown` → `serving.Shutdown` → `s.db.Close()`, no worker join. No `sync`/`WaitGroup` in the file. Justifies step 3. |
| A6 | Writing during shutdown needs a non-canceled ctx | Confirmed | existing failure path already uses `context.WithoutCancel(ctx)` (`server.go:273–274`); metering uses the same trick (`model.go:95`). |
| A7 | The lease-loss zombie (b) already writes nothing (fencing) — reclassifying to "do nothing" is behavior-preserving for a real takeover | Confirmed | after takeover the gen is superseded, so `EmitFailure`'s Append fails `ErrStaleLease` (SL-3, `docs/components.md:44`) and `Complete` matches 0 rows → `ErrLeaseLost` (`coordinator.go:203–205`). Both are no-ops; skipping is equivalent and also removes a latent false-fail on a *transient* Renew error. |
| A8 | Genuine budget failure is case (c): neither context canceled | Confirmed | budget `Deny` returns before any model call and never cancels a context; `TestBudgetExhausted` (`hostile_test.go:170–199`) asserts `run.failed` with "budget" text and zero `run.completed`. |
| A9 | No test pins "shutdown/ctx-cancel ⇒ failed" | Confirmed | grep of `_test.go` for `StatusFailed`/`run.failed`/`SIGTERM`/`context.Canceled`/`shutdown`/`Interrupt`: only `e2e_test.go` (SIGKILL, no assertion of `failed`) and `hostile_test.go` (budget → `failed`, a genuine case). None assert shutdown-→-failed. |
| A10 | The fake model respects ctx cancel (so the e2e reproduces the real cancel) | Confirmed | `fake.go:31–37` selects on `ctx.Done()` during `StepDelay` and returns `ctx.Err()`; `slow-site` has `StepDelay: 150ms` over 8 steps (`fake.go:96–105`) — a wide SIGTERM window. |

## Cross-validation — specs, invariants, conventions

- **RC-4 no limbo** (`docs/components.md:70`) → **satisfied by steps 1–2**: (a) lands in `recovering` (waiting) via `Relinquish`, or falls back to `running`→lease-expiry→`RecoverOrphans`→`recovering`; (c) reaches terminal `failed`; (b) is owned by the live new holder. No state reaches limbo.
- **RC-5 recovery scan** (`docs/components.md:71`) → **unchanged and reused**: the passive safety net rides the existing boot + periodic `RecoverOrphans` (`server.go:180–196`).
- **RC-3 generations / SL-3 fencing** (`docs/components.md:44,68`) → **preserved**: `Relinquish` is lease-fenced (step 1), and case (b') still writes nothing.
- **R-SES-1 / R-NFR-1 durability** (`PRD.md:155,267`) → **satisfied by S1/S5**: no path drops committed events; a shutdown now *stops* discarding recoverable work.
- **AGENTS.md conventions** →
  - [`AGENTS.md` "Stdlib-first"] Only three external deps. → **Reflected:** step 2–3 use `sync`/`context`/`errors`/`time` only; no new dep.
  - [`AGENTS.md` "Contracts are tests"] run-semantics changes keep RC conformance green + add tests. → **Reflected:** steps 4–6 (unit `Relinquish` + e2e SIGTERM + existing budget guard).
  - [`AGENTS.md` "Canonical commands"] `go build ./cmd/creo`, `go test ./...`, `go vet ./... && gofmt -l .`. → **Reflected:** step 7 runs exactly these.
  - [`AGENTS.md` "Plain-language userText authored in harness"] → **Doesn't constrain this plan:** the fix *suppresses* an erroneous emit on shutdown; it authors no new user text. Genuine failures keep the harness-authored `EmitFailure` text.
  - [`AGENTS.md` "Every /v1 route tenant-scoped / hostile isolation case"] → **Doesn't constrain this plan:** no new `/v1` route or handler; the change is worker/coordinator internals.
  - [`AGENTS.md` "Commits: area-prefixed, one per step"] → **Reflected:** land as `core:` commits (coordinator, then server, then tests).
  - [`AGENTS.md` "Web client"] → **Doesn't constrain this plan:** no `web/` source touched; committed `internal/webui/dist` stays valid, no re-embed.
- **Architecture** → the change stays inside the RunCoordinator/worker seam (`docs/architecture.md`); it routes recovery through the documented lease/status machinery rather than around it. Observability: adds `slog` lines for the shutdown-recoverable and lease-yield paths (currently indistinguishable in logs).

## Codebase conflict sweep

- **In-flight work:** `git log` on `internal/server internal/run internal/harness` — most recent touch is `165290b fix: live build progress`; nothing pending on shutdown/recovery. No collision.
- **Shadow duplication:** the only `context.Canceled`/`WithoutCancel` sites are `server.go:273–274`, `model/fake.go:36`, `model/model.go:95` — the reclassification is localized; no parallel shutdown handler exists to keep in sync.
- **Caller drift:** `Relinquish` is a new method — no existing callers to update. The `executeRun` signature is unchanged. `worker`'s signature is unchanged (the `WaitGroup` is a field, decremented via `defer`).
- **Test-infra gap:** the e2e harness already spawns/signals the real binary and has a `sigkill()` (`e2e_test.go:97–104`); step 5 adds a one-line-different `sigterm()`. No new infra needed.

## Plan-vs-success-criteria counterfactual — **Confirmed**

S1 → steps 2 (skip failure on (a)) + 5 (assert `run.failed == 0`). S2 → steps 1+3 (immediately claimable via `recovering` + drain) with the passive safety net, verified by step 5. S3 → step 2 case (c) + step 6 guard. S4 → steps 1–2 + RC-4 cross-check. S5 → step 2 case (b') + A7. S6 → step 7. Every criterion maps to at least one step.

## Risks & mitigations

- **Drain window too short / a stuck worker** → the run isn't relinquished. *Mitigation:* the passive safety net — it stays `running` with a soon-expiring lease and is recovered on reboot exactly like SIGKILL (already proven by `TestKillDashNineAndResume`). No work lost; only a small resume-latency cost.
- **`Relinquish` write lands but the process is then SIGKILLed before `db.Close()` flushes** → SQLite WAL + fsync-on-commit (`docs/architecture.md:150`, SL-5) means an acknowledged write is durable; a half-written one is rolled back, leaving the run `running` → safety net. Either way recoverable.
- **A provider that returns `context.Canceled` for a *genuine* error unrelated to our cancel** → would route to "leave recoverable" and never fail. *Mitigation:* the classifier keys on `ctx.Err()`/`runCtx.Err()` (our own cancellation state), **not** on `errors.Is(err, context.Canceled)`, so a provider's spurious wrap can't trigger (a)/(b) unless we actually canceled. If neither context is canceled, it's correctly case (c).
- **Double-fire: renewal goroutine logs a false "lease lost" during shutdown** (Renew fails on the canceled `runCtx`). *Mitigation:* cosmetic only — it still resolves to case (a) via `ctx.Err() != nil`; optionally suppress the warn when `runCtx.Err() != nil` from parent. Non-blocking.

## Out of scope

- Draining in-flight **builds** to completion on shutdown (rejected: build ≫ graceful window).
- Making the drain window a CLI flag (proposed constant 2 s; see open question).
- Changing lease TTL, `RecoverOrphans` cadence, or the `Claim` query.
- Findings 1–3 and any client/web change.

## Open questions

1. **Drain-window policy.** Proposed a fixed 2 s wait for the relinquish write (not the build). Acceptable as a constant, or should it be a `--drain-timeout` flag / derived from `LeaseTTL`? (The build is never waited on either way.)
2. **Immediate-claimable vs. simplest.** Plan commits to proactive `recovering` (near-instant resume) + the drain WaitGroup. If you'd rather ship the minimal change first, the **passive** variant (skip failure, leave `running`, recover via lease-expiry — no `Relinquish`, no WaitGroup) is a strict subset that still fixes S1/S3/S4/S5 and matches SIGKILL parity, at a ~`1.5×TTL` restart-resume latency. Say the word and I'll descope steps 1+3.
