# Blueprint: move `web/` onto Vite+ (`vp`) for build, test, lint, format, check

## Headline

Adopt Vite+ as the single frontend toolchain for `web/` — installed as a **devDependency**
(no `curl | bash`), driving build, test, lint, format, and typecheck through one `vp`
CLI and one config file. The migration is viable and cheap (`src/*.ts` needs **zero**
formatting changes), it **cuts our devDependencies from 4 to 3 and our npm
vulnerabilities from 5 to 0** — but it has one trap that must be closed deliberately:
**`vp build` does not typecheck**, so the type-safety gate we get today from
`tsc && vite build` is silently lost unless we enable it in config.

## Approach summary

- Install `vite-plus` as a devDependency; drive everything through `node_modules/.bin/vp`.
  No global binary, no runtime/package-manager takeover (`vp env`/`vp install` unused).
- Keep the existing `vite.config.ts` as the single unified config — but import
  `defineConfig` from `vite-plus` (not `vite`) and add a `lint` block.
- **Restore the type gate explicitly**: `lint: { options: { typeAware: true, typeCheck: true } }`,
  and make `npm run build` run `vp check` before `vp build`.
- Drop `vite` and `typescript` as direct deps (they become transitive); keep `vitest`
  (global test types) and `jsdom` (test environment) — both verified still required.
- Keep the documented command names (`npm run build`, `npm test`) so `AGENTS.md`'s
  build order and the Go embed workflow are unchanged.

## Plan

1. **`web/package.json`** — swap deps and scripts.
   - Remove: `vite`, `typescript` (now transitive via `vite-plus`).
   - Keep/upgrade: `vitest` (^2.1.8 → ^4.1.10), `jsdom` (^25 → ^30). Add `vite-plus` ^0.2.7.
   - Scripts:
     ```json
     "dev":   "vp dev",
     "build": "vp check && vp build",
     "test":  "vp test",
     "check": "vp check",
     "fmt":   "vp fmt",
     "lint":  "vp lint"
     ```
     `vp check &&` is the load-bearing part — see Assumption 3.
2. **`web/vite.config.ts`** — change the import to `from "vite-plus"` and add
   `lint: { options: { typeAware: true, typeCheck: true } }`. Everything else
   (`base: "./"`, `outDir: "../internal/webui/dist"`, proxy, `test`) is unchanged
   and verified working.
3. **Formatting pass** — run `vp fmt`. `src/*.ts` produces zero diff; `index.html`
   and `package.json` change (see Open Question 1). Commit the reformat *separately*
   from the tooling change so the tooling diff stays reviewable.
4. **Fix the 7 type-aware findings** — `vp check` reports 7 `no-floating-promises`
   warnings in `src/app.ts` (unawaited `send()`, `loadHome()`, `bootstrap()`,
   `refreshPreview()` in event handlers). These are real: a rejected promise in a
   listener is an unhandled rejection today. Fix with `void`/`.catch()` per site.
5. **Rebuild + verify the embed** — `npm run build` → `internal/webui/dist`,
   then `go build ./cmd/creo && go test ./...`. Commit the regenerated `dist`
   (it is committed by convention so `go build` works on a fresh checkout).
6. **`AGENTS.md`** — update the "Web client (`web/`)" bullet: name Vite+ as the
   toolchain, list the new commands, and record that `npm run build` type-checks
   via `vp check`. Note `jsdom`/`vitest` are retained deliberately.
7. **`.gitignore`** — no change needed, but record *why* it matters: oxlint/oxfmt
   scope themselves using `.gitignore` (Assumption 5).

## Assumptions validated

Every item below was executed against a real copy of `web/` in the scratchpad
(`vite-plus@0.2.7`, Node v24.19.0, npm 11.17.0).

1. **Vite+ is free and open source, usable without a licence.** *Corrected prior:* I
   remembered it as a paid, source-available commercial product — that was the
   Oct-2025 announcement. Current: "fully open source under the MIT license and
   framework-agnostic" ([beta announcement](https://voidzero.dev/posts/announcing-vite-plus-beta)).
   **Confirmed — but see Risk 1: it is beta, v0.2.7, pre-1.0.**
2. **`vp` runs from a devDependency; no `curl | bash`.** `npm i -D vite-plus` created
   `node_modules/.bin/vp` → `vp v0.2.7`. The docs' shell installer is optional.
   **Confirmed.**
3. **`vp build` does NOT typecheck — the gate must be restored.** Injected
   `const broken: number = "definitely not a number"` into `src/api.ts`:
   `tsc --noEmit` → `error TS2322`; **`vp build` built successfully** and
   **`vp check src` passed**. With `lint.options.typeAware + typeCheck` enabled,
   `vp check` reports `x typescript(TS2322): Type 'string' is not assignable to
   type 'number'` as a hard error (exit 1). **Confirmed — this is the finding that
   shapes the plan.**
