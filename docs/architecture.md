# Creo — Architecture

**Status:** Draft v0.1 · 2026-08-03 · companion to `PRD.md` v0.3 and `docs/solution-options.md`
**Scope:** mechanics of the core platform — the *how* behind the PRD's requirements. Sources: the managed-agents decomposition (PRD §3), plus mechanics validated and adopted from a third-party proposal (leases/fencing, outbox, projections, event envelope, gateway capability model, credential broker). Claims marked **[verify]** are inherited and not yet independently checked.

Type signatures below are illustrative pseudo-TypeScript, not a language commitment (see §11).

---

## 1. Decisions in force

| Decision | Status | Where argued |
|---|---|---|
| Project state is **code-canonical**; annotations (node IDs, content index) are derived, never authoritative | **Locked** | PRD v0.3 §3.1; semantic-model-as-canon rejected as "Wix with an LLM intent parser" |
| Custom lightweight harness; no embedded agent SDK | Chosen; effort validated by spike-01 | `solution-options.md` (A > D > B > C > E) |
| **Embedded SQLite** canonical store for all single-node profiles (Laptop + Server); **Postgres = Cluster-profile adapter**, added later behind the same narrow storage interface; pure-file storage rejected | **Decided** (2026-08-03 discussion) | Restores the single-binary self-host story; dual-semantics tax deferred and bounded by a conformance test suite; fencing + idempotency specified as interface contracts (`components.md` SL-3, RC-1/RC-3) |
| **Execution ladder L0/L1/L2**: websites vertical is L0/L1 (no container runtime required — file tools only, trusted tooling); dependency installation or any build step ⇒ L2, container mandatory; enforced by profile capability, not convention | **Decided** (2026-08-03 discussion) | `components.md` §5; spike-01 (agent tool surface was file-ops only) |
| **v-min deployment shape**: one binary, one process, SQLite, data dir; no outbox (in-process fan-out); bearer-token auth; two model adapters (anthropic + openai-compat); second port as T1 origin isolation (expires at T2) | **Decided** (2026-08-03 discussion) | `components.md` overview table |
| DB-backed jobs + leases; no message-broker dependency | Chosen (adapter seam kept for T3 scale) | §5.3 |
| HTTP commands + SSE event streaming; idempotency keys on every mutating command | Chosen | §3.4, §4.3 |
| Trust tiers T1/T2/T3 with per-tier documented guarantees | Locked | PRD §5.3 |
| Implementation language | **Open — downstream of spike-01** | §11 |

## 2. Component map

The PRD §3 diagram is the reference. Refinements at architecture level:

- The **control plane** decomposes into: auth/tenant service, project service, session+event service, run coordinator, approval service, product-profile registry, artifact/version service, preview gateway, usage/policy enforcement. On Laptop/Server profiles these are modules in one process, not services.
- A **model gateway** (§6) sits between harness workers and providers; harnesses never hold provider keys.
- A **tool & credential broker** (§7) sits between harness workers and anything privileged; sandboxes never hold any credentials.
- **Two planes, structurally separated (§9):** the control plane (identities, events, credentials, policy) and the execution plane (sandboxes, builds, previews). The execution plane has no route to the control-plane database.

## 3. Event model

### 3.1 Envelope

Every event in a session's append-only log:

```ts
type Event = {
  eventId: string;            // ULID
  tenantId: string;
  projectId: string;
  sessionId: string;
  runId?: string;

  sequence: bigint;           // per-session, gapless, monotonic — the client cursor
  type: string;               // dotted taxonomy, e.g. "run.started"
  createdAt: string;

  causationId?: string;       // event that directly caused this one
  correlationId?: string;     // ties a user request to everything it triggered
  clientRequestId?: string;   // idempotency key of the originating command

  payload: {
    userText?: string;        // plain-language, audience-ready (PRD R-AGT-2) — emitted server-side
    detail?: unknown;         // technical detail for diagnostics/operators
    [k: string]: unknown;
  };
  blobRefs?: BlobRef[];       // large payloads live in the blob store, referenced here
};
```

The two-layer presentation rule (PRD R-AGT-2) is implemented **in the envelope**, not by separate event streams: `userText` is what every no-code client renders verbatim; `detail` is present on the same event for operator/diagnostic clients. One log, projected per audience — the canonical history never forks.

