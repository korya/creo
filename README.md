# Creo

**Anyone can create and maintain real software by describing what they want —
and truly own the result, on infrastructure they control.** (`VISION.md`)

Creo is a self-hostable, headless agent platform for no-code app building: an
AI agent builds and evolves projects on a server you control; any client (web,
CLI, mobile) attaches to the same durable session and continues. This repo
contains the open-source core (Go) and the product/architecture docs.

**Status: M2 — artifacts & publish** (on the M0 spine + M1 tenancy). Durable
event-sourced sessions, crash-safe run coordination, the agent harness,
bearer-token auth with per-tenant isolation and budgets, plus preview URLs,
one-command publish/rollback to a live site (origin-isolated, static-only CSP),
and project export. The M0 acceptance test still holds: SIGKILL the server
mid-run, restart, the run resumes and completes. No web UI yet — see `PRD.md`
§9 (M3 websites vertical → M5 self-host release).

## Quickstart

```sh
go build -o creo ./cmd/creo

# dev loop — no token ceremony (loopback only):
./creo serve --data ./data --model fake:site --insecure &   # API :8080, sites :8081
./creo project new my-site            # prints project + session ids
./creo say  <SESSION_ID> "build me a site"
./creo watch <SESSION_ID>             # live event stream
./creo preview <PROJECT_ID>           # preview URL for the latest version
./creo publish <PROJECT_ID>           # -> live URL on :8081
./creo rollback <PROJECT_ID>          # revert to the previous version
./creo export  <PROJECT_ID> -o site.zip

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
