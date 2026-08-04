# Fix-03 — Hydrate user messages (stop the user's own turns vanishing on reload / second device)

## Headline

Make the **event stream the single source of truth** for the user's own messages:
add a `case "user.message"` to `handleEvent` and **remove** the optimistic
`addMessage("you", text)` from `send()`. Each user turn then renders exactly once
— live (streamed back over the already-open SSE subscription) and after
reload/resume (replayed by `hydrate()` via `fetchEvents`) — with no double-render.

## Approach summary

- **Root cause (verified live):** `handleEvent` has no `user.message` case, so the
  `hydrate()` replay path drops every user turn. During a live session the user's
  text is only on screen because `send()` renders it optimistically — a path the
  reload flow never exercises. (`web/src/app.ts:67`, `:121`.)
- **The greeting survives reload because it is static HTML** (`web/index.html:55`),
  not an event — which is exactly why the post-reload transcript looks one-sided:
  static greeting + event-driven Creo replies, no user turns.
- **Fix shape:** render user turns from the log like every other event. This is
  the architecture's stated intent — "every device renders the same stream"
  (`docs/components.md:256`); "clients render, never interpret" (`:93`).
- **The double-render trap and its resolution:** if we *only* add the case and
  keep the optimistic add, a live send renders twice (once optimistically, once
  when the same `user.message` returns on the stream). We resolve it by deleting
  the optimistic add — leaving the stream as the sole render path. Validated: the
  sender's own `user.message` reliably comes back over SSE (backfill + exactly-once
  cursor dedup), so removing the optimistic render leaves no gap.
- **Composes with fix-01:** the user bubble is a `.msg` element; the build-progress
  `#activity` line is a separate self-replacing element — they don't interact.

## Goal / constraints / non-goals / success criteria

**Goal.** A resumed or second-device client reconstructs the full conversation
from the log, including the user's own messages (PRD **R-SES-2**, `PRD.md:156`).

**Constraints.**
- No server changes required (the server already appends + streams `user.message`).
- Must not regress fix-01's transient build-progress line.
- Existing vitest suite (8 tests, currently green) must stay green.
- Web client conventions: TypeScript + Vite + vitest/jsdom, npm, build into
  `internal/webui/dist` (`AGENTS.md:66-72`).

**Non-goals.**
- No new event types, no server/eventlog changes.
- No message-editing, retry, or per-message status UI.
- Not touching the CLI or other clients.
- Not changing the static greeting bubble.

**Success criteria (each mapped to a plan step in §"Counterfactual").**
1. User messages appear **exactly once** while live.
2. User messages appear after reload / second-device resume.
3. Ordering with Creo's replies is preserved.
4. No double-render on the live path.

## Hypothesis & its falsifier

**Hypothesis.** Adding `case "user.message": if (e.userText) addMessage("you", e.userText)`
to `handleEvent` (`web/src/app.ts:70`) and deleting the optimistic
`addMessage("you", text)` in `send()` (`:121`) renders each user turn exactly once
on both the live and hydrate paths, ordered correctly by `seq`, with no dedup logic.

**What would prove the *shape* wrong.** "If the sender's own `user.message` does
not reliably (and timely) come back over the stream, or comes back with different
text or out of order relative to Creo's replies, then relying solely on the event
is the wrong shape and we'd need an optimistic placeholder reconciled against the
stream." → **Falsified (shape holds).** Evidence in §Assumptions: the server
appends `user.message` with `UserText = text` and publishes it
(`internal/api/api.go:377,388`); the SSE subscription guarantees exactly-once
delivery from the cursor via backfill + seq-dedup (`internal/eventlog/eventlog.go:188-249`),
covering even the first-message race; `seq` ordering places the user turn before
the run's later events.

## Plan (ordered, concrete)

**Step 1 — Add the `user.message` render case.**
File: `web/src/app.ts`, in `handleEvent` `switch` (after the `case "run.started"`
block, around `:70-75`), add:

```ts
case "user.message":
  if (e.userText) addMessage("you", e.userText);
  break;
```

This is the only path that renders user turns after the change. The existing
early-return `if (e.seq <= state.lastSeq) return;` (`:68`) already guards against a
seq being processed twice.

**Step 2 — Remove the optimistic render from `send()`.**
File: `web/src/app.ts:121`. Delete the line `addMessage("you", text);`.
Keep `input.value = ""` and `setBuilding(true)` — they give immediate feedback
(input clears, send disables, status → "Creo is working…"). The message bubble now
arrives via the stream a round-trip later.

**Step 3 — Extend the vitest suite.**
File: `web/src/app.test.ts`. Add a `describe("user message rendering")` block with
the three tests in §Test plan.

**Step 4 — Rebuild & verify.**
- `cd web && npm test` — all tests green.
- `cd web && npm run build` — tsc + vite build succeeds (outputs to
  `internal/webui/dist` per `AGENTS.md:68`).
