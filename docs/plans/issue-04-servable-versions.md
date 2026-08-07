# Blueprint — servable versions, derived from scratch (issue #4)

**Status:** **implemented 2026-08-07** — shipped in six commits (`profile:` → `project:` → `harness:` → `publish:`/`api:` → `model:`/`e2e:` → `docs:`). One premise was wrong and is corrected in step 7; one fixture had to change and was signed off.
**Provenance:** independent derivation per instruction; a superseded draft of the same fix existed at the time and was neither used as input nor cited as evidence (it has since been deleted, so nobody implements the wrong one). Every claim below carries its own HEAD citation. Root-cause analysis: the issue-#4 RCA in this session (5-whys chain verified in code).

**Accepted coverage limits (recorded at implementation).** The repair *mechanics* are owned by deterministic fixtures — `repairs-site` for the happy path, `no-page` for exhaustion, plus a kill-and-resume case proving the budget survives takeover. A real model's early stop could not be forced on demand, so the live qwen3.6 run proves only the absence of false positives on healthy output, not the presence of a real-world repair. Issue #4 records how it manifested in the wild.

## Headline

Make it structurally impossible for an unservable site to be minted as a version or pointed at by the live slug: the profile defines "servable", the **project store refuses to commit** an artifact that fails it, the harness gives the model silent repair turns before failing honestly, and the **publish store refuses in-transaction** to point live at any invalid version (including pre-existing ones).

## Problem statement

