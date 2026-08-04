# Creo — Solution Options

**Status:** Draft v0.1 · 2026-08-03 · companion to `PRD.md` v0.2
**Update (later same day):** superseded in part by `docs/architecture.md` — the SQLite laptop variant mentioned below was dropped in favor of Postgres-only across all profiles, and the harness-strategy decision (custom loop) is now recorded there. Rankings otherwise stand.
**Scope:** candidate end-to-end technical solutions for the core platform. Each option is a coherent stack answering PRD open questions #1–#3 together (session store, harness strategy, sandbox tech, runtime). Rationale and assumptions listed per option; assumptions marked **[verify]** are load-bearing and not yet validated.

## Decisions common to all options

These fall out of the PRD regardless of stack, so they are not differentiators:

- **Event log on Postgres** (SQLite-backed variant for the Laptop profile behind the same repository interface). An append-only log with range queries needs nothing exotic; a dedicated event-store dependency (EventStoreDB, Kafka) fails R-NFR-3 (laptop footprint) for no gain at our write rates.
- **Sandboxes are OCI containers at T1/T2** (Docker/Podman), hardened runtimes (gVisor/Kata) at T3. No option changes this; they differ only in who *talks* to the sandbox.
- **Provider access = Anthropic adapter + OpenAI-compatible adapter**, since OpenAI-compat is the de-facto protocol of local servers (vLLM, llama.cpp server, Ollama, LM Studio).
- **Client protocol = HTTP + SSE** (cursor-based event backfill per R-SES-4). WebSocket optional later.

---

## Option A — Go modular monolith, custom harness ("boring core") · **RECOMMENDED**

**Stack:** single Go binary containing gateway, control plane, and harness workers as internal modules; Postgres/SQLite event log; hand-written agent loop and provider adapters; Docker API client for sandboxes. Scale-out later = same binary launched in role modes (`creo gateway`, `creo harness-pool`).

**Rationale**
- The self-host story *is* the product's wedge, and Go is the best-in-class fit: one static cross-compiled binary, <1 GB idle is realistic, `creo up` is genuinely one command. Every other option fights R-NFR-3; this one gets it free.
- The harness and event schema are the core IP (the open-core boundary says the harness is the OSS core). Owning the loop means our event semantics — runs, approvals, semantic errors (R-AGT-2) — are first-class, not translated out of someone else's transcript format.
- The managed-agents article's own warning applies: embedded harnesses encode assumptions that rot as models improve. A loop we own is a loop we can re-derive.
- Modular monolith defers distribution until T3 actually exists, while the module boundaries (gateway/control/harness/sandbox interfaces) keep the Cluster profile honest.

