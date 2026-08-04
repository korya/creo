#!/usr/bin/env bash
# M0 acceptance demo against a real model: build a site, kill -9 the server
# mid-run, restart, and watch the run resume from the log and complete.
#
# Usage: set -a; source .env; set +a; ./scripts/demo-m0.sh [model-spec]
set -euo pipefail

MODEL="${1:-anthropic:claude-sonnet-5}"
ADDR="127.0.0.1:8090"
DATA="$(mktemp -d /tmp/creo-demo.XXXXXX)"
BASE="http://$ADDR"

cd "$(dirname "$0")/.."
go build -o /tmp/creo-demo-bin ./cmd/creo

serve() {
  /tmp/creo-demo-bin serve --addr "$ADDR" --data "$DATA" --model "$MODEL" --lease-ttl 5s &
  SERVER_PID=$!
  until curl -sf "$BASE/healthz" >/dev/null 2>&1; do sleep 0.2; done
}
events() { curl -sf "$BASE/v1/sessions/$SESSION/events?stream=false"; }
count()  { events | grep -o "\"type\":\"$1\"" | wc -l | tr -d ' '; }

echo "==> starting server ($MODEL, data: $DATA)"
serve

echo "==> creating project"
CREATE=$(curl -sf -X POST "$BASE/v1/projects" -d '{"name":"demo-bakery"}')
PROJECT=$(echo "$CREATE" | sed 's/.*"id":"\([^"]*\)".*/\1/')
SESSION=$(echo "$CREATE" | sed 's/.*"sessionId":"\([^"]*\)".*/\1/')
echo "    project=$PROJECT session=$SESSION"

echo "==> submitting request"
curl -sf -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Idempotency-Key: demo-k1" \
  -d '{"text":"Create a small website for a bakery called Kastanja in Haarlem: a home page and an about page, warm handmade feel, local SVG placeholders for images."}' >/dev/null

echo "==> waiting for the run to make progress..."
until [ "$(count tool.result)" -ge 2 ]; do sleep 0.5; done
echo "    $(count tool.result) tool results in the log — KILLING SERVER WITH SIGKILL"
kill -9 "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true

echo "==> restarting server (same data dir)"
serve

echo "==> waiting for the resumed run to complete..."
for _ in $(seq 1 240); do
  [ "$(count run.completed)" -ge 1 ] && break
  sleep 1
done
[ "$(count run.completed)" -ge 1 ] || { echo "FAILED: run did not complete"; exit 1; }

echo
echo "==> RESULT"
echo "    run.resumed events : $(count run.resumed) (want 1)"
echo "    final message      : $(events | tr ',' '\n' | grep -A0 'run.completed' >/dev/null; curl -sf "$BASE/v1/sessions/$SESSION/events?stream=false" | python3 -c 'import json,sys; evs=json.load(sys.stdin); print([e["userText"] for e in evs if e["type"]=="run.completed"][0])')"
echo "    versions           : $(curl -sf "$BASE/v1/projects/$PROJECT/versions" | grep -o '"id"' | wc -l | tr -d ' ')"
echo "    workspace files    : $(ls "$DATA/workspaces/$PROJECT" | tr '\n' ' ')"
echo
echo "M0 demo passed: the run survived kill -9 and completed from the log."
kill "$SERVER_PID" 2>/dev/null || true
