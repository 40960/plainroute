// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
models:
  demo:
    upstream_model: provider/model-one
    deployments:
      - name: account-1
        base_url: https://api.example.com/v1
        api_key_env: EXAMPLE_API_KEY_1
`), Env{"EXAMPLE_API_KEY_1": "test-key"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.Server.Listen != ":8080" {
		t.Errorf("listen: got %q, want :8080", cfg.Server.Listen)
	}
	if cfg.Routing.Strategy != StrategyLeastActive {
		t.Errorf("strategy: got %q, want %s", cfg.Routing.Strategy, StrategyLeastActive)
	}
	if cfg.Routing.Cooldown.Status[429].Std() != 10*time.Second {
		t.Errorf("429 cooldown: got %s, want 10s", cfg.Routing.Cooldown.Status[429])
	}
	if cfg.Routing.Cooldown.Max.Std() != 5*time.Minute {
		t.Errorf("cooldown max: got %s, want 5m", cfg.Routing.Cooldown.Max)
	}
	if cfg.Timeouts.Connect.Std() != 10*time.Second || cfg.Timeouts.ResponseHeader.Std() != 60*time.Second {
		t.Errorf("timeouts: got connect %s, response_header %s", cfg.Timeouts.Connect, cfg.Timeouts.ResponseHeader)
	}
	if cfg.Timeouts.StreamIdle.Std() != 5*time.Minute {
		t.Errorf("stream_idle: got %s, want 5m", cfg.Timeouts.StreamIdle)
	}
	if cfg.Timeouts.Request != 0 {
		t.Errorf("overall request timeout: got %s, want 0 so long reasoning streams are not killed", cfg.Timeouts.Request)
	}
	if cfg.Server.WriteTimeout != 0 {
		t.Errorf("write timeout: got %s, want 0 so streaming is not cut", cfg.Server.WriteTimeout)
	}
	if cfg.Retry.Enabled {
		t.Error("retry must default to disabled")
	}
	if cfg.Fallback.Enabled {
		t.Error("fallbacks must default to disabled")
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("log format: got %q, want json", cfg.Logging.Format)
	}

	if got := cfg.Models["demo"].Deployments[0].APIKey; got != "test-key" {
		t.Errorf("api key: got %q, want test-key", got)
	}
}

func TestParseResolvesSecretsFromEnvironment(t *testing.T) {
	env := Env{
		"EXAMPLE_API_KEY_1": "key-one",
		"EXAMPLE_API_KEY_2": "key-two",
		"LOCAL_API_KEY":     "local-key",
	}
	cfg, err := Parse([]byte(`
