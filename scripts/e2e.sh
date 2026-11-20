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

# The reload counter is read from the metrics page rather than from a log line, because a check that
# reads a log line can pass on a line the process wrote for a different reason.
reloadcount() {
  curl -s http://127.0.0.1:8080/metrics | grep "plainroute_config_reloads_total{outcome=\"$1\"}" | awk '{print $2}'
}

# A change is applied by the watcher, so the checks that follow a change wait for it. The bound is six
# intervals: a check that only slept a fixed time would pass on a watcher that never looked, and a wait
# without a bound would hang instead of failing when a change is not applied.
wait_for() { # outcome, the count before the change
  end=$((SECONDS + 6))
  while [ "$(reloadcount "$1")" = "$2" ] && [ "$SECONDS" -lt "$end" ]; do
    sleep 0.2
  done
  reloadcount "$1"
}

# The generation a reload line reports is read from the line the process wrote. Both sides of these
# checks come out of what the process printed, so a check cannot pass on a number the script picked.
gen() { sed -n 's/.*"generation":\([0-9]\{1,\}\).*/\1/p'; }

# The hash of the bytes the script wrote, which is the same content the process read. A check can name
# it because the script computed it outside the process; what is checked is that the process did not
# print it, in either form.
sha_of() { sha256sum "$1" | cut -d' ' -f1; }

# The gate is checked on the API endpoints only, so a caller-key check sends one request and prints the
# status code. "none" presents no header, which is the case a gate that reads as on in the file has to
# answer.
caller() {
  if [ "$1" = none ]; then
    curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d '{"model":"hotpair","messages":[{"role":"user","content":"prompt-zq"}]}' \
      http://127.0.0.1:8080/v1/chat/completions
  else
    curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
      -d '{"model":"hotpair","messages":[{"role":"user","content":"prompt-zq"}]}' \
      http://127.0.0.1:8080/v1/chat/completions
  fi
}

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
for port in 8080 8091 8092 8093 8094 8095 8096 8097 8098 8099 8100 8101 8102 8103; do
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
  "e2e-hot-a": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-hot-b": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-hot-d": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-hot-f": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-hot-g": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-hot-h": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645": { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" },
  "e2e-hot-c": {
    "sequence": [
      {
        "status": 200,
        "sse": ["data: frame one\n\n", "data: frame two\n\n", "data: [DONE]\n\n"],
        "gap": "4s",
        "wait_for_cancel": true
      },
      { "status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[]}" }
    ]
  },
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
  "REPLACE_ME_ACCOUNT_1": {
    "status": 200,
    "body": "{\"object\":\"chat.completion\",\"choices\":[]}"
  },
  "smoke-ok": {
    "sequence": [
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "delay": "150ms", "sse": ["data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: [DONE]\n\n"]},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"smoke_tool\",\"arguments\":\"{}\"}}]}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 429, "headers": {"Retry-After": "6"}, "body": "{\"status\":429,\"title\":\"Too Many Requests\"}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"}
    ]
  },
  "smoke-skip": {
    "sequence": [
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "delay": "150ms", "sse": ["data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: [DONE]\n\n"]},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"smoke_tool\",\"arguments\":\"{}\"}}]}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 429, "headers": {"Retry-After": "6"}, "body": "{\"status\":429,\"title\":\"Too Many Requests\"}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"}
    ]
  },
  "smoke-nodone": {
    "sequence": [
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "sse": ["data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"more\"}}]}\n\n"]},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"smoke_tool\",\"arguments\":\"{}\"}}]}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 429, "headers": {}, "body": "{\"status\":429,\"title\":\"Too Many Requests\"}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"}
    ]
  },
  "smoke-notool": {
    "sequence": [
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "delay": "150ms", "sse": ["data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: [DONE]\n\n"]},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"I will answer in prose instead\"}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 429, "headers": {}, "body": "{\"status\":429,\"title\":\"Too Many Requests\"}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"}
    ]
  },
  "smoke-html": {
    "sequence": [
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "delay": "150ms", "sse": ["data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: [DONE]\n\n"]},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"smoke_tool\",\"arguments\":\"{}\"}}]}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 429, "headers": {"Content-Type": "text/html"}, "body": "<html><body>429 written by something in front of the endpoint</body></html>"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"}
    ]
  },
  "smoke-accepts-empty": {
    "sequence": [
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "delay": "150ms", "sse": ["data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: [DONE]\n\n"]},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"smoke_tool\",\"arguments\":\"{}\"}}]}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"},
      {"status": 200, "headers": {}, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"an empty messages array was accepted\"}}]}"},
      {"status": 200, "body": "{\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"content\":\"answer\"}}]}"}
    ]
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
        api_key: e2e-stream
  two:
    targets:
      - name: account-a
        model: two
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-429
      - name: account-b
        model: two
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-stream
  allcooling:
    targets:
      - name: c1
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-429
      - name: c2
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-429
      - name: c3
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-429
      - name: c4
        model: allcooling
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-429
  toolcall:
    targets:
      - name: tool
        model: toolcall
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-tool
  cancel:
    targets:
      - name: cancellable
        model: cancel
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-cancel
  forwardcheck:
    targets:
      # The model here is longer than the route name, so the body the upstream is told about
      # is not the length the client counted: that is the case the Content-Length check needs.
      - name: forwarder
        model: e2e-forward-model
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-plain
        headers:
          X-Zq-Target: from-target
  forwardpath:
    targets:
      - name: pathed
        model: forwardpath
        base_url: http://127.0.0.1:8091/v1
        path: /zq-endpoint
        api_key: e2e-plain
  gzipcheck:
    targets:
      - name: gzipped
        model: gzipcheck
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-gzip
  distleast:
    targets:
      - name: d1
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-1
      - name: d2
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-2
      - name: d3
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-3
      - name: d4
        model: distleast
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-4
  rafar:
    targets:
      - name: far-future
        model: rafar
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-rafar
  unmapped:
    targets:
      - name: not-in-the-map
        model: unmapped
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-500
  safety503:
    targets:
      - name: service-level-503
        model: safety503
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-503
YAML


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
has "upstream attempts do not exceed client requests issued" 'no amplification.*ok' "$(cat "$WORK/bench.txt")"
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
        api_key: e2e-stream
