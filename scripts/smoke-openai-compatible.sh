#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Live check of an OpenAI-compatible endpoint as a client actually sees it: the model list, a
# non-stream answer, a streamed answer, a tool call, a top-level field this project does not define,
# an error answer, and one optional multimodal request. Every line reports the status, the time to
# the first byte and the total time, and prints structural facts only: not the key, not the prompt,
# not the answer body.
#
# This is an operator tool, not a CI test. CI runs it against the project's own fake upstream, so a
# green run in CI says these checks can pass and can fail, not that anyone's provider works. Nothing
# in CI contacts a provider.
#
# Usage:
#   BASE_URL=https://host/v1 API_KEY=key MODEL=model ./scripts/smoke-openai-compatible.sh
#   IMAGE_FIXTURE=/path/to.png   optional; adds one multimodal request
#   TIMEOUT=60                   seconds for each request
#   SMOKE_PROMPT=...             the text sent; this script never prints it
#
# Two checks assume a capability an endpoint may not have: a tool call, and a top-level field the
# endpoint has no definition for. A provider that answers those with plain content, or with a 400,
# fails the check, and that is a finding about the provider rather than a bug in this script.
#
# Exit: 0 when every check passed, 1 when at least one failed, 2 when the arguments were unusable.
set -u

BASE_URL=${BASE_URL:-}
API_KEY=${API_KEY:-}
MODEL=${MODEL:-}
IMAGE_FIXTURE=${IMAGE_FIXTURE:-}
TIMEOUT=${TIMEOUT:-60}
PROMPT=${SMOKE_PROMPT:-smoke prompt content qz}

if [ -z "$BASE_URL" ] || [ -z "$API_KEY" ] || [ -z "$MODEL" ]; then
  printf 'BASE_URL, API_KEY and MODEL are required\n' >&2
  exit 2
fi

# A key should not travel in the clear. Loopback is the one exception, because the project's own
# fake upstream answers on loopback, and that is where this script is exercised by CI.
case "$BASE_URL" in
  https://*) ;;
  http://127.0.0.1:* | http://localhost:*) ;;
  *)
    printf 'BASE_URL must be https, or http to loopback only\n' >&2
    exit 2
  ;;
esac

BASE=${BASE_URL%/}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILS=0
CHECKS=0

# One request, measured. The body comes from a file rather than from the command line, because a
# shell trace prints the command line, and the prompt is inside it.
call() { # id, method, path, body file (empty for GET)
  local id=$1 method=$2 path=$3 body=${4:-} line
  if [ "$method" = GET ]; then
    line=$(curl -s -o "$WORK/$id.body" -D "$WORK/$id.hdr" \
      -w '%{http_code} %{time_starttransfer} %{time_total}' \
      -H "Authorization: Bearer $API_KEY" --max-time "$TIMEOUT" "$BASE$path") || line=''
  else
    line=$(curl -s -o "$WORK/$id.body" -D "$WORK/$id.hdr" \
      -w '%{http_code} %{time_starttransfer} %{time_total}' \
      -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
      --max-time "$TIMEOUT" -X POST "$BASE$path" -d @"$body") || line=''
  fi
  if [ -z "$line" ]; then
    CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1))
    printf '  FAIL  %-28s status=- ttft=-ms total=-ms no response\n' "$id"
    return 1
  fi
  STATUS=$(printf '%s' "$line" | awk '{print $1}')
  TTFT=$(printf '%s' "$line" | awk '{printf "%d", $2 * 1000}')
  TOTAL=$(printf '%s' "$line" | awk '{printf "%d", $3 * 1000}')
  CT=$(sed -n 's/^[Cc]ontent-[Tt]ype: *//p' "$WORK/$id.hdr" | tr -d '\r' | head -1)
  return 0
}

report() { # ok or FAIL, name, facts
  CHECKS=$((CHECKS + 1))
  if [ "$1" = FAIL ]; then FAILS=$((FAILS + 1)); fi
  printf '  %-5s %-28s status=%s ttft=%sms total=%sms %s\n' "$1" "$2" "$STATUS" "$TTFT" "$TOTAL" "$3"
}

# A fact is one field of the answer, read as text. Nothing here parses the answer into a document
# and re-encodes it, which is why a shape this project has never seen can still be reported.
field() { grep -o "$1" "$2" | head -1; }

printf 'smoke: %s model=%s timeout=%ss\n' "$BASE" "$MODEL" "$TIMEOUT"

# 1. the model list. The list is reported as it arrives; a model that is not in it is a fact to
# print, not a failure, because a provider can serve a model it does not list.
call models GET /models
list_shape=$(field '"object":"[^"]*"' "$WORK/models.body")
entries=$(grep -o '"id":"' "$WORK/models.body" | wc -l | tr -d ' ')
listed=no
if grep -qF "$MODEL" "$WORK/models.body"; then listed=yes; fi
if [ "$STATUS" = 200 ] && [ "$list_shape" = '"object":"list"' ]; then
  report ok 'model list' "entries=$entries model_listed=$listed"
else
  report FAIL 'model list' "shape=${list_shape:-none}"
fi

