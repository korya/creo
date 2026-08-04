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

[doc("Build the web client, then the binary (order matters — Go embeds dist)")]
build: build-ts build-go

[doc("Build the creo binary into ./creo")]
build-go:
    go build ./cmd/creo

[doc("Build the web client into internal/webui/dist (type-checks first)")]
build-ts:
    cd web && npm run build

[doc("Run the full test suite (Go + TypeScript)")]
test: test-go test-ts

[doc("Go tests, including e2e — which builds and spawns the binary itself")]
test-go:
    go test ./...

[doc("Go tests without the subprocess/e2e ones — the fast inner loop")]
test-short:
    go test ./... -short

[doc("Web client tests (vitest + jsdom)")]
test-ts:
    cd web && npm test

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
    # `go mod tidy -diff` (Go 1.23+) prints what tidy would change and exits
    # non-zero without touching go.mod/go.sum — so this stays verify-only and
    # independent of git state (uncommitted go.mod edits are not a CI failure).
    set -euo pipefail
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
        echo "gofmt would rewrite:" >&2
        echo "$unformatted" >&2
        exit 1
    fi
    go vet ./...
    go mod tidy -diff

[doc("TypeScript: format, lint, type-check via Vite+ — fixing in place")]
check-ts:
    cd web && npm run check:fix

[doc("TypeScript: verify only")]
check-ts-ci:
    cd web && npm run check
