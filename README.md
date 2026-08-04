# Creo

**Anyone can create and maintain real software by describing what they want —
and truly own the result, on infrastructure they control.** (`VISION.md`)

Creo is a self-hostable, headless agent platform for no-code app building: an
AI agent builds and evolves projects on a server you control; any client (web,
CLI, mobile) attaches to the same durable session and continues. This repo
contains the open-source core (Go) and the product/architecture docs.

**Status: M0 — the spine.** Durable event-sourced sessions, crash-safe run
coordination, a working agent harness, and a CLI. The acceptance test: kill
the server with SIGKILL mid-run, restart it, and the run resumes from the log
and completes. No web UI, auth, or publishing yet — see `PRD.md` §9 for the
roadmap (M1 tenancy → M2 publish → M3 websites vertical → M5 self-host release).

## Quickstart

```sh
go build -o creo ./cmd/creo

# no API key needed — scripted fake model:
./creo serve --data ./data --model fake:site &
./creo project new my-site            # prints project + session ids
./creo say  <SESSION_ID> "build me a site"
./creo watch <SESSION_ID>             # live event stream

# real model:
export ANTHROPIC_API_KEY=sk-ant-...
./creo serve --data ./data --model anthropic:claude-sonnet-5
```

The M0 demo (kill -9 mid-run against a real model):

```sh
set -a; source .env; set +a
./scripts/demo-m0.sh
```

## Documents

- `VISION.md` — the North Star
- `PRD.md` — product requirements, trust tiers, milestones
- `docs/architecture.md` — decisions in force, event model, run coordination
- `docs/components.md` — the component catalog with testable contracts
- `AGENTS.md` — conventions for contributors (human or agent)

## Tests

```sh
go test ./...        # includes e2e: spawns the binary, SIGKILLs it, verifies resume
```
