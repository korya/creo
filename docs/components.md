# Creo — Component Catalog

**Status:** v1.0 · 2026-08-03 · companion to `PRD.md` v0.3 and `docs/architecture.md`
**Scope:** every component in the system: its responsibility (why it deserves to exist), its interface, its testable contracts, who uses it, and what backs it in v-min (single-server) vs. cluster. This file supersedes the contracts list formerly in `architecture.md` §10.

**Design rule — one authority per component.** Each component owns exactly one kind of authority: what happened, who acts, how the agent thinks, what it costs, where risk runs, what the project is, who is asking, what is allowed, what is served. When a future feature has no obvious owner among these, that is the signal to add a component or to stop before blurring one.

## Overview

| Component | Authority | v-min backing | Cluster backing |
|---|---|---|---|
| SessionLog | what happened | SQLite (WAL, fsync-on-commit) | Postgres, same interface |
| RunCoordinator | who may act now | SQLite tables + in-process dispatch | Postgres + queue adapter |
| AgentHarness | how the agent thinks/acts | in-process workers | harness pool (same code) |
| ModelGateway | what it costs, which model | 2 adapters, config file | + per-tenant provider config |
| SandboxProvider / Workspace | where risk runs | local dir, file-tools-only (L0/L1) | container/microVM provider (L2) |
| ProjectStore | what the project is | content-addressed dirs + SQLite rows | object store + Postgres rows |
| BlobStore | big dumb bytes | filesystem | S3-compatible |
| PreviewGateway | what is served | second port, static, CSP | separate origins/domains, CDN |
| API layer | the contract with clients | HTTP + SSE in the binary | horizontally scaled gateway |
| ProductProfile | what is allowed | embedded config | profile registry |
| IdentityService | who is asking | bearer tokens, one implicit tenant | OIDC, orgs, SCIM later |
| ToolBroker | privileged external ops | **dormant** (nothing at L0/L1 needs credentials) | credential-brokered operations |

Types below are illustrative pseudo-TypeScript, not a language commitment.

---

## 1. SessionLog

**Responsibility.** Owns the durable truth of *what happened*. Its guarantee — no committed interaction is ever lost; any process or device can reconstruct state from it — is what allows every other runtime piece (harness, sandbox, client connection) to be disposable. Resumability and multi-device are properties of this component, not features built elsewhere.

```ts
interface SessionLog {
  append(sessionId, events: NewEvent[], lease?: LeaseToken): Seq
  read(sessionId, after: Seq, filter?: EventType[]): Event[]
  subscribe(sessionId, after: Seq): AsyncStream<Event>   // backfill from cursor, then live tail
  checkpoint(sessionId): ContextSnapshot                 // compaction support (post-v-min)
}
```

**Contracts (testable):**
- **SL-1 Atomicity.** All events in one `append` become visible together or not at all.
- **SL-2 Sequence.** Per-session `seq` is gapless and strictly monotonic.
- **SL-3 Fencing.** An `append` carrying a lease generation older than the current one fails with `StaleLease` and writes nothing. This is enforced *in storage*, not in coordinator logic.
- **SL-4 Read-after-append.** An acknowledged append is immediately visible to `read` and delivered to open `subscribe` streams at-least-once (consumers dedupe by `seq`).
- **SL-5 Durability.** An acknowledged append survives process kill (fsync-on-commit; no relaxed-durability mode for this table, ever).

**Used by:** harness (context + emit), API (SSE ≡ `subscribe`), coordinator (lifecycle events).
**Backing:** v-min SQLite; `subscribe` is in-process fan-out. The transactional outbox slots in here when delivery first crosses a process boundary (cluster) — the durable thing was never the delivery, it's the log.

## 2. RunCoordinator

**Responsibility.** Owns *who may act right now*. Converts an unreliable world — crashes, double-taps, retries after timeouts, two devices — into a serialized, deduplicated, recoverable stream of runs. Without it, duplicate or concurrent agent runs silently corrupt projects; merged into the harness, its correctness rules would be re-implemented per harness type.

```ts
interface RunCoordinator {
  requestRun(projectId, sessionId, trigger, idempotencyKey): RunRef
  claim(workerId): Run | null
  renew(runId, workerId): LeaseToken       // carries generation
  complete(runId, outcome): void
  recoverOrphans(): Run[]                  // boot-time scan
}
```