auth:
  api_key: e2e-caller
YAML

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
has "the startup line reports the gate as on" '"gate":"on"' "$(head -1 "$WORK/gate.log")"
has "and names the file it read, so a reader can tell which file this gateway is on" 'config' "$(head -1 "$WORK/gate.log")"
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
        api_key: e2e-allow
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
        api_key: e2e-dist-5
      - name: r2
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-6
      - name: r3
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-7
      - name: r4
        model: distround
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-dist-8
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

printf '\n11. startup: the file is the secret, a bad file is a startup failure, and the failure does not print it\n'
# Startup stays strict, and it can: before the first configuration there is no last-known-good one to
# serve with, so a file that is missing, unreadable or invalid stops the process before it listens.
# Each startup check has a timeout: if a build stopped reporting the problem and let the process
# start, that process would listen and never exit, and the script would hang instead of failing.
#
# The needle below is a value that must never come back out of the process. It is used as a key, and
# nothing in the code can find a key wrong except that it is empty, so the only values a message can
# echo are the ones it is complaining about. A message that printed the value it found would print
# this one, and these checks would fail.
secret=PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645

out=$(timeout 10 "$BIN/plainroute" -config "$WORK/no-such-file.yaml" 2>&1); code=$?
check "a config file that is not there stops the process before it listens" "1" "$code"
has "and says what it could not read" 'read config' "$out"

# A key is a value in the file now, so a target with no key is not a variable that is unset: it is a
# field this package refuses, and the error names the target it was reading, because a file with a
# hundred targets is only actionable when the error says which one.
cat > "$WORK/badkey.yaml" <<'YAML'
server:
  # Each startup fixture gets its own port: a process that fails for a different reason, such as a
  # port already taken by a gateway this script started, would answer the exit-code checks.
  listen: 127.0.0.1:8095
routes:
  startupcheck:
    targets:
      - name: no-key-written
        model: startupcheck
        base_url: http://127.0.0.1:8091/v1
        api_key: ""
YAML
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/badkey.yaml" 2>&1); code=$?
check "a target with no key stops the process before it listens" "1" "$code"
has "and says what it refused" 'api_key is required' "$out"
has "and names the target, so the error is actionable in a file with a hundred of them" 'no-key-written' "$out"

# Every problem at once, so an operator fixes the file in one pass instead of one error per run.
cat > "$WORK/badmany.yaml" <<YAML
server:
  # Not 8096: a gateway this script starts later listens there, and a process that failed for the
  # wrong reason would answer this check.
  listen: 127.0.0.1:8097
  max_request_bytes: -1
timeouts:
  connect: -1s
logging:
  level: shout
routes:
  startupcheck:
    targets:
      - name: every-problem-at-once
        model: startupcheck
        base_url: https://example.invalid/v1?token=x
        api_key: $secret
YAML
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/badmany.yaml" 2>&1); code=$?
check "a file with several problems stops the process" "1" "$code"
has "and reports the size limit, the duration and the level together" 'max_request_bytes' "$out"
has "so the negative size is in the same error" 'positive' "$out"
has "and the bad level is in it too" 'level' "$out"
has "and the query in the url is in it too" 'query' "$out"
not_has "and none of them prints the value the file holds" "$secret" "$out"

# A field that no code reads is not a field the loader takes. The two fields removed for having no
# reader are refused by name, so a file that writes one gets an answer that says the field does not
# exist, rather than being accepted and ignored. The numbers below are values that could only come from
# the message: a message that printed what it read would print them.
cat > "$WORK/removedfields.yaml" <<YAML
server:
  # 8098, a port no gateway this script starts listens on, so a process that failed for a different
  # reason would not answer these checks.
  listen: 127.0.0.1:8098
retry: 918273647
fallbacks:
  log_event: 918273648
routes:
  startupcheck:
    targets:
      - name: fields-are-gone
        model: startupcheck
        base_url: https://example.invalid/v1
        api_key: $secret
YAML
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/removedfields.yaml" 2>&1); code=$?
check "a file that writes a field this project no longer has stops the process before it listens" "1" "$code"
has "and names the field it refused, which is what an operator can act on" 'retry' "$out"
has "and names the other one in the same error, because every problem is reported at once" 'log_event' "$out"
not_has "and prints neither value it read, at startup, which is the rule for every config error in this package" '918273647\|918273648' "$out"
# The other half of the rule is that a file writing the fields the schema does have is still accepted.
# That is what every gateway this script starts proves, and the shipped example is checked against the
# loader by a test in internal/config, so the file a new user copies cannot drift from the schema.

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
        api_key: e2e-rafar-2
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
        api_key: e2e-plain
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

# The signal is a look, not a rebuild: a file that has not changed is not reloaded, because there is
# nothing to reload. So this part changes the file first and then asks for a look. A check that passed
# on a signal alone would pass for the wrong reason, on a no-op.
cp "$WORK/config.yaml" "$WORK/config-before-reload.yaml"
printf '\n  reloadcheck:\n    targets:\n      - name: added-by-reload\n        model: reloadcheck\n        base_url: http://127.0.0.1:8091/v1\n        api_key: e2e-plain\n' >> "$WORK/config.yaml"

stats
before=$(total)
kill -HUP "$GATEWAY_PID"
sleep 1
has "the reload that worked is logged" 'config_reloaded' "$(cat "$WORK/gateway.log")"
has "and counted as the outcome for a reload that worked" 'outcome="ok".* 1' \
  "$(curl -s http://127.0.0.1:8080/metrics | grep 'plainroute_config_reloads_total')"
