#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# End-to-end check against a real running gateway and a real running fake upstream.
# This is not a unit test: it starts both processes, sends real HTTP, reads the gateway's
# metrics page back, and reads what the fake upstream recorded about the request it received.
# Every behaviour the project exists for must hold here.
#
# Usage: ./scripts/e2e.sh
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin"
WORK=$(mktemp -d)
FAIL=0
CHECKS=0
EXTRA_PIDS=()

cleanup() {
  kill "${FAKE_PID:-}" "${GATEWAY_PID:-}" "${GATE_PID:-}" 2>/dev/null
  wait "${FAKE_PID:-}" "${GATEWAY_PID:-}" "${GATE_PID:-}" 2>/dev/null
  for pid in ${EXTRA_PIDS[@]+"${EXTRA_PIDS[@]}"}; do
    kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
  done
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
# A key that was never used is not in the stats, so a check that expects none has to say so
# itself rather than compare an empty string.
count() { v=$(grep -o "\"$1\":[0-9]*" "$WORK/stats.json" | head -1 | cut -d: -f2); printf '%s' "${v:-0}"; }
total() { grep -o '"total":[0-9]*' "$WORK/stats.json" | cut -d: -f2; }

# What the upstream received for a key is one line per fact, which is how a check can
# tell what the gateway forwarded from what the client sent. A key that was never sent says
# so, so a check for an absent header cannot pass because the record is empty.
last() { curl -s "http://127.0.0.1:8091/last?key=$1"; }
field() { last "$1" | sed -n "s/^$2=//p" | head -1; }

send() { # url, body file, output file; prints the status code
  curl -s -o "$3" -w '%{http_code}' -H 'Content-Type: application/json' -d @"$2" "$1"
}

post_to() { # port, model, output file; prints the status code
  curl -s -o "$3" -w '%{http_code}' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt-zq\"}]}" \
    "http://127.0.0.1:$1/v1/chat/completions"
}

# Some claims need a config the running gateway cannot have at the same time: allow mode and
# round_robin are settings, not per-route settings, and a clamp of 0 is a routing setting. Each
# such case starts its own gateway so a failure points at the case that owns it.
start_gateway() { # config file, log file
  "$BIN/plainroute" -config "$1" > "$2" 2>&1 &
  EXTRA_PIDS+=("$!")
  sleep 1
}

post() { # model, output file; prints the status code
  curl -s -o "$2" -w '%{http_code}' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$1\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"prompt-zq\"}]}" \
    http://127.0.0.1:8080/v1/chat/completions
}

printf 'building\n'
mkdir -p "$BIN"
go build -o "$BIN/plainroute" ./cmd/plainroute
go build -o "$BIN/plainroute-fakeupstream" ./cmd/fakeupstream
go build -o "$BIN/plainroute-bench" ./cmd/bench

# A process left over from an earlier run answers the checks instead of the one this
# script starts, so a run can pass for the wrong reason or fail for one that is not its
# own.
for port in 8080 8091 8092 8093 8094 8095 8096 8097 8098 8099 8100 8101; do
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
  },
  "e2e-plain": {
    "status": 200,
    "body": "{\"object\":\"chat.completion\",\"choices\":[]}"
  },
  "e2e-allow": {
    "status": 200,
    "body": "{\"object\":\"chat.completion\",\"choices\":[]}"
  },
  "e2e-dist-1": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-2": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-3": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-4": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-5": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-6": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-7": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-dist-8": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-rafar": {
    "status": 429,
    "headers": { "Retry-After": "7200" },
    "body": "{\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\"}}"
  },
  "e2e-rafar-2": {
    "status": 429,
    "headers": { "Retry-After": "7200" },
    "body": "{\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\"}}"
  },
  "e2e-500": {
    "status": 500,
    "body": "{\"error\":{\"message\":\"server error\",\"type\":\"server_error\"}}"
  },
  "e2e-503": {
    "status": 503,
    "body": "{\"error\":{\"message\":\"content safety screening is temporarily unavailable\"}}"
  },
  "e2e-size": {
    "status": 200,
    "body": "{\"object\":\"chat.completion\",\"choices\":[]}"
  },
  "gen-key-1": {
    "status": 200,
    "body": "{\"object\":\"chat.completion\",\"choices\":[]}"
  }
}
JSON

cat > "$WORK/config.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8080
routing:
  strategy: least_active
  cooldown:
    # No status map is written here, so the map in force is the one the code defaults to: 429
    # alone. Case 13 is a check about that default, so a map written here would check this
    # script's own choice instead of the behaviour a new user gets from the code.
    max: 60s