**Contracts (testable):**
- **RC-1 Idempotency.** `requestRun` with a previously seen `(sessionId, idempotencyKey)` returns the original `RunRef` and never creates a second run. Retention of keys: ≥ 24 h.
- **RC-2 Single writer.** At most one active lease per project at any instant; `claim` never violates this.
- **RC-3 Generations.** Lease generations strictly increase per run; `renew` of a superseded lease fails; the failed holder can never commit again (see SL-3).
- **RC-4 No limbo.** Every accepted run reaches a terminal or waiting state; an expired lease moves the run to `recovering` and makes it claimable.
- **RC-5 Recovery scan.** `recoverOrphans` at boot finds every expired-lease run; combined with RC-4, a `kill -9` at any point loses no accepted work.

RC-1/RC-3 (with SL-3) are the two contracts that must never accrete single-node assumptions — they are the cluster-portability of the whole design, and they get their own conformance tests run against every storage backend.

**Used by:** API (message → `requestRun`), workers (claim/renew/complete), startup.
**Backing:** v-min SQLite + in-process dispatch (the queue table is durability, not coordination). Cluster: same table semantics on Postgres, queue adapter in front — the queue is never the only record that work exists.

## 3. AgentHarness

**Responsibility.** Owns turning intent into actions — the model↔tool loop and nothing else. Exists as a separate, stateless component specifically as the *assumption-rot defense*: how the agent thinks is the part that changes as models improve, so it must be swappable without touching storage, transport, or coordination. Everything it knows arrives as input; everything it decides leaves as events.

```ts
interface AgentHarness {
  run(input: {
    profile: ProductProfile
    session: SessionContext        // reconstructed from SessionLog
    workspace: Workspace           // from SandboxProvider, per profile level
    model: ModelHandle             // from ModelGateway, budget-checked
  }): AsyncStream<NewEvent>
}
```

**Contracts:** stateless (no durable state outside emitted events); emits plain-language `userText` on every user-facing event (error translation happens here, at emit time — clients render, never interpret); tool use restricted to the supplied `workspace` and `model` handles — a harness cannot reach providers or files any other way.

**Used by:** worker loop inside the coordinator's claim cycle.
**Backing:** in-process workers in v-min (1–2 concurrent runs); an identical-code harness pool at cluster scale. Validated by spike-01 at ~100 lines.

## 4. ModelGateway

**Responsibility.** Owns every token that flows to or from an LLM. Being the single choke point is the point: it makes providers genuinely swappable (normalization + capability declaration) and budgets genuinely enforceable (a hard stop nobody can route around). Provider credentials live here and nowhere else.

```ts
interface ModelGateway {
  complete(req: {provider, model, system, messages, tools}): Completion
  capabilities(provider, model): {toolCalling, vision, maxContext}
  checkBudget(tenantId): Allow | Deny(reason)
  recordUsage(tenantId, runId, usage): void
}
```

**Contracts:** budget is checked before every call and a `Deny` is final for that call (R-LLM-5 lives here); usage is recorded even for failed calls; no component other than the gateway holds provider credentials; a model lacking a capability the profile requires is rejected at run start with a plain-language event, not discovered mid-run.

**Used by:** harness only.
**Backing:** v-min ships two adapters — `anthropic` and `openai-compat` (covers OpenRouter, ChatGPT/OpenAI, Ollama/qwen, LM Studio, vLLM). Constraint to document for local models: the harness requires tool-calling capability (qwen3-class OK); structured-output fallback for weaker models is deferred.