has "the reload line names the file it read and the configuration in force, and never a value it read" \
  'generation' "$(grep 'config_reload' "$WORK/gateway.log")"
not_has "and a reload line prints no part of the content hash, which is the whole form" \
  '\b[0-9a-f]\{64\}\b' "$(grep 'config_reload' "$WORK/gateway.log")"
not_has "and none prints the short form of it that this log used to print" '\b[0-9a-f]\{10\}\b' \
  "$(grep 'config_reload' "$WORK/gateway.log")"
not_has "and the key in the file is not in that line" 'e2e-plain' "$(grep 'config_reloaded' "$WORK/gateway.log")"
stats
check "the cooldown survived the reload, because target state is keyed by route and target name" \
  "$before" "$(total)"
status=$(post_to 8080 rafar "$WORK/cooling.txt")
check "and the answer for the cooling target still comes from state" "429" "$status"
has "with the code for every target cooling down" 'all_targets_cooling_down' "$(cat "$WORK/cooling.txt")"
stats
before=$(count e2e-plain)
check "the route the changed file added is now in use, which is what says the reload applied" \
  "200" "$(post_to 8080 reloadcheck "$WORK/reloaded.txt")"
stats
check "and that request was one upstream attempt on the key the reloaded file names, so the file that was read is the file that routes" \
  "$((before + 1))" "$(count e2e-plain)"

# One signal on a file that has not changed is one look that finds nothing: no reload, and no new
# count. This is the dedup, and it is what keeps a watch loop from writing a line every second about
# a file that is not changing.
before=$(reloadcount ok)
kill -HUP "$GATEWAY_PID"
sleep 1
check "a signal that changes nothing is not a reload" "$before" "$(reloadcount ok)"
check "and the number of reload lines in the log did not grow" "1" \
  "$(grep -c 'config_reloaded' "$WORK/gateway.log")"

# A reload that fails keeps the config that was running, so an operator writing a bad file does not
# break a working gateway. The last known good configuration is the one that was in force at the
# failure, including the change the earlier reload applied: the check asks whether the route that the
# successful reload added is still served, which only the running configuration can answer.
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
        api_key: e2e-plain
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
check "and the route the last good file added is still in use, which is what last-known-good means" \
  "200" "$(post_to 8080 reloadcheck "$WORK/survived.txt")"

# The same bad content is looked at again, and the same problem is reported once: a file that stays
# bad is not a log line every second.
before=$(reloadcount failed)
for _ in 1 2 3 4 5; do kill -HUP "$GATEWAY_PID"; sleep 1; done
check "the same bad content is not counted again" "$before" "$(reloadcount failed)"
check "and it is not logged again" "1" "$(grep -c 'reload_failed\|config_reload_failed' "$WORK/gateway.log")"

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
        api_key: e2e-size
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

python3 "$ROOT/scripts/generate-config.py" --models "$WORK/models.json" --keys 1 \
  --base-url http://127.0.0.1:8091/v1 --listen 127.0.0.1:8101 --out "$WORK/gen.yaml" > "$WORK/gen.out" 2>&1
check "the generator runs from a snapshot, so this case never contacts an upstream" "0" "$?"
has "the request limit it writes is the recommended 32 MiB" 'max_request_bytes: 33554432' "$(cat "$WORK/gen.yaml")"
has "the response limit is the recommended 64 MiB" 'max_response_bytes: 67108864' "$(cat "$WORK/gen.yaml")"
has "the clamp is the recommended 30s" '^[ ]*max: 30s' "$(cat "$WORK/gen.yaml")"
has "429 keeps the documented prior" '^[ ]*429: 10s' "$(cat "$WORK/gen.yaml")"
has "and 503 writes no window, which is the default this script chose" '^[ ]*503: 0s' "$(cat "$WORK/gen.yaml")"

python3 "$ROOT/scripts/generate-config.py" --models "$WORK/models.json" --keys 1 \
  --base-url http://127.0.0.1:8091/v1 --listen 127.0.0.1:8101 \
  --max-request-bytes 1048576 --max-response-bytes 2097152 --cooldown-max 6s --cooldown-503 20s \
  --out "$WORK/gen2.yaml" > "$WORK/gen2.out" 2>&1
check "and the parameters change what the script writes" "0" "$?"
has "--max-request-bytes is written as it was given" 'max_request_bytes: 1048576' "$(cat "$WORK/gen2.yaml")"
has "--max-response-bytes is written as it was given" 'max_response_bytes: 2097152' "$(cat "$WORK/gen2.yaml")"
has "--cooldown-max is written as it was given" '^[ ]*max: 6s' "$(cat "$WORK/gen2.yaml")"
has "--cooldown-503 is written as it was given" '^[ ]*503: 20s' "$(cat "$WORK/gen2.yaml")"
has "and what it writes for a key is a placeholder, so a generated file can be committed without a secret in it" \
  'api_key: REPLACE_ME_ACCOUNT_1' "$(cat "$WORK/gen2.yaml")"
# An argument is the only way a script can be handed a value. `--keys` says how many slots, and there
# is no argument that takes a key, so a real key gets into the file only when a person types it in.
has "so the help text has no argument that could hold a key" 'usage: generate-config.py' "$(python3 "$ROOT/scripts/generate-config.py" --help 2>&1)"
not_has "and no argument name here can write a key into the file it makes" '--key-prefix' "$(python3 "$ROOT/scripts/generate-config.py" --help 2>&1)"
has "and the file tells a reader what to do with each placeholder" \
  'Each REPLACE_ME_ACCOUNT_n is a slot to fill in' "$(cat "$WORK/gen2.yaml")"

start_gateway "$WORK/gen.yaml" "$WORK/gen.log"
check "the generated file is a config the gateway accepts, not only text that looks like one" \
  "ok" "$(curl -s http://127.0.0.1:8101/healthz)"