- Manual: see §Test plan "Manual".

## Assumptions validated (evidence)

- **[`internal/api/api.go:377,388`]** `submit` appends a `user.message` event with
  `UserText: text` inside the idempotent transaction, then `Publish`es it after
  commit. → The sender's own message *is* an event on the stream, carrying the same
  text. **Confirmed.**
- **[`internal/eventlog/eventlog.go:188-249`]** `Subscribe(after)` registers the
  live channel first, then reads backfill from `after` and streams the live tail,
  deduping by `seq` (`if e.Seq <= last { continue }`). → Exactly-once delivery from
  the cursor. Even if `send()` opens the subscription in the *same tick* as the POST
  (first-ever message, via `ensureProject` at `app.ts:113`), the just-committed
  `user.message` is caught by the backfill `Read`, so it is never missed and never
  doubled. **Confirmed.**
- **[`web/src/app.ts:113`, `:125`, `:145-154`]** On the first message `ensureProject`
  opens the stream *before* `sendMessage`; on later messages the subscription is
  already open (`unsub` persists). `hydrate()` replays the whole log via
  `fetchEvents(sid, 0)` then opens the tail from `state.lastSeq`. → Both live and
  resume paths flow through `handleEvent`, so one case fixes both. **Confirmed.**
- **[`internal/eventlog/eventlog.go:30`, `:157`, `:175`]** The `Event` JSON field is
  `userText` and `Read` selects `user_text`; the client `Event` interface exposes
  `userText` (`web/src/api.ts:11-16`). → `e.userText` is populated on replayed and
  streamed `user.message` events. **Confirmed.**
- **[`web/index.html:55`]** The greeting is a static `.msg.creo` element, not an
  event. → It never duplicates and is orthogonal to this change. **Confirmed.**
- **[Baseline]** `cd web && npm test` → 8 tests pass (2 files) at HEAD. **Confirmed.**

## Spec & architecture cross-validation

- **R-SES-2 (MUST)** `PRD.md:156` — second-device resume shows current session
  state. → Satisfied by Step 1 (user turns now reconstructed from the log).
- **R-SES-4 (MUST)** `PRD.md:158` — log consumable from a cursor (live tail +
  backfill), used by client reconnect. → The fix relies on exactly this; no change
  needed, confirms the seam.
- **SessionLog is the source of truth; clients render the stream**
  `docs/components.md:31,93,256`. → The fix *removes* a client-side render path that
  bypassed the stream, moving the client toward the documented contract. No
  invariant violated. Tenant scoping, idempotency, auth — untouched (no server
  change).

## Project conventions to follow

- **[`AGENTS.md:66-72`]** Web client is TypeScript + Vite + vitest (jsdom), package
  manager **npm**, Node 24; tests run with `cd web && npm test`; build with
  `cd web && npm run build` → `internal/webui/dist`. → **Reflected in plan:** Steps
  3-4 use `npm test` / `npm run build`; no other package manager assumed.
- **[`web/package.json` scripts]** `test` = `vitest run`, `build` = `tsc && vite build`.
  → **Reflected in plan:** Step 4 commands match verbatim.
- **[`web/src/app.test.ts:8-21`]** Test convention: co-located `*.test.ts`, build the
  minimal DOM in `setupDom()`, reset `state.lastSeq`/`state.projectId`, drive
  `handleEvent` directly (no network, no module mocks). → **Reflected in plan:** new
  tests follow this exact pattern (Step 3 / §Test plan).
- **[`AGENTS.md:25-28`]** `internal/webui/` embeds `web/dist`; the Go core never
  imports the web client. → **Doesn't constrain this plan** beyond confirming no Go
  change is needed; a `go build` re-embed is only required to ship the built assets,
  not to validate the fix.
- **Lint/format:** repo has no JS/TS linter config for `web/` (no eslint/biome
  config present); `tsc` in the build is the type gate. → **Doesn't constrain** —
  Step 4's `npm run build` runs `tsc` and catches type errors.
- **Migrations / async machinery:** not touched (no schema, no background work). →
  **Doesn't constrain this plan.**

## Codebase conflict sweep

- **In-flight work:** last web commits are `165290b` (fix-01 build progress) and the
  M3 client `0d9810a`. fix-01 added the `tool.result` → `#activity` line and the
  `run.completed` transcript bubble. The new `user.message` case appends an
  independent `.msg` element; `#activity` is a separate id-keyed element created by
  `showActivity`/removed by `clearActivity`. **No interference.** (`web/src/app.ts:37-54`.)
- **Shadow duplication:** the only other place user text was rendered is the
  optimistic `send()` line being deleted — this fix *removes* the duplication rather
  than adding a branch.
