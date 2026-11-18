#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# End-to-end check against a real running gateway and a real running fake upstream.
# This is not a unit test: it starts both processes, sends real HTTP, and reads the
# gateway's metrics page back. Every behaviour the project exists for must hold here.
#
# Usage: ./scripts/e2e.sh
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin"
WORK=$(mktemp -d)
FAIL=0
CHECKS=0

cleanup() {
  kill "${FAKE_PID:-}" "${GATEWAY_PID:-}" "${GATE_PID:-}" 2>/dev/null
  wait "${FAKE_PID:-}" "${GATEWAY_PID:-}" "${GATE_PID:-}" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAIL=1; }

check() { # name, expected, actual
  CHECKS=$((CHECKS + 1))
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1: expected $2, got $3"
  fi
}

has() { # name, pattern, haystack
  CHECKS=$((CHECKS + 1))
  if printf '%s' "$3" | grep -q -- "$2"; then
    pass "$1"
  else
    fail "$1: no $2 in $3"
  fi
}

not_has() { # name, pattern, haystack
  CHECKS=$((CHECKS + 1))
  if printf '%s' "$3" | grep -q -- "$2"; then
    fail "$1: found $2 in $3"
  else
    pass "$1"
  fi
}

stats() { curl -s "http://127.0.0.1:8091/stats" > "$WORK/stats.json"; }
count() { grep -o "\"$1\":[0-9]*" "$WORK/stats.json" | head -1 | cut -d: -f2; }
total() { grep -o '"total":[0-9]*' "$WORK/stats.json" | cut -d: -f2; }

post() { # model, output file; prints the status code
  curl -s -o "$2" -w '%{http_code}' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$1\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"e2e\"}]}" \
    http://127.0.0.1:8080/v1/chat/completions
}

printf 'building\n'
mkdir -p "$BIN"
go build -o "$BIN/plainroute" ./cmd/plainroute
go build -o "$BIN/plainroute-fakeupstream" ./cmd/fakeupstream
go build -o "$BIN/plainroute-bench" ./cmd/bench

# A process left over from an earlier run answers the checks instead of the one this script
# starts, so a run can pass for the wrong reason or fail for one that is not its own.
for port in 8080 8091 8092; do
  if (exec 9<>/dev/tcp/127.0.0.1/"$port") 2>/dev/null; then
    printf 'something is already listening on 127.0.0.1:%s. Stop it before running this script.\n' "$port" >&2
    exit 1
  fi
done

cat > "$WORK/behaviors.json" <<'JSON'
{
  "e2e-stream": {
    "status": 200,
    "sse": [
      "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
      "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
      "data: [DONE]\n\n"
    ],
    "gap": "5ms"
  },
  "e2e-tool": {
    "status": 200,
    "sse": [
      "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\"}}}]}]}\n\n",
      "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n",
      "data: [DONE]\n\n"
    ]
  },
  "e2e-429": {
    "status": 429,
    "body": "{\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\"}}"
  },
  "e2e-cancel": {
    "status": 200,
    "sse": ["data: {\"delta\":{\"content\":\"first\"}}\n\n"],
    "wait_for_cancel": true
  }
}
JSON

cat > "$WORK/config.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8080
routing:
  strategy: least_active
  cooldown:
    status: {429: 10s}
    max: 60s
routes:
  stream:
    targets:
      - name: streamer
        model: stream
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_STREAM
  two:
    targets:
      - name: account-a
        model: two
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_429
      - name: account-b
        model: two
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_STREAM
  allcooling:
    targets:
      - name: c1
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_429
      - name: c2
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_429
      - name: c3
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_429
      - name: c4
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_429
  toolcall:
    targets:
      - name: tool
        model: toolcall
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_TOOL
  cancel:
    targets:
      - name: cancellable
        model: cancel
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_CANCEL
YAML

export E2E_STREAM=e2e-stream E2E_429=e2e-429 E2E_TOOL=e2e-tool E2E_CANCEL=e2e-cancel

