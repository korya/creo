# Creo — Deferral Ledger

**Status:** live document, started 2026-08-04
**Rule:** when a milestone ships without something its scope implied, the gap is recorded here — in the same commit as the milestone's "delivered" note. An entry leaves the ledger only by being **done** (link the commit) or **explicitly re-accepted** (owner + new owed-by milestone, in writing). The M5 release gate (PRD §9) requires this ledger to be empty or every remaining entry re-accepted.

Why this file exists: M1 deferred the storage quota "to M2"; M2 shipped without it and nothing noticed. Deferrals that live only inside milestone prose evaporate. (Verified 2026-08-04: no storage-quota or egress enforcement anywhere in `internal/`.)

## Open deferrals

| # | What | Requirement | Deferred at | Owed by | Status (verified 2026-08-06) |
|---|------|-------------|-------------|---------|------------------------------|
| ~~D1~~ | ~~Human users & login surface~~ | R-API-3, PRD open question #5 | M1 | M4 | **CLOSED at M4** (`b1c0ce4`, `f3e8dbd`, `cc5937b`): `internal/identity` — pluggable `Authenticator`, `static` driver, `users`/`user_identities`/`web_sessions`, cookie auth, `events.actor` attribution |
| ~~D2~~ | ~~Storage quota~~ | R-TEN-3 | M1, missed at M2 | M4 | **CLOSED at M4** (`a7cf67c`): CAS-aware per-tenant accounting enforced in `ProjectStore.Commit` before any bytes are written |
| D3 | Egress + CPU quotas for sandboxes | R-TEN-3, §5.1 network layer | M1 ("moot at L0") | **Trigger-based:** the first L2 vertical / container provider. Not owed by a date; owed by the trigger — the container provider PR that doesn't include egress policy is wrong | Dormant by design (L0 has no sandbox network to police). Legitimate, but tracked so L2 can't ship without it |
| D4 | Per-site origins/domains for preview + published content (currently: second port on same host) | R-PUB-3; components.md §8 "T1-only concession (expires at T2)" | M2 (explicit concession) | **Trigger-based: T2** — the moment a second untrusted human exists | Open by design. Tracked because "expires at T2" is a promise with no enforcement mechanism |
| D5 | Preview auth = capability secret in the URL (leaks via history/shoulder-surfing/shared links) | R-PUB-2 | M2 (T1 posture) | **Trigger-based: T2** — real per-user auth on preview URLs (depends on D1) | Open by design; same expiry as D4 |
| D6 | `oidc` Authenticator driver — password/IdP-backed login. Until it exists, the only human login is the passwordless picker, which is honest at T1 and refused above it (the startup fence enforces this, so the gap cannot be shipped past silently). | PRD open question #5; `components.md` §11 | M4 (2026-08-05, deliberate) | **M5** — the Server profile is T2, and T2 requires proven identity | Open. Needs `go-oidc` + `oauth2` (a dependency decision under the stdlib-first rule) and an IdP to test against; `mockoidc` is the intended test fixture |
| D7 | Asset upload (R-WEB-4) — a user cannot yet add their own images; the agent creates SVG placeholders instead. | R-WEB-4 (MUST) | Never formally scoped; surfaced by the M4 review | **M5** — a bakery site with no photo of the bakery is a hard limit on the north-star claim | Open. `BlobStore` exists; the API surface and client flow do not |

## Closed

| # | What | Closed at | Evidence |
|---|------|-----------|----------|
| D1 | Human users & login surface | M4 | `internal/identity`, `internal/e2e/login_test.go` |
| D2 | Per-tenant storage quota | M4 | `tenant.CheckStorage`, `internal/e2e/storage_test.go` |

## Watched (not deferrals — SHOULDs expected to promote themselves)

- **Checkpoint/compaction (R-SES-6, SHOULD).** VISION promises "maintain a site for years"; linear replay + context growth will make this a MUST around the first genuinely long-lived project. Watch resume cost metrics.
- **Audit-log vs. tenant-erasure tension (R-SEC-2 vs R-TEN-4).** Deleting a tenant's transcripts deletes the security audit trail of what their projects did. Needs a retention decision (e.g. pseudonymized security-relevant events survive erasure) before M5 docs make promises in either direction.