- **Caller-side drift:** no signature/schema/contract change; nothing else depends on
  the deleted line. `handleEvent` gains a case; its signature is unchanged.
- **Test-infra gap:** vitest for `handleEvent` already exists (`app.test.ts`); the
  live-`send()` path is *not* directly unit-tested today because `app.ts` holds a
  module-level `const api` that isn't injectable. See Open Questions / Test plan.

## Counterfactual — success criteria → plan steps

- **SC1 (exactly once, live):** Step 2 removes the second render path; Step 1 is the
  sole path. Test A + C. **Confirmed.**
- **SC2 (appears after reload/resume):** Step 1 renders replayed `user.message` from
  `hydrate()`/`fetchEvents`. Test B. **Confirmed.**
- **SC3 (ordering preserved):** `seq` ordering (user turn < run's later events),
  validated in Assumptions; Test B asserts transcript order. **Confirmed.**
- **SC4 (no double-render):** Steps 1+2 together; the `seq<=lastSeq` guard covers a
  duplicate delivery. Test C. **Confirmed.**

Overall: **Confirmed** (every criterion maps to a step + test).

## Risks & mitigations

- **Latency of the user's own echo.** The bubble now appears after a POST + SSE
  round-trip instead of instantly. On the T1/loopback deployment this is tens of ms,
  and input-clear + disabled send + "Creo is working…" give immediate feedback.
  *Mitigation / fallback:* if product wants an instant echo, use an **optimistic
  placeholder reconciled against the stream** — but there is no client-correlation id
  on the event today, so matching would rely on text+order (fragile). Recommend
  shipping the simple version first; revisit only if the latency is perceptible. See
  Open Questions.
- **SSE fails to connect → user turn never renders.** True, but in that state Creo's
  replies don't render either (whole app is degraded), and `send()`'s `catch` already
  surfaces the error. Not a new regression.
- **A future non-run `user.message` (e.g. a system-seeded turn).** Would now render;
  that is the correct behavior for a stream-sourced client.

## Out of scope

- Server / eventlog / schema changes.
- Optimistic placeholder + reconciliation (fallback only; see Risks).
- Per-message delivery/status indicators, edit, or retry.
- Refactoring `app.ts` for full `send()`-level testability beyond what Test C needs
  (unless the user wants the injectable-Api refactor — Open Questions).

## Open questions

1. **Instant echo vs. simplicity.** Is a ~round-trip delay before the user sees their
   own message acceptable on the target deployment? If not, we add the optimistic
   placeholder + reconciliation (more code, fragile matching without a correlation
   id). *Recommendation: ship the simple stream-sourced version; treat the placeholder
   as a follow-up only if latency is felt.*
2. **`send()`-level test.** `app.ts` uses a module-level `const api`, so unit-testing
   the live `send()` path needs a small refactor to inject the `Api` (or a mock). Do
   we want that refactor now, or is `handleEvent`-level coverage (Tests A-C) + the
   manual check sufficient for this fix?

## Test plan

**Unit (vitest, `web/src/app.test.ts`, following the existing `setupDom` pattern):**

- **Test A — renders a `user.message` exactly once.**
  `handleEvent(ev("user.message", "Build me a bakery site"))` → exactly one
  `.msg.you` element with that text; `querySelectorAll(".msg.you").length === 1`.
  *(Covers SC1.)*

- **Test B — reload/resume shows the user turn in order.**
  Replay a hydrate-like sequence through `handleEvent`:
  `user.message "Build me a bakery site"` → `run.started` → `run.completed "Your site is ready!"`.
  Assert the transcript `.msg` order is `["Build me a bakery site", "Your site is ready!"]`
  (user before Creo), `#activity` is gone (cleared on completion), and exactly one
  `.msg.you`. *(Covers SC2 + SC3, and composition with fix-01.)*

- **Test C — no double-render on duplicate delivery.**
  `handleEvent(ev("user.message","hi"))` then feed the **same seq** again
  (construct an event object reusing the prior `seq`) → still exactly one `.msg.you`
  (the `seq <= lastSeq` guard holds). *(Covers SC4 at the client; the server's
  exactly-once is belt-and-suspenders.)*

  *Note:* asserting the deleted optimistic path directly would require a
  `send()`-level test (blocked on Open Question 2). Tests A + C together prove the
  render-once contract given the stream is the only path.

**Build:** `cd web && npm run build` (tsc typecheck + vite build) succeeds.

**Manual (two-window smoke):**
1. `creo serve --insecure --web-dir …` (or `npm run dev` proxying to a running
   server). Send "Build me a bakery site" → the user bubble appears once, Creo
   builds, replies stream in.
2. Reload the page → the user bubble **and** Creo's replies are both present, in
   order (previously the user bubble vanished).
3. Open the same project URL in a second window (same token) → full conversation,
   including user turns, reconstructed. *(R-SES-2 acceptance.)*
