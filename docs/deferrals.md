# Creo — Deferral Ledger

**Status:** live document, started 2026-08-04
**Rule:** when a milestone ships without something its scope implied, the gap is recorded here — in the same commit as the milestone's "delivered" note. An entry leaves the ledger only by being **done** (link the commit) or **explicitly re-accepted** (owner + new owed-by milestone, in writing). The M5 release gate (PRD §9) requires this ledger to be empty or every remaining entry re-accepted.

Why this file exists: M1 deferred the storage quota "to M2"; M2 shipped without it and nothing noticed. Deferrals that live only inside milestone prose evaporate. (Verified 2026-08-04: no storage-quota or egress enforcement anywhere in `internal/`.)

## Open deferrals

| # | What | Requirement | Deferred at | Owed by | Status (verified 2026-08-04) |
|---|------|-------------|-------------|---------|------------------------------|
| D1 | Human users & login surface — user objects, login, per-user attribution. Bearer tokens are still the *tenant* principal; there is no "who" below the tenant. | R-API-3, PRD open question #5 (resolved 2026-08-04: pluggable `Authenticator` + fixed IdentityService, `static`/`oidc` drivers — design in `components.md` §11) | M1 ("deferred to M3/M4") | **M4** — cross-device approvals (AC-5) are meaningless without a subject | Open. `internal/tenant` has tokens only, no users |
| D2 | Storage quota — per-tenant artifact/blob storage limits | R-TEN-3 | M1 ("deferred to M2") | **Re-owed: M4** (was M2; M2 shipped without it) | Open. No enforcement in code; grew a sibling: `artifactPolicy.maxSizeBytes` is declared in `components.md` §10 but unimplemented |
| D3 | Egress + CPU quotas for sandboxes | R-TEN-3, §5.1 network layer | M1 ("moot at L0") | **Trigger-based:** the first L2 vertical / container provider. Not owed by a date; owed by the trigger — the container provider PR that doesn't include egress policy is wrong | Dormant by design (L0 has no sandbox network to police). Legitimate, but tracked so L2 can't ship without it |
| D4 | Per-site origins/domains for preview + published content (currently: second port on same host) | R-PUB-3; components.md §8 "T1-only concession (expires at T2)" | M2 (explicit concession) | **Trigger-based: T2** — the moment a second untrusted human exists | Open by design. Tracked because "expires at T2" is a promise with no enforcement mechanism |
| D5 | Preview auth = capability secret in the URL (leaks via history/shoulder-surfing/shared links) | R-PUB-2 | M2 (T1 posture) | **Trigger-based: T2** — real per-user auth on preview URLs (depends on D1) | Open by design; same expiry as D4 |

## Watched (not deferrals — SHOULDs expected to promote themselves)

- **Checkpoint/compaction (R-SES-6, SHOULD).** VISION promises "maintain a site for years"; linear replay + context growth will make this a MUST around the first genuinely long-lived project. Watch resume cost metrics.
- **Audit-log vs. tenant-erasure tension (R-SEC-2 vs R-TEN-4).** Deleting a tenant's transcripts deletes the security audit trail of what their projects did. Needs a retention decision (e.g. pseudonymized security-relevant events survive erasure) before M5 docs make promises in either direction.