status=$(post_to 8101 glm-4.5 "$WORK/genreq.out")
check "and a request through a generated route reaches the upstream" "200" "$status"
has "and the answer is the one this key is configured for, so the route the script wrote is the route the gateway reads" \
  'chat.completion' "$(cat "$WORK/genreq.out")"
has "and the key the upstream was given is the placeholder the script wrote, which is what says the file a reader reads is the file that runs" \
  'key=REPLACE_ME_ACCOUNT_1' "$(last REPLACE_ME_ACCOUNT_1)"

printf "\n16. the live smoke script, run against the project's own fake upstream\n"
# CI cannot check anybody's provider, so this runs the script against the fake upstream: what is
# checked is that its checks can pass, and that each can fail against an answer that should not
# satisfy it. A behaviour that answered every request the same way could not serve both the stream
# check and the non-stream check, so these are sequences in the order the script sends requests:
# non-stream, stream, tool call, unknown field, error answer, then the optional image. The model list
# is not in the sequence, because the fake upstream answers that path before it looks at the key.
# A delay on the stream entry is what makes the timing checks answerable: a script that printed a
# number instead of measuring one cannot tell a measured time from a made-up time.
printf 'not a real PNG: the script sends the bytes it is given and never reads them\n' > "$WORK/fake.png"

smoke() { # key, output file, fixture
  BASE_URL=http://127.0.0.1:8091/v1 API_KEY="$1" MODEL=smoke-model IMAGE_FIXTURE="${3:-}" \
    ./scripts/smoke-openai-compatible.sh > "$2" 2>&1
  return $?
}

smoke smoke-ok "$WORK/smoke-pass.txt" "$WORK/fake.png"; code=$?
check "the script passes against the fake upstream" "0" "$code"
check "and reports seven checks" "7 checks: all passed" "$(tail -1 "$WORK/smoke-pass.txt")"
for name in 'model list' 'non-stream completion' 'stream completion' 'tool call' 'unknown top-level field' 'error answer is its own' 'image fixture'; do
  has "the $name check reported ok" "  ok    $name" "$(cat "$WORK/smoke-pass.txt")"
done
# The words "stream completion" also occur inside "non-stream completion", so the line is picked by
# its fields rather than by a substring: a check that read the wrong line would measure the wrong
# request, and a delay on the stream entry is what makes a measured time tell them apart.
ttft=$(awk '$2 == "stream" && $3 == "completion" {print; exit}' "$WORK/smoke-pass.txt" | sed -n 's/.*ttft=\([0-9]\{1,\}\)ms.*/\1/p')
total=$(awk '$2 == "stream" && $3 == "completion" {print; exit}' "$WORK/smoke-pass.txt" | sed -n 's/.*total=\([0-9]\{1,\}\)ms.*/\1/p')
check "the timings are measured and not printed as constants" "1" \
  "$(awk -v t="$ttft" 'BEGIN { print (t >= 100) ? 1 : 0 }')"
check "and the first byte cannot arrive after the response ended" "1" \
  "$(awk -v a="$ttft" -v b="$total" 'BEGIN { print (a <= b) ? 1 : 0 }')"

has "the key travels as a bearer token, which is what says the request went with the key" \
  'Authorization=Bearer smoke-ok' "$(last smoke-ok)"
has "and the image request carried the payload as bytes, read back from what the upstream received" \
  'data:image/png;base64,' "$(last smoke-ok)"
check "so the body sent was the prompt plus the payload and not the prompt alone" "1" \
  "$(awk -v n="$(field smoke-ok content_length)" 'BEGIN { print (n > 100) ? 1 : 0 }')"
not_has "the output does not contain the key" 'smoke-ok' "$(cat "$WORK/smoke-pass.txt")"
not_has "and it does not contain the prompt either" 'smoke prompt content qz' "$(cat "$WORK/smoke-pass.txt")"
stats
check "one upstream request per check, so no check can pass on an answer another check received" \
  "6" "$(count smoke-ok)"

smoke smoke-skip "$WORK/smoke-skip.txt"
has "the optional check says it was skipped rather than passing, when no fixture is given" \
  '  skip  image fixture' "$(cat "$WORK/smoke-skip.txt")"
stats
check "and that run made five requests, not six" "5" "$(count smoke-skip)"

# Each check that a wrong answer could satisfy is shown to fail against that wrong answer, and the
# summary must still say exactly one failed: the other six passed for the right reason.
smoke smoke-nodone "$WORK/smoke-nodone.txt" "$WORK/fake.png"; code=$?
check "a stream without the frame that ends it fails the stream check" "1" "$code"
has "and names which check failed" '  FAIL  stream completion' "$(cat "$WORK/smoke-nodone.txt")"
check "and the other six checks still passed" "7 checks: 1 failed" "$(tail -1 "$WORK/smoke-nodone.txt")"

smoke smoke-notool "$WORK/smoke-notool.txt" "$WORK/fake.png"; code=$?
check "a prose answer to a tool-call request fails the tool check" "1" "$code"
has "and names which check failed" '  FAIL  tool call' "$(cat "$WORK/smoke-notool.txt")"
check "and the other six checks still passed" "7 checks: 1 failed" "$(tail -1 "$WORK/smoke-notool.txt")"

smoke smoke-html "$WORK/smoke-html.txt" "$WORK/fake.png"; code=$?
check "a 429 written as an HTML page is not the endpoint's own document" "1" "$code"
has "and says why" 'html=yes' "$(cat "$WORK/smoke-html.txt")"
check "and the other six checks still passed" "7 checks: 1 failed" "$(tail -1 "$WORK/smoke-html.txt")"

smoke smoke-accepts-empty "$WORK/smoke-empty.txt" "$WORK/fake.png"; code=$?
check "an endpoint that accepts an empty messages array is reported, not praised" "1" "$code"
has "and names which check failed" '  FAIL  error answer is its own' "$(cat "$WORK/smoke-empty.txt")"
check "and the other six checks still passed" "7 checks: 1 failed" "$(tail -1 "$WORK/smoke-empty.txt")"