### 3.2 Taxonomy (initial)

```
session.created | session.state.changed
user.message.submitted
run.requested | run.started | run.waiting | run.resumed | run.completed | run.failed | run.cancelled
assistant.message.started | assistant.message.delta* | assistant.message.completed
tool.requested | tool.started | tool.completed | tool.failed
artifact.version.created            // carries previousVersionId / newVersionId
preview.build.started | preview.ready
publish.requested | publish.completed | publish.rolled_back
approval.requested | approval.responded
input.requested | input.provided
sandbox.created | sandbox.lost | sandbox.restored
repair.started | repair.completed   // auto-repair, surfaced per the R-AGT-3 escalation ladder
error.translated                    // plain-language failure event
```

\* deltas stream over SSE but are not persisted individually; the completed message persists with full text. Keeps the log meaningful, not a token firehose.

### 3.3 Projections

Clients and the harness never replay a raw log for reads. Derived, rebuildable projections (ordinary Postgres tables updated from the outbox consumer):

```
session_current_state     run_current_state      pending_inputs_approvals
project_current_version   preview_current_state
```

Events remain authoritative; every projection can be dropped and rebuilt from the log. Brief eventual consistency between append and projection is accepted (bounded, single-digit ms in-process on small profiles).

### 3.4 Delivery

- **Commands:** authenticated HTTP, every mutating command carries `Idempotency-Key`. Replays return the original result (§4.3).
- **Streaming:** `GET /sessions/{id}/events?after={sequence}` over SSE. The client stores the last seen sequence; reconnect = same call with the new cursor. This one endpoint is the entire multi-device story (PRD R-SES-2/4): live tail and backfill are the same operation.
- **Transactional outbox:** appending an event and recording its outbox entry happen in one DB transaction; SSE fan-out and projection updates consume the outbox. A commit can never succeed while its notification silently disappears — this is what makes "zero committed-event loss" (R-NFR-1) implementable rather than aspirational.

## 4. Run coordination

### 4.1 State machine

```
requested → queued → running ─┬→ waiting_for_input ──┐
                              ├→ waiting_for_approval ┤→ running (resumed)
                              ├→ recovering ──────────┘
                              └→ completed | failed | cancelled
```

Every accepted run reaches an authoritative terminal or waiting state (PRD R-RUN-3). `recovering` is entered on lease expiry or infrastructure failure and exits only to `running` (new worker) or `failed` (with prior work intact — the last committed artifact version stands).

### 4.2 Single-writer rule

One authoritative run mutates a project at a time (PRD R-RUN-2); reads and observers are unlimited. Concurrent no-code mutations produce semantic conflicts no merge algorithm can adjudicate ("one agent restyles the theme while another writes content against the old theme"), so v1 serializes writes per project and queues the rest. Revisit only with a real collaboration feature, not before.

### 4.3 Idempotency

- Client commands: `Idempotency-Key` per mutating request, stored with the accepted result; duplicate delivery (double-tap, retry-after-timeout, two devices) returns the stored result and starts nothing.
- Tool operations that mutate state carry stable operation IDs, so a model-stream failure mid-run can be retried without double-applying a mutation that was already accepted.

### 4.4 Leases with generation fencing (PRD R-RUN-5)

```ts
type Lease = { runId: string; workerId: string; generation: bigint; expiresAt: string };
```

- A worker acquires the lease (transactionally, in Postgres) before touching a run, renews it while active, and every event append includes the lease generation.
- Appends with a superseded generation are **rejected at the store** — a stale worker that woke up after a GC pause or network partition cannot write, regardless of what it believes. Takeover is safe by construction, not by timing.
- Lease expiry moves the run to `recovering` and makes it claimable. This is the entire crash-recovery protocol; there is no other path.

### 4.5 Recovery scenarios (acceptance-mapped)

