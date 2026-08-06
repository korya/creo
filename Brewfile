# Developer tooling for Creo on macOS: `brew bundle` installs everything
# needed to run `just check`, `just test-full`, and `just run`.
#
# Not listed here on purpose: the web client's toolchain (vite-plus, vitest,
# jsdom) is pinned in web/package.json and installed by `npm ci`. Pinning a
# JS dependency twice — once in a lockfile, once in a Brewfile — is a way to
# have two answers to the same question.

# The Go toolchain. The exact version CI uses is read from go.mod, so keep
# this new enough to satisfy it (`go version` vs the `go` line in go.mod).
brew "go"

# Task runner. `just` on its own lists every recipe.
brew "just"

# Node for the web client, pinned to the major CI uses. Plain `node` tracks
# latest (26 at the time of writing) — running the client on a different major
# than CI is a divergence you only discover when the build breaks there.
#
# This is a keg-only formula: `brew install node@24` does not put it on PATH.
# Either link it (`brew link --overwrite --force node@24`) or let the vite-plus
# runtime provide node, which is what happens on this machine today — `node`
# resolves to ~/.vite-plus/bin/node before anything Homebrew installs.
brew "node@24"

# Linter, wired into `just check` and `just check-ci`.
#
# Version note: CI pins v2.12.2 via golangci-lint-action. Homebrew tracks
# latest, so the two can drift and a new release can fail CI on code that
# linted clean locally. If that happens, pin locally to match CI:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
brew "golangci-lint"

# GitHub CLI — PR creation and CI status from the terminal.
brew "gh"

# Optional: one e2e test reads the server's SQLite file directly to assert
# that model usage was metered. Without it that single assertion skips; the
# rest of the suite is unaffected.
brew "sqlite"
