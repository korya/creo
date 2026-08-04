# Creo

**Anyone can create and maintain real software by describing what they want —
and truly own the result, on infrastructure they control.** (`VISION.md`)

Creo is a self-hostable, headless agent platform for no-code app building: an
AI agent builds and evolves projects on a server you control; any client (web,
CLI, mobile) attaches to the same durable session and continues. This repo
contains the open-source core (Go) and the product/architecture docs.

**Status: M1 — tenancy & safety** (on top of the M0 spine). Durable
event-sourced sessions, crash-safe run coordination, the agent harness, plus
bearer-token auth, structural per-tenant isolation, hard daily token budgets,
per-tenant run quotas, and a hostile-project containment test. The M0
acceptance test still holds: SIGKILL the server mid-run, restart, the run
resumes from the log and completes. No web UI or publishing yet — see `PRD.md`
§9 (M2 publish → M3 websites vertical → M5 self-host release).

## Quickstart

```sh
go build -o creo ./cmd/creo

# dev loop — no token ceremony (loopback only):
./creo serve --data ./data --model fake:site --insecure &
./creo project new my-site            # prints project + session ids
./creo say  <SESSION_ID> "build me a site"
./creo watch <SESSION_ID>             # live event stream

# with auth (production shape):
./creo serve --data ./data --model fake:site &
TENANT=$(./creo tenant new acme --daily-tokens 500000 | awk '{print $2}')
export CREO_TOKEN=$(./creo token new "$TENANT" | grep creo_)
./creo project new my-site            # now authenticated via CREO_TOKEN

# real model:
export ANTHROPIC_API_KEY=sk-ant-...
./creo serve --data ./data --model anthropic:claude-sonnet-5 --insecure
```

Put a reverse proxy (Caddy, Tailscale) in front for TLS and remote access;
`--insecure` binds loopback only and maps unauthenticated requests to the
default tenant — never expose it to a network.

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