| Failure | Behavior | PRD criterion |
|---|---|---|
| Browser/device disconnect | Nothing stops; reconnect with cursor | AC-4 |
| Harness worker crash | Lease expires → `recovering` → new worker resumes from log + projections | AC-1 |
| Sandbox lost | Rebuilt from the current artifact version; `sandbox.lost/restored` events; run continues | AC-2 |
| Model stream failure | Incomplete attempt recorded; retry honors accepted operation IDs | — |
| Stale approval response | Checked against approval ID + run + lease generation; rejected with plain-language explanation | AC-5 |
| Platform restart | DB scan: expired leases, queued runs, pending approvals, sandboxes to reconcile; work resumes | AC-1/2 |

## 5. Persistence

### 5.1 Canonical store — SQLite (single-node) / Postgres (Cluster adapter)

Tenants, users, projects, sessions, runs, **events**, projections, artifact/version metadata, approvals, leases, jobs, policies, quotas, usage records — behind a narrow storage interface (append/range/cursor, KV metadata, CAS lease, claim, counters; no SQL leaking through). **Single-node profiles (Laptop, Server) run embedded SQLite**: WAL mode + `busy_timeout` from day one, one writer connection, fsync-on-commit for the event log (no relaxed-durability mode for the source of truth), Litestream-class continuous backup as the operator story. **Postgres is the Cluster-profile adapter**, added when that profile becomes real; both backends must pass the same conformance suite, with lease fencing and idempotency specified as explicit interface contracts (`components.md` SL-3, RC-1/RC-3) rather than incidental SQL behavior. Row-level tenancy: every tenant-owned row carries `tenant_id`, enforced by application-layer scoping + unguessable external IDs (+ Postgres RLS where practical, on that backend).

### 5.2 Blob store

```ts
interface BlobStore {
  put(input: BlobInput): Promise<BlobRef>;
  get(ref: BlobRef): Promise<ReadableStream>;
  delete(ref: BlobRef): Promise<void>;
}
```

Implementations: local filesystem (Laptop), S3-compatible (Server/Cluster). Holds uploaded/generated assets, artifact bundles, exports, screenshots, large logs, model request/response archives. Events reference blobs; they never embed them.

### 5.3 Jobs and queues

Postgres-backed job claims (transactional, `FOR UPDATE SKIP LOCKED`-style) for run dispatch — no broker dependency on any profile. A queue adapter seam exists for T3 scale, with one invariant: **the queue is never the only record that work exists.** The run row in Postgres is; the queue is delivery, not truth.

## 6. Model gateway

All model traffic flows through one platform-controlled gateway. Responsibilities: provider adapters (Anthropic; OpenAI-compatible, which covers vLLM/llama.cpp/Ollama/LM Studio), streaming and tool-call normalization, retry classification, usage metering, **hard budget enforcement (PRD R-LLM-5 lives here — a run that would exceed the cap is refused at the gateway, the one chokepoint that cannot be bypassed)**, logging/redaction, local-endpoint config, capability discovery.

```ts
type ModelCapabilities = {
  toolCalling: boolean; structuredOutput: boolean; vision: boolean; streaming: boolean;
  maxContextTokens?: number; maxOutputTokens?: number;
};
```

Vertical profiles declare minimum capabilities (PRD R-LLM-4). For models without native tool calling, the harness may fall back to constrained structured output with schema validation of the returned action plan — degraded but functional, and the degradation is *declared*, not discovered in production.

## 7. Tool & credential broker

Tools execute outside the model and are authorized by platform policy, not by prompt. Categories under the code-canonical decision:

- **Workspace tools** (harness ↔ sandbox): `read_file`, `write_file`, `patch`, `list` — the coding loop's hands.
- **Build/preview tools:** `build`, `start_preview`, `screenshot`, `run_validator` (vertical-defined validators: build success, links, accessibility, responsive checks, content policy).
- **Platform tools:** `create_artifact_version`, `publish`, `rollback`, asset ingest — these touch the control plane and are brokered, never direct.
- **Integration tools** (later): domains, lead destinations — brokered with per-use policy checks.

Credential flow (PRD §5.1, made mechanical): sandboxes hold **zero** credentials. A privileged operation = harness requests brokered op → policy checks tenant/project/tool/approval → broker obtains the credential and performs the operation (or mints a narrow, short-lived capability) → use is audited as events. Prompt injection in generated code finds nothing to steal and no privileged channel to speak on.

## 8. Sandbox provider interface