4. **`defineConfig` must come from `vite-plus`.** Importing from `"vite"` with
   typecheck on yields `TS2769: No overload matches this call` on the `test:`/`lint:`
   keys. Switching the import → `Found 0 errors`. **Confirmed.**
5. **Lint scoping comes from `.gitignore`, not `.oxlintrc.json`.** With no
   `.gitignore`, `vp check` walked `node_modules` — 3,626 files / 19,229 warnings.
   An `.oxlintrc.json` with `ignorePatterns` did **not** help (3,483 files). Adding
   a `.gitignore` with `node_modules/` fixed it → 5 files. The repo already ignores
   `node_modules/` and `web/node_modules/`, so `vp check` needs no path args here.
   **Confirmed (via a refuted first attempt).**
6. **Our code is clean under oxlint, and oxfmt agrees with our TypeScript.**
   `vp lint src` → zero findings. `vp fmt` on `src/*.ts` → **zero diff**.
   `index.html` → 1155 diff lines (compact one-line CSS rules expanded);
   `package.json` → key reorder. **Confirmed.**
7. **Tests pass on Vitest 4.** `vp test` ran Vitest 4.1.10 against our 13 tests
   (2 files) using `environment: "jsdom", globals: true` from the config — all pass,
   despite our being pinned to Vitest 2.1.9. **Confirmed.**
8. **`vitest` and `jsdom` must remain direct deps.** With only `vite-plus`:
   `vp test` → "no tests, 4 errors" (jsdom missing) and `vp check` →
   `Cannot find type definition file for 'vitest/globals'`. Restoring both →
   all green. **Confirmed (refuted the "one dep replaces all" hope).**
9. **The Rolldown bundle is byte-different but behaviourally identical.**
   12.51 kB vs 13.22 kB (≈5% smaller), new content hash. Served the Rolldown
   output through `creo serve --web-dir` and loaded it in a browser: renders
   pixel-identical, and a full functional pass (chip → `user.message` → build
   events → `previewState: "ready"` → publish enabled) works. **Confirmed.**
10. **The Go embed is agnostic to asset filenames.** `internal/webui/webui.go:22-46`
    serves whatever is in `dist` with an SPA fallback; `index.html` references the
    hashed asset. Hash changes are a non-event. **Confirmed by reading the source.**
11. **Security posture improves.** Current `web/`: `5 vulnerabilities (3 moderate,
    1 high, 1 critical)` — esbuild `<=0.24.2` (GHSA-67mh-4wv8-2f99) via
    `vitest`/`vite-node`. Vite+ tree: `found 0 vulnerabilities`. **Confirmed.**

## Project conventions to follow

- **[`AGENTS.md:49-50`]** Convention: *"Stdlib-first. Three external deps… Adding a
  dependency is an architecture decision, not a convenience."* → **Reflected in plan:**
  Step 1 is net **dependency-reducing** for the JS side (4 direct → 3) and removes 5
  CVEs. The rule names Go deps, but its spirit is honoured: `vp` replaces four tools
  with one, and we decline its optional runtime/package-manager surface.
- **[`AGENTS.md:66-72`]** Convention: *web client is TypeScript, npm (Node 24), Vite,
  vitest; `cd web && npm run build` → `internal/webui/dist` (committed); tests
  `cd web && npm test`.* → **Reflected in plan:** Steps 1, 5, 6 keep npm and the
  `npm run build` / `npm test` entry points and the committed-dist workflow exactly;
  only what runs *underneath* changes. Step 6 updates this bullet to name Vite+.
- **[`AGENTS.md:34-41`]** Convention: canonical Go commands; *"`go vet ./... && gofmt -l .`
  both must be clean before committing."* → **Reflected in plan:** Step 5 runs
  `go build` + `go test ./...` after regenerating `dist`. The Go side is otherwise
  untouched — `vp` never runs Go.
- **[`AGENTS.md:74`]** Convention: *one reviewable commit per component/step;
  imperative subject prefixed with the area.* → **Reflected in plan:** Steps 1-2
  (tooling), 3 (reformat), 4 (promise fixes), 5 (dist), 6 (docs) land as separate
  `web:`/`docs:` commits — deliberately splitting the 1155-line reformat from the
  logic change.
- **[`AGENTS.md:76-83`]** Testing policy: *unit tests co-located; fake model adapter
  keeps tests deterministic.* → **Doesn't constrain this plan** — it governs Go tests;
  the vitest suite is co-located already (`src/*.test.ts`) and Step 1 preserves it.
- **[`AGENTS.md:58`]** Ports: API `127.0.0.1:8080`. → **Reflected in plan:** the dev
  proxy block in `vite.config.ts` keeps `:8080` verbatim (Step 2 changes only the
  import and adds `lint`).
- **Package manager**: `web/package-lock.json` + `npm 11.17.0` → **npm**, confirmed
  from the lockfile, not assumed. Step 1 keeps npm; `vp` auto-detects it.
