// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

// The caller key is optional, and it is a value in the file like any other value. These tests pin
// what optional means: nothing written means no gate, and something written is the value in force.
// There is no half state left, because there is no second place a value can come from: a gate that
// reads as on in the file is on, and that is the whole check.

func TestCallerKeyIsOffWhenNothingIsConfigured(t *testing.T) {
	cfg, err := Parse([]byte(`
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: target-key
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Auth.APIKey != "" {
		t.Errorf("gate should be off with nothing configured, got %q", cfg.Auth.APIKey)
	}
}

func TestCallerKeyIsTheValueInTheFile(t *testing.T) {
	cfg, err := Parse([]byte(`
auth:
  api_key: caller-key
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: target-key
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Auth.APIKey != "caller-key" {
		t.Errorf("caller key: got %q, want caller-key", cfg.Auth.APIKey)
	}
	if cfg.Routes["m"].Targets[0].APIKey != "target-key" {
		t.Errorf("the caller key and the target key are different values and must stay apart")
	}
}

// An empty string is what YAML reads as nothing, so an explicit empty key means off in the same way
// an absent block does. A gate written but empty must not read as a gate that is on: the comparison
// would then run against an empty value, and a caller presenting nothing would pass.
func TestEmptyCallerKeyMeansOffTheSameWayAsAbsent(t *testing.T) {
	cfg, err := Parse([]byte(`
auth:
  api_key: ""
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: target-key
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Auth.APIKey != "" {
		t.Errorf("got %q, want off", cfg.Auth.APIKey)
	}
}

// The field is called api_key because that is what it is: the same value, in the same header, with
// the same meaning as any other api key. A field named token is a different word for the same thing,
// and a config that accepts both words is a config with two names for one idea.
func TestCallerKeyHasOneName(t *testing.T) {
	_, err := Parse([]byte(`
auth:
  token: caller-key
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: target-key
`))
	if err == nil {
		t.Fatal("expected a key that nothing reads to be rejected")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("the error must name the unknown key: %v", err)
	}
}

// The field this change removed is not accepted, and the error says so instead of ignoring it. A
// config that still read api_key_env would be a config with two credential systems, which is what
// this change is about getting rid of.
func TestTheRemovedCredentialFieldIsRejected(t *testing.T) {
	_, err := Parse([]byte(`
auth:
  api_key_env: CALLER_KEY
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: target-key
`))
	if err == nil {
		t.Fatal("expected the removed field to be rejected rather than ignored")
	}
	if !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("the error must name the field that no longer exists: %v", err)
	}
}