headers:
  mode: deny
  deny: [Cookie, X-Forwarded-For]
  forward_request_id: true
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
  forwardcheck:
    targets:
      # The model here is longer than the route name, so the body the upstream is told about
      # is not the length the client counted: that is the case the Content-Length check needs.
      - name: forwarder
        model: e2e-forward-model
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_PLAIN
        headers:
          X-Zq-Target: from-target
  forwardpath:
    targets:
      - name: pathed
        model: forwardpath
        base_url: http://127.0.0.1:8091/v1
        path: /zq-endpoint
        api_key_env: E2E_PLAIN
  gzipcheck:
    targets:
      - name: gzipped
        model: gzipcheck
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_GZIP
  distleast:
    targets:
      - name: d1
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_1
      - name: d2
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_2
      - name: d3
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_3
      - name: d4
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_4
  rafar:
    targets:
      - name: far-future
        model: rafar
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_RAFAR
  unmapped:
    targets:
      - name: not-in-the-map
        model: unmapped
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_500
  safety503:
    targets:
      - name: service-level-503
        model: safety503
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_503
YAML

export E2E_STREAM=e2e-stream E2E_429=e2e-429 E2E_TOOL=e2e-tool E2E_CANCEL=e2e-cancel
export E2E_PLAIN=e2e-plain E2E_GZIP=e2e-gzip E2E_ALLOW=e2e-allow E2E_500=e2e-500
export E2E_503=e2e-503 E2E_SIZE=e2e-size
export E2E_RAFAR=e2e-rafar E2E_RAFAR_2=e2e-rafar-2
for i in 1 2 3 4 5 6 7 8; do export "E2E_DIST_$i=e2e-dist-$i"; done

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
  -d '{"model":"allcooling","messages":[{"role":"user","content":"prompt-zq"}]}' \
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
# The prompt content uses letters that are not hex digits: a request id is hex, so a
# content string that is hex-valid can appear inside one by chance, which made this
# check flaky.
not_has "no prompt content appears in the gateway log" 'prompt-zq' "$(cat "$WORK/gateway.log")"

printf '\n6. client disconnect: the upstream request is cancelled\n'
curl -s --max-time 2 -o "$WORK/disconnect.txt" -H 'Content-Type: application/json' \
  -d '{"model":"cancel","stream":true,"messages":[{"role":"user","content":"prompt-zq"}]}' \
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
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt-zq\"}]}" \
    http://127.0.0.1:8092/v1/chat/completions
}

check "nothing presented is rejected" "401" "$(body_of stream)"
has "the error is the OpenAI shape with the api key code" '"code":"invalid_api_key"' "$(cat "$WORK/gate-body.txt")"
check "a wrong key is rejected" "401" "$(curl -s -o "$WORK/gate-body.txt" -w '%{http_code}' -H 'Authorization: Bearer not-the-key' -H 'Content-Type: application/json' -d '{"model":"stream","messages":[{"role":"user","content":"prompt-zq"}]}' http://127.0.0.1:8092/v1/chat/completions)"
check "the configured key is served" "200" "$(curl -s -o "$WORK/gate-body.txt" -w '%{http_code}' -H 'Authorization: Bearer e2e-caller' -H 'Content-Type: application/json' -d '{"model":"stream","messages":[{"role":"user","content":"prompt-zq"}]}' http://127.0.0.1:8092/v1/chat/completions)"
has "the upstream key was used, not the caller key" '[DONE]' "$(cat "$WORK/gate-body.txt")"
stats
c=$(count e2e-caller)
check "the caller key never became an upstream key" "0" "${c:-0}"
check "the health endpoint is not gated" "ok" "$(curl -s http://127.0.0.1:8092/healthz)"
has "the gate is counted on the metrics page" 'invalid_api_key' "$(curl -s http://127.0.0.1:8092/metrics)"
has "the metrics page prints the route label, not model" 'plainroute_gateway_rejected_total{code="invalid_api_key",route="none"}' "$(curl -s http://127.0.0.1:8092/metrics)"
not_has "no value taken from client input appears as a label" 'e2e-caller' "$(curl -s http://127.0.0.1:8092/metrics)"
has "the startup line reports the gate and names the variable it read" 'api_key_env' "$(head -1 "$WORK/gate.log")"
not_has "the startup line does not print the value" 'e2e-caller' "$(head -1 "$WORK/gate.log")"
not_has "the caller key value does not appear in the log" 'e2e-caller' "$(cat "$WORK/gate.log")"

kill "$GATE_PID" 2>/dev/null
wait "$GATE_PID" 2>/dev/null

