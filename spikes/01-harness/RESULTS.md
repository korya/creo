# Spike 01 — scoreboard (completed 2026-08-03)

Model: `claude-sonnet-5`. 40 tasks total (S1 12×2 arms, S2 8×2 arms). Full campaign cost ≈ **$4.50** in tokens, ~75 min wall clock. Raw data: `results/*/log.jsonl`, screenshots per task.

Success: 0 / ½ / 1 · Collateral: none / minor / major · Compounding: do earlier edits still hold after this one?

## S1 bakery — Arm A (pure code-gen)

| # | Type | Success | Collateral | Compounding | Turns | Tokens in/out | Sec | Notes |
|---|------|---------|-----------|-------------|-------|----------------|-----|-------|
| 1 | create | 1 | — | — | 18 | 110k/12.7k | 139 | Coherent warm site; localized copy to Dutch unprompted (Haarlem in brief) |
| 2 | content | 1 | none | ✓ | 4 | 16k/3.1k | 31 | Hours updated correctly in the hours block |
| 3 | content | 1 | none | ✓ | 5 | 20k/3.7k | 41 | New hero SVG created and wired |
| 4 | edit | 1 | none | ✓ | 5 | 29k/5.7k | 53 | Testimonials added below services |
| 5 | edit | 1 | none | ✓ | 4 | 12k/2.6k | 27 | Moved; 1 file touched |
| 6 | edit (global) | 1 | none (intended global) | ✓ | 6 | 74k/16.4k | 170 | Visibly warmer: handwritten headings, textures, dashed borders |
| 7 | edit | 1 | none | ✓ | 7 | 92k/11.6k | 101 | Menu page + nav updated on all pages |
| 8 | click | 1 | none | ✓ | 4 | 26k/5.8k | 47 | Hero heading visibly smaller |
| 9 | edit (responsive) | 1 | none | ✓ | 5 | 37k/6.8k | 61 | Mobile shots readable; not deeply audited |
| 10 | edit (remove) | 1 | none | ✓ | 7 | 63k/8.2k | 80 | Gallery gone from all pages |
| 11 | edit (restore) | **0** | none | ✓ | 3 | 11k/0.4k | 28 | **Refused to fabricate; asked what the gallery looked like. Correct behavior — restore needs platform versioning, not an LLM** |
| 12 | probe (decline) | 1 | none | ✓ | 3 | 6k/0.5k | 14 | Declined payments in plain language, offered pickup-form alternative |

## S1 bakery — Arm B (annotated)

| # | Type | Success | Collateral | Det. | Turns | Tokens in/out | Notes |
|---|------|---------|-----------|------|-------|----------------|-------|
| 1 | create | 1 | — | — | 5 | 31k/11.5k | 3.5× cheaper than Arm A's create (n=1, may be variance) |
| 2 | content | **0** | **major** | hit (wrong) | 0 | 0/0 | **Token-free path hit `hours-heading` via fuzzy match: section title replaced with hours string, real hours list left stale — page self-contradicts** |
| 3 | content | 1 | none | miss→agent | 4 | 16k/3.3k | Miss was correct: asset didn't exist yet; agent fallback succeeded |
| 4–12 | | 1 except #11 (0, same as A) | none | — | 3–8 | similar to A | No convention violations logged; content.json stayed valid all 12 tasks |

## S2 portfolio — Arm A / Arm B

| # | Type | A | B | Notes |
|---|------|---|---|-------|
| 1 | create | 1 | 1 | Minimal, elegant, correct |
| 2 | zine | 1 (distinctiveness **5/5**) | 1 | Torn paper, tape, typewriter/hand-drawn type — impossible under a fixed section schema; the code-canonical decision earns its keep here |
| 3 | contact | 1 | 1 | |
| 4 | click swap | 1 | 1 (det. miss→agent, correct) | A resolved the described target correctly without node IDs |
| 5 | reorder (accidental no-op) | 1 | 1 | Both honestly reported "already in that order, no change" — no phantom edits |
| 6 | less text | 1 | 1 | |
| 7 | dark mode | 1 | 1 | |
| 8 | un-zine, keep dark | 1 | 1 | Full aesthetic reversal, dark kept, swapped photo + reduced text preserved |

## Verdicts

**H1 — code-gen edit reliability: CONFIRMED.** 37/38 agent-run tasks succeeded with tight change scope (1–3 files on scoped edits; larger only where legitimate) and coherent compounding across 12 sequential edits including a global restyle and a full aesthetic reversal. The single failure (gallery restore) is a *platform* gap — a fresh-context agent has no version history and correctly refused to fabricate one. This is direct evidence for artifact versions as the restore mechanism (PRD R-WEB-5), not against code-gen.

**H2 — annotation payoff: NOT CONFIRMED as designed; reframe.** Arm B showed no reliability gain over Arm A — Arm A matched it on every edit including click-targeted ones resolved from plain descriptions. The token-free deterministic path fired once and produced the campaign's only wrong *and* self-contradicting result, because (a) fuzzy key resolution picked the wrong node and (b) "opening hours" spans multiple nodes, so a single-key edit can't be consistent. Conventions were followed faithfully (content.json valid throughout), so the mechanism works — it's the payoff that's absent at this model tier, since LLM content edits cost only ~$0.05 and ~30 s anyway.
**Recommendation:** v1 ships **Arm A + stable `data-node-id` annotations for click-targeting only** (exact IDs passed from the UI — never pattern-resolved). Skip the `content.json` indirection; revisit deterministic editing only with exact node identity from the click UI plus structured content groups.

**H3 — harness effort: CONFIRMED "weeks, not months".** The agent loop itself was ~100 lines and worked on the first run; the whole spike (loop + tools + build + screenshots + task runner) was under a day. Solution-options assumption A2 holds; Option A (custom harness) stands, Temporal retreat not needed on this evidence.

**Language-decision input:** nearly all spike machinery beyond the loop was JS-native (cheerio, Playwright, static serving) — the websites vertical's validator/preview tooling will live in that ecosystem regardless of core language. Strengthens TS for the *vertical*; the *core* remains a separate call.

## Product findings (feed into PRD/architecture)

1. **Version restore must be a platform primitive** — artifact versions + "restore" in user language. Both arms independently proved an LLM cannot and should not do this. (Validates PRD §3.6, R-WEB-5; raises priority of R-SES-7 session/version history.)
2. **Click-to-edit requires exact node identity end-to-end.** Any fuzzy resolution between user intent and node is where wrongness enters silently.
3. **Site language must be an explicit vertical setting** — the model inferred Dutch from "Haarlem". Charming until it isn't.
4. **The static-only guardrail held via prompt, but that's model goodwill** — the payments probe was declined perfectly, yet enforcement must still be structural (sandbox/artifact policy, PRD R-WEB-2).
5. **Sonnet-class is sufficient for the websites vertical MVP**: ~$0.02–0.35 per operation, 15–170 s. An Opus-class ceiling pass remains optional tuning, not a necessity.
