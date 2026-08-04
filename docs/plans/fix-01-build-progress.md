# Blueprint — Fix 1: no build progress ("dead air")

**Status:** validated plan, awaiting approval · 2026-08-04
**Headline:** Give the user continuous, plain-language progress during a build by populating the *already-emitted, already-persisted* `tool.result` events with a profile-authored phrase, and rendering it in the client as a transient activity line + a non-blank preview state — no new event types, no model-context or resume impact.

## Problem statement

- **Goal:** during a 60–140s build the user sees live, understandable progress ("Working on your home page", "Styling your site"), not a static "Creo is working…" over a blank white preview.
- **Root cause (verified live):** the harness emits `assistant.message` events whose `userText` is empty on tool-only turns (`joinText` of tool_use blocks = ""), and the client renders assistant/completion messages only `if (e.userText)`. The rich per-file `tool.result` stream carries no user-facing text, so the whole file-writing phase is invisible. (Observed: 17 events emitted server-side, 0 shown in the UI.)
- **Constraints:** progress *language is a platform responsibility, emitted as semantic events* — the client renders, never interprets file paths into English (PRD R-AGT-2 / `docs/components.md` §"AgentHarness"/§4.2). Must not pollute the model context or break log-first resume. Must not spam the transcript. No new durable event bloat beyond what already exists.
- **Non-goals:** create-vs-update precision in wording; a progress *bar*/percentage; streaming partial file content; fixing the other findings (duplicate completion msg, hydration user-message gap) — those are separate.
- **Success criteria:** (S1) during a build the client shows continuously-updating plain-language progress. (S2) the phrase is authored server-side (profile), not by client path-parsing. (S3) progress is transient — the final transcript has no per-step clutter — and is cleared on run.completed/failed. (S4) `reconstruct()` ignores it: model context and resume are unchanged. (S5) the preview pane shows a "building" state instead of blank white. (S6) existing Go + vitest suites stay green (esp. the hostile leak assertion and tool.result counts).

## Hypothesis (and its falsifier)

Populate `UserText` on the existing `tool.result` events with a plain-language phrase from a new `profile.Profile.ProgressPhrase(toolName, path) string` (vertical knowledge → profile). The client gains a `case "tool.result"` in `handleEvent` that updates a single self-replacing activity element (not a transcript append), shown from `run.started`/`run.resumed` and removed on `run.completed`/`run.failed`; plus a "building" placeholder in the preview iframe area. No new event type; `tool.result` is already emitted per batch and already persisted.

**Falsifier for the shape:** if `tool.result` events were only emitted at run end (not incrementally), populating them couldn't drive live progress and the fix would need a new streaming channel. **Refuted** — the harness appends `tool.result` per tool-execution batch mid-run (`harness.go:114–127`), and I observed 15+ arriving live during the build. Second falsifier: if `reconstruct()` fed `tool.result.userText` back into the model, this would corrupt context — **refuted**, `reconstruct` reads `tool.result` from `Detail` only and never touches `UserText` (`harness.go:248–254`).

## Ordered steps

1. **`internal/profile/profile.go` — add `ProgressPhrase(toolName, path string) string`.** Websites-vertical mapping, neutral verbs (correct for both first build and refine): `write_file` on `index.html` → "Working on your home page"; other `*.html` → "Working on your " + pretty(name) + " page" (strip dir + `.html`, replace `-`/`_` with space, title-case); `*.css` → "Styling your site"; `assets/*` or image extensions → "Adding images"; other → "Working on your site". `delete_file` → "Removing a page". `read_file`/`list_files`/unknown → "" (no progress shown for inspection). Lowercase "home" deliberately (see Risks — hostile canary is capital "Home"). Unit-tested.
2. **`internal/harness/harness.go` — set the phrase on successful tool results.** In the tool-execution loop (`~line 112–124`), parse the path from `call.ToolInput` (same shape `executeTool` already parses) and set `UserText: h.Profile.ProgressPhrase(call.ToolName, path)` on the `tool.result` `NewEvent` — **only when `!isErr`** (failed/blocked tools get no phrase; keeps escape-attempt errors silent and leak-free). `Detail` unchanged. No behavior change to `assistant.message`, `run.completed`, or `reconstruct`.
3. **`web/src/app.ts` — render transient progress.** Add `case "run.started"`/`"run.resumed"` → show the activity element with an initial "Getting started…". Add `case "tool.result"` → if `e.userText`, set the activity element's text (create-once, update-in-place; never `addMessage`). On `run.completed`/`run.failed` (existing block) → remove the activity element. Give it a subtle animated indicator (CSS ellipsis/pulse) so it reads as live. Extend the `Event` type in `web/src/api.ts` — `userText` already optional; no shape change.
4. **`web/index.html` — preview building state + activity styling.** Add a small `.activity` style (distinct from `.msg`, e.g. muted with an animated dot) and a neutral "building" placeholder shown in/над the preview iframe while a run is active (e.g. a centered "Building your site…" overlay cleared on `refreshPreview()`), replacing the blank white.
5. **Rebuild + tests.** `cd web && npm run build` (regenerates `internal/webui/dist`), `npm test`; `go test ./...`. Add a harness unit test asserting a successful `write_file index.html` result carries a non-empty phrase and a `read_file` result carries none; add a vitest asserting `handleEvent` on a `tool.result` with `userText` creates/updates the activity element and `run.completed` removes it.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | `tool.result` events are emitted incrementally per batch during a run | Confirmed | `harness.go:114–127` appends per batch inside the iteration loop; observed 15+ live during the browser build |
| A2 | The harness knows `toolName` + `path` at the emit site | Confirmed | loop var `call` has `ToolName`/`ToolInput`; `executeTool` (`harness.go`) already parses `{path}` from `ToolInput` |
| A3 | `reconstruct()` ignores `tool.result.userText` — no model-context/resume impact | Confirmed | `harness.go:248–254` unmarshals `Detail` only; switch has no `default`, unknown fields untouched |
| A4 | The SSE endpoint forwards every event type unfiltered | Confirmed | `api.go:449–455` marshals and writes every `e` from the subscription; no type filter |
| A5 | The client `handleEvent` is a `switch (e.type)` easily extended; `Event.userText` is optional | Confirmed | `web/src/app.ts` handleEvent; `web/src/api.ts` `Event { userText?: string }` |
| A6 | No test pins `tool.result.userText` to empty | Confirmed | grep: tests only *count* `tool.result` (`e2e_test.go:202`, `hostile_test.go:111`); none assert its userText |
| A7 | Hostile leak assertion keys on capital `"Home"`, not lowercase | Confirmed | `hostile_test.go:115–119` checks `Contains(ev.UserText, "Home")`; phrases use lowercase "home page" and only on success (escape attempts error → no phrase) |
| A8 | No in-flight work / drift | Confirmed | clean tree at HEAD (M3 + type-hierarchy commits) |

