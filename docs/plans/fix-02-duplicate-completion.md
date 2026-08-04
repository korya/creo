# Blueprint — Fix 2: duplicate completion message

**Status:** validated plan, awaiting approval · 2026-08-04
**Headline:** The build's final "…is ready!" bubble renders twice because the harness emits the model's final text on *both* the last `assistant.message` and `run.completed`. Fix it at the source — suppress `userText` on the **final** `assistant.message` turn only — so `run.completed` is the single home for the completion message. No client change; the reference client stays a faithful renderer.

## Problem statement

- **Goal:** the run-completion message appears in the transcript **exactly once**.
- **Root cause (verified at HEAD):** on every model turn the harness appends an `assistant.message` with `UserText: joinText(comp.Blocks)` (`harness.go:147–153`). On the *final* turn (`StopReason != StopToolUse`) it sets `finalText = joinText(comp.Blocks)` — the **same string** — and then emits `run.completed` with `UserText: finalText` (`harness.go:155–158`, `168–171`). The client renders **both** as transcript bubbles: `case "assistant.message"` → `addMessage("creo", …)` (`app.ts:80–82`) and `case "run.completed"` → `addMessage("creo", …)` (`app.ts:84–86`). Two identical bubbles.
- **Constraints:** must not regress the fix-01 progress activity line (`tool.result` → transient `#activity`, cleared on completion); must not break other consumers of `run.completed.userText` (CLI `watch`, demo script, Go tests); `userText` is authored at emit time in the harness, never synthesized client-side (`AGENTS.md:56–57`); model context and log-first resume must be unaffected.
- **Non-goals:** changing the activity-line design; touching `tool.result`/`assistant.message` semantics for *working* turns (commentary during a build must still show); the other findings (hydration user-message gap, error translation, graceful-shutdown resume).
- **Success criteria:** (S1) the completion message renders exactly once. (S2) no regression to progress rendering (fix-01 `#activity` line still shows during the build and clears on completion). (S3) no regression to other `run.completed.userText` consumers (CLI, demo, e2e). (S4) model context / resume unchanged. (S5) Go + vitest suites stay green.

## Hypothesis (and its falsifier)

**Chosen: server-side.** In the harness model-completion block (`harness.go:147–158`), compute `final := comp.StopReason != model.StopToolUse` **before** appending. Append the `assistant.message` with `UserText: ""` when `final` (keep `Detail.Blocks` intact); keep `UserText: joinText(comp.Blocks)` for working turns. `finalText` (and thus `run.completed.UserText`) is unchanged. Result: working turns still surface commentary; the completion text is delivered **once**, via `run.completed`. The client is not touched.

**Why not the client:** the client cannot distinguish the *final* `assistant.message` from a working-commentary one at render time — events stream in order, so `assistant.message` is rendered *before* `run.completed` arrives. The only client-side dedup is content-matching the last bubble against `run.completed.userText` — a fragile hack that breaks on legitimately-identical consecutive messages and papers over a server that emits redundant data.

**Why not the task's suggested server variant ("strip text from `run.completed`"):** that is the *wrong* server edge. `run.completed` is already the canonical home for the final text — `harness_test.go:117–119` asserts `run.completed.UserText == finalText`. Worse, the step-limit fallback message (`harness.go:160–162`) exists **only** on `run.completed` — no `assistant.message` ever carries it (the last assistant turn there is a `StopToolUse` turn with real model text). Stripping `run.completed.userText` would silently delete the step-limit message and blank the CLI/demo completion line. Suppressing the *duplicated* final `assistant.message` text keeps `run.completed` whole and preserves the one case where its text is unique.

**Falsifier for the shape:** if `reconstruct()` (model context / resume) read `assistant.message.UserText`, blanking it would corrupt context — **refuted**: `reconstruct` reads assistant turns from `Detail.Blocks` only, never `UserText` (`harness.go:261–266`). Second falsifier: if a test pinned the final `assistant.message.UserText` to the completion text, this would break it — **refuted**: no test asserts final-assistant `UserText` (see A5).

## Ordered steps

1. **`internal/harness/harness.go` (~147–158) — suppress the final turn's UI text.** Replace the unconditional append with:
   - `final := comp.StopReason != model.StopToolUse`
   - `uiText := joinText(comp.Blocks)`
   - append `EvAssistant` with `UserText: ""` when `final`, else `uiText`; `Detail: assistantDetail{Blocks: comp.Blocks}` **unchanged** in both cases.
   - keep `msgs = append(...)`; in the `if final` branch set `finalText = uiText` and `break`.
   Add a one-line comment: the completion message is delivered once, via `run.completed`; the final assistant turn carries context (Blocks), not UI text.