- **Goal.** A run that ends without a servable site must not report success, must not enable Publish, and must never 404 live (issue #4). Observed: a qwen3.6 run wrote only `css/style.css`; the platform celebrated, published, and served 404.
- **Root cause (from the RCA).** Success is derived from the model's stop reason (`harness.go:206`), not from artifact state; no component owns "servable". `Commit` accepts any file set (`store.go:62-94`, quota check only), `publishProject` checks only that a version resolves (`api.go:257-262`), and the gateway 404s any root without `index.html` (`serving.go:64-65,80`).
- **Constraints.** Stdlib-first (`AGENTS.md:81`); one authority per component (`AGENTS.md:85-87`); plain-language userText authored at emit time (`AGENTS.md:88-93`); R-AGT-3 escalation ladder — silent retry first, one decision max, no error codes (`PRD.md:177`); healthy builds must not change behaviour.
- **Non-goals.** The model's early-stop habit; deep linting (links, a11y); styled 404 for missing sub-pages (issue #13); proactive client-side Publish disabling beyond what exists; web-client changes of any kind.
- **Success criteria.**
  (S1) No `run.completed` for an unservable artifact; exactly one plain-language failure if unrepairable.
  (S2) Neither publish nor rollback can point live at an unservable version — including versions minted before this change.
  (S3) Silent repair happens before any user-visible failure (R-AGT-3 ladder).
  (S4) No partial work is lost; the project is never left worse than before the run.
  (S5) Healthy builds are behaviourally identical — no new events, no extra model calls.
  (S6) Deterministic, key-free coverage of repair-success, repair-exhaustion, and refuse-to-publish.

## Hypothesis and falsifiers

**Invariant:** *the version store never mints, and the live pointer never targets, an artifact the serving gateway cannot serve at its root.* Enforced at three layers, outermost first:

1. **Mint gate (choke point).** `project.Store.Commit` gains an injected `Validate` hook (the exact shape of the existing injected `Quota` hook, `store.go:88-94`) that refuses the commit. `Commit` has exactly two callers, both in the harness (`harness.go:231,271`), but the hook covers all future callers by construction.
2. **Repair loop (UX).** The harness validates at the moment the model claims to be done; on failure it appends a logged repair instruction and gives the model bounded extra turns; on exhaustion it fails the run with tailored plain language and commits nothing.
3. **Live-pointer gate (legacy backstop).** `publish.Store.Publish`/`Rollback` gain an injected `Validate(ctx, projectID, versionID)` hook called before the pointer flip, in-transaction — this is what protects against versions minted before the invariant existed (at least one exists in real data, per the issue's QA project).

**Shape falsifiers, checked:**
- *If a legitimate site could omit `index.html`, requiring it would be arbitrary.* Refuted: our own gateway resolves every directory root to `index.html` and 404s otherwise (`serving.go:64-65,79-81`) — unservable by construction, not convention.
- *If a repair instruction couldn't survive crash/resume, in-loop repair would be the wrong shape.* Checked: `reconstruct()` rebuilds the conversation **solely** from logged events (`harness.go:353-394`), so the instruction must be — and will be — an event; resume then sees a text-only assistant message plus a user message, no pending tools, and proceeds to `Complete()` (`harness.go:398-409`, `:137`).
- *If the workspace didn't survive a failed run, refusing to commit would destroy partial work.* Refuted: workspaces are per-project directories that persist across runs (`workspace/local.go:33`), and the empty-workspace recovery path only triggers when the workspace has zero files (`harness.go:120-128`) — partial CSS survives to the next turn.
- *If validation inside the publish transaction could deadlock SQLite,* the in-tx gate would be the wrong shape. Checked: the publish store already reads the `versions` table inside its write transaction (`publish.go:108`), and the hook's read goes through the project store's separate reader connection (`store.go:216`, `db.R`) against committed rows only.

## Ordered steps

1. **`internal/profile/profile.go`** — the policy.
   - `var ErrArtifactInvalid = errors.New(...)`.
   - `RequiredFiles []string` on `Profile`; `Websites()` sets `[]string{"index.html"}`.
   - `func (p Profile) ValidateArtifact(files map[string]int64) error` — pure; for each required file: present at exactly that slash-relative path (nested `pages/index.html` does not count — the gateway joins at the root, `serving.go:65`) and size > 0 (a zero-byte `index.html` is unservable in the way that matters). Wraps `ErrArtifactInvalid`. Empty `RequiredFiles` accepts anything. Mirrors `ValidatePalette` (`profile.go:42`) — same owner, same shape, other end of the run.

2. **`internal/project/store.go`** — the mint gate.
   - New field `Validate func(files []File) error` beside `Quota`; called in `Commit` after the manifest is built and **before the quota check** — servability is decided before capacity, so a tenant near their storage limit mid-repair still gets `ErrArtifactInvalid` (repairable) rather than `ErrStorageExceeded` (terminal). Same refuse-before-write discipline as the quota check (`store.go:86-94`). Nil hook = no gate (unit tests of the store stay untouched).
   - The injected closure returns an error wrapping `profile.ErrArtifactInvalid` whose message names the missing/empty file; the store propagates with `%w` — `errors.Is` holds for callers, and the store itself imports no policy package. Note: the store calls an *injected* policy exactly as it does `Quota` (owned by tenant) — no policy authored in the store.

3. **`internal/harness/harness.go`** — the repair loop.
   - Constants `EvRepairStarted = "repair.started"`, `EvRepairCompleted = "repair.completed"` — both already specified, unemitted, in `docs/architecture.md` (§3.2 event vocabulary; the `repair.started | repair.completed` line) — plus `RepairDetail{Reason, Instruction string}`.
   - **The harness does not validate independently — it calls `Commit` and branches.** (Review simplification, 2026-08-07: no `Workspace.Size`, no harness-side map-builder, no second input shape — the gate and the check are the same code path and cannot disagree. Cost: one wasted manifest hash per repair turn, unhappy path only.)
   - At `final` (`harness.go:222-224`): call `h.Projects.Commit`. On `errors.Is(err, profile.ErrArtifactInvalid)` with repair budget remaining: append `repair.started` (**`userText` empty** — the start renders nothing), whose detail carries a terse imperative instruction derived from the error's named missing file (e.g. "The site has no index.html home page. Create it now, reusing the work already in the workspace."); append the same instruction to `msgs` as a user-role message; `continue` instead of `break`. On a later `final` where `Commit` succeeds, append `repair.completed` **with a brief plain-language acknowledgment as userText** (e.g. *"That one took a little longer than usual."* — acknowledge the time, not the artifact, so the line doesn't invite worry about the home page) — decided in review (2026-08-07): repairs are acknowledged, not hidden.
   - **The step-limit path goes through the gate too.** When `MaxIterations` exhausts, the existing post-loop commit ("the work so far is saved", `harness.go:227-229`) can now be refused with no turns left to repair — in that case return the `ErrArtifactInvalid`-wrapped error instead of claiming the work is saved: the "saved" message would be exactly the lie this issue is about. The workspace still persists (A9), so nothing is actually lost.
   - **Budget counted from the log:** `repair.started` events carrying this run's ID in `prior` (`harness.go:98`) plus in-invocation increments — a takeover mid-repair cannot reset it.
   - Budget exhausted → return an error wrapping `profile.ErrArtifactInvalid` **without committing** — the server already routes Execute errors to `EmitFailure` + run-failed (`server.go:420-425`), and with zero versions the client's existing logic disables Publish and shows the empty state (`web/src/app.ts:219,236`, `hasVersion` set only when a version exists, `:261`).
   - `reconstruct()` gains `case EvRepairStarted`: flush, then append the detail's instruction as a user-role text message — the same pattern `EvInputProvided` uses (`harness.go:380-389`).
   - `commitProgress()` (`harness.go:270-279`, the pre-question park): treat `profile.ErrArtifactInvalid` from `Commit` as "nothing servable to snapshot yet" — skip silently, no `version.created`. Parking mid-build with no page is legitimate; minting a broken version is not.
   - `EmitFailure` (`harness.go:284-297`) gains a case beside the budget/storage ones: e.g. *"I couldn't finish your site this time — it doesn't have a home page yet. Nothing went online, and everything built so far is kept. Try asking once more."* (exact copy to pass `assertPlainLanguage`, `e2e/language_test.go:35`).

4. **`internal/publish/publish.go`** — the live-pointer gate.
   - New field `Validate func(ctx context.Context, projectID, versionID string) error`.
   - `Publish` (`publish.go:76`): call it on the requested version before the upsert. `Rollback` (`publish.go:97`): call it on the resolved `parent` inside the transaction, between the parent lookup (`:108`) and the `UPDATE` (`:115`) — race-free by construction. Nil hook = no gate.

5. **`internal/api/api.go`** — error translation only (no policy).
   - `publishProject`/`rollbackProject` (`api.go:246,273`) map `errors.Is(err, profile.ErrArtifactInvalid)` → 409 with plain language, e.g. *"That version of your site doesn't have a home page, so it can't go online. Ask for the page you want and publish again."* Both routes are already tenant-scoped (`api.go:248,275`); no new routes.

6. **`internal/server/server.go`** — wiring (one construction site for everything).
   - After `project.New` (`server.go:101`) and `publish.New` (`:169`), with `p := harness.DefaultProfile()` (already used at `:166,185`):
     `ps.Validate = func(files []project.File) error { … p.ValidateArtifact … }`;
     `pub.Validate = func(ctx, projectID, versionID) error { files := ps.VersionFiles(…); return p.ValidateArtifact(…) }`.

7. **`internal/model/fake.go`** — deterministic adversaries. The step index is the count of assistant messages (`fake.go:40-45`) and an exhausted script answers "All done." with `StopEndTurn` (`:46-52`), so:
   - `no-page`: one step writing only `css/style.css`, then done → every repair turn re-answers "All done." with no writes → deterministic exhaustion path.
   - `repairs-site`: step 0 writes only CSS and stops; step 1 (reached only via the repair turn) writes `index.html` → deterministic repair-success path.
   - These stay registered permanently: the fake fleet modelled only cooperative models, which is *why* this class was invisible to tests.

   **Correction (2026-08-07, found during implementation).** The RCA's link 5 asserted that *every* script writes `index.html`. An audit of all nine says seven of nine did; `slow-site` wrote `page1.html`…`page8.html` and no home page at all. That fixture backed the two AC-1 acceptance tests and passed the entire suite for four milestones — **because nothing anywhere asserted that a version was servable.** This is stronger evidence for the same conclusion, not a retraction of it: the gap was not merely that fixtures were too kind, but that an outright impossible output — an eight-page website a visitor cannot open — sat in the fleet unnoticed. It surfaced the moment the gate existed. `slow-site`'s first page is now `index.html` (step count, delay, and kill-window unchanged, so both AC-1 tests keep proving kill → resume → completion), and `assertServable` in step 8 is what makes the class visible from here on.

8. **`internal/e2e/artifact_test.go`** + `helpers_test.go` — scenarios and the standing guardrail.
   - Repair succeeds: no `run.failed`, exactly one `run.completed`, `repair.started` (empty userText) + `repair.completed` (acknowledgment text, passes `assertPlainLanguage`) in the log, preview root serves the repaired page.
   - Repair exhausts: exactly one `run.failed` whose text passes `assertPlainLanguage`; **zero versions**; Publish returns 400 ("nothing to put online yet", existing path `api.go:258-260`); client-visible state is "empty", not 404.
   - Kill-and-resume mid-repair (the crash-helper pattern, `AGENTS.md:144-145`): budget not reset, instruction survives via `reconstruct`.
   - The existing `ask_user` park still works when nothing servable exists yet (guards the `commitProgress` change).
   - **Standing guardrail:** `assertServable(t, e, token, projectID)` in `helpers_test.go` — fetch the newest version's preview root via the existing preview-URL flow (pattern: `publish_test.go:44-50,68-70`) and require HTTP 200. Called after every run-completion wait in e2e scenarios, so any future path that mints a version a visitor can't open fails loudly, not silently.
   - **Legacy backstop coverage** cannot be an e2e scenario — post-fix, the product can no longer mint an invalid version (that's the point). It becomes a co-located test in `internal/publish` (or `internal/api` with httptest): commit a CSS-only version through a store with a nil `Validate`, then wire the hook into the publish store and assert `Publish` and `Rollback` both refuse.

9. **Docs.**
   - `docs/components.md` §10: `validators` (`components.md:227`, declared, previously zero implementations) becomes real in its minimal form; the ProductProfile contract gains the artifact-side refusal beside the palette-side one.
   - `docs/architecture.md` §3.2: mark `repair.*` as emitted.
   - `PRD.md` open question #6 (auto-repair visibility, `PRD.md:369`): record the partial answer this ships — missing-page repairs are performed autonomously and acknowledged with one brief plain-language line, never a decision (decided 2026-08-07) — explicitly not closing the general question.
   - `AGENTS.md` testing policy: the registry keeps adversarial scripts (`no-page`, `hostile`) so the fake fleet can't drift back to all-cooperative models — **and fixtures must model outputs a real model could actually produce**, citing `slow-site` as the in-repo example of that drift (an eight-page site with no home page is not an output a real build should ever leave standing).

Commits, one per component (`AGENTS.md:136-137`): `profile:`, `project:`, `harness:`, `publish:` + `api:`, `model:` + `e2e:`, `docs:`.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | Root path resolves to `index.html`; absent → 404 | Confirmed | `serving.go:64-65,79-81` |
| A2 | Success is stop-reason-derived; commit + `run.completed` unconditional | Confirmed | `harness.go:206,231-237` |
| A3 | `Commit` builds the manifest first and has an injected-policy precedent (`Quota`) refusing before write | Confirmed | `store.go:62-94` |
| A4 | `Commit` has exactly two callers, both harness | Confirmed | grep: `harness.go:231,271` only |
| A5 | `publishProject` gates only on version existence; `Rollback` resolves parent in-tx | Confirmed | `api.go:257-262`; `publish.go:97-122` |
| A6 | `version_files` carries `path, blob_sha, size`; `VersionFiles` exists | Confirmed | `store.go:206-232`; `migrations/0001_init.sql:61` |
| A7 | `reconstruct` uses only logged events; `EvInputProvided` is the user-role-injection precedent | Confirmed | `harness.go:353-394` |
| A8 | A post-repair-instruction resume has no pending tools and proceeds to `Complete` | Confirmed | `pendingTools` scans the last assistant message (`harness.go:398-409`); the repair turn's last assistant message is text-only |
| A9 | Workspace persists per project; recovery only fires on zero files | Confirmed | `local.go:33`; `harness.go:120-128` |
| A10 | `ListFiles` yields slash-relative paths | Confirmed | `local.go:108-112` (`filepath.Rel` + `ToSlash`) |
| A11 | Fake adapter: step = assistant-message count; exhausted script ends turn with no writes | Confirmed | `fake.go:40-52` |
| A12 | Execute errors reach `EmitFailure` → `run.failed`, which already branches per error kind | Confirmed | `server.go:420-425`; `harness.go:284-297` |
| A13 | Client disables Publish and shows empty state when no version exists; `hasVersion` set only from real versions | Confirmed | `web/src/app.ts:219,236,261` |
| A14 | `repair.started/completed` are specified but never emitted | Confirmed | `docs/architecture.md` §3.2 vocabulary block; grep `repair` in `internal/` → no hits |
| A15 | One construction site wires all stores + profile | Confirmed | `server.go:101,169,166,185` |
| A16 | `ErrArtifactInvalid` in `profile` creates no import cycle | Confirmed | `profile` imports only `model` (`profile.go:7-12`). The stores gain **no** profile edge — hooks are injected closures held by `server`, and the stores propagate the closure's error with `%w`. The only new import is `api` → `profile` for the `errors.Is` sentinel, which is acyclic (`profile` imports nothing of ours but `model`) |
| A17 | Profile validation precedent to mirror | Confirmed | `ValidatePalette`, `profile.go:39-55` |

## Spec cross-validation

- **R-WEB-2** (`PRD.md:217`, constrained static bundle, enforced): steps 1–2 extend "enforced" from *ceiling* (no server code) to *floor* (has a page) — the RCA's link 5 named this asymmetry as the root gap.
- **R-AGT-3** (`PRD.md:177`, silent retry → one plain decision, no error codes): step 3's repair turn is the silent rung; the single `run.failed` text is the one message; 409 bodies are sentences.
- **P7** (`PRD.md:32`, "build repair: the platform decides or repairs silently"): step 3 follows P7's substance — the platform decides and repairs without asking the user anything — while the decided-in-review acknowledgment line surfaces *that* a repair happened, in plain language with zero decisions. This is a deliberate, recorded softening of P7's letter (see PRD open question #6 update, step 9), not a silent deviation.
- **R-SES-5 / conventions** (`AGENTS.md:94-96`, "a client that decides for itself what `run.completed` means is a bug"): the fix keeps success semantics platform-side; zero client changes.
- **AC / R-TEN-5** (`PRD.md:189`, export as anti-lock-in): export stays ungated — it is data portability, not a success claim; under the mint gate no *new* export can be a pageless site, and exporting legacy partial work remains the user's right.
- **Tenant invariant** (`AGENTS.md:101-104`): no new routes; steps 4–5 extend two handlers that already resolve tenancy and 404 foreign projects — no new hostile case required, noted not skipped.
- **Event-log-is-truth**: the repair instruction is *in* the log (step 3), which is exactly what makes takeover/resume correct.
- **components.md §10** (`components.md:227`): `validators` goes from declared to real (step 9) — no new concept invented.

## Project conventions to follow

- **[`AGENTS.md:81`]** Stdlib-first; deps are architecture decisions. → Steps 1–6 add zero dependencies; validation is a map lookup.
- **[`AGENTS.md:83`]** Contracts are tests; eventlog/run semantics keep conformance green. → No semantics change to append/lease; step 8's kill-and-resume case guards the interaction.
- **[`AGENTS.md:85-87`]** One authority per component. → Profile owns "servable" (step 1); store and publish execute *injected* policy (steps 2, 4 — the `Quota` pattern); API only translates errors (step 5).
- **[`AGENTS.md:88-93`]** Plain-language userText at emit time; API errors are sentences; `language_test.go` enforces. → Steps 3, 5 author copy at emit; step 8 asserts it.
- **[`AGENTS.md:100`]** Ports: API `:8080`, sites `:8081`. → Doesn't constrain this plan — no new listeners; e2e uses the spawned binary's own ports.
- **[`AGENTS.md:101-104`]** Tenant scoping + hostile cases for new routes. → No new routes (step 5 extends existing scoped handlers).
- **[`AGENTS.md:108-135`]** Web client toolchain (npm, Vite+). → Doesn't constrain this plan — no client change; `internal/webui/dist` untouched (A13 shows the client already behaves correctly with zero versions).
- **[`AGENTS.md:136-137`]** One reviewable commit per component, area-prefixed imperative subject. → Commit plan at end of Ordered steps.
- **[`AGENTS.md:141-145`]** Co-located `*_test.go`; `model.FakeScript` keeps tests deterministic and key-free; crash tests re-exec the binary. → Steps 7–8 follow all three patterns.
- **[`justfile`]** `just check` fixes / `check-ci` verifies (both run golangci-lint), `just test` inner loop (`-short` skips e2e), `just test-full` includes e2e. → Gate for every step; new e2e runs under `test-full`; `errcheck` means the new `VersionFiles` calls get handled errors.
- **[Migrations: `internal/store/migrations/`]** → Doesn't constrain this plan — no schema change; validity is derived from `version_files` at check time, never stored.

## Codebase conflict sweep

- **In-flight work:** none — `git status` shows only an untracked plan doc; recent commits are lint/CI chores (`4c24deb`…`b08a3a1`) and none touch harness/publish/profile semantics.
- **Shadow duplication:** the only existing validation is `ValidatePalette` — step 1 deliberately mirrors it rather than inventing a second style; the only injected-policy hook is `Quota` — steps 2 and 4 reuse that shape.
- **Caller-side drift:** no exported signature changes. New struct fields (`Profile.RequiredFiles`, `project.Store.Validate`, `publish.Store.Validate`) are additive; nil hooks preserve current behaviour, so existing unit tests and constructors compile and pass unchanged.
- **Test-infrastructure gaps:** none blocking — e2e spawns the binary, `assertPlainLanguage` exists (`language_test.go:35`), fetch-and-assert pattern exists (`publish_test.go:44-50`). One relocation: legacy-invalid-version coverage must live at unit/integration level, since post-fix the product cannot create that state end-to-end (step 8).
- **Existing bad data:** at least one CSS-only version exists (issue's QA project). Step 4 stops it going live; nothing cleans it up — the next successful run supersedes it.

## Counterfactual vs success criteria — **Confirmed**

| Criterion | Satisfied by |
|---|---|
| S1 no false success, one plain failure | steps 2–3 (no mint, typed error), 3 (`EmitFailure` copy) |
| S2 live pointer never targets invalid, incl. legacy | step 4 (in-tx gate on publish *and* rollback), steps 2–3 (new invalids never exist) |
| S3 silent repair first | step 3 (empty-userText repair events, budgeted turns) |
| S4 nothing lost | A9 (workspace persists); valid states still commit; `commitProgress` skips rather than fails |
| S5 healthy builds identical | validation passes → no events, no extra turns, no behaviour change; nil-hook default elsewhere |
| S6 deterministic coverage | steps 7–8 |

Against the captured repro: the qwen3.6 run fails `ValidateArtifact` at its first `final`, gets a repair instruction (the manual follow-up in the issue proves one more turn fixes it), and either completes with a real page or fails honestly with zero versions — Publish disabled by existing client logic (A13). The chain breaks at link 3 of the RCA (the broken version is never minted); the step-4 gate breaks link 2 for versions that already exist.

## Risks & mitigations

- **Repair turns cost time/tokens on slow local models** (~minutes/turn on qwen3.6). Bounded budget (proposed: 2); zero cost on healthy builds. The alternative — reporting failure when one more turn fixes it — is strictly worse.
- **The mint gate changes `commitProgress` semantics:** parking before anything servable exists now yields no version (previously it minted one). This is correct (nothing to show) but is a behaviour change to the M4 question flow — step 8 asserts the park/resume path explicitly. **Known UX consequence** (review, 2026-08-07): the user now answers the agent's question against an *empty* preview instead of a partial one. Correct — a CSS-only "preview" was a 404 anyway — but visibly different; expect it to be reported as a regression and don't treat it as one.
- **Instruction echo:** the repair instruction is a user-role message; chatty models may echo it (issue #7 shows qwen3.6 echoes freely). Detail text stays terse and imperative; the echo lands in `assistant.message` context, not `run.completed` userText — worth one manual check against qwen3.6.
- **In-tx validate reads:** the hook reads via the project store's reader while the publish writer holds a tx. Same DB, different connection, committed rows only (A5, A6-adjacent) — WAL-safe; if this ever bites, the fallback is one duplicated manifest query inside the tx.
- **Legacy invalid versions remain in history** (latest for affected projects). Gated from going live, not cleaned up; superseded by the next good run.

## Out of scope

Export gating (data portability, not a success claim — see spec cross-validation) · styled 404 (#13) · richer validators (links, a11y) · model early-stop behaviour · any `web/` change · cleanup of existing invalid versions.

## Decisions (resolved with the user, 2026-08-07)

1. **Repair budget: 2** attempts before the honest failure.
2. **Repair visibility: brief acknowledgment** — `repair.completed` carries one low-key plain-language line acknowledging the *time*, not the artifact (e.g. *"That one took a little longer than usual."* — naming the home page would invite "what went wrong with it?"); `repair.started` stays invisible. A deliberate softening of P7's "repairs silently" — recorded in the PRD open-question #6 update (step 9).
3. **Policy form: `RequiredFiles` as data** on `Profile`; the `ValidatorRef` function form waits for a real second validator.

## Test plan

- **Unit `internal/profile`:** valid set; missing `index.html`; zero-size `index.html`; nested-only `pages/index.html` rejected; empty `RequiredFiles` accepts all.
- **Unit `internal/project`:** `Commit` refuses via injected hook, wraps `ErrArtifactInvalid`, writes nothing (no blobs, no rows); nil hook unchanged; an artifact that is both invalid *and* over quota fails with `ErrArtifactInvalid`, not `ErrStorageExceeded` (ordering).
- **Unit `internal/publish` (+/or httptest in `internal/api`):** seed an invalid version with a nil-hook store; hooked `Publish` and `Rollback` both refuse; API maps to 409 sentences.
- **Unit `internal/harness`:** budget derived from prior log events survives takeover; `reconstruct` orders the repair instruction correctly.
- **e2e `internal/e2e/artifact_test.go`:** repair-success (silent), repair-exhaustion (one plain `run.failed`, zero versions), kill-and-resume mid-repair, `ask_user` park with nothing servable; `assertServable` guardrail wired after run-completion waits across scenarios.
- **Regression:** `just test-full` green, especially publish/rollback/export e2e and the kill/resume choreography; `just check-ci` clean.
- **Manual:** re-run the qwen3.6 repro; confirm silent repair or honest failure — no celebration over a 404.