**Assumptions**
1. You're productive in Go (or willing to be) — the stack has no fallback ergonomics if not.
2. Building a competent agent loop (tool-use, streaming, retries, context compaction, prompt caching) is weeks of work, not months, given how much of the hard part is now well-documented practice. **[verify — this is the option's biggest bet]**
3. Docker Desktop (or Podman) as a Laptop-profile dependency on macOS/Windows is acceptable for v1 (PRD open question #3 resolved as "yes, for now").
4. Two storage backends (Postgres + SQLite) behind one interface stay maintainable because the access pattern is narrow (append, range-read, cursor-tail).

**Main risk:** slowest of the five to a *good-looking* vertical demo — harness quality is earned, not imported. Mitigation: M0 scope is deliberately tiny (one sandbox, one adapter, CLI only).

**Best when:** the 5–10-year platform matters more than the 3-month demo. That is what the PRD says.

---

## Option B — TypeScript core embedding an agent SDK ("speed-to-market")

**Stack:** Node/TS services; the harness worker embeds an existing agent SDK (Claude Agent SDK, or Vercel AI SDK for provider breadth) and wraps it with our event log; Postgres; Docker for sandboxes; shared TS types with the reference web client.

**Rationale**
- Fastest path to a working vertical: the SDK brings the loop, streaming, tool dispatch, and retry logic on day one, and the reference web client shares the language and types.
- The Claude Agent SDK is the productized descendant of exactly the architecture the PRD is modeled on — least impedance mismatch of any embed option.

**Assumptions
**
1. The SDK runs fully headless and exposes interceptable, machine-readable events rich enough to become *our* canonical log rather than a lossy transcript. **[verify]**
2. Provider-agnosticism (R-LLM-1) survives the embed: the Claude Agent SDK is Anthropic-centric; covering OpenAI-compat/local models likely means a bridging proxy (LiteLLM-class) or the Vercel AI SDK instead — each with its own coupling. **[verify]**
3. One Node harness process per active session fits the laptop RAM budget once idle-parking works. **[verify]**
4. SDK licensing permits redistribution inside an OSS core. **[verify — Claude Agent SDK is not open source; this may force the AI SDK path by itself]**

**Main risk:** abstraction inversion — the platform's core semantics end up defined by SDK internals we don't control, and "no secrets in sandboxes" plus our run/approval model must be retrofitted around a loop that wasn't designed for them. Exactly the assumption-rot the source article warns about, imported voluntarily.

**Best when:** proving product demand fast matters more than owning the core, and a later harness rewrite is an acceptable cost of that proof.

---

## Option C — Elixir/OTP ("durability-native runtime")

**Stack:** Elixir + Phoenix; one supervised process per active session; Phoenix Channels/Presence for multi-device streaming; Postgres via Ecto; Oban for background jobs; ports/HTTP to Docker for sandboxes.

**Rationale**
- The BEAM is the only mainstream runtime whose native primitives (cheap supervised processes, crash-and-restart, distribution) are the PRD's P3 made executable. Session-per-process with a supervisor is idiomatic, not architecture.
- Multi-device live streaming (R-SES-2) is Phoenix's home turf.

**Assumptions**
1. Durable state still lives in Postgres — OTP supervision gives *recovery*, not *persistence*. The option is honest only if we don't double-count: BEAM saves us orchestration code, not the event log.
2. You accept owning a smaller-ecosystem language: no official Anthropic SDK (community libs + raw HTTP), thinner tooling, smaller hiring pool.
3. BEAM releases are an acceptable self-host artifact (heavier than a Go binary, still single-directory).

**Main risk:** the runtime's superpower overlaps heavily with what the event log must provide anyway (a crashed session must resume from *the log*, not from a supervisor restart, or multi-node T3 breaks). We'd pay the ecosystem cost for a benefit the architecture partially duplicates.

**Best when:** the team already loves Elixir. As a from-scratch choice for this PRD, charming but not compelling.

---

## Option D — Durable-execution engine (Temporal) under a Go/TS core

**Stack:** runs modeled as Temporal workflows (LLM calls and tool executions as activities); Go or TS workers; Temporal dev-server (single process, SQLite) on Laptop profile, clustered Temporal at T3; our own Postgres event log for the client-facing protocol.

**Rationale**
- R-SES-1, R-RUN-2, R-RUN-3 — durability, exactly-once triggering, deterministic recovery — are literally Temporal's product, battle-tested at brutal scale. We'd buy the hardest correctness work instead of building it.
- Idempotency keys, retries, heartbeats, and "run survives worker death" come as configuration, not code.

**Assumptions**
1. `temporal server start-dev` (or equivalent embedded mode) is light enough for the Laptop profile's <1 GB idle budget. **[verify]**
2. The dual event model is manageable: Temporal's internal history is *not* our user-facing log (wrong granularity, wrong retention, not a client protocol), so we maintain both and keep them consistent. This is the option's tax, paid forever.
3. Workflow determinism constraints coexist with interactive streaming LLM sessions (signals/queries handle interrupts and approvals without contortions). **[verify — interactive agents are not Temporal's classic shape]**
4. Operating Temporal at T3 is acceptable ops burden for future large operators.

**Main risk:** two sources of truth by construction, and a heavyweight dependency wired into the core's most identity-defining component. If the event log is the product's spine, outsourcing half its guarantees splits the spine.

**Best when:** the team is more afraid of distributed-systems correctness bugs than of dependency weight. A strong #2: if M0's crash-recovery demo proves harder than expected in Option A, this is the retreat position — and A's module boundaries make that retreat cheap.

---

## Option E — Thin control plane wrapping an existing OSS coding agent

**Stack:** minimal control plane (any language) that provisions a sandbox per project and runs an existing open agent (OpenHands, opencode, or similar) inside/beside it; a translator turns the agent's transcript into our event log; previews and publish handled by the platform.

**Rationale**
- Maximum leverage: agent capability is inherited and improves upstream for free; the believable demo arrives in weeks.
- Honest prior art: several commercial app-builders started exactly this way.

**Assumptions**
1. The chosen agent runs truly headless with a machine-readable event stream stable enough to build a product protocol on. **[verify per agent]**
2. Licensing is compatible with the open-core plan (OpenHands and opencode are MIT-family; Claude Code is *not* OSS and is excluded as an embed). **[verify current licenses]**
3. The agent's credential model can be inverted: these tools expect API keys in their environment — inside our sandbox — which violates §5's "no secrets in sandboxes, ever." An egress LLM-proxy that injects auth outside the sandbox must be workable. **[verify — this is the dealbreaker candidate]**
4. Upstream roadmap alignment: their harness assumptions (developer-facing, code-visible, terminal-native) can be suppressed hard enough to meet P1 (no-code) without forking.

**Main risk:** this is "vertical first, extract core later" wearing a trench coat — the strategy you already rejected. The core's value hollows out; the no-code translation layer fights the embedded agent's developer-facing grain forever; and the security model bends around a tool that wasn't built for hostile multi-tenancy.

**Best when:** validating demand in weeks with explicit intent to throw it away. As the actual platform: no.

---

## Comparison

| | A: Go monolith | B: TS + SDK | C: Elixir | D: Temporal | E: Wrap OSS agent |
|---|---|---|---|---|---|
| Time to M0 (spine) | ●●○ | ●●● | ●●○ | ●●○ | ●●● |
| Time to M3 (vertical) | ●○○ | ●●● | ●○○ | ●●○ | ●●● |
| Laptop self-host fit (R-NFR-3) | ●●● | ●●○ | ●●○ | ●○○ | ●○○ |
| Own the core IP (harness + log) | ●●● | ●○○ | ●●● | ●●○ | ○○○ |
| Durability engineering risk | ●○○ borne by us | ●●○ | ●●○ | ●●● solved | ●○○ |
| Provider-agnosticism (R-LLM) | ●●● | ●○○ | ●●● | ●●● | ●○○ |
| Security model fit (§5) | ●●● | ●●○ | ●●● | ●●● | ●○○ |
| Long-term assumption rot | low | high | low | medium | high |

## Recommendation

**Option A**, with two hedges taken from the others:

1. **From D:** design the run state machine as if it were a workflow engine's — explicit states, idempotency keys, resumable steps — so that if hand-rolled durability proves harder than assumption A2 predicts, swapping Temporal in under the run executor is a module replacement, not a rewrite.
2. **From B/E:** before committing, spend a timeboxed week validating assumption A2 by building the M0 harness loop spike (one tool, one adapter, kill-and-resume). If the spike says "months, not weeks," re-rank with D second and B third.

Ranking: **A > D > B > C > E.**

The deciding logic: the PRD's two hardest promises are the laptop-to-cluster self-host story and the durable event log as the product's spine. A is the only option that serves both without a standing tax; D buys durability at the cost of the spine's unity; B and E buy speed by mortgaging the core; C pays an ecosystem premium for benefits the log must provide anyway.