printf '\n17. the watcher: a changed file is applied without a signal, and a bad file keeps the last good one\n'

# This is the same process the script started at the beginning. Nothing in this case sends a signal and
# nothing is restarted: the change is found by the watcher looking at the file, which is what the feature
# is for, and the checks say so by checking that the same process is still there and that its log has
# exactly one startup line.
#
# Every step here writes the whole file, because writing the file is the change. A check that reads the
# effect sends a request after the change is applied, so the answer comes from what is in force at that
# moment, and not from a counter that only says a read happened.
secret=PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645
strategy=least_active
cap=200
interval=1s
held_cap=1
caller_key=
listen=127.0.0.1:8080
targets='      - name: pair-1
        model: hotpair
        base_url: http://127.0.0.1:8091/pair-a
        api_key: PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645'

write_config() { # the targets block for route hotpair
  {
    printf 'server:\n  listen: %s\n  max_response_bytes: %s\n' "$listen" "$cap"
    printf 'config_reload:\n  interval: %s\n' "$interval"
    printf 'routing:\n  strategy: %s\n  cooldown:\n    max: 60s\n' "$strategy"
    printf 'routes:\n  hotpair:\n    targets:\n%s' "$1"
    printf '\n  hotstream:\n    targets:\n      - name: held\n        model: hotstream\n        base_url: http://127.0.0.1:8091/held\n        api_key: e2e-hot-c\n        max_active: %s\n' "$held_cap"
    if [ -n "$caller_key" ]; then
      printf 'auth:\n  api_key: %s\n' "$caller_key"
    fi
  } > "$WORK/config.yaml"
}

# The file case 12 left on disk was refused, so writing a valid one here is the recovery case: content
# coming back to being valid is a change, and a change is acted on without a signal.
write_config "$targets"
baseline=$(reloadcount ok)
check "content coming back to being valid is a change, and it is applied with no signal"   "$((baseline + 1))" "$(wait_for ok "$baseline")"

# 17.1 a key is a value in the file, so rotating it is a change to the file, and the running process picks
# it up with no signal.
stats
base_needle=$(count "$secret")
check "the file as written routes on the key it names" "200" "$(post_to 8080 hotpair /dev/null)"
stats
check "and the record is for that key" "$((base_needle + 1))" "$(count "$secret")"

targets='      - name: pair-1
        model: hotpair
        base_url: http://127.0.0.1:8091/pair-a
        api_key: e2e-hot-b'
write_config "$targets"
baseline=$(reloadcount ok)
check "the changed file is applied with no signal and no restart" "$((baseline + 1))" "$(wait_for ok "$baseline")"
check "and the same process is still serving" "alive" "$(kill -0 "$GATEWAY_PID" 2>/dev/null && echo alive)"
check "and its log has exactly one startup line, so this was not a restart" "1" \
  "$(grep -c '"msg":"listening"' "$WORK/gateway.log")"

stats
base_old=$(count "$secret")
base_new=$(count e2e-hot-b)
check "a request now goes to the key the changed file names" "200" "$(post_to 8080 hotpair /dev/null)"
stats
check "and that key is the one that answered" "$((base_new + 1))" "$(count e2e-hot-b)"
check "and the key the file no longer names is not used again" "$base_old" "$(count "$secret")"
has "the reload line names the file it read and the configuration in force, and never a value it read" \
  'generation' "$(grep 'config_reloaded' "$WORK/gateway.log" | tail -1)"
not_has "and no reload line prints the content hash, in the whole form or the short form the log used to carry" \
  '\b[0-9a-f]\{64\}\b\|\b[0-9a-f]\{10\}\b' "$(grep 'config_reload' "$WORK/gateway.log")"

# 17.2 a file that decodes but does not validate is refused, and the configuration in force is the last
# one that was valid. Only the running process can answer which configuration routes.
cp "$WORK/config.yaml" "$WORK/config-good.yaml"
cat > "$WORK/config.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8080
routing:
  cooldown:
    max: -1s
routes:
  badfilecheck:
    targets:
      - name: not-applied
        model: badfilecheck
        base_url: http://127.0.0.1:8091/v1
        api_key: PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645
YAML

baseline=$(reloadcount failed)
check "a file that decodes but does not validate is reported" "$((baseline + 1))" "$(wait_for failed "$baseline")"
check "and the gateway is still serving" "ok" "$(curl -s http://127.0.0.1:8080/healthz)"
check "and the process is the same one, so a bad file did not take it down" "alive" \
  "$(kill -0 "$GATEWAY_PID" 2>/dev/null && echo alive)"
check "a route only the refused file names is not in use" "404" "$(post_to 8080 badfilecheck /dev/null)"
not_has "and neither the log nor the metrics page prints the value the refused file held" "$secret" \
  "$(cat "$WORK/gateway.log"; curl -s http://127.0.0.1:8080/metrics)"
stats
base_good=$(count e2e-hot-b)
check "and the configuration that was in force keeps serving, on the key the last good file named" \
  "200" "$(post_to 8080 hotpair /dev/null)"
stats
check "and that request went to the key the last good file named, so nothing from the refused file was applied" \
  "$((base_good + 1))" "$(count e2e-hot-b)"

# Three more looks at the same content: one problem, reported once. A file that stays bad must not become
# a log line and a counter every second, so this waits past three intervals and checks the numbers did
# not move.
before_failed=$(reloadcount failed)
before_lines=$(grep -c 'config_reload_failed' "$WORK/gateway.log")
sleep 4
check "the same bad content is not counted again over three more looks" "$before_failed" "$(reloadcount failed)"
check "and it is not logged again" "$before_lines" "$(grep -c 'config_reload_failed' "$WORK/gateway.log")"

cp "$WORK/config-good.yaml" "$WORK/config.yaml"
baseline=$(reloadcount ok)
check "the file coming back to valid is applied, and the refusal did not replace what was in force" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"
check "and the route the refused file named is still not in use, because that content was never applied" \
  "404" "$(post_to 8080 badfilecheck /dev/null)"

