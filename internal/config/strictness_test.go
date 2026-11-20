// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

func TestParseRefusesASecondYAMLDocument(t *testing.T) {
	_, err := Parse([]byte(`
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: key-a
---
routes:
  other:
    targets:
      - name: b
        model: m
        base_url: https://example.invalid/v1
        api_key: key-b
`))
	if err == nil || !strings.Contains(err.Error(), "second YAML document") {
		t.Errorf("a second document was accepted: %v", err)
	}
}

func TestBaseURLErrorsNeverEchoCredentialsOrQuery(t *testing.T) {
	const secret = "password-that-must-not-print"
	cases := []string{
		"ftp://user:" + secret + "@example.invalid/v1",
		"https://user:" + secret + "@example.invalid:notaport/v1",
		"https://example.invalid/v1?token=" + secret,
	}
	for _, base := range cases {
		_, err := Parse([]byte("routes:\n  m:\n    targets:\n      - name: a\n        model: m\n        base_url: " +
			base + "\n        api_key: key-a\n"))
		if err == nil {
			t.Errorf("base_url %q was accepted", base)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error echoed a credential from the value: %v", err)
		}
		if !strings.Contains(err.Error(), "base_url") {
			t.Errorf("error did not name the field: %v", err)
		}
	}
}

func TestConfiguredHeadersFailAtLoadWhenRuntimeCannotHonourThem(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"authorization", "extra:\n    Authorization: Bearer from-config", "reserved"},
		{"host", "extra:\n    Host: example.invalid", "reserved"},
		{"content length", "extra:\n    Content-Length: 10", "reserved"},
		{"hop by hop", "extra:\n    Connection: close", "reserved"},
		{"bad name", "extra:\n    Bad Name: value", "valid field name"},
		{"crlf value", "extra:\n    X-Test: \"value\\r\\nInjected: yes\"", "control character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte("headers:\n  " + c.extra + "\n" + targetConfig))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected %q, got %v", c.want, err)
			}
		})
	}
}

const targetConfig = `routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: key-a
`

func TestRuntimeOnlyUsesTheFirstFallback(t *testing.T) {
	_, err := Parse([]byte(`routes:
  a:
    fallbacks: [b, c]
    targets:
      - name: a1
        model: a1
        base_url: https://example.invalid/v1
        api_key: key-a
  b:
    targets:
      - name: b1
        model: b1
        base_url: https://example.invalid/v1
        api_key: key-b
  c:
    targets:
      - name: c1
        model: c1
        base_url: https://example.invalid/v1
        api_key: key-c
`))
	if err == nil || !strings.Contains(err.Error(), "more than one fallback") {
		t.Errorf("a partly ignored fallback list was accepted: %v", err)
	}
}

func TestNegativeMaxHopsIsAStartupError(t *testing.T) {
	_, err := Parse([]byte("fallbacks:\n  max_hops: -1\n" + targetConfig))
	if err == nil || !strings.Contains(err.Error(), "max_hops") {
		t.Errorf("negative max_hops was silently normalized: %v", err)
	}
}

func TestMetricsPathCannotCollideWithABuiltIn(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/v1/models", "/v1/chat/completions"} {
		_, err := Parse([]byte("metrics:\n  path: " + path + "\n" + targetConfig))
		if err == nil || !strings.Contains(err.Error(), "collides") {
			t.Errorf("metrics.path %q was accepted: %v", path, err)
		}
	}
}
