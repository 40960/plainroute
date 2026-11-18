// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

// The api key a caller presents is optional. These tests pin the two halves of that:
// nothing configured means no gate, and a named variable that is not set is a startup
// failure rather than a quietly open gate.

func TestCallerKeyIsOffWhenNothingIsConfigured(t *testing.T) {
	cfg, err := Parse([]byte(`
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://example.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`), Env{"DEPLOYMENT_KEY": "target-key"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Auth.APIKey != "" || cfg.Auth.APIKeyEnv != "" {
		t.Errorf("gate should be off with nothing configured, got %+v", cfg.Auth)
	}
}

func TestCallerKeyIsResolvedFromTheEnvironment(t *testing.T) {
	cfg, err := Parse([]byte(`
auth:
  api_key_env: CALLER_KEY
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://example.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`), Env{"CALLER_KEY": "caller-key", "DEPLOYMENT_KEY": "target-key"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Auth.APIKey != "caller-key" {
		t.Errorf("caller key: got %q, want the value from the environment", cfg.Auth.APIKey)
	}
	if cfg.Models["m"].Targets[0].APIKey != "target-key" {
		t.Errorf("the target key must stay separate from the caller key")
	}
}

// A half-configured gate is worse than no gate: the operator thinks callers are checked.
func TestNamedCallerKeyMustExistInTheEnvironment(t *testing.T) {
	_, err := Parse([]byte(`
auth:
  api_key_env: CALLER_KEY
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://example.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`), Env{"DEPLOYMENT_KEY": "target-key"})
	if err == nil {
		t.Fatal("expected a startup failure for a named but unset caller key")
	}
	if !strings.Contains(err.Error(), "CALLER_KEY") {
		t.Errorf("the error must name the environment variable: %v", err)
	}
}

// The field is called api key because that is what it is: the same value, in the same
// header, with the same meaning as any other api key. A field named token is a different
// word for the same thing, and a config that accepts both words is a config with two names
// for one idea.
func TestCallerKeyHasOneName(t *testing.T) {
	_, err := Parse([]byte(`
auth:
  token: CALLER_KEY
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://example.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`), Env{"DEPLOYMENT_KEY": "target-key"})
	if err == nil {
		t.Fatal("expected a key that nothing reads to be rejected")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("the error must name the unknown key: %v", err)
	}
}