# 17.3 what a reload changes, read back from what the upstream received. The target name does not change
# through any of this, which is what keeps its state: each fact below is a fact about the one target the
# changed file writes, so an answer can only come from the configuration that is in force.
targets='      - name: pair-1
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-b
        api_key: e2e-hot-b'
write_config "$targets"
baseline=$(reloadcount ok)
check "the model and the base_url the changed file writes are the ones in use" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"
check "and a request is served by the target the changed file names" "200" "$(post_to 8080 hotpair /dev/null)"
has "and its body carries the model as the changed file writes it" 'hotmodel' "$(field e2e-hot-b body)"
check "and it arrived on the path the changed file names" '/pair-b/chat/completions' "$(field e2e-hot-b path)"

# A target the file adds is a target the running config did not have, and one it no longer names is a
# target it no longer has. While both targets are idle, least_active keeps configuration order, so the
# order the requests go is what says which set of targets and which strategy is in force.
targets='      - name: pair-1
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-b
        api_key: e2e-hot-b
      - name: pair-2
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-c
        api_key: e2e-hot-d'
write_config "$targets"
baseline=$(reloadcount ok)
check "the target the file added is applied" "$((baseline + 1))" "$(wait_for ok "$baseline")"

stats
first=$(count e2e-hot-b)
second=$(count e2e-hot-d)
for _ in 1 2 3 4; do post_to 8080 hotpair /dev/null > /dev/null; done
stats
check "least_active keeps configuration order while both targets are idle" "$((first + 4))" "$(count e2e-hot-b)"
check "and the target the file added is not used yet" "$second" "$(count e2e-hot-d)"

strategy=round_robin
write_config "$targets"
baseline=$(reloadcount ok)
check "the strategy the changed file names is applied" "$((baseline + 1))" "$(wait_for ok "$baseline")"
before_b=$(count e2e-hot-b)
for _ in 1 2; do post_to 8080 hotpair /dev/null > /dev/null; done
stats
check "and the requests now alternate, which is the strategy in the file and not the one the process started with" \
  "$((before_b + 1))" "$(count e2e-hot-b)"
check "and the other request went to the target the file added" "$((second + 1))" "$(count e2e-hot-d)"

targets='      - name: pair-1
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-b
        api_key: e2e-hot-b'
write_config "$targets"
baseline=$(reloadcount ok)
check "the target the file removed is applied" "$((baseline + 1))" "$(wait_for ok "$baseline")"
stats
before=$(count e2e-hot-d)
check "the route is still served, by the target that is left" "200" "$(post_to 8080 hotpair /dev/null)"
stats
check "and the key the removed target had is not used again" "$before" "$(count e2e-hot-d)"

# 17.4 a stream that was already running when the file changed keeps the generation it started with. The
# order is made certain by the fake upstream: it writes the first frame, then waits four seconds before
# the next one, so the change is applied while the request is in the middle of it. What the request still
# has to decide afterwards is how much of the body it may read, and that is where a handler that read the
# configuration twice shows up.
# The baseline has to be read before the request is sent. Read after it, the number can already
# include the request being checked, and the check compares the count against itself plus one: it
# fails on a machine fast enough to record the request before the first read, and passes on one
# slow enough not to. A baseline that can include the thing it is measuring is not a baseline.
stats
base_held=$(count e2e-hot-c)
touch "$WORK/held.txt"
curl -s -N --max-time 20 -o "$WORK/held.txt" -H 'Content-Type: application/json' \
  -d '{"model":"hotstream","stream":true,"messages":[{"role":"user","content":"prompt-zq"}]}' \
  http://127.0.0.1:8080/v1/chat/completions > /dev/null &
held=$!

end=$((SECONDS + 6))
while [ "$(count e2e-hot-c)" = "$base_held" ] && [ "$SECONDS" -lt "$end" ]; do
  sleep 0.2
  stats
done
stats
check "the stream reached the upstream, so it was in flight when the file changed" "$((base_held + 1))" \
  "$(count e2e-hot-c)"
has "and the first frame arrived before the change, which is what makes the frame that follows the one read after it" 'frame one' "$(cat "$WORK/held.txt")"

cap=10
write_config "$targets"
baseline=$(reloadcount ok)
check "the cap the changed file names is applied while that stream is still running" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"

status=$(post_to 8080 hotstream "$WORK/full.txt")
check "while the stream holds the only slot of its cap, the next request is answered from state with no \
upstream call" "429" "$status"
has "and the answer is the code for every target being full" 'No healthy targets are currently available' \
  "$(cat "$WORK/full.txt")"
stats
check "and that request cost no upstream call, so the answer came from the state the stream is holding" \
  "$((base_held + 1))" "$(count e2e-hot-c)"

held_cap=2
write_config "$targets"
baseline=$(reloadcount ok)
check "the value the changed file writes is applied while the stream still holds a slot" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"
check "and the next request fits under it, which only the state that is still holding the slot can say" \
  "200" "$(post_to 8080 hotstream /dev/null)"
stats
check "and that request is one upstream attempt on the key that was already holding a slot, so the state \
survived the reload" "$((base_held + 2))" "$(count e2e-hot-c)"

check "and the stream is not cancelled by the reload" "" \
  "$(grep -o '"cancelled":\[[^]]*\]' "$WORK/stats.json" | grep -o 'e2e-hot-c' | head -1)"

wait "$held" 2>/dev/null
has "and the whole stream arrives, because the cap that ends a stream is the one in force when it starts, \
not the one that arrives later" 'frame two' "$(cat "$WORK/held.txt")"
has "and the terminal frame of that stream is in the bytes the client got, which is what a cap read after the change would have denied" '\[DONE\]' "$(cat "$WORK/held.txt")"
stats
has "and the client leaving is what cancelled that stream, so the check above answered the right thing" \
  'e2e-hot-c' "$(grep -o '"cancelled":\[[^]]*\]' "$WORK/stats.json")"