**`openai-compat` (implemented M4, `internal/model/openai.go`).** Written against the wire protocol, not a vendor SDK — that is what keeps one adapter covering every self-hosted server. Selected by the spec `openai:<model-id>[@<base-url>]` (default base URL is OpenAI's own); the key comes from `CREO_OPENAI_KEY`/`OPENAI_API_KEY` and an absent key is normal, since local servers want none. Two protocol differences are the entire adapter: tool arguments travel as a JSON-encoded *string* rather than an object, and tool results are top-level `role:"tool"` messages rather than blocks inside a user turn. One hard-won rule: **`finish_reason` is not trustworthy on local servers** — several report `"stop"` while emitting tool calls, so the presence of tool calls decides and the label only breaks ties. `internal/e2e/openai_test.go` drives the real binary through the real protocol against a scripted server (including that quirk); `scripts/demo-local-model.sh` is the AC-14 gate against a genuine local model, which CI cannot run.

## 5. SandboxProvider / Workspace

**Responsibility.** Owns the boundary where potentially hostile generated content is materialized and acted upon. Guarantees capability-by-construction — a tool that does not exist on the Workspace cannot be invoked by any prompt injection — and disposability: any workspace can be destroyed and rebuilt from a ProjectStore version. Exists separately so that execution risk is a pluggable policy (the ladder below), not a fixed property of the platform.

```ts
interface SandboxProvider {
  open(projectId, artifactVersion?): Workspace
  destroy(workspaceId): void
}
interface Workspace {
  listFiles(): Path[]
  readFile(path): Bytes
  writeFile(path, content): void
  deleteFile(path): void
  exec?(cmd): ExecResult    // exists ONLY on the L2 container provider
}
```

**The execution ladder:**

| Level | What runs | Host requirement | Who uses it |
|---|---|---|---|
| **L0** — no execution | agent writes files; generated code never executes server-side | none | websites vertical v-min |
| **L1** — trusted tooling | *our* code processes generated files (serving, screenshots, validators, image optimization) | none (our code, our updates) | websites vertical + polish |
| **L2** — arbitrary toolchain | deps installation, builds, generated scripts | **container, mandatory** — Docker/Podman (Linux), Colima/Lima/Apple container (macOS) | future verticals |

Dependency installation or any build step is the L2 trigger — `npm install` is arbitrary code execution (postinstall + supply chain) and is never run outside a container. The platform refuses to bind an L2 tool palette to an L0/L1 provider (see ProductProfile).

**Contracts:** path confinement (workspace paths cannot escape the root — canonicalize, then verify); `materialize → mutate → snapshot` round-trips exactly; a workspace is never authoritative — destroying one at any moment loses nothing committed.

**Known L1 residuals (accepted at T1, mitigated):** the screenshot step executes generated JS in headless Chromium — request interception restricts it to the preview origin; published JS runs in visitors' browsers — mitigated by the no-external-resources rule enforced as CSP at serve time (see PreviewGateway).

**Used by:** harness (its entire tool surface *is* a Workspace), ProjectStore (materialize/snapshot).
**Backing:** v-min `LocalWorkspaceProvider` (a directory per project, no `exec`). Container provider added when the first L2 vertical exists.

## 6. ProjectStore

**Responsibility.** Owns *what the project is*: immutable, content-addressed versions traceable to the events that produced them. Exists because the log is the wrong shape for "give me the site as of yesterday" and workspaces are explicitly disposable — spike-01 demonstrated empirically that restore must live here (an honest agent cannot reconstruct history it never saw). Sole source for sandbox rebuilds, restore, and publishing.

```ts
interface ProjectStore {
  commit(projectId, workspace, producedBy: EventRef): VersionId
  materialize(projectId, versionId, into: Workspace): void
  listVersions(projectId): VersionMeta[]   // parent link, producing event, timestamp
}
```

**Contracts:** versions are immutable; content-addressed (identical content ⇒ identical id); every version records its producing event (and thus run, session, actor); `materialize(commit(w))` reproduces `w` byte-for-byte; deleting a project deletes its versions (tenant erasure path).

**Used by:** harness (commit after successful change), SandboxProvider (open-from-version), restore command, PreviewGateway.
**Backing:** content-addressed directories on local disk + version rows in SQLite; object store + Postgres rows at cluster scale.

## 7. BlobStore

**Responsibility.** Owns big dumb bytes — uploaded assets, screenshots, exports. Exists so the log and metadata stay small, fast, and cheap to back up, and so large data has its own lifecycle and retention. Events and versions reference blobs; they never embed them.

```ts
interface BlobStore {
  put(bytes, meta): BlobRef
  get(ref): Stream
  delete(ref): void
}
```

**Used by:** API (asset upload — R-WEB-4: uploads go here, never "into the chat"), harness (via refs), export.
**Backing:** filesystem in v-min; S3-compatible at scale; interface unchanged.

## 8. PreviewGateway — **implemented M2**

**Responsibility.** Owns serving untrusted generated sites to browsers — preview and published — and the atomic publish/rollback pointer. Exists so the trust boundary between *the product* and *user-generated content* (origins, CSP, static-only) is enforced in exactly one place, and so publish is a first-class reversible act rather than a scripted file copy.

**Implementation (`internal/serving` + `internal/publish`):** a second `http.Server` on `:8081` (origin-isolated from the API on `:8080`; zero `/v1` routes) streams a version's files straight from the content-addressed store — no workspace materialization on the read path — under a strict static-only CSP (`default-src 'self'; connect-src 'none'; object-src 'none'; …`). `publish`/`rollback` are single-statement pointer-table flips; rollback walks `versions.parent_id` (the restore primitive). Preview access is a per-project capability secret in the URL (T1 posture — real per-user auth and per-site origins arrive at T2, the same boundary the shared-origin concession expires at). Export streams a `zip` of a version.

```ts
interface PreviewGateway {
  previewUrl(projectId, versionId): URL     // auth-gated
  publish(projectId, versionId): URL        // atomic pointer flip
  rollback(projectId): URL                  // = publish(previous version)
}
```

**Contracts:** publish/rollback are atomic — a visitor sees the old version or the new, never a mix; serving is static-only for static profiles, with the profile's CSP injected (this is where "no external network resources" is enforced on visitors' behalf); preview URLs require authentication; published origin never shares cookies/auth with the product UI.