printf '\n8. what the gateway forwards, read back from what the upstream received\n'
# The prompt content and the header values in this case use letters that are not hex
# digits: a request id is hex, so a needle that is hex-valid can appear inside one by
# chance.
sent='{"model":"forwardcheck", "zq_unknown":{"kept":true}, "temperature":0.5, "stream":false}'
fwd='{"model":"e2e-forward-model", "zq_unknown":{"kept":true}, "temperature":0.5, "stream":false}'
printf '%s' "$sent" > "$WORK/sent.json"

status=$(curl -s -o "$WORK/fwd.txt" -w '%{http_code}' -H 'Content-Type: application/json' \
  -H 'Cookie: zq-session-value' -H 'X-Forwarded-For: 203.0.113.7' -H 'Connection: close' \
  -H 'Accept-Encoding: gzip' -H 'X-Zq-Client-Header: kept' -H 'X-Zq-Target: from-client' \
  -H 'X-Request-Id: zq-client-id' -d @"$WORK/sent.json" \
  'http://127.0.0.1:8080/v1/chat/completions?zq_query=1')
check "the forwarded request succeeded" "200" "$status"
record=$(last e2e-plain)
# A check for an absent header can pass because nothing arrived, so the guard is that
# the record is a request, not an empty record.
has "the record is a request this case sent, not an empty record" 'method=POST' "$record"

check "the body arrives as the bytes it was sent, with only the model value replaced" \
  "$fwd" "$(field e2e-plain body)"
check "the client's query string does not travel" "" "$(field e2e-plain query)"

# The recorded length is what the sender framed the request with, so it must match the
# body that arrived. The two differ here because the target model is longer than the
# route name, which is what lets the check tell them apart. It cannot tell whether the
# gateway dropped the client's Content-Length or the transport sized the body, because
# Go sizes a body it can measure: the check states the invariant a reader cares about,
# that the length the upstream is told matches the bytes it receives.
check "the length the upstream was told is the length of the body that arrived" \
  "${#fwd}" "$(field e2e-plain content_length)"
CHECKS=$((CHECKS + 1))
if [ "${#fwd}" != "${#sent}" ]; then
  pass "the forwarded length is not the length the client counted, so the check can tell them apart"
else
  fail "the two lengths are equal, so the length check proves nothing"
fi

not_has "the deny list drops Cookie" 'Cookie=' "$record"
not_has "the deny list drops X-Forwarded-For" 'X-Forwarded-For=' "$record"
not_has "hop-by-hop Connection is dropped whatever the config says" 'Connection=' "$record"
has "a header outside the deny list travels" 'X-Zq-Client-Header=kept' "$record"
has "Accept-Encoding travels as the client wrote it" 'Accept-Encoding=gzip' "$record"
has "Authorization is the key configured for the target, not the caller's" \
  'Authorization=Bearer e2e-plain' "$record"
has "per-target headers override what the client sent for the same name" \
  'X-Zq-Target=from-target' "$record"
has "a request id within the limit is the client's own value" 'X-Request-Id=zq-client-id' "$record"

# An id over the limit is not forwarded: the gateway generates one, and a generated id
# is hex, so the needle here is deliberately not hex.
long=$(printf 'zq%.0s' {1..70})
curl -s -o /dev/null -H 'Content-Type: application/json' -H "X-Request-Id: $long" \
  -d @"$WORK/sent.json" http://127.0.0.1:8080/v1/chat/completions > /dev/null
record=$(last e2e-plain)
not_has "an id longer than 128 characters is not forwarded" "$long" "$record"
has "the gateway generated an id instead" 'X-Request-Id=[0-9a-f]\{16\}' "$record"

# The client's path is not used either: the upstream URL is the target's base URL plus
# its own endpoint.
printf '%s' '{"model":"forwardpath", "zq_unknown":1}' > "$WORK/path.json"
send http://127.0.0.1:8080/v1/chat/completions "$WORK/path.json" "$WORK/path-out.txt" > /dev/null
check "the path that travels is the target's endpoint, not the client's" \
  "/v1/zq-endpoint" "$(field e2e-plain path)"

