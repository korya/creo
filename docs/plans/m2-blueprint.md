# M2 Blueprint — artifacts & publish

**Status:** validated plan, awaiting approval · 2026-08-04
**Headline:** Serve every artifact version at a preview URL and promote one to a live URL with atomic publish/rollback, on an origin-isolated second port with a strict CSP — plus project export — proving the PreviewGateway component and the R-PUB requirements.

## Problem statement

- **Goal (PRD §9 M2):** artifact versions (exist since M0) + preview URLs + publish/rollback with built-in serving + export. Demo: describe → preview → publish → rollback, from the CLI.
- **Constraints:** origin isolation (served content on a different port from the product API — `docs/components.md` §8 T1 concession); static-only CSP enforced at serve time (R-PUB-3); publish/rollback atomic (a visitor sees old or new, never a mix); serve directly from the content-addressed store (no workspace materialization on the read path); auth unchanged on :8080.
- **Non-goals:** custom domains / TLS (reverse-proxy documented), publish adapters (S3/Netlify — R-PUB-5 LATER), per-user preview auth (T1 uses capability URLs; real auth arrives at T2 with per-site origins).
- **Success criteria:** (S1) an artifact version is fetchable at a stable preview URL on :8081; a wrong/rotated secret 404s. (S2) `publish` promotes a version to a public live URL; the pointer flip is atomic. (S3) `rollback` serves the previous version; publish/rollback emit events to the project's session. (S4) every served response carries a strict CSP that forbids external resources. (S5) `export` returns a valid zip of a version's files. (S6) served content never shares the product origin; the serving port has no product API routes.

## Hypothesis (and its falsifier)

New `internal/serving` package (a second `http.Server` on :8081 that reads `version_files` + CAS blobs directly, sets CSP, gated by a per-project preview secret for `/preview/…` and public for `/sites/…`); a `published` pointer table + preview-secret column via migration 0003; publish/rollback/export handlers on the existing :8080 API; server wires both listeners. Serving reuses `ProjectStore`'s existing `version_files`→CAS mapping — no new storage.

**Falsifier:** if preview/publish needed to *materialize* a workspace per request (stateful, slow) rather than stream from CAS, the "serve from the content store" shape is wrong. Checked (A1): `version_files(project_id, version_id, path, blob_sha)` + `cas/<sha>` already give a direct path→bytes lookup; serving is a read, no workspace involved.

## Ordered steps

