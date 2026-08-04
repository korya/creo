# Creo task runner. `just` on its own lists every recipe.
#
# These recipes wrap the raw commands in AGENTS.md rather than replacing them —
# a fresh checkout still works with plain `go` and `npm` if `just` isn't around.
#
# The pairing to know: `check` FIXES in place (local dev), `check-ci` only
# REPORTS and never writes (CI gate). Both cover Go and TypeScript; run
# `check-go` / `check-ts` when you only touched one side.

set shell := ["bash", "-c"]

data := "./data"

[doc("List available recipes")]
default:
    @just --list

[doc("Run the server locally (fake model by default; pass a spec for a real one)")]
run model="fake:site":
    #!/usr/bin/env bash
    # Sources the gitignored .env so `just run anthropic:claude-sonnet-5` picks
    # up ANTHROPIC_API_KEY. --insecure maps unauthenticated requests to
    # t_default so the web client works without a site key; it refuses to bind
    # anything but loopback.
    set -euo pipefail
    if [ -f .env ]; then set -a; source .env; set +a; fi
    go run ./cmd/creo serve --data {{ data }} --model "{{ model }}" --insecure

[doc("Fix formatting and lint issues everywhere (Go + TypeScript)")]
check: check-go check-ts

[doc("Verify everything without writing a file — the CI gate")]
check-ci: check-go-ci check-ts-ci

[doc("Go: format, tidy, vet — fixing in place")]
check-go:
    gofmt -w .
    go mod tidy
    go vet ./...

[doc("Go: verify only — fails if gofmt or go mod tidy would change anything")]
check-go-ci:
    #!/usr/bin/env bash
    # The tidy check is what stops go.mod's dependency graph drifting out of
    # sync with what the code actually imports. `go mod tidy` has no --check
    # mode and always writes, so snapshot both files, compare, and restore via
    # a trap — this recipe must leave the tree exactly as it found it, and must
    # not depend on git state (uncommitted go.mod edits are not a CI failure).
    set -euo pipefail
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
        echo "gofmt would rewrite:" >&2
        echo "$unformatted" >&2
        exit 1
    fi
    go vet ./...
    snapshot="$(mktemp -d)"
    cp go.mod go.sum "$snapshot/"
    trap 'cp "$snapshot/go.mod" "$snapshot/go.sum" . ; rm -rf "$snapshot"' EXIT
    go mod tidy
    if ! diff -q "$snapshot/go.mod" go.mod >/dev/null || ! diff -q "$snapshot/go.sum" go.sum >/dev/null; then
        echo "go mod tidy would change go.mod/go.sum — run 'just check-go'" >&2
        exit 1
    fi

[doc("TypeScript: format, lint, type-check via Vite+ — fixing in place")]
check-ts:
    cd web && npm run check:fix

[doc("TypeScript: verify only")]
check-ts-ci:
    cd web && npm run check