# 17.5 one whole generation per request, across a reload: the key a request reached and the path it
# reached have to belong to the same version of the file, because a request takes one snapshot and keeps
# it. A pairing no version of the file wrote is a request that read the file twice.
targets='      - name: pair-1
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-f
        api_key: e2e-hot-f'
write_config "$targets"
baseline=$(reloadcount ok)
check "the first version of the file for this run is applied" "$((baseline + 1))" "$(wait_for ok "$baseline")"

stats
base_f=$(count e2e-hot-f)
check "a request before the change goes to the key that version names" "200" "$(post_to 8080 hotpair /dev/null)"
stats
check "and its record names the path that version wrote" "$((base_f + 1))" "$(count e2e-hot-f)"

targets='      - name: pair-1
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-g
        api_key: e2e-hot-g'
write_config "$targets"

# The run is sent over a window longer than one reload interval, so the swap lands inside it: a run that
# ended before the watcher looked could only have checked one side, and a check on one side cannot tell a
# request that read the file twice from one that did not. Each key belongs to one version of the file, and
# so does the path its record names, so a pairing that no version of the file wrote is the mixed read.
mixed=0
seen_f=0
seen_g=0
base_f=$(count e2e-hot-f)
base_g=$(count e2e-hot-g)
baseline=$(reloadcount ok)
end=$((SECONDS + 3))
while [ "$SECONDS" -lt "$end" ]; do
  status=$(post_to 8080 hotpair /dev/null)
  if [ "$status" != "200" ]; then
    fail "a request in the run across the reload was not served"
    break
  fi
  stats
  key=""
  want=""
  if [ "$(count e2e-hot-f)" -gt "$base_f" ]; then
    key=e2e-hot-f
    base_f=$(count e2e-hot-f)
    want='/pair-f/chat/completions'
    seen_f=$((seen_f + 1))
  elif [ "$(count e2e-hot-g)" -gt "$base_g" ]; then
    key=e2e-hot-g
    base_g=$(count e2e-hot-g)
    want='/pair-g/chat/completions'
    seen_g=$((seen_g + 1))
  fi
  if [ -z "$key" ]; then
    fail "a request in the run reached neither key, so the upstream did not record it"
    break
  fi
  if [ "$(field "$key" path)" != "$want" ]; then
    mixed=$((mixed + 1))
  fi
done
check "the reload applied inside the run, so requests on both sides of the swap were checked" \
  "$((baseline + 1))" "$(reloadcount ok)"
check "and the run really crossed it, which is what makes the pairing check mean something" "1" \
  "$([ "$seen_f" -gt 0 ] && [ "$seen_g" -gt 0 ] && echo 1)"
check "and no request paired a key with the path the other version of the file wrote" "0" "$mixed"

# 17.6 the floor is a startup error, not a silent clamp: a file that would make the watcher a busy loop
# stops the process before it listens.
cat > "$WORK/floor.yaml" <<'YAML'
server:
  listen: 127.0.0.1:8102
config_reload:
  interval: 50ms
routes:
  floorcheck:
    targets:
      - name: floor
        model: floorcheck
        base_url: http://127.0.0.1:8091/v1
        api_key: e2e-hot-f
YAML
out=$(timeout 10 "$BIN/plainroute" -config "$WORK/floor.yaml" 2>&1); code=$?
check "a reload interval under the floor is a startup error" "1" "$code"
has "and says what it expects" 'config_reload.interval must be 100ms or more' "$out"
check "and the process did not listen" "" "$(curl -s --max-time 3 http://127.0.0.1:8102/healthz)"

# 17.7 the caller key rotates the same way: the gate is a value in the file, so changing it changes the
# gate for new requests, and the process is still the same process.
caller_key=e2e-caller-old
write_config "$targets"
baseline=$(reloadcount ok)
check "the gate is applied from the file without a signal" "$((baseline + 1))" "$(wait_for ok "$baseline")"
check "a request without a key is refused" "401" "$(caller none)"
check "a request with the key the file names is served" "200" "$(caller e2e-caller-old)"

caller_key=e2e-caller-new
write_config "$targets"
baseline=$(reloadcount ok)
check "the caller key is rotated by the watcher" "$((baseline + 1))" "$(wait_for ok "$baseline")"
check "and the key the file no longer names no longer opens the gate" "401" "$(caller e2e-caller-old)"
check "and the key the file now names does" "200" "$(caller e2e-caller-new)"
check "and the same process served both" "alive" "$(kill -0 "$GATEWAY_PID" 2>/dev/null && echo alive)"

# 17.8 the boundary between what a running process can act on and what it cannot. The listener this
# process answers on is the one it built at startup, so a file that asks for a different one cannot be
# applied by the running process, and that is what the refusal is for.
#
# The file here changes two things at once: the listener, which a running process cannot act on, and
# the key, which a new request would have read. Refusing the file as a whole is the transactional half
# these checks read: the answer for a request can only come from the configuration that is in force,
# so an answer on the key the refused file names would mean part of a refused file was applied.
caller_key=
write_config "$targets"
baseline=$(reloadcount ok)
check "the gate is turned off from the file, so these checks send one request each" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"

gen_before=$(grep 'config_reloaded' "$WORK/gateway.log" | tail -1 | gen)

listen=127.0.0.1:8103
targets='      - name: pair-1
        model: hotmodel
        base_url: http://127.0.0.1:8091/pair-g
        api_key: e2e-hot-h'
write_config "$targets"
refused_hash=$(sha_of "$WORK/config.yaml")
baseline=$(reloadcount restart_required)
check "a change to a field the process captured at startup is refused as a whole" \
  "$((baseline + 1))" "$(wait_for restart_required "$baseline")"

