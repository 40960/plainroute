// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

// SECURITY.md claims that upstream URLs come only from configuration. The part of that
// claim that can be checked at load time is here: a base_url that carries credentials or
// a query is refused, because both are ways for a config file to smuggle a target or a
// secret that the gateway would then carry on every request.

func TestBaseURLRulesAreEnforcedAtLoad(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{"https://example.invalid/v1", ""},
		{"http://127.0.0.1:11434/v1", ""},
		{"ftp://example.invalid/v1", "http or https"},
		{"https://user:pass@example.invalid/v1", "credentials"},
		{"https://example.invalid/v1?token=x", "query"},
		{"example.invalid/v1", "http or https"},
		{"https:///v1", "must include a host"},
		{"https://example.invalid", ""},
	}

	for _, c := range cases {
		_, err := Parse([]byte("routes:\n  m:\n    targets:\n      - name: a\n        model: m\n        base_url: " + c.baseURL + "\n        api_key: key-a\n"))
		if c.want == "" {
			if err != nil {
				t.Errorf("base_url %q: unexpected rejection: %v", c.baseURL, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("base_url %q: expected %q to be refused", c.baseURL, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("base_url %q: error is missing %q: %v", c.baseURL, c.want, err)
		}
	}
}