models:
  demo:
    upstream_model: provider/model-one
    deployments:
      - name: account-one
        base_url: https://api.example.com/v1
        api_key_env: EXAMPLE_API_KEY_1
      - name: account-two
        base_url: https://api.example.com/v1
        api_key_env: EXAMPLE_API_KEY_2
  local:
    upstream_model: llama-3.1-8b
    deployments:
      - name: llama-cpp
        base_url: ${LOCAL_BASE_URL:-http://127.0.0.1:8081}/v1
        api_key_env: LOCAL_API_KEY
`), env)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	deployments := cfg.Models["demo"].Deployments
	if len(deployments) != 2 {
		t.Fatalf("deployments: got %d, want 2", len(deployments))
	}
	if deployments[0].APIKey != "key-one" || deployments[1].APIKey != "key-two" {
		t.Errorf("keys resolved incorrectly: %q %q", deployments[0].APIKey, deployments[1].APIKey)
	}

	local := cfg.Models["local"].Deployments[0]
	if local.BaseURL != "http://127.0.0.1:8081/v1" {
		t.Errorf("env default: got %q, want http://127.0.0.1:8081/v1", local.BaseURL)
	}
	if local.APIKey != "local-key" {
		t.Errorf("local key: got %q, want local-key", local.APIKey)
	}
}

func TestParseRejectsMissingSecret(t *testing.T) {
	_, err := Parse([]byte(`
models:
  demo:
    upstream_model: provider/model-one
    deployments:
      - name: account-one
        base_url: https://api.example.com/v1
        api_key_env: EXAMPLE_API_KEY_MISSING
`), Env{})
	if err == nil {
		t.Fatal("expected an error for a missing api_key_env")
	}
	if !strings.Contains(err.Error(), "EXAMPLE_API_KEY_MISSING") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Parse([]byte(`
routing:
  strategy: random
  cooldown:
    status:
      429: -1s
    max: -1s
timeouts:
  connect: -1s
headers:
  mode: forward-everything
logging:
  format: yaml
  level: shout
models:
  demo:
    deployments:
      - name: ""
        base_url: ftp://example.invalid/v1
        api_key_env: MISSING_KEY
      - name: dup
        base_url: https://example.invalid/v1?token=x
        api_key_env: OK_KEY
      - name: dup
        base_url: https://user:pass@example.invalid/v1
        api_key_env: OK_KEY
`), Env{"OK_KEY": "x"})
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"random", "connect", "mode", "level", "upstream_model",
		"ftp", "credentials", "query", "MISSING_KEY", "dup", "positive",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestParseRejectsBadDuration(t *testing.T) {
	_, err := Parse([]byte(`
timeouts:
  connect: soon
models:
  m:
    upstream_model: m
    deployments: [{name: a, base_url: http://127.0.0.1:8081, api_key_env: K}]
`), Env{"K": "x"})
	if err == nil {
		t.Fatal("expected a duration error")
	}
	if !strings.Contains(err.Error(), "soon") {
		t.Errorf("got: %v", err)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte(`
server:
  listen: ":8080"
  not_a_real_key: 1
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: http://127.0.0.1:8081
        api_key_env: K
`), Env{"K": "x"})
	if err == nil {
		t.Fatal("expected an error for an unknown config key")
	}
	if !strings.Contains(err.Error(), "not_a_real_key") {
		t.Errorf("error should name the unknown key, got: %v", err)
	}
}

func TestFallbackCyclesAreRefusedWhenEnabled(t *testing.T) {
	// Cycle detection runs only when fallbacks are opt-in, so a config that
	// describes a graph but keeps fallbacks off stays loadable.
	env := Env{"K": "x"}
	_, err := Parse([]byte(`
fallbacks:
  enabled: true
models:
  a:
    upstream_model: a
    fallbacks: [b]
    deployments: [{name: d1, base_url: http://127.0.0.1:8081, api_key_env: K}]
  b:
    upstream_model: b
    fallbacks: [a]
    deployments: [{name: d2, base_url: http://127.0.0.1:8081, api_key_env: K}]
`), env)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("got: %v", err)
	}

	if _, err := Parse([]byte(`
fallbacks:
  enabled: true
models:
  a:
    upstream_model: a
    fallbacks: [nope]
    deployments: [{name: d1, base_url: http://127.0.0.1:8081, api_key_env: K}]
`), env); err == nil {
		t.Fatal("expected an error for an unknown fallback target")
	}

	// Self fallback is refused even when the graph is trivial.
	if _, err := Parse([]byte(`
fallbacks:
  enabled: true
models:
  a:
    upstream_model: a
    fallbacks: [a]
    deployments: [{name: d1, base_url: http://127.0.0.1:8081, api_key_env: K}]
`), env); err == nil {
		t.Fatal("expected a self-fallback error")
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct {
		base, endpoint, want string
	}{
		{"https://api.example.com/v1", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1/", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"http://127.0.0.1:8081", "/chat/completions", "http://127.0.0.1:8081/chat/completions"},
	}
	for _, c := range cases {
		if got := JoinPath(c.base, c.endpoint); got != c.want {
			t.Errorf("JoinPath(%q, %q) = %q, want %q", c.base, c.endpoint, got, c.want)
		}
	}
}

func TestModelNamesAreStable(t *testing.T) {
	models := map[string]ModelConfig{"zeta": {}, "alpha": {}, "mid": {}}
	got := strings.Join(ModelNames(models), ",")
	if got != "alpha,mid,zeta" {
		t.Errorf("ModelNames: got %q, want alpha,mid,zeta", got)
	}
}

func TestMetricsPageIsOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	env := Env{"K": "x"}

	cfg, err := Parse([]byte(`
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: http://127.0.0.1:8081
        api_key_env: K
`), env)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Metrics.On() {
		t.Error("the metrics page should be on when the config does not say otherwise")
	}
	if cfg.Metrics.Path != "/metrics" {
		t.Errorf("metrics path: got %q, want /metrics", cfg.Metrics.Path)
	}

	// Enabled is a pointer precisely so that an explicit false is still an explicit
	// false, instead of being read as "not written".
	cfg, err = Parse([]byte(`
metrics:
  enabled: false
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: http://127.0.0.1:8081
        api_key_env: K
`), env)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Metrics.On() {
		t.Error("metrics.enabled: false must be honoured")
	}
}

func TestLoggingHasNoFieldListToConfigure(t *testing.T) {
	// The logger writes a fixed set of safe routing facts. There is no field list, so
	// there is no way to configure losing one of them.
	_, err := Parse([]byte(`
logging:
  format: json
  fields:
    ttft: false
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: http://127.0.0.1:8081
        api_key_env: K
`), Env{"K": "x"})
	if err == nil {
		t.Fatal("logging.fields should not be accepted")
	}
	if !strings.Contains(err.Error(), "fields") {
		t.Errorf("error should name the unknown key, got: %v", err)
	}
}