```ts
interface SandboxProvider {
  create(input: CreateSandboxInput): Promise<SandboxRef>;
  execute(id: string, cmd: SandboxCommand): Promise<ExecutionRef>;
  streamExecution(execId: string, after?: number): AsyncIterable<ExecutionEvent>;
  readFile(id: string, path: string): Promise<Uint8Array>;
  writeFiles(id: string, files: FileWrite[]): Promise<void>;
  exposePort(id: string, port: number): Promise<PreviewEndpoint>;
  pause(id: string): Promise<void>;
  resume(id: string): Promise<void>;
  snapshot?(id: string): Promise<SnapshotRef>;   // optional acceleration, never authoritative
  destroy(id: string): Promise<void>;
}
```

- A sandbox is reconstructable from `(artifact version, vertical profile)` alone; snapshots are caches (PRD §3.1 authority ruling). Idle sandboxes are destroyed, not preserved — log-first resume means inference restarts before the sandbox is warm (R-NFR-2).
- Implementations: local Docker/Podman (T1/T2), hardened runtime on Kubernetes (T3). Third-party options behind the same interface — OpenSandbox (self-hostable, Docker/K8s backends) and managed providers with pause/resume such as E2B — are **[verify]** candidates, not dependencies.
- Network policy per PRD §5.1: default-deny both directions; egress allowlist via caching proxy; no control-plane route; no cross-sandbox traffic; no infrastructure metadata endpoints.

## 9. Security planes

```
Control plane (trusted)              Execution plane (untrusted)
  identities, sessions, events         sandboxes, builds
  credentials, policies, quotas        preview servers
  model gateway, broker                generated artifacts
        │  brokered, audited, one-way  ▲
        └──────────── execute() ───────┘
```

- The execution plane never connects to the control-plane database, gateway, or broker storage; its only inbound channel is `execute()`, its only outbound privileges are brokered.
- Preview and published content on separate origins from the product app (`*.preview.<usercontent-domain>` / published domains); no cookie or auth sharing (PRD R-PUB-3, AC-8).
- Trust tiers (T1/T2/T3) parameterize *strength* (runc → gVisor/Kata, egress enforcement depth) — never the *presence* of a plane boundary (PRD R-SEC-1).

## 10. Core contracts

**Moved to `docs/components.md`** — the full catalog: per-component responsibility, interface, testable contracts (including fencing SL-3 and idempotency RC-1/RC-3 as conformance-suite material), usage, and v-min vs. cluster backing. The component set: SessionLog, RunCoordinator, AgentHarness, ModelGateway, SandboxProvider/Workspace, ProjectStore, BlobStore, PreviewGateway, API layer, ProductProfile, IdentityService, ToolBroker (dormant). These are the OSS core's API surface and its stability promise (PRD open question #12).

Two properties worth restating here: `ProjectStore` is deliberately **opaque** about artifact contents (the code-canonical decision expressed as an interface), and each component owns exactly one kind of authority — the design rule that decides where future features land.

## 11. Open at this level

1. **Implementation language** — Go vs TypeScript. Spike-01 input: the loop itself is trivial in either; nearly all surrounding machinery (DOM manipulation, Playwright screenshots, static serving, future validators) is JS-native. That argues TS for the *vertical's* tooling regardless; core language still open — decide at M0 kickoff.
2. ~~Annotation scope for the websites vertical~~ — **resolved by spike-01:** stable `data-node-id` annotations for click-targeting only, with exact IDs passed from the UI (never pattern-resolved); no content.json indirection, content edits go through the agent. Deterministic editing revisit-able later only with exact node identity + structured content groups. See `spikes/01-harness/RESULTS.md`.
3. ~~Laptop Postgres packaging~~ — **moot**: single-node profiles run embedded SQLite (see §1 decisions); no bundled Postgres anywhere until the Cluster profile.
4. **[verify] backlog:** OpenSandbox architecture claims; E2B pause/resume semantics (both only relevant at L2/Cluster).
5. **From spike-01 product findings:** version restore is platform-only (artifact versions — reinforces §3.1 authority ruling); site language is an explicit vertical-profile setting, never inferred; static-only must be enforced by artifact policy, not prompt alone. All three now encoded in `components.md` (§6, §10, §8 respectively).
