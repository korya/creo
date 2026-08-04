# Creo — Product Requirements Document

**Status:** Draft v0.3 · 2026-08-03
**Owner:** Dmitri
**Decisions locked so far:** v1 = headless core + one vertical (simple websites) · open-source core, commercial verticals · build **and** publish is part of the core loop · **project state is code-canonical — the platform never enforces a semantic schema of the final artifact** (a "Wix with an LLM intent parser" architecture was evaluated and rejected as too limiting; structured *annotations* on top of generated code are being evaluated in spike-01)

**v0.3 changes:** recorded the code-canonical decision; folded validated mechanics from a third-agent proposal into `docs/architecture.md` (worker leases with generation fencing, transactional outbox, event-log projections, event envelope, model-gateway capability model, credential-broker mechanics); session-store recommendation updated to Postgres-only (open question #1); spike-01 defined in `docs/spikes/`.

**v0.2 changes:** merged the strongest material from a second-agent PRD after validation — runs + idempotency (§4.2), session state machine (§4.1), security trust tiers replacing the "identical isolation everywhere" claim (§5), server-side error translation with owner assigned (§4.3), source-of-truth ruling between event log and artifacts (§3.1), hard budget caps (§4.6), acceptance criteria (§10), child-safety placeholder (§2.3), expanded open questions.

---

## 1. Vision

Creo is a self-hostable platform that lets **people who cannot code build real software by describing what they want**. The actual building is done by an AI agent running on a server in an isolated environment; the user watches, steers, and approves from any device. A project is never "on someone's machine" — it lives on the platform, and any client (web, mobile, TUI, CLI) can attach to it and resume work.

Creo is two things at once:

1. **A headless agent platform (the core, OSS):** durable agent sessions, sandboxed execution, multi-tenant isolation, provider-agnostic LLM access, artifact publishing — exposed only through APIs and events. No UI opinions.
2. **Verticals (commercial):** opinionated products built on the core for a specific audience and artifact type. First vertical: **a simple-websites builder for non-coders** (frontend-only sites, not generic web apps). Later: a kids' web-game builder, and others.

### 1.1 Product principles

| # | Principle | Consequence |
|---|-----------|-------------|
| P1 | **No-code first** | The primary user never sees code, a terminal, or a stack trace. Every surface speaks in terms of *their* artifact ("your About page"), not implementation ("`about.html`"). Code visibility is a later, opt-in power feature — v1 is optimized for people who will never turn it on. |
| P2 | **Headless core** | If a feature can only be used from the web UI, it doesn't belong in the core. Everything the reference frontend does must go through the public API. This includes error translation and progress language (§4.3): the platform emits semantic, plain-language events; clients render, they never interpret. |
| P3 | **Sessions are durable, everything else is cattle** | The event log is the only interaction state that matters. Harness processes, sandboxes, and client connections can die at any time without losing work. |
| P4 | **Same isolation layers everywhere; documented strength per tier** | Every deployment profile has every isolation layer (§5.1) — none are optional. But a laptop container is not a hardened microVM, and the docs must never pretend otherwise. Each deployment profile maps to a documented **trust tier** (§5.3) stating which threat model it actually withstands. |
| P5 | **Provider-agnostic** | Any LLM behind a supported protocol works, including locally hosted models. No provider-specific behavior leaks above the provider adapter. |
| P6 | **Runs anywhere** | One person on a laptop with `creo up`, a small team on a single VM, or a large operator on Kubernetes — same core, different deployment profile. |
| P7 | **The system owns routine technical decisions** | Framework choice, dependency conflicts, build repair: the platform decides or repairs silently. The user is only asked questions that affect *their* outcome, phrased in their vocabulary ("Should visitors contact you through a form, phone, or both?" — never "serverless API route or client-side mail provider?"). |

### 1.2 Non-goals (v1)

- Generic web-app generation (backends, databases, auth for *user-built* apps). The first vertical is frontend-only sites.
- Code editing UI, git UI, or any "developer mode."
- Real-time multi-user collaboration on one project (co-editing). Multi-*device* for one user: yes. Multi-*user*: later.
- Marketplace/plugin ecosystem for third-party verticals (design for it, don't ship it).
- Fine-tuning or model hosting. Creo consumes LLMs; it does not serve them.
- Offline/disconnected operation. Local *models* are supported (P5); a fully air-gapped platform (offline package registries, no-TLS publish) is a separate future project, deliberately not a bullet point.
- Identical *capacity* across deployment profiles. Concepts and interfaces stay identical (P6); throughput and isolation strength are documented per tier, not equalized.

---

## 2. Users & personas

### 2.1 Core platform (developer-facing, OSS)

- **Vertical builder** — a developer (initially: us) building a product on the core. Needs: stable API, good events, local dev story, docs.
- **Self-hosting operator** — runs Creo for themselves, family, a school, or a company. Ranges from "Docker Compose on a NAS" to "platform team with a K8s cluster." Needs: simple install, upgrade path, backup story, resource limits, and an honest statement of which trust tier (§5.3) their setup provides.

### 2.2 First vertical: simple-websites builder (end-user-facing)

- **Primary: the non-coder site owner.** A freelancer, club organizer, small-business owner, hobbyist. Has content and intent ("I need a site for my bakery with a menu and opening hours"), zero interest in how it's made. Success = a live URL they're proud of, reachable by their customers, editable next month from their phone.
- **Secondary: the helper.** A more technical friend/relative who sets up self-hosted Creo for others. Interacts mostly with the operator surface.

**Key persona constraints that drive requirements:**
- They will close the laptop mid-build and come back on a phone. → resumability (R-SES-\*), duplicate suppression (R-RUN-\*)
- They will say "make it prettier," not "increase whitespace and use a serif." → the vertical owns prompt scaffolding and taste, not the user.
- They will never debug. → every failure must resolve into either automatic recovery or a plain-language choice (R-AGT-3).

### 2.3 Future vertical: kids' game builder (placeholder, not v1)

Recorded now because it constrains core design: age-appropriate language, guardian controls, safe-content enforcement, and strong external-access restrictions must be expressible as a **vertical profile** (R-AGT-4) — enforced by the platform, not merely described to the model. This vertical drags in COPPA-class legal obligations (parental consent, data minimization for minors); the core's tenant-deletion and telemetry controls (R-TEN-4, R-SEC-4) are prerequisites, and a dedicated compliance review gates that vertical, not this PRD.

---

## 3. Architecture

Creo adopts the managed-agents decomposition (per Anthropic's engineering write-up): **brain / hands / session** as three independently virtualized layers, connected by a control plane.

```
┌────────────┐   ┌────────────┐   ┌────────────┐   ┌───────────┐
│  Web app   │   │ Mobile/TUI │   │    CLI     │   │ Vertical  │  ← clients (thin)
└─────┬──────┘   └─────┬──────┘   └─────┬──────┘   └─────┬─────┘
      └────────────────┴───────┬────────┴────────────────┘
                               ▼
                    ┌─────────────────────┐
                    │   Gateway / API      │  authn, authz, tenancy, rate limits
                    └──────────┬──────────┘
                               ▼
                    ┌─────────────────────┐
                    │   Control plane      │  projects, sessions, runs, quotas
                    └───┬──────────┬──────┘
              wake()    │          │ provision()
                        ▼          ▼
              ┌──────────────┐   ┌──────────────┐
              │ Harness pool  │──▶│ Sandbox pool │  execute(name, input)
              │  ("brains")   │   │  ("hands")   │
              └──────┬───────┘   └──────┬───────┘
        emitEvent()  │                  │ artifacts
                     ▼                  ▼
              ┌──────────────┐   ┌──────────────┐
              │ Session store │   │ Artifact +    │
              │ (event log)   │   │ publish store │
              └──────────────┘   └──────────────┘
                     ▲
                     │ LLM calls via
              ┌──────┴───────┐
              │ Provider     │  Anthropic / OpenAI-compat / Ollama / …
              │ adapters     │
              └──────────────┘
```

### 3.1 Session store (the source of truth for interaction)

- Append-only, per-session event log stored outside harness and sandbox. Events: user messages, model turns, tool calls/results, run lifecycle markers, artifact-version references, approval requests/responses, errors and repairs.
- **The event log is the resume mechanism.** `wake(sessionId)` boots a fresh harness that reconstructs context via `getEvents(sessionId, range)`. Nothing critical ever lives only in a process.
- Events are the client protocol too: every client renders by subscribing to the same log (live tail + backfill from a cursor position), so web/mobile/CLI trivially show identical state.
- **Authority ruling (the sentence both draft PRDs were missing):** the event log is authoritative for *session and interaction* state; artifact versions (§3.6) are authoritative for *project* state; and every artifact version must be traceable to the event that produced it. On crash recovery, the log determines where the conversation resumes; the newest referenced artifact version determines what the project *is*. A sandbox workspace is never authoritative for anything.
- **Code-canonical, annotations derived (locked v0.3):** the artifact is generated code; the platform never requires the project to conform to a semantic schema of the final result (no fixed section taxonomy, no materializer as source of truth). Verticals MAY maintain derived structured annotations over the artifact — stable node IDs for click-to-edit, a content index for instant token-free text/image edits — but annotations are always derived *from* the artifact, never the reverse. How much annotation the websites vertical needs is the subject of spike-01.
- Two layers with distinct retention: the **transcript** (full fidelity, for resume/audit, tenant-scoped, exportable/deletable for GDPR) and derived **checkpoints** (compressed context snapshots so resume doesn't replay everything).

### 3.2 Harness ("brain")

- Stateless agent loop: pull context from session store → call LLM via provider adapter → emit events → request tool execution in a sandbox. Crash-safe by construction (P3).
- Headless by definition — no rendering, no client awareness. It emits semantic events; clients decide presentation.
- Harness behavior (system prompts, tool palette, guardrails, artifact vocabulary) is configured per **vertical profile** — the websites vertical ships a profile; the core ships a generic default.
- Work within a session is organized into **runs** (§4.2): a run is one triggered unit of agent work with its own lifecycle, recorded in the log. At most one authoritative run is active per session.

### 3.3 Sandbox ("hands")

- On-demand isolated execution environment per project: filesystem workspace, constrained toolchain, **no ambient credentials** (see §5), egress controlled by policy.
- Cattle: killable at any time; workspace state that matters is snapshotted to the artifact store; a dead sandbox is a tool-call error the harness recovers from.
- Implementation is profile-dependent (see §7): containers on laptop/VM profiles; stronger options (gVisor/microVM/Kata) on the K8s profile. The interface (`provision`, `execute`, `snapshot`, `destroy`) is identical.

### 3.4 Control plane

- Owns tenants, users, projects, sessions, runs, quotas, and scheduling (which harness wakes, which sandbox pool serves it).
- Enforces the tenancy model (§5) at the API boundary — isolation is not left to lower layers to "also check."
- Enforces run idempotency (R-RUN-2): duplicate triggers are deduplicated here, before any harness wakes.

### 3.5 Provider adapters

- One interface: chat/completion with tool use + streaming. Adapters for Anthropic API, OpenAI-compatible endpoints (which covers most hosted and local servers: vLLM, LM Studio, llama.cpp server, Ollama's OpenAI mode), and room for native adapters where the compat layer loses capability.
- Per-tenant and per-project provider config: bring-your-own-key, or operator-provided pool with per-tenant metering and hard budget caps (R-LLM-5). Model capability tiers declared per adapter so verticals can require (e.g.) tool-use support.
- **Reality check (stated, not hidden):** the *interface* is provider-agnostic; the *product quality* is not. The websites vertical will be tuned against 1–2 reference models; others are supported-but-not-tuned. Operators can restrict which providers/models are available to their tenants.

### 3.6 Artifact & publish pipeline

- Sandbox builds produce **artifacts** (for the websites vertical: a static bundle). Artifacts are content-addressed, versioned, and stored per project. Artifact versions are the authoritative project state (§3.1); each records the producing run/event for traceability.
- **Preview:** every artifact version is instantly viewable at a preview URL (sandboxed origin, not the public site), access-protected, revocable, and stable across sandbox replacement — the preview belongs to the project, not to a running environment.
- **Publish:** one explicit user action promotes an artifact version to the live URL. Publishing is a core primitive with pluggable targets: built-in serving (default; the platform serves `site-name.<operator-domain>` with TLS), plus adapters (custom domain, S3-compatible, Netlify-style) later.
- **Rollback** = promote a previous version. Instant, always available. "Restore the version from this morning" — never "revert the commit."

---

## 4. Functional requirements

Grouped, with IDs for traceability. **MUST** = v1, **SHOULD** = v1 if cheap, **LATER** = designed-for but not built.

### 4.1 Sessions & resumability (R-SES)

- **R-SES-1 (MUST):** A project's session survives harness crash, sandbox death, server restart, and client disconnect with zero user-visible data loss up to the last committed event.
- **R-SES-2 (MUST):** A user can open a project from a second device and see the current session state (live if active, last state if idle) within seconds; continuing from either device is seamless. Pending questions and approvals are answerable from any authorized device.
- **R-SES-3 (MUST):** Sessions idle-park automatically (harness and sandbox released) and wake on demand; cold-wake p95 fast enough that a returning user doesn't perceive "booting" (target: first response streaming < 10 s at wake, thanks to log-first resume — inference can start before the sandbox is warm).
- **R-SES-4 (MUST):** Event log is queryable by range/type and consumable from a cursor position (live tail + backfill) — used by harness resume, client reconnect, and export.
- **R-SES-5 (MUST):** Sessions expose an explicit state machine to clients: `idle · queued · working · waiting-for-input · waiting-for-approval · recovering · failed`. Clients render state; they never infer it from event patterns.
- **R-SES-6 (SHOULD):** Checkpoint/compaction so long-lived projects don't pay linear replay cost on wake.
- **R-SES-7 (LATER):** Session branching (try a direction, discard it) — the append-only log makes this natural; expose it when a vertical needs "undo an idea," which the websites vertical will want early.

### 4.2 Runs & idempotency (R-RUN)

> 💡 A **run** is one triggered unit of agent work inside a session — "user asked for X, agent worked, run ended in a result." Sessions are the conversation; runs are the discrete jobs within it.

- **R-RUN-1 (MUST):** Every unit of agent work is a run recorded in the event log with: trigger, session/project, model configuration used, capabilities available, status, outcome, and produced artifact versions. Runs are the unit of cost attribution and audit.
- **R-RUN-2 (MUST):** At most one authoritative run is active per session. Duplicate triggers — double-tap, impatient retry, two devices submitting at once, client retry after network timeout — must not create duplicate authoritative work. Deduplication happens in the control plane via client-supplied idempotency keys, not by hoping clients behave.
- **R-RUN-3 (MUST):** A run interrupted by any infrastructure failure ends in a deterministic state (`recovering` → resumed, or `failed` with prior work intact); it never silently vanishes or half-applies.
- **R-RUN-4 (MUST):** The user can cancel the active run; cancellation is itself an event, and the project state remains the last committed artifact version.
- **R-RUN-5 (MUST):** Run execution is guarded by time-limited worker leases with **generation fencing**: a worker whose lease has been superseded cannot commit further authoritative events, ever — takeover after a crash is safe by construction, not by timing luck. Mechanics in `docs/architecture.md` §4.

### 4.3 Agent & interaction (R-AGT)

- **R-AGT-1 (MUST):** Interaction is a conversation plus a live artifact view; the user can interrupt/redirect mid-task ("stop — the header should be green").
- **R-AGT-2 (MUST):** **Error translation and progress language are platform responsibilities, emitted as semantic events — never client-side interpretation.** The harness/vertical profile translates technical reality into user vocabulary at emit time ("The preview could not start. I'm repairing it." — not a Vite stack trace). Raw technical detail is retained on the event for diagnostics/operators, but the default payload every client renders is the plain-language one. This is what keeps N clients from drifting into N translations.
- **R-AGT-3 (MUST):** Failures surface as plain-language, actionable states with at most one decision for the user ("I couldn't load that image — use a different one, or skip it?"). Silent auto-retry first; user only sees what needs them. No error codes, ever, on the primary surface.
- **R-AGT-4 (MUST):** Vertical profiles define the system prompt, tool palette, artifact vocabulary, required clarifying questions, and content restrictions; the platform enforces profile restrictions structurally (sandbox policy, tool availability) — restrictions are never merely described to the model.
- **R-AGT-5 (MUST):** Approvals are durable events phrased in user terms and consequences ("This will replace your published site. Continue?"), answerable from any device, and blocking the run in `waiting-for-approval` until resolved. Approval copy never promises side effects the platform doesn't perform.
- **R-AGT-6 (SHOULD):** Structured "intent" shortcuts alongside free text (click an element in the preview → "change this"), because pointing beats describing for non-coders.
- **R-AGT-7 (LATER):** Multi-agent orchestration inside a session (many hands, one brain). The interface (`execute` against a named sandbox) should not preclude it.

### 4.4 Projects & tenancy (R-TEN)

- **R-TEN-1 (MUST):** Tenant → users → projects hierarchy. Every API object and every stored byte is tenant-scoped; cross-tenant access is structurally impossible at the API layer (scoped tokens, scoped queries), not merely forbidden by checks.
- **R-TEN-2 (MUST):** Projects within a tenant are mutually isolated at the sandbox and artifact level (a compromised project sandbox cannot read a sibling project's workspace).
- **R-TEN-3 (MUST):** Per-tenant quotas: concurrent active sessions, sandbox CPU/RAM/disk, token spend, artifact storage, egress. Sane defaults in every profile, including the laptop.
- **R-TEN-4 (MUST):** Tenant-level data deletion (projects, transcripts, artifacts) and tenant suspension.
- **R-TEN-5 (SHOULD):** Tenant export: full takeout of projects, transcripts, artifacts in open formats. For the websites vertical, project export (the static bundle) is near-free and ships v1 — the anti-lock-in guarantee costs us a zip file.

### 4.5 Headless API & clients (R-API)

- **R-API-1 (MUST):** 100% of user-facing functionality available via documented HTTP API + event stream (SSE or WebSocket): auth, projects, sessions, runs, events, approvals, previews, versions, publish, export. The reference web client uses only this API (P2 enforcement mechanism: no private endpoints).
- **R-API-2 (MUST):** A CLI client ships with the core — it is the proof of headlessness and the operator's tool.
- **R-API-3 (MUST):** Auth: sessions for humans (email or OIDC, profile-dependent), scoped API tokens for programmatic clients.
- **R-API-4 (SHOULD):** Official client SDK (TypeScript first) generated from the API spec.

### 4.6 LLM providers (R-LLM)

- **R-LLM-1 (MUST):** Anthropic adapter + OpenAI-compatible adapter (covers most local servers).
- **R-LLM-2 (MUST):** Operator- and tenant-level provider/key configuration; keys never enter sandboxes or client payloads (§5). Operators can restrict available providers/models per tenant.
- **R-LLM-3 (MUST):** Per-run and per-session token/cost metering, exposed to operator and (in plain terms) to the tenant.
- **R-LLM-4 (SHOULD):** Model capability declaration + graceful degradation messaging when a configured model can't support a vertical's requirements — phrased per R-AGT-2, without model jargon.
- **R-LLM-5 (MUST):** Hard budget caps, not just metering: per-tenant (and optionally per-project) spend limits that stop new runs at the limit with a plain-language explanation. The self-hosting parent and the commercial operator both need "it cannot cost more than X" as a guarantee, not a dashboard.

### 4.7 Publishing (R-PUB)

- **R-PUB-1 (MUST):** One-click publish of an artifact version to a live HTTPS URL served by the platform; publish/rollback both instant.
- **R-PUB-2 (MUST):** Preview URLs for unpublished versions: access-protected to the project's users, revocable, stable across sandbox replacement.
- **R-PUB-3 (MUST):** Published-site serving is isolated from the platform (separate origin/domain, static-only for the websites vertical — no path from a served site into the control plane; preview and published content never share origin or credentials with the trusted product UI).
- **R-PUB-4 (SHOULD):** Custom-domain support with automated TLS.
- **R-PUB-5 (LATER):** Publish adapters for third-party targets (S3-compatible, Netlify-style).

### 4.8 First vertical: simple-websites builder (R-WEB)

- **R-WEB-1 (MUST):** From blank state to a published multi-page static site purely through conversation + choices (template/style pickers are choices, not code).
- **R-WEB-2 (MUST):** Artifact type is a constrained static bundle (HTML/CSS/JS/assets, no server code) — enforced by the vertical's sandbox policy; this constraint is what makes R-PUB-3's static-only serving guarantee possible.
- **R-WEB-3 (MUST):** Core loop instrumented end-to-end: describe → see preview → refine → publish. Target: first meaningful preview < 5 min from project creation; publish < 30 s.
- **R-WEB-4 (MUST):** Asset handling: user uploads images/logos from any device; the agent uses them. Upload goes through the API to the artifact store, never "into the chat."
- **R-WEB-5 (MUST):** Version restore in user language ("go back to how it was this morning") — powered by artifact versions, with no commits/branches/patches surfacing anywhere.
- **R-WEB-6 (SHOULD):** "Edit by pointing" (R-AGT-6) scoped to site elements.
- **R-WEB-7 (LATER):** Forms/contact blocks via platform-provided endpoints (the first controlled crack in "static-only," designed deliberately, not accidentally).

---

## 5. Security model

Threat model in one line: **assume everything that touches a project is attacker-influenced — user prompts, uploaded files, generated code, packages, model output, preview content — and the platform must stay safe anyway.**

### 5.1 Isolation layers

Present in **every** deployment profile (P4); strength varies by tier (§5.3), presence never does.

| Boundary | Mechanism | Guarantee |
|---|---|---|
| Tenant ↔ tenant | Scoped identity in every token; row/prefix-level scoping in session, artifact, and object stores; per-tenant encryption keys (SHOULD) | No API call, query, or storage path can name another tenant's data |
| Project ↔ project (same tenant) | Separate sandbox + workspace + artifact prefix per project | Compromised sandbox reads nothing of sibling projects |
| Sandbox ↔ host/system | Container (laptop/VM profile) or hardened runtime — gVisor/Kata/microVM (K8s profile); non-root, read-only base image, resource caps, no host mounts | Generated code cannot reach the host, control plane, or session store |
| Sandbox ↔ network | Default-deny egress; per-vertical allowlist (e.g. package registries via caching proxy); no ingress except the platform's `execute` channel | Exfiltration and lateral movement constrained; supply-chain surface reduced to the proxy |
| Sandbox ↔ credentials | **No secrets in sandboxes, ever.** LLM keys live only in the harness/provider layer; publish and storage credentials live in the control plane; anything a tool needs is brokered through a proxy that injects auth outside the sandbox | Prompt injection in generated code can't steal keys — there are none to steal |
| Served/preview content ↔ platform | Distinct domain/origin, static serving, strict CSP defaults; preview treated as untrusted content, never sharing origin or auth with the product UI | A malicious site or preview can't attack the builder or other tenants' visitors beyond its own origin |

### 5.2 Requirements

- **R-SEC-1 (MUST):** Every isolation layer above exists in every deployment profile; profiles differ in *strength within a layer*, never in *presence of a layer* — and the strength differences are documented per trust tier (§5.3), not glossed over.
- **R-SEC-2 (MUST):** Audit trail: the session log doubles as a security audit log (every tool execution and run attributable to tenant/project/session/event).
- **R-SEC-3 (MUST):** Secrets management pluggable (env/file for laptop, Vault/KMS-class for K8s).
- **R-SEC-4 (SHOULD):** Per-tenant data deletion (GDPR erasure) covering transcripts, artifacts, and backups policy; operator control over whether any telemetry leaves the installation.
- **R-SEC-5 (LATER):** SSO/SCIM for enterprise operators.

### 5.3 Trust tiers (honesty requirement)

Isolation claims are made per tier, published in operator docs, and surfaced at install time. **The platform never pretends all deployments provide identical isolation** — "mostly isolated" is just a creative spelling of "breached."

| Tier | Typical profile | Supported threat model | Explicitly NOT claimed |
|---|---|---|---|
| **T1 Personal** | Laptop | Accidental damage, buggy generated code, basic prompt-injection containment. Users trust each other (it's you and your family). | Safe hosting of mutually *hostile* users on container-only isolation |
| **T2 Trusted-private** | Server | T1 + multiple semi-trusted tenants (a school, a company); containment of a compromised project from other projects/tenants at container/gVisor strength | Resistance to a well-resourced attacker with a container-escape zero-day |
| **T3 Hostile multi-tenant** | Cluster | Arbitrary anonymous internet users actively attacking the platform and each other; hardened runtimes (Kata/gVisor/microVM), full egress control | — (this is the full claim) |

- **R-SEC-6 (MUST):** Each deployment profile declares its trust tier; the install flow states it; running a below-tier setup for a use case that needs a higher one is an explicit, logged operator override — never a silent default.

---

## 6. Non-functional requirements

- **R-NFR-1 Resumability:** zero committed-event loss on any single-component failure (this is P3 stated as an SLO).
- **R-NFR-2 Latency:** streaming-first everywhere; time-to-first-token on an active session p95 < 3 s (log-first resume lets inference start before sandboxes are warm — the article's ~60–90% TTFT win is the design precedent).
- **R-NFR-3 Laptop footprint:** idle platform (no active sessions) < 1 GB RAM; single `creo up` → working system with embedded/simple dependencies (no K8s, no external services required).
- **R-NFR-4 Scale ceiling (design target, not v1 benchmark):** architecture supports horizontal scaling of gateway, control plane, harness pool, and sandbox pool independently; session store is the only stateful scaling problem, and it's an append-only log — a well-understood one.
- **R-NFR-5 Upgrades:** event log schema is versioned; an operator can upgrade the platform without invalidating stored sessions; interrupted upgrades are recoverable; backup/restore is documented and tested.
- **R-NFR-6 Observability:** metrics (sessions, runs, tokens, sandbox utilization), structured logs, and traces across gateway→harness→sandbox; per-tenant cost attribution; an operator health surface (capabilities available, dependencies unavailable, provider health) consistent across profiles even where detail differs.

---

## 7. Deployment profiles

Same core, three supported shapes; each maps to a trust tier (§5.3):

| Profile | Trust tier | Target | Composition | Sandbox tech |
|---|---|---|---|---|
| **Laptop** | T1 | One person / family | **Single binary + data dir**; embedded SQLite; local disk artifacts | None needed at L0/L1 (file-tools-only workspace); container optional for L2 verticals |
| **Server** | T2 | Team / school / small operator | Single binary on a VM (or Compose); embedded SQLite; local or S3-compatible artifacts | Containers (mandatory for L2), optional gVisor |
| **Cluster** | T3 | High-scale operator | Kubernetes (Helm); Postgres + object store; autoscaled harness & sandbox pools | Kata/gVisor/microVM |

> 💡 L0/L1/L2 is the **execution ladder** (`docs/components.md` §5): L0 = generated code never executes server-side (websites vertical — agent writes static files only), L1 = only *our* trusted tooling processes generated files (screenshots, validators), L2 = arbitrary toolchains (deps installation, builds) which always require a container. The websites vertical needs no container runtime at all.

- **R-DEP-1 (MUST v1):** Laptop and Server profiles.
- **R-DEP-2 (LATER):** Cluster profile — but every interface decision is reviewed against it now (P6).
- **R-DEP-3 (MUST):** Core configuration (vertical profiles, providers, security policies, limits, retention) is expressible independently of any one deployment system — the same config concepts move from Compose to Helm without translation of meaning.

---

## 8. Open-core boundary

**OSS (the core):** session store, harness, sandbox runtime + interface, control plane, provider adapters, API + CLI, publish primitive with built-in serving, generic vertical profile, Laptop/Server profiles.

**Commercial (verticals & operator conveniences):** the simple-websites vertical's product layer (its web app, templates, taste/prompt engineering), future verticals (kids' games, …), hosted/managed Creo, and eventually enterprise operator features (SSO, advanced quotas/billing).

**Boundary rule:** anything required to *build a vertical* is open; the verticals themselves are the product. This keeps the OSS core genuinely usable (a developer can build their own vertical) without giving away the consumer products.

*Risk to watch:* the temptation to keep "the good prompts" closed will pull quality out of the OSS harness. Mitigation: the generic profile must always produce a decent result end-to-end — it's the core's demo, docs, and test fixture.

---

## 9. Milestones

Sequenced to de-risk the two scariest claims first (durable resume; safe multi-tenant execution), then earn the vertical. Each milestone's demo is its acceptance gate; §10 maps criteria to milestones.

- **M0 — Spine (core proof):** session store + harness + runs + one sandbox + Anthropic adapter + CLI client. Demo: start a session from the CLI, kill every process mid-run, resume from a second terminal, run completes; a duplicate submit creates no duplicate run. **✅ DELIVERED 2026-08-04** — blueprint `docs/plans/m0-blueprint.md`; contracts SL-1..5 / RC-1..5 tested; acceptance demo performed live against Anthropic (SIGKILL mid-run → resume at gen 2 → completion) via `scripts/demo-m0.sh`.
- **M1 — Tenancy & safety:** gateway, tenants/users/projects, sandbox hardening + egress policy, quotas + budget caps. Demo: two tenants, hostile prompt-injected project actively tries and fails to reach the other tenant, the host, and any credential. **✅ DELIVERED 2026-08-04** — blueprint `docs/plans/m1-blueprint.md`; bearer-token auth + structural tenant scoping (foreign = 404), hard daily token budgets enforced at the gateway, per-tenant run quotas, workspace symlink hardening, atomic submit. `internal/e2e/hostile_test.go` proves containment: escape attempts error, no cross-tenant leak, 404 on foreign routes, budget-exhausted run refused in plain language. (Users deferred to the human-login surface at M3/M4; egress/CPU quotas moot at L0; storage quota deferred to M2.)
- **M2 — Artifacts & publish:** artifact versions, preview URLs, publish/rollback with built-in serving, export. **✅ DELIVERED 2026-08-04** — blueprint `docs/plans/m2-blueprint.md`; `internal/serving` streams versions from the content store on an origin-isolated port (:8081) under a static-only CSP; publish/rollback are atomic pointer flips emitting session events, rollback walks version lineage (the restore primitive); zip export; preview via capability URL (T1). e2e proves build→publish→rollback→export→preview plus origin isolation and wrong-secret 404.
- **M3 — Websites vertical alpha:** vertical profile + reference web client; the R-WEB-3 loop works for a friendly non-coder with a human on call.
- **M4 — Multi-device & polish:** second-device resume UX, cross-device approvals, idle-park/wake tuning, error-language pass (R-AGT-2/3), OpenAI-compat adapter validated against at least one local model.
- **M5 — Self-host release:** Laptop + Server profiles documented with trust-tier statements, upgrade + backup/restore path, OSS repo public.

---

## 10. Acceptance criteria

The platform is ready for external use when all of the following hold (gating milestone in parentheses):

**Durability & idempotency**
1. A session survives harness/worker failure with no committed-event loss. (M0)
2. A project survives permanent sandbox loss. (M0)
3. Duplicate client requests do not create duplicate authoritative work. (M0)
4. A user can close the browser and continue from another browser/device. (M4)
5. Pending questions and approvals can be answered from another authorized device. (M4)

**Security**
6. One project cannot access another project; one tenant cannot access another tenant. (M1)
7. Project/sandbox code cannot reach platform credentials — demonstrated by an actively hostile test project. (M1)
8. Preview and published content cannot access trusted platform authentication (origin separation verified). (M2)
9. Each shipped profile documents its trust tier, and install surfaces it. (M5)

**No-code bar**
10. A non-technical user can create, modify, and publish a project without seeing code, terminals, or raw technical errors. (M3)
11. Version restore works in user language, with no source-control concepts surfaced. (M3)
12. Every failure path ends in auto-recovery or a single plain-language decision. (M4)

**Headlessness & portability**
13. The CLI can drive the complete workflow (create → converse → approve → publish) using only the public API. (M2)
14. At least two model providers, including one privately hosted model, work without any client-visible behavior change. (M4)
15. A user can export their project; the platform is not a hostage-taking mechanism. (M2)
16. Backup and restore of a self-hosted installation is documented and demonstrated. (M5)

---

## 11. Success metrics

**Core:** time-to-resume p95; committed-event loss (must be 0); duplicate-action rate (target ~0, watched from M0); sandbox escape / credential-leak / cross-tenant incidents (must be 0 — the acceptable number of confirmed cross-tenant incidents is famously not "low"); external developers able to build a toy vertical from docs alone (qualitative, M5+).

**Websites vertical:** % of new projects reaching first preview; % reaching publish; median time-to-publish; frequency of raw technical errors reaching the primary surface (target 0); % of returning users who successfully resume and change a published site; unassisted-completion rate for non-coder testers (the north-star number).

**Self-hosting (M5+):** successful-install rate; time to first project on a fresh install; upgrade success rate; backup/restore success rate.

---

## 12. Open questions

Ordered roughly by how soon they block work.

1. **Session store technology — RESOLVED (2026-08-03).** Embedded **SQLite for all single-node profiles** (Laptop + Server — the actual deployment reality of ~95% of self-host installs); **Postgres as the Cluster-profile adapter**, added behind the same narrow storage interface when that profile becomes real; pure-file storage evaluated and rejected (hand-building ACID is more code and more risk than SQLite, which ships it). Restores the true single-binary self-host story. The dual-semantics tax is deferred and bounded: one backend for the whole M0–M5 arc, then a shared conformance suite with fencing and idempotency as explicit interface contracts (`docs/components.md`).
2. **Harness implementation strategy — direction chosen, effort unvalidated.** Custom lightweight harness (all three evaluated documents independently converged on this; no embedded SDK). Spike-01 doubles as the effort check: if the loop is months rather than weeks, revisit per `docs/solution-options.md` (Temporal-backed Option D is the retreat position).
3. **Sandbox tech for the Laptop profile on macOS/Windows** (no native containers): Docker Desktop dependency acceptable, or ship a lighter path?
4. **Preview exposure on local installs:** how does a Laptop-profile user show a preview to another device on their LAN (or a friend) without punching undocumented holes?
5. **Identity for self-hosters:** bundle a minimal email+password identity, or require OIDC even on Laptop profile (bad for the family use case)?
6. **Auto-repair visibility policy:** which failures are repaired silently vs surfaced as "I fixed something" vs requiring a decision? (Defines the R-AGT-3 escalation ladder.)
7. **Admin vs end-user setting split:** which knobs belong to the operator, which to the tenant, which to the project?
8. **Can multiple verticals share one installation, and can a project move between vertical profiles?** (Affects profile schema and artifact typing now, even though it ships later.)
9. **Importing existing sites/code:** out of scope for v1 build, but does *export → re-import* need to round-trip from day one?
10. **Naming/branding** of core vs vertical (working name: Creo for the core).
11. **Licensing specifics** for the open core (Apache-2.0 vs AGPL-family) — affects who can offer hosted Creo against us. Decide before M5, not now.
12. **Version-compatibility promise:** what API/event-schema stability guarantee do we make to vertical builders at OSS launch?

---

## Appendix A — Vocabulary

- **Session** — durable unit of ongoing work on a project: the event log plus whatever brain/hands are currently animating it.
- **Run** — one triggered unit of agent work within a session, with its own lifecycle, cost, and produced artifact versions. At most one authoritative run active per session.
- **Event** — one append-only record in a session (message, tool call/result, run lifecycle, artifact reference, approval, error/repair).
- **Harness / brain** — stateless agent loop turning events + LLM into actions.
- **Sandbox / hands** — disposable isolated execution environment.
- **Artifact** — versioned build output of a project (websites vertical: a static bundle); the authoritative project state.
- **Vertical profile** — the configuration (prompts, tools, artifact vocabulary, restrictions, sandbox policy) that specializes the generic harness into a product; restrictions enforced by the platform, not the model.
- **Trust tier** — the documented threat model a deployment profile actually withstands (T1 personal / T2 trusted-private / T3 hostile multi-tenant).
- **Deployment profile** — a supported way to run the platform (Laptop / Server / Cluster).