refusal=$(grep 'restart_required_field_changed' "$WORK/gateway.log" | tail -1)
has "the refusal names the field it refused" 'server.listen' "$refusal"
has "and says that a running process cannot act on it" 'restart_required_field_changed' "$refusal"
not_has "and prints the address the file asked for, which is the value it refused" '8103' "$refusal"
not_has "and prints a value from the file" 'e2e-hot-h' "$refusal"

# A check that compares two reads from the log passes on two empties, so the presence of the field has
# to be answered by itself: without these two, a build that stopped naming the generation would pass the
# check below on an empty compared with an empty.
check "the generation the process reports is a number it counted, and not an empty field" "yes" \
  "$([ -n "$gen_before" ] && echo yes)"
check "and the refusal line names one" "yes" "$([ -n "$(printf '%s' "$refusal" | gen)" ] && echo yes)"

# A refusal does not make a generation: the number in the refusal line is the number of the
# configuration that is still in force.
check "and the refusal names the same generation as the last file that was applied" \
  "$gen_before" "$(printf '%s' "$refusal" | gen)"

check "and the process still answers on the listener it was built with" "ok" \
  "$(curl -s http://127.0.0.1:8080/healthz)"
check "and nothing listens on the address the refused file asked for" "" \
  "$(curl -s --max-time 3 http://127.0.0.1:8103/healthz)"

stats
base_g=$(count e2e-hot-g)
check "a request is still served, by the key the last applied file named" "200" \
  "$(post_to 8080 hotpair "$WORK/in-force.txt")"
stats
check "and that request went to it, so nothing from the refused file was applied" \
  "$((base_g + 1))" "$(count e2e-hot-g)"
check "and the key only the refused file names was never used" "0" "$(count e2e-hot-h)"

# One refusal per content, not one per second, which is the same dedup the bad file is checked for:
# the hash is what the watcher keeps for itself, and the check is that the process did not print it.
before_refused=$(reloadcount restart_required)
before_lines=$(grep -c 'restart_required_field_changed' "$WORK/gateway.log")
sleep 4
check "the same refused content is not counted again over three more looks" \
  "$before_refused" "$(reloadcount restart_required)"
check "and it is not logged again" "$before_lines" \
  "$(grep -c 'restart_required_field_changed' "$WORK/gateway.log")"
not_has "and no reload line prints the content hash, in the whole form or the short form" \
  "$refused_hash\|${refused_hash:0:10}" \
  "$(grep 'config_reload' "$WORK/gateway.log"; curl -s http://127.0.0.1:8080/metrics; cat "$WORK/in-force.txt")"

# The file comes back to what this process was built with, plus a key a new request can read. That is
# applied: refusing a change did not freeze the watcher, and the generation moved only for the content
# that was acted on.
listen=127.0.0.1:8080
write_config "$targets"
baseline=$(reloadcount ok)
check "a file that only changes a field the process can act on is applied" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"
stats
base_h=$(count e2e-hot-h)
check "and the new key is the one that answers" "200" "$(post_to 8080 hotpair "$WORK/applied.txt")"
stats
check "and that request went to it" "$((base_h + 1))" "$(count e2e-hot-h)"
check "and the generation is one more than the last applied one, because the refusal made none" \
  "$((gen_before + 1))" "$(grep 'config_reloaded' "$WORK/gateway.log" | tail -1 | gen)"
check "and it is still the same process, so this was not a restart" "1" \
  "$(grep -c '"msg":"listening"' "$WORK/gateway.log")"

# 17.9 a field the schema does not have is refused at reload as well as at startup. The two fields
# removed for having no reader are refused by name, so a live file that writes one gets an answer that
# says the field does not exist, and the configuration in force keeps serving. A loader that accepted
# and ignored such a field would tell an operator the file said something.
cp "$WORK/config.yaml" "$WORK/config-good-179.yaml"
cat > "$WORK/config.yaml" <<YAML
server:
  # The listen value is left as the one in force, so the refusal here is the unknown field and not a
  # process-level change: the two rules have to be answerable apart from each other.
  listen: 127.0.0.1:8080
retry: 918273649
fallbacks:
  log_event: 918273650
routes:
  hotpair:
    targets:
      - name: pair-1
        model: hotpair
        base_url: http://127.0.0.1:8091/pair-a
        api_key: e2e-hot-179
YAML

baseline=$(reloadcount failed)
check "a live file that writes a field the schema does not have is reported" "$((baseline + 1))" "$(wait_for failed "$baseline")"
refusal=$(grep 'config_reload_failed' "$WORK/gateway.log" | tail -1)
has "the refusal names the field it refused" 'field retry not found' "$refusal"
has "and the other one, in the same message, because every problem is reported at once" 'field log_event not found' "$refusal"
not_has "and prints neither value it read, which is the rule for every config error in this package" '918273649\|918273650' \
  "$(grep 'config_reload' "$WORK/gateway.log"; curl -s http://127.0.0.1:8080/metrics)"
check "and the gateway is still serving, on the last file that was acted on" "ok" "$(curl -s http://127.0.0.1:8080/healthz)"
check "and it is the same process, so this was not a restart" "1" "$(grep -c '"msg":"listening"' "$WORK/gateway.log")"
stats
base_179=$(count e2e-hot-h)
check "and a request still routes on the configuration in force" "200" "$(post_to 8080 hotpair /dev/null)"
stats
check "and that request went to the key the last good file named, so nothing from the refused file was applied" \
  "$((base_179 + 1))" "$(count e2e-hot-h)"
check "and the key only the refused file names was never used" "0" "$(count e2e-hot-179)"
cp "$WORK/config-good-179.yaml" "$WORK/config.yaml"
baseline=$(reloadcount ok)
check "and the file coming back to fields the schema has is applied, so the refusal did not freeze the watcher" \
  "$((baseline + 1))" "$(wait_for ok "$baseline")"

printf '\n%d checks: %s\n' "$CHECKS" "$([ "$FAIL" = 0 ] && echo 'all passed' || echo 'failures')"
exit "$FAIL"




