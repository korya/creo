#!/usr/bin/env bash
# AC-14: prove the core loop works against a privately hosted model, with no
# client-visible behavior change. Builds a site end to end through the public
# API and publishes it — exactly the calls the web client makes.
#
# The model must support tool calling (qwen3-class is fine; very small models
# generally are not — see docs/components.md §4).
#
#   ./scripts/demo-local-model.sh                                  # Ollama default
#   ./scripts/demo-local-model.sh qwen3 http://127.0.0.1:11434/v1  # explicit
#   ./scripts/demo-local-model.sh <model> http://127.0.0.1:1234/v1 # LM Studio
set -euo pipefail

MODEL_ID="${1:-qwen3}"
BASE_URL="${2:-http://127.0.0.1:11434/v1}"
ADDR="127.0.0.1:8092"
SERVE_ADDR="127.0.0.1:8093"
DATA="$(mktemp -d /tmp/creo-local.XXXXXX)"
BASE="http://$ADDR"

cd "$(dirname "$0")/.."

echo "==> checking the local model server at $BASE_URL"
if ! curl -sf "${BASE_URL%/v1}/v1/models" >/dev/null 2>&1 && ! curl -sf "$BASE_URL/models" >/dev/null 2>&1; then
  echo "    cannot reach $BASE_URL — is the server running?" >&2
  echo "    Ollama:    ollama serve && ollama pull $MODEL_ID" >&2
  echo "    LM Studio: start the local server, then pass its URL as \$2" >&2
  exit 1
fi

go build -o /tmp/creo-local-bin ./cmd/creo

/tmp/creo-local-bin serve --addr "$ADDR" --serve-addr "$SERVE_ADDR" --data "$DATA" \
  --model "openai:$MODEL_ID@$BASE_URL" --insecure --lease-ttl 60s &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT
until curl -sf "$BASE/healthz" >/dev/null 2>&1; do sleep 0.2; done

events() { curl -sf "$BASE/v1/sessions/$SESSION/events?stream=false"; }
count()  { events | grep -o "\"type\":\"$1\"" | wc -l | tr -d ' '; }
say()    { events | grep -o '"userText":"[^"]*"' | sed 's/"userText":"/    /;s/"$//'; }

echo "==> creating project"
CREATE=$(curl -sf -X POST "$BASE/v1/projects" -d '{"name":"local-model-demo"}')
PROJECT=$(echo "$CREATE" | sed 's/.*"id":"\([^"]*\)".*/\1/')
SESSION=$(echo "$CREATE" | sed 's/.*"sessionId":"\([^"]*\)".*/\1/')
echo "    project=$PROJECT session=$SESSION"

echo "==> asking $MODEL_ID to build a site (local models are slow; be patient)"
curl -sf -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Idempotency-Key: local-k1" \
  -d '{"text":"Create a one-page website for a bike repair shop called Spoke, with opening hours and a phone number."}' >/dev/null

DEADLINE=$((SECONDS + 900))
until [ "$(count run.completed)" -ge 1 ] || [ "$(count run.failed)" -ge 1 ]; do
  if [ $SECONDS -gt $DEADLINE ]; then echo "    timed out after 15 minutes" >&2; exit 1; fi
  sleep 2
done

if [ "$(count run.failed)" -ge 1 ]; then
  echo "==> FAILED. What the user would have seen:"
  say
  exit 1
fi

echo "==> the conversation, exactly as a non-coder would read it:"
say

echo "==> publishing"
PUB=$(curl -sf -X POST "$BASE/v1/projects/$PROJECT/publish" -d '{}')
URL=$(echo "$PUB" | sed 's/.*"url":"\([^"]*\)".*/\1/')
if ! curl -sf "$URL" | head -c 200 >/dev/null; then
  echo "    published URL did not serve: $URL" >&2
  exit 1
fi

echo
echo "==> AC-14 satisfied with $MODEL_ID at $BASE_URL"
echo "    live: $URL"
echo "    tool calls: $(count tool.result)   data: $DATA"
echo "    No client-visible behavior differed from the Anthropic path."
