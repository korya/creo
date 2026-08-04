# Spike 01 — Code-gen edit reliability & annotation scope

**Status:** **COMPLETED 2026-08-03** — ran in under a day, ~$4.50 of tokens. Verdicts: H1 confirmed (37/38 tasks, tight scope, compounding holds), H2 not confirmed (annotations gave no reliability gain; the one deterministic edit was the campaign's only self-contradicting failure — ship node-IDs for click-targeting only, skip content.json indirection), H3 confirmed (loop ≈100 lines, first-try). Full scoreboard and product findings: `spikes/01-harness/RESULTS.md`.
**Decides:** PRD §3.1 annotation scope · architecture §11.1 language lean · solution-options assumption A2 (harness effort)

## Reframed question

The original bake-off ("semantic model vs code-gen as canonical state") is moot — code-canonical is locked. What remains open, and what this spike measures:

1. **H1 — Reliability:** a lightweight code-gen harness handles realistic non-coder *edit* requests on an existing site without breaking unrelated parts. (The crux: creation demos always look good; *iteration* is where code-gen builders get sloppy.)
2. **H2 — Annotation payoff:** adding derived structure — stable `data-node-id` attributes on visible elements plus an externalized content index (`content.json`) — measurably improves targeted-edit reliability and enables token-free content edits, at acceptable harness complexity.
3. **H3 — Effort:** the harness loop (context → model → tools → build → screenshot → repair) is weeks of work, not months. The spike harness *is* the estimate: if a throwaway version takes >3 days to get coherent, that's data.

## Arms

Both arms: same model, same system-prompt skeleton, same tool loop (`read_file`, `write_file`, `patch`, `build`, `screenshot`), fresh context per task with the current site state provided.

- **Arm A — pure code-gen.** Site is plain HTML/CSS/JS files; edits located by the model reading source.
- **Arm B — annotated code-gen.** Conventions enforced by prompt + a post-write validator: every visible element carries a stable `data-node-id`; user-visible text/images live in `content.json` and are injected at build. Two extra behaviors: (a) click-targeted tasks include the selected node ID in the prompt; (b) pure content edits bypass the LLM entirely — a deterministic `set_content(nodeId, value)` path — and we record which tasks qualified.

## Task scripts

Two scenarios to avoid overfitting; tasks run **sequentially** on the same evolving site (that's the point — compounding edits, not one-shots).

**S1 — bakery site ("Kastanja", ~12 tasks):** create initial 3-page site from a paragraph brief → change opening hours (content-only) → swap hero image (content-only) → add a testimonials section below services → move testimonials above the gallery → "make the whole thing feel warmer and more handmade" (global restyle) → add a Menu page and put it in the navigation → click-target: "make this smaller" on the hero heading → "it looks cramped on my phone" (responsive, vague) → remove the gallery → "put the gallery back like it was" (restore) → out-of-scope probe: "let customers order and pay online" (must decline gracefully in user language, not attempt a backend).

**S2 — photographer portfolio (~8 tasks):** create from brief → "make it look like a hand-drawn zine" (the anti-Wix stressor — schema-free expressiveness is why we chose code-canonical; prove it) → add a contact section → click-target: replace a specific photo → reorder portfolio categories → "less text everywhere" (global, destructive-leaning) → dark mode → "undo the zine thing, back to minimal."

## Scoring (per task, both arms)

| Metric | How |
|---|---|
| Success | 0 / partial / 1 — did the requested change happen, judged from screenshot + DOM |
| **Collateral damage** | primary metric: unrelated changes (visual diff of untouched pages/sections + source diff outside the expected scope) |
| Compounding integrity | after each task, do all *previous* changes still hold? |
| Tokens & wall time | from gateway logs; note which Arm B tasks cost ~0 (deterministic path) |
| Quality/distinctiveness | human (Dmitri) 1–5, especially on the zine task |
| Failure language | did errors surface as plain language or stack traces (R-AGT-2 dry run) |

## Exit criteria

- **Adopt Arm B conventions** if targeted+content edits reach ≥90% success with near-zero collateral and the annotation machinery stayed thin (≤ ~1 day of the build).
- **Ship Arm A + click-annotations only** if `content.json` indirection confuses the model or buys <10% reliability.
- **Escalate** if *both* arms show heavy collateral damage on S1's compounding sequence — that reopens harness strategy (solution-options: Temporal-backed D for repair loops, or heavier validation stage), before M0 commits.
- H3 verdict recorded either way: actual days spent on the throwaway harness → M0 estimate.

## Implementation notes

- Throwaway code under `spikes/01-harness/` — explicitly not the production harness; conclusions and prompt library carry forward, code does not.
- Model: one reference model for both arms (Sonnet-class for cost; one confirmation pass of key tasks on a Fable/Opus-class model to check the ceiling). Provider access via env; requires an API key.
- Build/screenshot: static server + Playwright screenshot per page per task; screenshots archived per task for scoring.
- Keep a `RESULTS.md` scoreboard filled during runs, not reconstructed after.