- **Lint/format tooling today**: none — no eslint/prettier/biome config exists in the
  repo. → **Reflected in plan:** Steps 3-4 introduce the *first* lint/format gate, which
  is why the warning triage in Step 4 is part of the work rather than a follow-up.
- **Migration tooling**: no schema change → **Doesn't constrain this plan.**
- **CI**: none (`.github/` absent). → **Reflected in plan:** commands are written to be
  CI-ready (single `npm run check`), but adding CI is Out of Scope.

## Risks & mitigations

1. **Vite+ is beta at v0.2.7 on a build-critical path.** Its own announcement says
   *"Vite+ is stable, but not yet complete"* and *"complex projects may still need
   manual follow-up."* A pre-1.0 minor could break our build. *Mitigation:* pin an
   exact version (no `^`) in `package.json`, keep `package-lock.json` committed, and
   keep `dist/` committed — a broken `vp` upgrade can never block `go build` or a
   release, only local rebuilds. Rollback is a one-line revert of `package.json` +
   `vite.config.ts`.
2. **Losing the type gate silently** (Assumption 3). *Mitigation:* Step 1's
   `"build": "vp check && vp build"` plus Step 2's config. Add a regression guard:
   the test plan's manual step re-injects a type error and asserts `npm run build`
   fails.
3. **Vitest 2 → 4 and jsdom 25 → 30 are major upgrades** riding along. All 13 tests
   pass today, but our suite is small — it may not exercise what changed.
   *Mitigation:* they are also the *fix* for the 5 CVEs, so staying put has its own
   cost. Step 5's full `go test ./...` plus the browser pass covers the integration.
4. **oxfmt reformats `index.html` (1155 lines).** Verified cosmetic — the reformatted
   file rendered pixel-identical in the browser — but it is churn on a file written
   this session. *Mitigation:* Open Question 1; a separate commit either way.

## Out of scope

- Adopting `vp env` (Node version manager) or `vp install` (package-manager wrapper) —
  npm + system Node stay as-is.
- `vp run` task-caching / monorepo features — one package, no need.
- Adding CI (`.github/workflows`). Worth doing, but a separate decision.
- Any change to Go tooling, `gofmt`/`go vet`, or the Go build.
- Extracting `index.html`'s inline CSS into a stylesheet (see Open Question 1).

## Open questions

1. **`index.html` formatting.** oxfmt expands our compact one-line CSS rules
   (`.brand { display: flex; align-items: center; gap: 9px; }` → 5 lines), a 1155-line
   diff. Options: **(a)** accept it — conventional CSS style, one-time churn, formatter
   never argued with again; **(b)** add `index.html` to a `.prettierignore` to preserve
   the dense design-system style; **(c)** extract the CSS into `src/styles.css` (cleaner
   anyway, but scope creep). *My recommendation: **(a)**.* Fighting a formatter on one
   file creates a permanent exception, and the rendering is provably identical.
2. **The 7 floating-promise warnings** — fix them (Step 4, my recommendation: they are
   genuine unhandled-rejection paths) or downgrade the rule to off?
3. **Beta tolerance.** Are you comfortable putting a v0.2.7 pre-1.0 tool on the build
   path given the mitigations in Risk 1? If not, the honest alternative is to *stay on
   plain Vite* and just upgrade vitest/jsdom to clear the CVEs — a much smaller change
   that gets ~half the benefit (security, not unification).

## Test plan

- **Unit** — `npm test` (`vp test`, Vitest 4 + jsdom): all 13 existing tests must pass
  unchanged. *Already verified in the scratchpad.*
- **Static** — `npm run check` must exit 0 (format + lint + type-aware check) after
  Steps 3-4.
- **Regression guard (manual, required)** — re-inject a type error into `src/api.ts`
  and assert `npm run build` **fails**. This is the specific regression Assumption 3
  proves is possible; it must be checked at least once by hand.
- **Integration** — `npm run build` → `go build ./cmd/creo` → `go test ./...`
  (full suite incl. e2e) must be green with the regenerated `dist`.
- **Manual/browser** — `creo serve --data /tmp/x --model fake:site --insecure`, then
  walk home → new site → build → publish → history → reload-resume, confirming the
  Rolldown bundle behaves identically. *Already verified once in the scratchpad.*
- **Supply chain** — `npm audit` must report 0 vulnerabilities after Step 1.

## Counterfactual: does the plan meet its success criteria?

**Confirmed.**

| Success criterion | Satisfied by |
|---|---|
| All frontend workflows run through Vite+ | Steps 1-2 (`dev`/`build`/`test`/`check`/`fmt`/`lint` → `vp`) |
| Type errors still fail the build | Step 1 `vp check && vp build` + Step 2 config; guarded by the regression test |
| Committed `dist` still works when embedded | Step 5; Assumptions 9-10 |
| Tests still pass | Step 1; Assumption 7 |
| No global install; reproducible from lockfile | Step 1; Assumption 2 |
| Conventions doc reflects reality | Step 6 |