1. **Migration `0003_publish.sql`** — `published(project_id PK, version_id, slug UNIQUE, published_at)`; `ALTER TABLE projects ADD COLUMN preview_secret TEXT NOT NULL DEFAULT ''` (backfilled lazily on first preview URL request, or at project create going forward).
2. **`internal/project` read helpers** — `VersionFiles(ctx, projectID, versionID) []{path, blobSha, size}` and `BlobPath(sha)` so serving streams bytes without importing store internals. Keep CAS dir private; expose an `Open(sha) io.ReadCloser`.
3. **`internal/publish`** — `Store` over the pointer table: `EnsurePreviewSecret(projectID)`, `Publish(ctx, projectID, versionID, slug)`, `Rollback(ctx, projectID) (newVersion, error)` (find current's parent via `versions.parent_id`), `Current(ctx, projectID)`, `BySlug(ctx, slug)`. Publish/rollback are single-statement upserts (atomic). Unit tests: publish then rollback walks the version lineage; slug uniqueness.
4. **`internal/serving`** — `Gateway` = `http.Server`. Routes: `GET /preview/{project}/{secret}/{version}/{path...}` (constant-time secret check → 404 on mismatch), `GET /sites/{slug}/{path...}` (public → resolves slug→current version), `GET /healthz`. Both stream a file from CAS with content-type by extension and a strict CSP header; directory/empty path → `index.html`. Path within a version is validated (no traversal). CSP default: `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'self'` — self-contained sites only.
5. **API (:8080) additions** — `POST /v1/projects/{id}/publish` (body `{versionId?}`, default latest) → publish + append `publish.completed` to the project's session, returns the live URL; `POST /v1/projects/{id}/rollback` → append `publish.rolled_back`; `GET /v1/projects/{id}/preview?version=<id>` → returns the preview URL (mints/uses the secret); `GET /v1/projects/{id}/export?version=<id>` → streams a zip. All tenant-scoped (foreign = 404). Publish URL base comes from server config.
6. **Server wiring** — second `http.Server` on `--serve-addr` (default `127.0.0.1:8081`); both started in `Run`, both shut down on ctx cancel; publish base URL passed to the API.
7. **CLI** — `creo publish SESSION_ID [--version V]`, `creo rollback SESSION_ID`, `creo export SESSION_ID [--version V] -o out.zip`, `creo preview SESSION_ID [--version V]` (prints URL). Session→project resolved server-side.
8. **e2e (`internal/e2e/publish_test.go`)** — build (fake:site) → publish → GET live URL = 200 with the built HTML + CSP header (S2,S4) → make a 2nd version (say again) → publish v2 → live serves v2 → rollback → live serves v1 (S3) → export returns a zip whose entries match the version (S5) → preview URL serves, wrong secret 404 (S1) → assert :8081 has no `/v1/*` route (S6).
9. **Docs** — README publish flow; AGENTS ports/serving note; PRD M2 delivered; components.md §8 status.

## Assumptions validated

| # | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | `version_files(project_id, version_id, path, blob_sha, size)` + `cas/<sha>` give direct path→bytes; no materialize needed to serve | Confirmed | `0001_init.sql` version_files schema; `project/store.go` Commit writes CAS blobs named by sha, Materialize reads them |
| A2 | `versions.parent_id` gives rollback its target (previous version) | Confirmed | `0001_init.sql` versions.parent_id; `project/store.go` Commit sets parent to prior latest |
| A3 | A project has a resolvable session to attach publish events to | Confirmed | createProject inserts one session per project (`api.go`); sessions.project_id join exists |
| A4 | stdlib `archive/zip` + `net/http` FileServer-style streaming suffice | Confirmed | stdlib; no dep needed |
| A5 | Two `http.Server`s in one process shut down cleanly on ctx | Confirmed | M0 server already runs one via goroutine + Shutdown; second is the same pattern |
| A6 | No in-flight work / drift | Confirmed | clean tree at `349023a` |

## Spec cross-validation

- **R-PUB-1** (one-click publish to live HTTPS URL; instant publish/rollback) → steps 3,5 (TLS via reverse proxy — documented, step 9). **R-PUB-2** (preview URLs, access-protected, revocable, stable across sandbox replacement) → step 4 (capability-secret; stable because keyed by version, not workspace). **R-PUB-3** (served content isolated: separate origin, static-only, no path to control plane) → steps 4,6 (:8081 has zero `/v1` routes; CSP static-only). **R-WEB-5 / restore** → rollback is the restore primitive (step 3), the spike-01 finding realized. **R-TEN-5** (export in open formats) → step 5. Trust tier: T1 concession (path-based preview auth, shared serving origin across projects on one port) is explicit in components.md §8 and re-stated in step 4; expires at T2.

## Project conventions to follow

- **[`AGENTS.md` "Every /v1 route is tenant-scoped… foreign = 404"]** → step 5 scopes publish/rollback/export/preview; step 8 adds the cross-tenant case.
- **[`AGENTS.md` "Credentials never enter workspaces" / one-authority]** → PreviewGateway owns serving only; publish pointer in its own package; no blurring.
- **[`AGENTS.md` "Ports: :8081 reserved for served sites (M2)"]** → step 6 uses exactly :8081; this plan fulfills the reservation.
- **[`AGENTS.md` "Stdlib-first"]** → steps 4,5 use `archive/zip`, `net/http`, `mime` — no new deps.
- **[`AGENTS.md` migrations]** numbered SQL under `internal/store/migrations/` → step 1 as 0003.
- **[`AGENTS.md` contracts-are-tests]** → publish/serving get unit + e2e; existing SL/RC/e2e stay green.

## Codebase conflict sweep

In-flight: none (clean tree, single author). Shadow duplication: none — no serving code exists. Caller-side drift: `server.Config` gains a field and `api.New` gains publish deps — both internal, updated in the same steps; CLI e2e helpers extended, not changed. Test infra: e2e harness reused; publish_test is a new file.

## Counterfactual — **Confirmed.** S1→4/8, S2→3/5/8, S3→3/5/8, S4→4/8, S5→5/8, S6→4/6/8.

## Risks & mitigations

- **Path traversal in served version paths** → validate the `{path...}` against the version's file list (only known paths served); no filesystem walk.
- **CSP too strict / too loose** → default blocks external origins (matches the no-external-resources profile rule) but allows `unsafe-inline` (agent writes inline styles/scripts); documented as the T1 static-site policy, tightened per-profile later.
- **Preview secret in URL history/logs** → capability-URL model, explicitly a T1 posture; rotatable via re-mint; real per-user auth at T2. Documented, not hidden.
- **Publish base URL correctness behind a proxy** → configurable `--public-url`; defaults to the serve-addr.

## Out of scope

Custom domains, TLS termination, publish adapters (S3/Netlify), per-user/session preview auth, published-site analytics, incremental/CDN caching, cache-busting.

## Open questions

None blocking — preview auth as a capability URL is the documented T1 posture (components.md §8), publish base URL is configurable. Proceeding on these defaults.

## Test plan

Unit: publish/rollback lineage walk, slug uniqueness, preview-secret constant-time check, CSP header presence, zip contents. E2E: the full build→publish→rollback→export→preview choreography (S1–S6) incl. wrong-secret 404 and the :8081-has-no-/v1 assertion. Manual: `creo publish` then open the live URL in a browser.