# 2. a non-stream answer.
printf '%s' "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" > "$WORK/plain.json"
call plain POST /chat/completions "$WORK/plain.json"
shape=$(field '"object":"[^"]*"' "$WORK/plain.body")
choices=$(grep -o '"choices":\[' "$WORK/plain.body" | wc -l | tr -d ' ')
if [ "$STATUS" = 200 ] && [ "$shape" = '"object":"chat.completion"' ] && [ "$choices" -ge 1 ]; then
  report ok 'non-stream completion' "shape=$shape choices=$choices content_type=$CT"
else
  report FAIL 'non-stream completion' "shape=${shape:-none} choices=$choices content_type=$CT"
fi

# 3. a streamed answer: frames, and the frame that says the stream ended.
printf '%s' "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" > "$WORK/stream.json"
call stream POST /chat/completions "$WORK/stream.json"
frames=$(grep -c '^data: ' "$WORK/stream.body")
delta_frames=$(grep -c '"delta":' "$WORK/stream.body")
last_frame=$(grep '^data: ' "$WORK/stream.body" | tail -1)
if [ "$STATUS" = 200 ] && [ "$CT" = "text/event-stream" ] && [ "$frames" -ge 2 ] \
  && [ "$last_frame" = 'data: [DONE]' ] && [ "$delta_frames" -ge 1 ]; then
  report ok 'stream completion' "frames=$frames delta_frames=$delta_frames last_frame=[DONE] content_type=$CT"
else
  report FAIL 'stream completion' "frames=$frames delta_frames=$delta_frames last_frame=${last_frame:-none} content_type=$CT"
fi

# 4. a tool call, as a client that asked for one sees it.
printf '%s' "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT call the tool named smoke_tool\"}],\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"smoke_tool\",\"parameters\":{\"type\":\"object\",\"properties\":{}}}}]}" > "$WORK/tool.json"
call tool POST /chat/completions "$WORK/tool.json"
tool_calls=$(grep -o '"tool_calls":' "$WORK/tool.body" | wc -l | tr -d ' ')
tool_name=$(field '"name":"[^"]*"' "$WORK/tool.body")
if [ "$STATUS" = 200 ] && [ "$tool_calls" -ge 1 ]; then
  report ok 'tool call' "tool_calls=$tool_calls name=${tool_name:-none}"
else
  report FAIL 'tool call' "tool_calls=$tool_calls name=${tool_name:-none}"
fi

# 5. a top-level field this project does not define, and does not touch.
printf '%s' "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}],\"smoke_unknown_field\":{\"nested\":[1,2,3]}}" > "$WORK/unknown.json"
call unknown POST /chat/completions "$WORK/unknown.json"
if [ "$STATUS" = 200 ]; then
  report ok 'unknown top-level field' "accepted=1 shape=$(field '"object":"[^"]*"' "$WORK/unknown.body")"
else
  report FAIL 'unknown top-level field' "accepted=0 shape=$(field '"object":"[^"]*"' "$WORK/unknown.body")"
fi

# 6. an error answer. A request an endpoint can be expected to reject: an empty messages array. What
# is checked is that the answer kept its own status and its own body, and that the body is the
# endpoint's own document rather than an HTML page from something in front of it.
printf '%s' "{\"model\":\"$MODEL\",\"messages\":[]}" > "$WORK/err.json"
call error POST /chat/completions "$WORK/err.json"
html=no
if grep -q '<' "$WORK/error.body"; then html=yes; fi
retry_after=$(sed -n 's/^[Rr]etry-[Aa]fter: *//p' "$WORK/error.hdr" | tr -d '\r' | head -1)
if [ "$STATUS" -ge 400 ] && [ "$STATUS" -le 499 ] && [ "$html" = no ]; then
  report ok 'error answer is its own' "rejected=1 html=no retry_after=${retry_after:-none}"
else
  report FAIL 'error answer is its own' "rejected=${STATUS} html=$html retry_after=${retry_after:-none}"
fi

# 7. the optional image. The file is sent as bytes; nothing here reads it, so a file that is not a
# real PNG travels the same path as one that is. Without IMAGE_FIXTURE this line says it was skipped,
# which is a fact the operator can see rather than a check that passed for the wrong reason.
if [ -z "$IMAGE_FIXTURE" ]; then
  CHECKS=$((CHECKS + 1))
  printf '  skip  %-28s not given\n' 'image fixture'
elif [ ! -r "$IMAGE_FIXTURE" ]; then
  CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1))
  printf '  FAIL  %-28s not readable\n' 'image fixture'
else
  B64=$(base64 < "$IMAGE_FIXTURE" | tr -d '\n')
  printf '%s' "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"$PROMPT\"},{\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,$B64\"}}]}]}" > "$WORK/image.json"
  call image POST /chat/completions "$WORK/image.json"
  if [ "$STATUS" = 200 ]; then
    report ok 'image fixture' "file_bytes=$(wc -c < "$IMAGE_FIXTURE" | tr -d ' ') body_bytes=$(wc -c < "$WORK/image.json" | tr -d ' ') accepted=1"
  else
    report FAIL 'image fixture' "file_bytes=$(wc -c < "$IMAGE_FIXTURE" | tr -d ' ') body_bytes=$(wc -c < "$WORK/image.json" | tr -d ' ') accepted=0"
  fi
fi

printf '\n%d checks: %s\n' "$CHECKS" "$([ "$FAILS" = 0 ] && echo 'all passed' || printf '%s failed' "$FAILS")"
exit "$FAILS"
