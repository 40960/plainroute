// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Two fields were removed because nothing read them. The point is not tidiness: a config value that
// parses, validates and can be written in a document, while no code uses it, is a setting an operator
// can set and get nothing from, and a loader that accepts it is a loader that tells the operator the
// file said something. The strict loader is what answers, so these tests check that it refuses.

const oneTarget = `routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: key-one
`

func TestTheRemovedRetryBlockIsRefusedRatherThanIgnored(t *testing.T) {
	_, err := Parse([]byte("retry:\n  enabled: true\n" + oneTarget))
	if err == nil {
		t.Fatal("a config that writes retry was accepted, so the loader can still take a field nothing reads")
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("the error must name the field that no longer exists: %v", err)
	}

	// The error names the field and not the value it was given, which is the rule for every config
	// error in this package: a value in this file can be a secret.
	if strings.Contains(err.Error(), "true") {
		t.Errorf("the error printed the value it read: %v", err)
	}
}

func TestTheRemovedFallbackLogFieldIsRefusedRatherThanIgnored(t *testing.T) {
	_, err := Parse([]byte("fallbacks:\n  log_event: true\n" + oneTarget))
	if err == nil {
		t.Fatal("a config that writes log_event was accepted, so a field nothing reads is still loadable")
	}
	if !strings.Contains(err.Error(), "log_event") {
		t.Errorf("the error must name the field that no longer exists: %v", err)
	}
	if strings.Contains(err.Error(), "true") {
		t.Errorf("the error printed the value it read: %v", err)
	}
}

// The half that keeps a dead field from arriving again: every name in the schema has to be matched
// with the code that reads it, and the audit has to name nothing that is not in the schema. A field
// added to the struct fails this until someone says what reads it, which is the only way this rule can
// be checked from the code. It cannot prove that a name on the list is really read: that is what the
// runtime tests do, one field at a time, and this test says plainly that it is a guard on the schema
// rather than a proof about behaviour.
func TestEveryNameInTheSchemaHasANamedReader(t *testing.T) {
	readers := map[string]string{
		"server":                     "the block the http.Server and the listener were built from",
		"server.listen":              "net.Listen, once, at startup",
		"server.read_header_timeout": "the http.Server this process built",
		"server.read_timeout":        "the http.Server this process built",
		"server.write_timeout":       "the http.Server this process built",
		"server.idle_timeout":        "the http.Server, and the idle connection timeout of each transport",
		"server.shutdown_grace":      "the shutdown path in main.go",
		"server.max_request_bytes":   "the body read on every request",
		"server.max_response_bytes":  "the copy limit on every response",
		"server.max_header_bytes":    "the http.Server this process built",

		"timeouts":                 "the transport and the request context",
		"timeouts.connect":         "the dialer inside the transport",
		"timeouts.response_header": "the transport a generation was built with",
		"timeouts.stream_idle":     "the stream copy",
		"timeouts.request":         "the context of one attempt",

		"routing":                  "the registry",
		"routing.strategy":         "which target a request picks",
		"routing.cooldown":         "the window state per target",
		"routing.cooldown.status":  "the window an upstream status applies",
		"routing.cooldown.max":     "the clamp on a Retry-After from the upstream",
		"routing.cooldown.timeout": "the window a connection or idle failure applies",

		"fallbacks":          "the chain walk in the request path",
		"fallbacks.enabled":  "whether that walk happens at all",
		"fallbacks.max_hops": "the bound on the walk",

		"headers":                    "the header copy",
		"headers.mode":               "which list wins",
		"headers.deny":               "the headers kept out",
		"headers.allow":              "the headers let through in allow mode",
		"headers.extra":              "the headers added to the upstream request",
		"headers.forward_request_id": "whether the request id is forwarded",

		"logging":        "the logger built once at startup",
		"logging.format": "the handler that logger uses",
		"logging.level":  "the level that logger reports at",

		"metrics":         "whether the page is served at all",
		"metrics.enabled": "the switch for the metrics page",
		"metrics.path":    "the path the metrics page answers on",

		"config_reload":          "the watcher",
		"config_reload.interval": "how often the watcher looks, and the floor startup enforces",

		"auth":         "the caller gate",
		"auth.api_key": "the value the gate compares, and whether it is on",

		"routes":                    "the registry and the route a request resolves to",
		"routes.fallbacks":          "the chain walk, per route",
		"routes.targets":            "the targets one route can pick",
		"routes.targets.name":       "the registry key, and the name an error reports",
		"routes.targets.model":      "the model value spliced into the body",
		"routes.targets.base_url":   "the upstream URL",
		"routes.targets.api_key":    "the authorisation header sent upstream",
		"routes.targets.path":       "the endpoint the target is asked on",
		"routes.targets.headers":    "the per-target headers added to the upstream request",
		"routes.targets.max_active": "the concurrency cap for one target",
	}

	schema := map[string]bool{}
	walk(reflect.TypeOf(Config{}), "", schema)

	for name := range schema {
		if readers[name] == "" {
			t.Errorf("%s is in the schema with no reader named for it", name)
		}
	}
	for name := range readers {
		if !schema[name] {
			t.Errorf("the audit names %s, which the schema does not have", name)
		}
	}

	// The guard is only worth having if the schema walk finds something, so the count is stated.
	if len(schema) < 30 {
		t.Fatalf("the schema walk found %d names, which is not the schema this test is about", len(schema))
	}
}

// walk lists the schema by the yaml names a reader writes, including the blocks, so a name is matched
// against the same text the loader refuses or accepts.
func walk(t reflect.Type, prefix string, found map[string]bool) {
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		if prefix != "" {
			name = prefix + "." + tag
		}
		found[name] = true

		field := t.Field(i).Type
		if field.Kind() == reflect.Struct {
			walk(field, name, found)
			continue
		}
		if field.Kind() == reflect.Ptr {
			field = field.Elem()
		}
		if field.Kind() == reflect.Struct {
			walk(field, name, found)
			continue
		}
		if field.Kind() == reflect.Map || field.Kind() == reflect.Slice {
			if elem := field.Elem(); elem.Kind() == reflect.Struct {
				walk(elem, name, found)
			}
		}
	}
}

// The shipped example is the file a new user copies, so it is checked against the loader: an example
// that no longer matches the schema is an example that a reader copies and the process refuses.
func TestTheShippedExampleIsStillAValidConfigForTheStrictLoader(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}

	if len(cfg.Routes) == 0 {
		t.Fatal("the example config has no routes, so this test read an empty file")
	}
	if cfg.Fallback.Enabled {
		t.Error("the example must show fallbacks off, which is the default it states")
	}
}
