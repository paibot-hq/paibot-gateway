#!/usr/bin/env bash
# End-to-end tests against a running Paigate in front of a real model.
#
# Expects Paigate serving e2e/paigate.json on $GW with its database
# migrated, and Ollama serving qwen2.5:0.5b behind it. $PAIGATE is how to
# run the paigate command (default ./paigate). Codex and Claude Code are
# tried when they are installed.
set -euo pipefail

GW=${GW:-http://127.0.0.1:8080}
PAIGATE=${PAIGATE:-./paigate}
CONFIG=${CONFIG:-e2e/paigate.json}
SYNC=6 # keys and usage reach the database within 5 s

failures=0
ok() { echo "ok    $1"; }
bad() { echo "FAIL  $1"; failures=$((failures + 1)); }
check() { # name, command...
  local name=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi
}

newkey() { $PAIGATE key create -config "$CONFIG" "$@" | grep -o 'pg_[A-Za-z0-9_-]*'; }

# post PATH KEY BODY [curl args]: sets $status and $r to the response.
post() {
  local path=$1 key=$2 body=$3
  shift 3
  local out
  out=$(curl -sS -m 300 -w '\n%{http_code}' "$GW$path" -H "Authorization: Bearer $key" \
    -H 'Content-Type: application/json' "$@" -d "$body")
  status=${out##*$'\n'}
  r=${out%$'\n'*}
}

suffix=$(date +%s)
K=$(newkey -models chat,claude "alice-$suffix")
B=$(newkey -models chat -budget 1 "bob-$suffix")
T=$(newkey -models chat "carol-$suffix")
sleep $SYNC

say='"messages":[{"role":"user","content":"Say hello in one word."}]'

# Chat Completions
post /v1/chat/completions "$K" "{\"model\":\"chat\",$say}"
check "chat: answers" test "$status" = 200
check "chat: has text" jq -e '.choices[0].message.content | length > 0' <<<"$r"
check "chat: reports usage" jq -e '.usage.prompt_tokens > 0' <<<"$r"

post /v1/chat/completions "$K" "{\"model\":\"chat\",\"stream\":true,\"stream_options\":{\"include_usage\":true},$say}"
check "chat stream: answers" test "$status" = 200
check "chat stream: ends with [DONE]" grep -q '^data: \[DONE\]' <<<"$r"
check "chat stream: carries usage" grep -q '"usage":{' <<<"$r"

# Responses API
post /v1/responses "$K" '{"model":"chat","input":"Say hello in one word."}'
check "responses: answers" test "$status" = 200
check "responses: completed" jq -e '.status == "completed"' <<<"$r"
check "responses: reports usage" jq -e '.usage.input_tokens > 0' <<<"$r"

post /v1/responses "$K" '{"model":"chat","input":"Say hello.","stream":true}'
check "responses stream: completes" grep -q 'response.completed' <<<"$r"

post /v1/responses "$K" '{"model":"chat","input":"x","store":true}'
check "responses: refuses store:true" test "$status" = 400

# Anthropic Messages API
anth=(-H "x-api-key: $K" -H 'anthropic-version: 2023-06-01')
post /v1/messages "$K" "{\"model\":\"claude\",\"max_tokens\":32,$say}" "${anth[@]}"
check "messages: answers" test "$status" = 200
check "messages: has text" jq -e '.content[0].text | length > 0' <<<"$r"
check "messages: reports usage" jq -e '.usage.input_tokens > 0' <<<"$r"

post /v1/messages "$K" "{\"model\":\"claude\",\"max_tokens\":32,\"stream\":true,$say}" "${anth[@]}"
check "messages stream: stops" grep -q 'message_stop' <<<"$r"

# Keys
post /v1/chat/completions pg_wrong "{\"model\":\"chat\",$say}"
check "wrong key: 401" test "$status" = 401
post /v1/messages "$B" "{\"model\":\"claude\",\"max_tokens\":8,$say}" -H 'anthropic-version: 2023-06-01'
check "model not allowed: 404" test "$status" = 404

# Budget: one token a month, so the first request uses it up.
post /v1/chat/completions "$B" "{\"model\":\"chat\",$say}"
check "budget: first request served" test "$status" = 200
post /v1/chat/completions "$B" "{\"model\":\"chat\",$say}"
check "budget: then refused" test "$status" = 429
check "budget: says why" jq -e '.error.code == "insufficient_quota"' <<<"$r"

# Revocation reaches the gateway within the sync interval.
post /v1/chat/completions "$T" "{\"model\":\"chat\",$say}"
check "revoke: key works before" test "$status" = 200
$PAIGATE key revoke -config "$CONFIG" "carol-$suffix" >/dev/null
sleep $SYNC
post /v1/chat/completions "$T" "{\"model\":\"chat\",$say}"
check "revoke: key refused after" test "$status" = 401

# Usage lands in the database, as totals only.
sleep $SYNC
u=$($PAIGATE usage -config "$CONFIG")
check "usage: recorded for alice" grep -q "alice-$suffix" <<<"$u"

# Real clients, when installed.
if command -v codex >/dev/null; then
  home=$(mktemp -d)
  out=$(cd "$home" && CODEX_HOME="$home" PAIGATE_KEY="$K" codex exec --skip-git-repo-check --sandbox read-only \
    -c model_provider=paigate \
    -c "model_providers.paigate={name=\"Paigate\",base_url=\"$GW/v1\",env_key=\"PAIGATE_KEY\",wire_api=\"responses\"}" \
    -m chat "Reply with just the word hello." </dev/null 2>&1) && rc=0 || rc=$?
  check "codex: answers through Paigate" test "$rc" = 0
  [ "$rc" = 0 ] || echo "$out" | tail -20
else
  echo "skip  codex: not installed"
fi

if command -v claude >/dev/null; then
  home=$(mktemp -d)
  limit=""
  if command -v timeout >/dev/null; then limit="timeout 600"; fi
  out=$(cd "$home" && CLAUDE_CONFIG_DIR="$home" ANTHROPIC_BASE_URL="$GW" ANTHROPIC_AUTH_TOKEN="$K" \
    ANTHROPIC_MODEL=claude ANTHROPIC_DEFAULT_HAIKU_MODEL=claude ANTHROPIC_SMALL_FAST_MODEL=claude \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    $limit claude -p "Reply with just the word hello." </dev/null 2>&1) && rc=0 || rc=$?
  check "claude code: answers through Paigate" test "$rc" = 0
  [ "$rc" = 0 ] || echo "$out" | tail -20
else
  echo "skip  claude code: not installed"
fi

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures failed"
  exit 1
fi
echo "all passed"