**T1-only concession (explicit, expires at T2):** v-min serves preview + published sites from a second port on the same host (cheap origin isolation). The moment a second untrusted human exists (T2), per-site origins/domains are mandatory.

**Used by:** API (publish/rollback commands → events), visitor traffic directly.

## 9. API layer

**Responsibility.** Owns the contract with every client: authentication, tenant scoping, idempotent commands, one event stream. Deliberately a *thin translation* onto the other components — its existence enforces headlessness (the web app has no private path the CLI lacks) and places tenancy enforcement at a single door instead of sprinkled through business logic. If a feature needs more than translation here, a component is missing.

**Surface:** REST commands (every mutation carries `Idempotency-Key`; replays return the original result) + `GET /sessions/{id}/events?after=N` SSE; bearer-token auth; tenant scoping middleware on every route; the web client is static assets served from the same binary, consuming only this API.

**Contracts:** no mutation without an idempotency key; no route without a tenant scope; nothing reachable by the bundled web client that is not reachable by a third-party client with the same token.

## 10. ProductProfile — **implemented M3**

**Responsibility.** Owns the definition of a vertical *as data*: prompts, tool palette, artifact policy, execution level, vocabulary. Exists so a new product is configuration rather than a fork — and so restrictions live where the platform can *enforce* them rather than where the model is politely asked to behave.

**Implementation (`internal/profile`):** `Websites()` is the M3 vertical — L0 execution level, file-tools-only palette, static-only CSP, explicit `SiteLanguage` (substituted into the system prompt; never inferred, per spike-01). `ValidatePalette()` runs at run start and refuses any palette containing an execution tool below L2 — capability-by-construction, not prompt request. The CSP flows to the PreviewGateway; the reference web client (`web/` → `internal/webui`) is the profile's front end, a thin consumer of the public API served at `/`.

```ts
interface ProductProfile {
  id, version
  systemPrompt: string
  toolPalette: ToolName[]                  // must be satisfiable by the Workspace level
  executionLevel: "L0" | "L1" | "L2"
  artifactPolicy: {staticOnly: bool, cspTemplate, maxSizeBytes}
  siteLanguage: "explicit-setting"          // never inferred (spike-01 finding)
  validators: ValidatorRef[]                // L1 trusted tooling
  vocabulary: {...}                         // user-facing language of the product
}
```

**Contracts:** the platform refuses to start a run whose palette exceeds the profile's execution level or the bound provider's capabilities; profile versions are recorded on every run (reproducibility); a profile cannot grant what the platform layer forbids.

**Backing:** embedded config in v-min (the websites profile ships in the binary); a registry when third-party verticals exist.

## 11. IdentityService (minimal) — **implemented M1**

**Responsibility.** Owns *who is calling*. Tiny in v-min — token mint/verify/revoke plus per-tenant budget and quota queries — but exists from day one so every authorization decision has a subject. Retrofitting identity under a system that assumed "the one user" is a rewrite; carrying `tenant_id` on every row from the start is a column.