2. **`internal/harness/harness_test.go` — add a regression test.** After a normal `fake:site` run: (a) the last `EvAssistant` event has empty `UserText`; (b) `run.completed.UserText == finalText` (already covered by the existing completion test — keep it green); (c) exactly one event in the log carries `finalText` as `UserText` (the `run.completed`). Assert `reconstruct(log)` yields the same model messages with and without the change is implicitly held by (a)+(the Detail assertion), since only `UserText` changed.
3. **No client change.** `web/src/app.ts` stays a faithful renderer: it shows `assistant.message.userText` when present and `run.completed.userText` when present. With the server no longer duplicating, the final bubble appears once. `app.test.ts:42–50` (completion renders one bubble) stays green as-is.
4. **Build + tests.** `go test ./...` (harness unit + e2e). Web is untouched, but per the embed convention the committed `internal/webui/dist` is already current — **no rebuild needed** (no `web/` source changed). Optionally re-run `cd web && npm test` to confirm nothing drifted.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | The final `assistant.message` and `run.completed` carry the *same* string | Confirmed | `harness.go:149` `UserText: joinText(comp.Blocks)`; `harness.go:156` `finalText = joinText(comp.Blocks)`; `harness.go:170` `run.completed … UserText: finalText`. With `fake:site` both = `"All done."` (`fake.go:47`). |
| A2 | The client renders both events as transcript bubbles | Confirmed | `app.ts:80–82` (`assistant.message` → `addMessage`) and `app.ts:84–86` (`run.completed` → `addMessage`). |
| A3 | Blanking final-assistant `UserText` does not affect model context / resume | Confirmed | `reconstruct` rebuilds assistant turns from `Detail.Blocks`, never `UserText` (`harness.go:261–266`); step 1 leaves `Detail` untouched. |
| A4 | `run.completed` is the *only* home for the step-limit fallback text | Confirmed | `harness.go:160–162` sets `finalText` synthetically when the loop exhausts iterations; no `assistant.message` carries it (last assistant turn there is `StopToolUse`). Stripping `run.completed` text would lose it — hence server-fix targets the assistant event, not `run.completed`. |
| A5 | No test pins the final `assistant.message.UserText` | Confirmed | grep: `harness_test.go:164` sets an assistant `UserText` as *test input* for the resume test (not an assertion of harness output); `TestResumeWithPendingTools` only *counts* assistants (`:211`); `hostile_test.go:115–116` keys on content leak, not the completion text. |
| A6 | `run.completed.UserText == finalText` is an existing, desired contract | Confirmed | `harness_test.go:117–119` asserts it; step 1 keeps `finalText`/`run.completed` unchanged. |
| A7 | `StopReason`/`StopToolUse` are the right discriminator | Confirmed | `model.go:23–24` (`StopEndTurn`, `StopToolUse`); harness already branches on `comp.StopReason != model.StopToolUse` at `:155`. |
| A8 | No in-flight work / drift in this area | Confirmed | clean tree at HEAD; last relevant commit `165290b` (fix-01 progress) landed the `tool.result`/activity path this fix must compose with. |

## Spec cross-validation

- **R-AGT-2 / `AGENTS.md:56–57`** (plain-language `userText` authored at emit time in the harness, never client-side) → **satisfied**: step 1 keeps authorship server-side; it *removes* a duplicate emission, it does not move logic to the client. Reflected in step 1 (server) + step 3 (no client change).
- **`docs/components.md:93`** (harness "emits plain-language `userText` on every user-facing event") → still honored: `run.completed` remains the user-facing completion event with text; the *final* `assistant.message` is a context-carrying turn, not a distinct user-facing message (its text is the completion message, now delivered once via `run.completed`). No user-facing event loses its text.
- **Invariant: events are the only interaction state (`AGENTS.md:53–55`)** → step 1 changes one cosmetic `UserText` value; no new state, no `Detail`/log-shape change; `reconstruct` unaffected (A3).
- **fix-01 composition** (`docs/plans/fix-01-build-progress.md` S3/S4) → the `tool.result` activity line and its clearing on `run.completed`/`run.failed` are untouched; this fix only blanks the final `assistant.message.UserText`. No overlap.

## Project conventions to follow

- **[`AGENTS.md:56–57`]** userText authored at emit time in the harness, never client-side. → **Reflected in plan:** fix is in `harness.go` (step 1); client untouched (step 3).
- **[`AGENTS.md:51–52`]** Contracts are tests; harness/eventlog changes keep conformance green. → **Reflected in plan:** step 2 adds a co-located `harness_test.go` case; step 4 runs `go test ./...` incl. e2e.
- **[`AGENTS.md:66–72`]** Web client is a thin consumer; `src/api.ts` sole surface; build → `internal/webui/dist`; vitest. → **Reflected in plan:** no `web/` source changes, so no rebuild/embed needed (step 3–4); this is why the fix is deliberately server-side and leaves `dist` untouched.
- **[`AGENTS.md:59–62`]** tenant-scoping / new-route rules → **Doesn't constrain this plan:** no new routes, no tenant surface touched.
- **Migrations / ports / async machinery** → **Doesn't constrain this plan:** no schema, no ports, no background-work changes — a single event-field value.
- **[sibling reuse — `harness.go:155`]** The harness already branches on `comp.StopReason`; reuse that discriminator rather than inventing a "final turn" flag. → **Reflected in plan:** step 1 computes `final` from the existing condition.

