# Spike 01 harness — throwaway code

Protocol and scoring: `docs/spikes/spike-01-codegen-edit-reliability.md`. This code does not carry into the product.

## Setup

```sh
cd spikes/01-harness
npm install
npx playwright install chromium
```

Requires `ANTHROPIC_API_KEY` in the repo-root `.env` (loaded via `node --env-file`).

## Run

```sh
npm run run -- --arm A --scenario s1-bakery            # all 12 tasks, sequential
npm run run -- --arm B --scenario s1-bakery
npm run run -- --arm A --scenario s2-portfolio
npm run run -- --arm A --scenario s1-bakery --from 3 --to 5   # resume/subset
SPIKE_MODEL=claude-opus-5 npm run run -- ...                  # ceiling check
```

Default model: `claude-sonnet-5` (per protocol; one confirmation pass of key tasks on `claude-opus-5`).

## Outputs

- `work/<arm>/<scenario>/` — the evolving site source (state carries across tasks; delete to restart a scenario)
- `results/<arm>/<scenario>/task-NN/site/` — built output snapshot after each task
- `results/<arm>/<scenario>/task-NN/shots/` — desktop + mobile screenshots per page
- `results/<arm>/<scenario>/log.jsonl` — per task: tokens, turns, tool calls, written paths, duration, Arm B deterministic hits/misses and convention issues

## Scoring

Fill `RESULTS.md` during runs. Collateral damage = diff written paths + visually diff screenshots of pages the task should not have touched (`git diff --no-index results/.../task-04/site results/.../task-05/site` works well).