**Surface (`internal/tenant`):** `Create`, `CreateToken` (plaintext shown once, SHA-256 at rest), `RevokeToken`, `Authenticate`, `CheckBudget` (daily token limit, UTC-midnight window — the R-LLM-5 hard stop, called from the ModelGateway), `TenantOfRun`. CLI: `creo tenant new|ls`, `creo token new|revoke` (local, operate on the data dir). Auth is mandatory on every `/v1` route; `serve --insecure` (loopback-only) maps to the default tenant for dev. Tokens are the tenant principal in M1; user objects arrive with the human-login surface (M4, deferral D1).

### Human login design (decided 2026-08-04, resolves PRD open question #5)

The pluggable part is **login, not tokens**. An `Authenticator` driver answers exactly one question — *which human just proved themselves* — as a discrete authentication event; the IdentityService (ours, fixed, not pluggable) then maps that to a local user row and mints a **Creo-native session/token**, identical in format regardless of driver. No external token ever flows past the login step; the rest of the platform sees exactly one principal artifact.

```ts
interface Authenticator {                       // pluggable — the ONLY pluggable part
  beginLogin(input): Challenge                  // e.g. account picker, or OIDC redirect
  completeLogin(response): VerifiedIdentity     // subject + display info, nothing more
}
// fixed pipeline: VerifiedIdentity → local user row → Creo session/token
```

**Drivers (two, no zoo):**
- **`static`** — a few manually precreated accounts that trust each other (passwordless account-switch). This is the **permanent T1 answer**, not a stopgap: a family install must never require running an IdP. Production code with a real account-switch UX. Also the e2e-test driver (tests authenticate through the real seam — auth is never mocked around) and the dev driver: `serve --insecure` collapses into a `static` config with one dev account instead of a bespoke code path.
- **`oidc`** — the one external protocol (OAuth alone is not an identity protocol). Covers hosted IdPs (Google, Microsoft) and everything self-hosters actually run (Keycloak, Authentik, Authelia, Pocket ID, Dex). Mandatory from T2 up. SAML/LDAP/SCIM: not until an enterprise operator pays for it (R-SEC-5).

**Rules:**
- **Local user rows are canonical; the IdP only authenticates.** Events, approvals, and tenant membership reference Creo's user ID; the IdP subject (`iss`+`sub`) is a link stored on the user row. An IdP migration must never orphan attribution history — external identity is an input, never the source of truth (the §3.1 authority ruling, applied to people).
- **Authz stays entirely Creo-side.** The IdP answers *who*, never *what they may touch*; no group→tenant mapping.
- **Deployment pairing:** OIDC redirect flows want HTTPS — fine over Tailscale (`ts.net` certs), awkward on plain-HTTP LAN. So: LAN → `static`, tailnet/T2+ → `oidc`. The degraded-transport case and the simple driver are the same case.
- The T1 honesty caveat stands regardless of driver (PRD §5.3): under `static`, approvals attribute intent but do not authenticate the human.

## 12. ToolBroker (dormant in v-min)

**Responsibility.** Owns privileged operations on external services, with credentials that generated content can never touch — the mechanical form of "no secrets in sandboxes, ever." Deliberately an empty seat at the table in v-min: nothing at L0/L1 requires external credentials (LLM keys live in the ModelGateway; publish is internal). Reserved now because inserting a credential boundary *after* integrations exist means migrating live secrets. Activates with L2 verticals or third-party integrations (domains, form destinations).

---

## Composition — life of one message

```
API: POST message (Idempotency-Key) ─▶ SessionLog.append(user.message)
                                    ─▶ RunCoordinator.requestRun          (dedup here)
worker: claim ─▶ SessionLog.read (context) ─▶ AgentHarness.run
  loop: ModelGateway.complete  ⇄  Workspace.read/write                    (budget + capability checks)
        SessionLog.append(events, lease)                                  (fencing here)
  done: ProjectStore.commit ─▶ SessionLog.append(artifact.version.created)
        PreviewGateway.previewUrl ─▶ SessionLog.append(preview.ready)
clients: SessionLog.subscribe ─▶ SSE     (every device renders the same stream)
user: POST publish ─▶ PreviewGateway.publish ─▶ SessionLog.append(publish.completed)
```

Every arrow crosses exactly one interface — the property that lets v-min be one process today and services later without any arrow changing.