# A compressed body is not decompressed: the gateway requires JSON text, so gzip bytes
# are a parse failure and nothing reaches the upstream. The encoding has to be
# announced, otherwise this only checks that unparseable bytes are rejected, which is a
# different claim.
printf '%s' '{"model":"gzipcheck", "zq_unknown":1}' > "$WORK/gzip-body.json"
gzip -c "$WORK/gzip-body.json" > "$WORK/sent.gz"
status=$(curl -s -o "$WORK/gzip.txt" -w '%{http_code}' -H 'Content-Type: application/json' \
  -H 'Content-Encoding: gzip' --data-binary @"$WORK/sent.gz" \
  http://127.0.0.1:8080/v1/chat/completions)
check "a compressed body is rejected rather than decompressed" "400" "$status"
has "and the error is invalid_json" '"code":"invalid_json"' "$(cat "$WORK/gzip.txt")"
stats
c=$(count e2e-gzip)
check "nothing reached the upstream for that request" "0" "${c:-0}"

# A stream response has no Content-Length: the gateway cannot know its length in
# advance, so a stream that ends early is not described by a number that is wrong.
curl -s -D "$WORK/stream-headers.txt" -o "$WORK/stream-body.txt" -H 'Content-Type: application/json' \
  -d '{"model":"stream","stream":true,"messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8080/v1/chat/completions > /dev/null
has "the stream response is framed so a length cannot be wrong" 'Transfer-Encoding: chunked' \
  "$(cat "$WORK/stream-headers.txt")"
not_has "and no Content-Length is claimed for it" 'Content-Length:' "$(cat "$WORK/stream-headers.txt")"

printf '\n9. allow mode forwards only what the allow list names\n'
cat > "$WORK/allow-config.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8093
headers:
  # Connection is named in the allow list on purpose: a per-connection name is dropped even when
  # the config would allow it, and that precedence is what this case checks. Authorization is
  # named too, so the check that the configured key wins is not passed by a list that would have
  # dropped the client's key anyway.
  mode: allow
  allow: [Cookie, Content-Type, Connection, Authorization]
routes:
  allowcheck:
    targets:
      - name: allowlisted
        model: allowcheck
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_ALLOW
YAML
start_gateway "$WORK/allow-config.yaml" "$WORK/allow.log"

curl -s -o /dev/null -H 'Content-Type: application/json' -H 'Cookie: zq-cookie-value' \
  -H 'X-Zq-Outside-Allow: dropped' -H 'Connection: close' \
  -H 'Authorization: Bearer sk-zq-client-key' \
  -d '{"model":"allowcheck","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8093/v1/chat/completions > /dev/null
record=$(last e2e-allow)
has "the record is a request this case sent, not an empty record" 'method=POST' "$record"
has "the allow list forwards what it names" 'Cookie=zq-cookie-value' "$record"
not_has "a header outside the allow list is not forwarded" 'X-Zq-Outside-Allow=' "$record"
not_has "hop-by-hop is dropped even when the allow list names it" 'Connection=' "$record"
has "the key is still the configured one even when the allow list names Authorization" \
  'Authorization=Bearer e2e-allow' "$record"
# The fake upstream keys by the key it sees, so the same break that lets the caller's key travel
# shows up as a record under that key. Read both records: the request must arrive under the
# configured key and not under the one the client sent.
has "and nothing arrived under the key the client sent" 'nothing was received for this key' \
  "$(last sk-zq-client-key)"

printf '\n10. sequential traffic over four targets\n'
# Twelve requests, one at a time, so nothing is in flight when the next target is picked. This
# is the distribution the README states: round_robin spreads twelve over four targets as 3 each,
# while least_active keeps choosing the quietest one and a tie keeps config order, so a single
# target takes all twelve.
cat > "$WORK/round-config.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8094
routing:
  strategy: round_robin
routes:
  distround:
    targets:
      - name: r1
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_5
      - name: r2
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_6
      - name: r3
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_7
      - name: r4
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_DIST_8
YAML
start_gateway "$WORK/round-config.yaml" "$WORK/round.log"

for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do post_to 8080 distleast /dev/null > /dev/null; done
stats
check "least_active: one target took all twelve" "12" "$(count e2e-dist-1)"
check "least_active: the second target got none" "0" "$(count e2e-dist-2)"
check "least_active: the third target got none" "0" "$(count e2e-dist-3)"
check "least_active: the fourth target got none" "0" "$(count e2e-dist-4)"

for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do post_to 8094 distround /dev/null > /dev/null; done
stats
check "round_robin: first target" "3" "$(count e2e-dist-5)"
check "round_robin: second target" "3" "$(count e2e-dist-6)"
check "round_robin: third target" "3" "$(count e2e-dist-7)"
check "round_robin: fourth target" "3" "$(count e2e-dist-8)"

printf '\n11. startup: a key is a name, and every missing name is reported at once\n'
cat > "$WORK/badkey.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8095
routes:
  startupcheck:
    targets:
      - name: written-as-a-value
        model: startupcheck
        base_url: http://127.0.0.1:8091/v1
        api_key_env: sk-zq-not-a-variable-name
      - name: missing-one
        model: startupcheck
        base_url: http://127.0.0.1:8091/v1
        api_key_env: ZQ_MISSING_ONE
      - name: missing-two
        model: startupcheck
        base_url: http://127.0.0.1:8091/v1
        api_key_env: ZQ_MISSING_TWO
YAML
# Each startup check has a timeout: if a build stops reporting the problem and lets the process
# start, the process would listen and never exit, and the script would hang instead of failing.
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/badkey.yaml" 2>&1); code=$?
check "a key written as a value stops the process before it listens" "1" "$code"
has "and names what it was given as a variable that is not set" \
  'sk-zq-not-a-variable-name is not set' "$out"
has "the error reports every missing name, not only the first" 'ZQ_MISSING_ONE is not set' "$out"
has "so the second missing name is in the same error" 'ZQ_MISSING_TWO is not set' "$out"

cat > "$WORK/badref.yaml" <<'YAML'
server:
  # Each startup fixture gets its own port: a process that fails for a different reason, such as
  # a port already taken by a gateway this script started, would answer the exit-code checks.
  listen: 127.0.0.1:8097
routes:
  startupcheck:
    targets:
      - name: reference-without-a-default
        model: startupcheck
        base_url: http://127.0.0.1:8091/v1
        # The reference is put where an empty value would be accepted, so the exit code is
        # attributable to the unset reference alone. On base_url the same process would still
        # exit for a different reason, and the check would pass for the wrong reason.
        path: ${ZQ_MISSING_REF}
        api_key_env: E2E_PLAIN
YAML
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/badref.yaml" 2>&1); code=$?
check "an environment reference without a default stops the process" "1" "$code"
has "and names the reference" 'ZQ_MISSING_REF' "$out"

cat > "$WORK/default.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8098
routes:
  startupcheck:
    targets:
      - name: reference-with-a-default
        model: startupcheck
        base_url: ${ZQ_UNSET_BASE:-http://127.0.0.1:8091/v1}
        api_key_env: E2E_PLAIN
YAML
start_gateway "$WORK/default.yaml" "$WORK/default.log"
check "the default is used when the variable is not set, and the process starts" "ok" \
  "$(curl -s http://127.0.0.1:8098/healthz)"
check "and a request through that route reaches the upstream" "200" \
  "$(post_to 8098 startupcheck /dev/null)"

printf '\n12. the cooldown window: the clamp, the map, and what a reload keeps\n'
# The number a client sees is the window the gateway applied, not the number the upstream
# printed. The checks allow the value to drop by one second between requests, because it is the
# time that is left, so they do not guess a second.
status=$(curl -s -D "$WORK/h1.txt" -o "$WORK/r1.txt" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"rafar","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8080/v1/chat/completions)
check "the upstream answer is forwarded as it arrived" "429" "$status"
has "the error body is forwarded too" 'rate limited' "$(cat "$WORK/r1.txt")"
has "the answer carries the window the gateway applied, not the 7200 the upstream printed" \
  'Retry-After: \(60\|59\)' "$(cat "$WORK/h1.txt")"
not_has "and the number the upstream printed is not what travels to the client" \
  'Retry-After: 7200' "$(cat "$WORK/h1.txt")"

stats
before=$(total)
status=$(curl -s -D "$WORK/h2.txt" -o "$WORK/r2.txt" -w '%{http_code}' \
  -H 'Content-Type: application/json' -d '{"model":"rafar","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8080/v1/chat/completions)
stats
check "the next request is answered from state, with no upstream call" "$before" "$(total)"
check "and the answer is the code for every target cooling down" "429" "$status"
has "the answer is the configured clamp, not the 7200 the upstream claimed" \
  'Retry-After: \(60\|59\)' "$(cat "$WORK/h2.txt")"

cat > "$WORK/clampoff.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8096
routing:
  cooldown:
    status: {429: 10s}
    max: 0s
routes:
  rafar2:
    targets:
      - name: no-clamp
        model: rafar2
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_RAFAR_2
YAML
start_gateway "$WORK/clampoff.yaml" "$WORK/clampoff.log"

status=$(curl -s -D "$WORK/h3.txt" -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"rafar2","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8096/v1/chat/completions)
check "with the clamp off the claim is what the gateway applies" "429" "$status"
has "so the client sees the number as written" 'Retry-After: \(7200\|7199\)' "$(cat "$WORK/h3.txt")"

stats
before=$(count e2e-rafar-2)
status=$(curl -s -D "$WORK/h5.txt" -o "$WORK/r3.txt" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"rafar2","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8096/v1/chat/completions)
stats
check "the next request is answered from state, with no upstream call" "$before" "$(count e2e-rafar-2)"
check "and the answer comes from the gateway's own state, not from the upstream" "429" "$status"
has "with the code for every target cooling down" 'all_targets_cooling_down' "$(cat "$WORK/r3.txt")"
has "and the window that state holds is the number the upstream printed, because nothing clamped it" \
  'Retry-After: \(7200\|7199\)' "$(cat "$WORK/h5.txt")"

cat > "$WORK/badmax.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8099
routing:
  cooldown:
    max: -1s
routes:
  startupcheck:
    targets:
      - name: negative-clamp
        model: startupcheck
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_PLAIN
YAML
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/badmax.yaml" 2>&1); code=$?
check "a negative clamp is a startup error rather than a silent no clamp" "1" "$code"
has "and the message says what it expects" 'must be positive, or 0 for no clamp' "$out"

# A status that is not in the map does not cool a target down, and no window is claimed with it.
# A cooled target is skipped, so a second request on the same route would be answered from state
# with 429. The second request is what makes this falsifiable: the first reaches the upstream
# whatever the code does, so a check on the first could never fail.
stats
before=$(total)
status=$(curl -s -D "$WORK/h4.txt" -o "$WORK/u1.txt" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"unmapped","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8080/v1/chat/completions)
check "an unmapped status is answered by the upstream as it arrived" "500" "$status"
check "and no cooldown is claimed with it" "" "$(grep -i 'Retry-After' "$WORK/h4.txt")"
stats
before=$(total)
check "so the target stayed selectable: the next request reached the upstream too" "500" \
  "$(post_to 8080 unmapped "$WORK/u2.txt")"
stats
check "and the upstream counted that second request" "$((before + 1))" "$(total)"

stats
before=$(total)
kill -HUP "$GATEWAY_PID"
sleep 1
has "the reload that worked is logged" 'config_reloaded' "$(cat "$WORK/gateway.log")"
has "and counted as the outcome for a reload that worked" 'outcome="ok".* 1' \
  "$(curl -s http://127.0.0.1:8080/metrics | grep 'plainroute_config_reloads_total')"
status=$(post_to 8080 rafar "$WORK/r4.txt")
stats
check "the cooldown survived the reload: no upstream call" "$before" "$(total)"
check "and the answer still comes from state" "429" "$status"
has "with the code for every target cooling down" 'all_targets_cooling_down' "$(cat "$WORK/r4.txt")"

# A reload that fails keeps the config that was running, so an operator writing a bad file does
# not break a working gateway. The reload reads the file the process started with, so this
# replaces that file, and the check asks whether a route from the bad file exists, which only
# the running config can answer.
cat > "$WORK/config.yaml" <<'YAML'
routing:
  cooldown:
    max: -1s
routes:
  newroute:
    targets:
      - name: bad-reload
        model: newroute
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_PLAIN
YAML
kill -HUP "$GATEWAY_PID"
sleep 1
has "the failed reload is logged with the problem it found" \
  'must be positive, or 0 for no clamp' "$(cat "$WORK/gateway.log")"
has "and counted as the other outcome" 'outcome="failed".* 1' \
  "$(curl -s http://127.0.0.1:8080/metrics | grep 'plainroute_config_reloads_total')"
check "the gateway is still serving after a failed reload" "ok" \
  "$(curl -s http://127.0.0.1:8080/healthz)"
check "a route from the file that failed to load is not in use, so the running config survived" \
  "404" "$(post_to 8080 newroute /dev/null)"

printf '\n13. a 503 is answered as the upstream answered it, and does not cool the target\n'
# The config this run uses writes no status map, so the map in force is the code default: 429
# alone. A window names a target, and a 503 normally says something about the service behind a
# key rather than about the key, so a window on 503 would hold a working key out of the pool for
# a failure that may not belong to it. The second request is what makes this falsifiable: the
# first reaches the upstream whatever the code does with the map, so only a target that was taken
# out of the pool can answer the second one differently.
# Written without a trailing newline: the upstream body has none, so a byte comparison here has
# to compare the same bytes, and a heredoc would add one.
printf '%s' '{"error":{"message":"content safety screening is temporarily unavailable"}}' > "$WORK/expected503.txt"

stats
before=$(total)
status=$(curl -s -D "$WORK/h6.txt" -o "$WORK/g1.txt" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"safety503","messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8080/v1/chat/completions)
check "the 503 reaches the client as the upstream answered it" "503" "$status"
if cmp -s "$WORK/g1.txt" "$WORK/expected503.txt"; then
  CHECKS=$((CHECKS + 1)); pass "and the body is the upstream body, not a gateway error"
else
  CHECKS=$((CHECKS + 1)); fail "the body changed: $(cat "$WORK/g1.txt")"
fi
stats
check "and one client request was one upstream attempt" "$((before + 1))" "$(total)"
check "so no window is claimed with it" "" "$(grep -i 'retry-after' "$WORK/h6.txt")"
not_has "and the gateway did not take that target out of the pool" 'cooldown.*safety503' \
  "$(cat "$WORK/gateway.log")"

stats
before=$(count e2e-503)
status=$(post_to 8080 safety503 "$WORK/g2.txt")
stats
check "the next request reached the same target, which is what selectable means" \
  "$((before + 1))" "$(count e2e-503)"
check "and it was answered by the upstream again, not by gateway state" "503" "$status"

# The rule about the default map is also a rule about the files a new user copies, so the files
# are checked: a rule the shipped example contradicts is not the rule a reader gets.
not_has "the example config does not recommend a window for 503" '^[ ]*503:' \
  "$(cat "$ROOT/config.example.yaml")"
not_has "and the README example does not show one either" '^[ ]*503:' "$(cat "$ROOT/README.md")"

printf '\n14. the size limits: the boundary, and the numbers the shipped files write\n'
# A limit is a limit, not a refusal at the boundary: a body of exactly the configured size is
# forwarded, and one byte more is a 413 before anything reaches an upstream. The model behind this
# route is longer than the route name, so the length the upstream is told is not the length the
# client counted, which is the case the Content-Length claim is about.
cat > "$WORK/sizes.yaml" <<'YAML'
server:
  # Each startup fixture and each case gateway has its own port, so a failure points at the case
  # that owns it rather than at a process the script started earlier.
  listen: 127.0.0.1:8100
  max_request_bytes: 4096
routes:
  sizelimit:
    targets:
      - name: at-the-cap
        model: e2e-size-model
        base_url: http://127.0.0.1:8091/v1
        api_key_env: E2E_SIZE
YAML
start_gateway "$WORK/sizes.yaml" "$WORK/sizes.log"

# The bodies are written by awk so the length is computed rather than typed: a fixture length that
# someone typed by hand is a number that can be wrong without anything noticing. The two length
# checks below are guards on the fixture, not on the implementation: they say this case is testing
# the boundary and not a round number.
awk 'BEGIN {
  prefix = "{\"model\":\"sizelimit\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt content ";
  suffix = "\"}]}";
  pad = ""; for (i = 0; i < 4096 - length(prefix) - length(suffix); i++) pad = pad "a";
  printf "%s%s%s", prefix, pad, suffix;
}' > "$WORK/atcap.json"
check "the body sent is exactly the configured limit" "4096" "$(wc -c < "$WORK/atcap.json")"
status=$(send http://127.0.0.1:8100/v1/chat/completions "$WORK/atcap.json" "$WORK/atcap.out")
check "a body exactly at the limit is forwarded" "200" "$status"
check "and the upstream was told the length of the body that arrived, not the one the client sent" \
  "4101" "$(field e2e-size content_length)"
field e2e-size body > "$WORK/atcap.got"
# The pad is the one the client sent, counted from the client's own prefix: the model value is the
# only difference, so the body that arrives is longer than the one that left by the length of that
# value, which is the same reason Content-Length cannot be copied from the client.
awk 'BEGIN {
  sent = "{\"model\":\"sizelimit\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt content ";
  got = "{\"model\":\"e2e-size-model\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt content ";
  suffix = "\"}]}";
  pad = ""; for (i = 0; i < 4096 - length(sent) - length(suffix); i++) pad = pad "a";
  printf "%s%s%s\n", got, pad, suffix;
}' > "$WORK/atcap.want"
if cmp -s "$WORK/atcap.want" "$WORK/atcap.got"; then
  CHECKS=$((CHECKS + 1))
  pass "and the bytes that arrived are the bytes that were sent, with only the model value changed"
else
  CHECKS=$((CHECKS + 1)); fail "the body changed at the boundary"
fi

stats
before=$(count e2e-size)
awk 'BEGIN {
  prefix = "{\"model\":\"sizelimit\",\"messages\":[{\"role\":\"user\",\"content\":\"prompt content ";
  suffix = "\"}]}";
  pad = ""; for (i = 0; i < 4097 - length(prefix) - length(suffix); i++) pad = pad "a";
  printf "%s%s%s", prefix, pad, suffix;
}' > "$WORK/overcap.json"
check "and the second body is one byte past the limit" "4097" "$(wc -c < "$WORK/overcap.json")"
status=$(send http://127.0.0.1:8100/v1/chat/completions "$WORK/overcap.json" "$WORK/overcap.out")
check "one byte past the limit is a 413" "413" "$status"
has "and names the reason the gateway has for it" 'request_body_too_large' "$(cat "$WORK/overcap.out")"
stats
check "and nothing reached the upstream, so the gateway did not forward a body it could not answer" \
  "$before" "$(count e2e-size)"

# The recommended numbers are a claim about the files a new user copies, so the files are read:
# a recommendation CI can read is a recommendation CI can check.
has "the example config writes the 32 MiB request limit it recommends" 'max_request_bytes: 33554432' \
  "$(cat "$ROOT/config.example.yaml")"
has "and the 64 MiB response limit" 'max_response_bytes: 67108864' "$(cat "$ROOT/config.example.yaml")"

printf '\n15. the generator: the numbers it writes, the parameters that change them, and that its output is a config\n'
# A snapshot is used here so this case does not touch the network: CI is not a place to check
# somebody's provider. The generator is a file a new user copies, so its numbers are checked
# against the recommendation the README states, and the generated file is then loaded by the gateway
# itself, which is the only way to say "its output is a config" instead of "its output is text that
# looks like a config".
printf '%s' '["glm-4.5", "qwen3:8b"]' > "$WORK/models.json"
export GEN_KEY_1=gen-key-1

python3 "$ROOT/scripts/generate-config.py" --models "$WORK/models.json" --keys 1 --key-prefix GEN_KEY \
  --base-url http://127.0.0.1:8091/v1 --listen 127.0.0.1:8101 --out "$WORK/gen.yaml" > "$WORK/gen.out" 2>&1
check "the generator runs from a snapshot, so this case never contacts an upstream" "0" "$?"
has "the request limit it writes is the recommended 32 MiB" 'max_request_bytes: 33554432' "$(cat "$WORK/gen.yaml")"
has "the response limit is the recommended 64 MiB" 'max_response_bytes: 67108864' "$(cat "$WORK/gen.yaml")"
has "the clamp is the recommended 30s" '^[ ]*max: 30s' "$(cat "$WORK/gen.yaml")"
has "429 keeps the documented prior" '^[ ]*429: 10s' "$(cat "$WORK/gen.yaml")"
has "and 503 writes no window, which is the default this script chose" '^[ ]*503: 0s' "$(cat "$WORK/gen.yaml")"

python3 "$ROOT/scripts/generate-config.py" --models "$WORK/models.json" --keys 1 --key-prefix GEN_KEY \
  --base-url http://127.0.0.1:8091/v1 --listen 127.0.0.1:8101 \
  --max-request-bytes 1048576 --max-response-bytes 2097152 --cooldown-max 6s --cooldown-503 20s \
  --out "$WORK/gen2.yaml" > "$WORK/gen2.out" 2>&1
check "and the parameters change what the script writes" "0" "$?"
has "--max-request-bytes is written as it was given" 'max_request_bytes: 1048576' "$(cat "$WORK/gen2.yaml")"
has "--max-response-bytes is written as it was given" 'max_response_bytes: 2097152' "$(cat "$WORK/gen2.yaml")"
has "--cooldown-max is written as it was given" '^[ ]*max: 6s' "$(cat "$WORK/gen2.yaml")"
has "--cooldown-503 is written as it was given" '^[ ]*503: 20s' "$(cat "$WORK/gen2.yaml")"
has "and the generator writes the name of a variable, never a key" 'api_key_env: GEN_KEY_1' "$(cat "$WORK/gen2.yaml")"
not_has "so a generated file cannot be committed with a key in it" 'gen-key-1' "$(cat "$WORK/gen2.yaml")"

start_gateway "$WORK/gen.yaml" "$WORK/gen.log"
check "the generated file is a config the gateway accepts, not only text that looks like one" \
  "ok" "$(curl -s http://127.0.0.1:8101/healthz)"
status=$(post_to 8101 glm-4.5 "$WORK/genreq.out")
check "and a request through a generated route reaches the upstream" "200" "$status"
has "and the answer is the one this key is configured for, so the route the script wrote is the route the gateway reads" \
  'chat.completion' "$(cat "$WORK/genreq.out")"

printf '\n%d checks: %s\n' "$CHECKS" "$([ "$FAIL" = 0 ] && echo 'all passed' || echo 'failures')"
exit "$FAIL"




