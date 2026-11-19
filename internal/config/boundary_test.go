// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

// A boundary claim is a claim about what a running process can act on, so the test asks what the
// process can answer: which side of a changed file a new request can see, and which side it cannot.
// A test that only checked that a name is in a list would pass on a list that forgot a field, so
// these tests compare two parsed configurations and read what the comparison returns.

// The needles are values that must not come back out: an address the file asked for, and a key.
// A field name is what a refusal is allowed to show.
const needleAddress = "127.0.0.1:9999"
const needleKey = "PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645"

const oneRoute = `routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: key-one
        path: /v1
        headers: {X-Test: on}
        max_active: 4
`

func TestARestartRequiredChangeNamesTheFieldAndNotTheValue(t *testing.T) {
	inForce, err := Parse([]byte(`server:
  listen: 127.0.0.1:8080
  shutdown_grace: 30s
routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: ` + needleKey + `
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// The file now asks for a different listener, and at the same time changes a key, a timeout and
	// a log level. Only the listener and the level are things a running process cannot act on, so
	// only those two are named.
	next, err := Parse([]byte(`server:
  listen: ` + needleAddress + `
  max_response_bytes: 64
logging:
  level: debug
timeouts:
  response_header: 90s
routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: other-key
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	changed := RestartRequiredChanges(inForce, next)
	if len(changed) != 2 {
		t.Fatalf("fields named: got %v, want the listener and the log level", changed)
	}
	if changed[0] != "logging.level" || changed[1] != "server.listen" {
		t.Errorf("fields named: got %v, want logging.level and server.listen", changed)
	}

	// What a refusal shows is names. A line that carried the values would print the address the file
	// asked for and the key the file holds.
	for _, name := range changed {
		if strings.Contains(name, needleAddress) || strings.Contains(name, needleKey) {
			t.Errorf("the reported field carries a value from the file: %s", name)
		}
	}
}

// A change a new request can read is not a reason to refuse a reload. These are the fields the
// request path reads from the generation a request started with.
func TestARequestLevelChangeIsNotReportedAsRestartRequired(t *testing.T) {
	inForce, err := Parse([]byte(oneRoute + "routing:\n  strategy: least_active\n" +
		"fallbacks:\n  enabled: false\nmetrics:\n  path: /metrics\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	next, err := Parse([]byte(oneRoute + "routing:\n  strategy: round_robin\n  cooldown:\n" +
		"    status: {429: 30s}\n    max: 60s\n    timeout: 2s\n" +
		"fallbacks:\n  enabled: true\n  max_hops: 2\nheaders:\n  mode: allow\n  allow: [X-Test]\n" +
		"  extra: {X-Extra: on}\n  forward_request_id: false\n" +
		"metrics:\n  enabled: false\n  path: /metrics\nconfig_reload:\n  interval: 250ms\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if changed := RestartRequiredChanges(inForce, next); len(changed) != 0 {
		t.Errorf("a change a new request can read was reported as restart-required: %v", changed)
	}

	// The same target name with a different key, model, endpoint and cap: the registry is built per
	// generation, so a new request sees all of it, and the request cap is read per request.
	nextTargets, err := Parse([]byte(`server:
  max_request_bytes: 2048
  max_response_bytes: 4096
timeouts:
  connect: 2s
  response_header: 30s
  stream_idle: 60s
  request: 120s
auth:
  api_key: caller-key
routes:
  demo:
    fallbacks: [other]
    targets:
      - name: one
        model: other-model
        base_url: https://example.invalid/other
        api_key: key-two
        path: /other
        headers: {X-Test: off2}
        max_active: 9
  other:
    targets:
      - name: two
        model: m
        base_url: https://example.invalid/v1
        api_key: key-three
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if changed := RestartRequiredChanges(inForce, nextTargets); len(changed) != 0 {
		t.Errorf("a target change was reported as restart-required: %v", changed)
	}

	// Every field the comparison must be able to name is a field the request path does not read.
	// This is the half that fails when the set loses a field, which a table in a document cannot
	// catch on its own.
	for _, name := range []string{
		"server.listen", "server.read_header_timeout", "server.read_timeout", "server.write_timeout",
		"server.idle_timeout", "server.shutdown_grace", "server.max_header_bytes",
		"logging.format", "logging.level",
	} {
		if _, ok := processLevelFields(inForce)[name]; !ok {
			t.Errorf("the boundary does not include %s", name)
		}
	}
}

// An absent field and its default are the same value, so a file that stopped writing a
// process-level field is not a change. Without this, deleting a line would refuse a whole reload for
// a value that did not change.
func TestAnAbsentValueAndItsDefaultAreTheSameValue(t *testing.T) {
	fromAbsent, err := Parse([]byte(oneRoute))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fromWritten, err := Parse([]byte(`server:
  listen: ":8080"
  read_header_timeout: 10s
  idle_timeout: 120s
  shutdown_grace: 30s
  max_header_bytes: 1048576
logging:
  format: json
  level: info
` + oneRoute))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if changed := RestartRequiredChanges(fromAbsent, fromWritten); len(changed) != 0 {
		t.Errorf("writing a default out as a value was reported as a change: %v", changed)
	}
}