"$BIN/plainroute-fakeupstream" -listen 127.0.0.1:8091 -behaviors "$WORK/behaviors.json" > "$WORK/fake.log" 2>&1 &
FAKE_PID=$!
"$BIN/plainroute" -config "$WORK/config.yaml" > "$WORK/gateway.log" 2>&1 &
GATEWAY_PID=$!
sleep 1

printf '\n1. one target streaming normally\n'
status=$(post stream "$WORK/stream.txt")
check "status" "200" "$status"
cat > "$WORK/expected.txt" <<'TXT'
data: {"object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant"}}]}

data: {"object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}

data: [DONE]

TXT
if cmp -s "$WORK/stream.txt" "$WORK/expected.txt"; then
  CHECKS=$((CHECKS + 1)); pass "the stream is byte for byte, [DONE] included"
else
  CHECKS=$((CHECKS + 1)); fail "the stream changed: $(cat "$WORK/stream.txt")"
fi
has "the gateway logged the request as a stream" '"stream":true' "$(cat "$WORK/gateway.log")"

printf '\n2. target A returns 429, A cools down, no retry inside that request, the next request uses B\n'
status=$(post two "$WORK/a.txt")
check "first request status" "429" "$status"
has "the upstream error body is forwarded as it arrived" "rate limited" "$(cat "$WORK/a.txt")"
stats
check "only one upstream attempt happened during the first request" "1" "$(count e2e-429)"
status=$(post two "$WORK/b.txt")
check "the next independent request status" "200" "$status"
stats
check "A was not used again while it was cooling" "1" "$(count e2e-429)"
has "the next request went to B" 'account-b' "$(cat "$WORK/gateway.log")"
has "the cooldown was logged" '"cooldown"' "$(cat "$WORK/gateway.log")"

printf '\n3. all targets cooling: no upstream call, 429 with Retry-After\n'
for _ in 1 2 3 4; do post allcooling "$WORK/warm.txt" > /dev/null; done
stats
before=$(total)
status=$(curl -s -D "$WORK/headers.txt" -o "$WORK/cooled.txt" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"allcooling","messages":[{"role":"user","content":"e2e"}]}' \
  http://127.0.0.1:8080/v1/chat/completions)
stats
check "status" "429" "$status"
check "no upstream call while every target is cooling" "$before" "$(total)"
has "Retry-After comes from the gateway's own state" 'Retry-After: 10' "$(cat "$WORK/headers.txt")"
has "the OpenAI error shape with the routing code" 'all_targets_cooling_down' "$(cat "$WORK/cooled.txt")"
has "the gateway was ready throughout" 'ready' "$(curl -s http://127.0.0.1:8080/readyz)"

printf '\n4. tool-call streaming passes through completely\n'
status=$(post toolcall "$WORK/tool.txt")
check "status" "200" "$status"
cat > "$WORK/tool-expected.txt" <<'TXT'
data: {"object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file"}}}]}]}