## Spec cross-validation

- **R-AGT-2** (progress/error language authored server-side, emitted as semantic events; clients render, never interpret) → satisfied by step 1–2 (phrase in the profile/harness, not the client). This is the load-bearing spec and the reason the fix is server-side, not a client path-parser.
- **R-AGT-3** (silent auto-retry; user sees only what needs them; no error codes) → step 2 emits phrases only on success, so failures/repairs stay silent — consistent.
- **Invariant: event log is the sole interaction state; nothing feeds the model that shouldn't** → step 2 adds only cosmetic `userText` to an existing event; `reconstruct` unaffected (A3). Resume/kill tests unaffected.
- **P1 (no code/jargon on the primary surface)** → phrases are plain language ("home page", not "index.html").

## Project conventions to follow

- **[`AGENTS.md` "progress/error language authored at emit time, never synthesized client-side"]** → steps 1–2 put the phrase server-side; step 3 only renders `e.userText`. → **Reflected in plan:** the whole server/client split.
- **[`AGENTS.md` "one authority per component"]** → "what a file means" is vertical knowledge, so it lives in `internal/profile` (step 1), not the harness or client.
- **[`AGENTS.md` "Web client … thin consumer … src/api.ts sole surface; build → internal/webui/dist; vitest"]** → step 3–5 touch only `web/`, rebuild into the embed dir, add a vitest.
- **[`AGENTS.md` "Contracts are tests" / co-located `*_test.go`]** → step 5 adds a harness unit test co-located; SL/RC/e2e stay green.
- **[`AGENTS.md` "Stdlib-first (Go)"]** → `ProgressPhrase` is pure `strings`; no new dep.
- **Ports/migrations/async** → `Doesn't constrain this plan` — no schema, no ports, no new routes; purely event-field + rendering.

## Codebase conflict sweep

In-flight: none (clean tree, single author). Shadow duplication: none — no existing progress code. Caller-side drift: `ProgressPhrase` is additive; setting `UserText` on `tool.result` changes a value that was empty and that no code reads except the client (new) — verified no reader in Go (`reconstruct` uses Detail). Test infra: harness unit tests + e2e + vitest already exist; extend, don't create.

## Counterfactual — **Confirmed.** S1→3, S2→1/2, S3→3, S4→2 (A3), S5→4, S6→5.

## Risks & mitigations

- **Hostile leak-test false positive** if a phrase contained the capital canary "Home" → phrases use lowercase "home page", and step 2 emits only on `!isErr` (escape attempts error). Re-run `TestHostileProjectContained` after (step 5).
- **Progress line flicker / spam** if rendered as transcript appends → step 3 uses a single update-in-place element, removed on terminal events. Not `addMessage`.
- **Batch of N tool results updates the line N times in one tick** → acceptable (arrives in ms; user sees the latest phrase); no throttle needed at this scale.
- **create-vs-update wording imprecision** ("Working on…" is neutral) → accepted, called out as non-goal.
- **Reference client is slated for redesign** → the *server-side* progress authoring (steps 1–2) is durable and reused by any future client; only the small render code (steps 3–4) is throwaway. Worth it for a working demo and the API contract.

## Out of scope

Duplicate-completion-message fix (Finding 2), hydration user-message gap (Finding 3), graceful-shutdown resume (Finding 4), raw-error translation (Finding 5), publish/rollback UI, version history UI, responsive layout, token-field UX. Each is its own change.

## Open questions

None blocking — the server-authored, reuse-`tool.result`-userText approach follows R-AGT-2 and avoids new log events. Proceeding on these defaults if approved.

## Test plan

- **Go unit (`internal/profile`):** `ProgressPhrase` maps write/delete to non-empty plain phrases, read/list/unknown to ""; `index.html` → lowercase "home page"; a derived page name title-cases.
- **Go unit (`internal/harness`):** a successful `write_file` run's `tool.result` events carry non-empty `userText`; inspection results carry none; `reconstruct` output unchanged (model messages identical with/without phrases).
- **Go e2e/regression:** full suite green, especially `TestHostileProjectContained` (leak) and the kill-resume/tool.result-count tests.
- **vitest (`web/src`):** `handleEvent({type:"tool.result", userText:"Working on your home page"})` creates/updates the activity element; `run.completed` removes it; a `tool.result` with empty userText does nothing.
- **Manual (browser):** real-model build shows the activity line updating through phases and the preview "building" state, then clears on completion — the exact dead-air scenario I observed, now fixed.