## Codebase conflict sweep

- **In-flight work:** none — clean tree; fix-01 (progress) just landed and is the thing to compose with (done, no overlap).
- **Shadow duplication:** the completion text has exactly two emission sites (final `assistant.message`, `run.completed`); step 1 removes the redundant one. No third site.
- **Caller-side drift of `run.completed.userText`:** enumerated — `app.ts:84–86` (client, unchanged), `cmd/creo/main.go:442–444` (CLI `watch` prints any event's `UserText` — still populated), `scripts/demo-m0.sh:56` (reads `run.completed` `userText` — still populated), Go tests `harness_test.go:117–119` (asserts `== finalText`, still true), `e2e_test.go:208/268/277`, `submit_test.go:32`, `webui_test.go:50–51`, `hostile_test.go:192`, `helpers_test.go:95` (all *count* `run.completed`, none read/omit its text). **None break** — the fix does not touch `run.completed`.
- **Consumers of `assistant.message.userText`:** only the client (`app.ts:80–82`) and the CLI `watch` (prints it when non-empty). Blanking the *final* one means CLI `watch` shows the completion line once (via `run.completed`) instead of twice — an improvement, not a regression. Working-turn commentary is unaffected.
- **Test-infra:** harness unit tests, e2e, and vitest all exist for this area — extend, don't create.

## Counterfactual — **Confirmed.**

- **S1** (message once) → step 1 removes the duplicate emission; step 2 locks it.
- **S2** (no progress regression) → fix-01 `tool.result`/`#activity` path untouched (step 1 scope; sweep confirms no overlap).
- **S3** (other consumers intact) → `run.completed` unchanged; caller sweep shows all green.
- **S4** (context/resume unchanged) → `Detail.Blocks` untouched; `reconstruct` reads only `Detail` (A3).
- **S5** (suites green) → step 2 + step 4; no `web/` change so `dist` stays valid.

## Risks & mitigations

- **A working turn legitimately ends with `StopEndTurn` but earlier had tools** — not a risk: `final` is per-turn; only the turn that actually stops (no tool calls) is blanked. Working turns are `StopToolUse` and keep their text.
- **A future non-reference client that relied on the final `assistant.message` carrying text** — mitigated: `run.completed.userText` is the documented completion surface and remains authoritative; clients should render that. Called out here so the contract is explicit.
- **Someone re-adds a client-side render of `run.completed`** expecting it to be textless — mitigated by A6 (the `run.completed.UserText == finalText` contract is intentional and tested).
- **Fallback if the server fix is rejected:** client-side dedup — in `app.ts` `run.completed` case, skip `addMessage` if the last `.msg` bubble's text equals `e.userText`. Documented as inferior (fragile content-match; still ships redundant bytes) and not chosen.

## Out of scope

Hydration user-message gap (Finding 3), raw-error translation (Finding 5), graceful-shutdown resume (Finding 4), activity-line visual redesign, publish/rollback UI, any `web/` rebuild. Each is its own change.

## Open questions

None blocking. One judgment call to confirm: **is the final `assistant.message` allowed to carry empty `userText`?** The plan says yes — its role on the final turn is to carry `Blocks` for context/resume, while the user-facing completion text belongs to `run.completed`. If the user instead wants the final text on `assistant.message` and `run.completed` to be a *silent* lifecycle event, that's the mirror-image fix (blank `run.completed.userText` in the normal case, keep it for step-limit) — more invasive (breaks `harness_test.go:117–119` and the CLI/demo completion line) and not recommended.

## Test plan

- **Go unit (`internal/harness`):** new `harness_test.go` case (step 2) — after a `fake:site` run, last `EvAssistant.UserText == ""`, `run.completed.UserText == finalText`, and exactly one logged event carries `finalText` as `UserText`. Keep the existing `run.completed == finalText` assertion green.
- **Go unit (resume):** `TestResumeWithPendingTools` still passes (counts assistants, not their text) — confirms context/resume unaffected.
- **Go e2e/regression:** full suite green, especially the `run.completed`-count tests (`e2e_test.go`, `webui_test.go`, `submit_test.go`, `hostile_test.go`) and the kill-resume test.
- **vitest (`web/src`):** unchanged; `app.test.ts:42–50` (completion → one bubble) stays green with no client edit. Optional: add a case asserting an `assistant.message` with empty `userText` produces no bubble (already implied by the `if (e.userText)` guard).
- **Manual (browser):** run a real build; confirm the final "…is ready!" bubble appears once, the `#activity` line still updates during the build and clears on completion, and the preview refreshes.