data: {"object":"chat.completion.chunk","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}

data: [DONE]

TXT
if cmp -s "$WORK/tool.txt" "$WORK/tool-expected.txt"; then
  CHECKS=$((CHECKS + 1)); pass "the opening role delta and both tool call deltas survived as separate frames"
else
  CHECKS=$((CHECKS + 1)); fail "the tool call stream changed: $(cat "$WORK/tool.txt")"
fi

printf '\n5. 100+ concurrent requests: no race, no panic, no leak\n'
"$BIN/plainroute-bench" -url http://127.0.0.1:8080 -model stream -requests 140 -concurrency 140 \
  -stream > "$WORK/bench.txt" 2>&1
has "every concurrent request succeeded" '2xx=140' "$(cat "$WORK/bench.txt")"
has "no transport error" 'transport errors: 0' "$(cat "$WORK/bench.txt")"
has "client requests and upstream attempts match" 'no amplification.*ok' "$(cat "$WORK/bench.txt")"
has "the only request the gateway answered from its own state is the all-cooling one" 'plainroute_gateway_rejected_total[[:space:]]\+1' "$(cat "$WORK/bench.txt")"
has "the active counter came back to zero" 'plainroute_active_requests[[:space:]]\+0' "$(cat "$WORK/bench.txt")"
has "in flight is back to zero" 'plainroute_in_flight_requests[[:space:]]\+0' "$(cat "$WORK/bench.txt")"
not_has "nothing panicked in the gateway" 'panic' "$(cat "$WORK/gateway.log")"
not_has "no API key value appears in the gateway log" 'e2e-429' "$(cat "$WORK/gateway.log")"
not_has "no prompt content appears in the gateway log" 'e2e"' "$(cat "$WORK/gateway.log")"

printf '\n6. client disconnect: the upstream request is cancelled\n'
curl -s --max-time 2 -o "$WORK/disconnect.txt" -H 'Content-Type: application/json' \
  -d '{"model":"cancel","stream":true,"messages":[{"role":"user","content":"e2e"}]}' \
  http://127.0.0.1:8080/v1/chat/completions > /dev/null
sleep 1
stats
has "the upstream saw the cancellation" '"cancelled":\["e2e-cancel"\]' "$(cat "$WORK/stats.json")"
has "the gateway recorded the client going away" 'client_gone' "$(cat "$WORK/gateway.log")"
has "the gateway is still serving after a disconnect" 'ok' "$(curl -s http://127.0.0.1:8080/healthz)"

printf '\n7. optional api key gate: off when nothing is configured, on when it is\n'
cat > "$WORK/gate-config.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8092
routes:
  stream:
    targets:
      - name: gated
        model: stream
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_STREAM
auth:
  api_key_env: E2E_CALLER
YAML
export E2E_CALLER=e2e-caller

"$BIN/plainroute" -config "$WORK/gate-config.yaml" > "$WORK/gate.log" 2>&1 &
GATE_PID=$!
sleep 1

body_of() { # model
  curl -s -o "$WORK/gate-body.txt" -w '%{http_code}' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"e2e\"}]}" \
    http://127.0.0.1:8092/v1/chat/completions
}

check "nothing presented is rejected" "401" "$(body_of stream)"
has "the error is the OpenAI shape with the api key code" '"code":"invalid_api_key"' "$(cat "$WORK/gate-body.txt")"
check "a wrong key is rejected" "401" "$(curl -s -o "$WORK/gate-body.txt" -w '%{http_code}' -H 'Authorization: Bearer not-the-key' -H 'Content-Type: application/json' -d '{"model":"stream","messages":[{"role":"user","content":"e2e"}]}' http://127.0.0.1:8092/v1/chat/completions)"
check "the configured key is served" "200" "$(curl -s -o "$WORK/gate-body.txt" -w '%{http_code}' -H 'Authorization: Bearer e2e-caller' -H 'Content-Type: application/json' -d '{"model":"stream","messages":[{"role":"user","content":"e2e"}]}' http://127.0.0.1:8092/v1/chat/completions)"
has "the upstream key was used, not the caller key" '[DONE]' "$(cat "$WORK/gate-body.txt")"
stats
c=$(count e2e-caller)
check "the caller key never became an upstream key" "0" "${c:-0}"
check "the health endpoint is not gated" "ok" "$(curl -s http://127.0.0.1:8092/healthz)"
has "the gate is counted on the metrics page" 'invalid_api_key' "$(curl -s http://127.0.0.1:8092/metrics)"
has "the startup line reports the gate and names the variable it read" 'api_key_env' "$(head -1 "$WORK/gate.log")"
not_has "the startup line does not print the value" 'e2e-caller' "$(head -1 "$WORK/gate.log")"
not_has "the caller key value does not appear in the log" 'e2e-caller' "$(cat "$WORK/gate.log")"

kill "$GATE_PID" 2>/dev/null
wait "$GATE_PID" 2>/dev/null

printf '\n%d checks: %s\n' "$CHECKS" "$([ "$FAIL" = 0 ] && echo 'all passed' || echo 'failures')"
exit "$FAIL"
