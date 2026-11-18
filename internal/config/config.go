// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads PlainRoute's YAML configuration.
//
// PlainRoute is a single process with no database and no external service:
// everything it needs is described in one file. Secrets are never written in
// that file - a target names an environment variable and the value is read
// at startup, so the configuration itself can be committed to a repository.
//
// String values support environment substitution:
//
//	${NAME}          replaced by the value of NAME
//	${NAME:-default} replaced by NAME, or by default when NAME is unset
//
// Substitution happens on the raw file before YAML parsing, so it works for any
// string value.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Env is the environment lookup used while loading. It is a parameter so tests
// can supply their own values without touching the process environment.
type Env map[string]string

// DefaultEnv reads the real process environment.
func DefaultEnv() Env {
	env := Env{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[name] = value
	}
	return env
}

// Duration accepts YAML values such as "10s", "5m" or "1h30m". A plain number is
// read as seconds, which keeps short timeouts readable in a config file.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string such as 10s, got tag %s", node.Tag)
	}
	if node.Value == "" {
		*d = 0
		return nil
	}
	if v, err := time.ParseDuration(node.Value); err == nil {
		*d = Duration(v)
		return nil
	}
	if n, err := strconv.ParseInt(node.Value, 10, 64); err == nil {
		*d = Duration(n * int64(time.Second))
		return nil
	}
	return fmt.Errorf("invalid duration %q: expected a value such as 10s or 5m", node.Value)
}

// Config is the whole gateway configuration.
type Config struct {
	Server   ServerConfig           `yaml:"server"`
	Timeouts TimeoutsConfig         `yaml:"timeouts"`
	Routing  RoutingConfig          `yaml:"routing"`
	Retry    RetryConfig            `yaml:"retry"`
	Fallback FallbackConfig         `yaml:"fallbacks"`
	Headers  HeadersConfig          `yaml:"headers"`
	Logging  LoggingConfig          `yaml:"logging"`
	Metrics  MetricsConfig          `yaml:"metrics"`
	Auth     AuthConfig             `yaml:"auth"`
	Models   map[string]ModelConfig `yaml:"models"`
}

// AuthConfig is the api key a caller presents, read from the environment. It is a single
// value, not a table of keys: a gateway that knows who its callers are needs accounts, and
// accounts are the point this project stays out of.
//
// Empty means off, which is also what an absent block means. A key that is named but not
// present in the environment is a startup failure, because a half-configured gate reads as
// a working one.
type AuthConfig struct {
	APIKeyEnv string `yaml:"api_key_env"`

	// APIKey is resolved from APIKeyEnv at load. It is never logged and never written
	// back to YAML.
	APIKey string `yaml:"-"`
}

type ServerConfig struct {
	Listen            string   `yaml:"listen"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout"`
	WriteTimeout      Duration `yaml:"write_timeout"` // 0 means no limit, which streaming needs
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownGrace     Duration `yaml:"shutdown_grace"`

	MaxRequestBytes  int64 `yaml:"max_request_bytes"`
	MaxResponseBytes int64 `yaml:"max_response_bytes"`
	MaxHeaderBytes   int   `yaml:"max_header_bytes"`
}

type TimeoutsConfig struct {
	Connect        Duration `yaml:"connect"`
	ResponseHeader Duration `yaml:"response_header"`
	StreamIdle     Duration `yaml:"stream_idle"`
	Request        Duration `yaml:"request"` // 0 means no overall limit
}

type RoutingConfig struct {
	Strategy string         `yaml:"strategy"`
	Cooldown CooldownConfig `yaml:"cooldown"`
}

// CooldownConfig is deliberately small. A status code maps to a cooldown
// duration; 0 means that status does not cool a target down. Retry-After
// from the upstream always wins, up to Max.
type CooldownConfig struct {
	Status map[int]Duration `yaml:"status"`
	Max    Duration         `yaml:"max"`

	// Timeout cools a target down after a connection, response-header or stream
	// idle failure. 0 disables it. Authentication failures are handled through the
	// status map, for example 401 or 403.
	Timeout Duration `yaml:"timeout"`
}

// DefaultCooldown is the behaviour the project is built around: a 429 takes the
// target that produced it out of the pool for ten seconds.
func DefaultCooldown() map[int]Duration {
	return map[int]Duration{429: Duration(10 * time.Second)}
}

// RetryConfig is off by default. PlainRoute does not retry inside one client
// request; the client owns backoff. When enabled, a retry only happens before
// any byte of the upstream response has reached the client.
type RetryConfig struct {
	Enabled     bool `yaml:"enabled"`
	MaxAttempts int  `yaml:"max_attempts"`
}

// FallbackConfig is off by default. Falling back to a different model is opt-in,
// bounded, and only allowed before the first byte reaches the client.
type FallbackConfig struct {
	Enabled  bool `yaml:"enabled"`
	MaxHops  int  `yaml:"max_hops"`
	LogEvent bool `yaml:"log_event"`
}

// HeadersConfig controls which request headers reach the upstream. Hop-by-hop
// headers are always removed. Mode "deny" (the default) forwards everything
// except Deny; mode "allow" forwards only Allow plus the per-target headers.
type HeadersConfig struct {
	Mode             string            `yaml:"mode"`
	Deny             []string          `yaml:"deny"`
	Allow            []string          `yaml:"allow"`
	Extra            map[string]string `yaml:"extra"`
	ForwardRequestID bool              `yaml:"forward_request_id"`
}

// LoggingConfig is all the logger needs. There is no list of fields: the gateway logs
// a fixed set of routing facts and every one of them is safe to write down, so a
// config that could turn a field off would only add a way to lose information.
type LoggingConfig struct {
	Format string `yaml:"format"` // json (default) or text
	Level  string `yaml:"level"`  // info (default), debug, warn, error
}

// MetricsConfig. Enabled is a pointer so that "not written" and "written as false" are
// different things: the page is on by default, and an operator can still turn it off.
type MetricsConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// On reports whether the metrics page is served. Absent means on.
func (m MetricsConfig) On() bool { return m.Enabled == nil || *m.Enabled }

type ModelConfig struct {
	UpstreamModel string         `yaml:"upstream_model"`
	Fallbacks     []string       `yaml:"fallbacks"`
	Targets       []TargetConfig `yaml:"targets"`
}

type TargetConfig struct {
	Name      string            `yaml:"name"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Path      string            `yaml:"path"`
	Headers   map[string]string `yaml:"headers"`
	MaxActive int               `yaml:"max_active"`

	// APIKey is resolved from APIKeyEnv at load. It is never logged and never
	// written back to YAML.
	APIKey string `yaml:"-"`
}

const (
	StrategyLeastActive = "least_active"
	StrategyRoundRobin  = "round_robin"

	DefaultEndpoint = "/chat/completions"
)

// envPattern matches ${NAME} and ${NAME:-default}.
var envPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// Load reads, expands and validates a configuration file.
func Load(file string, env Env) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, env)
}

// Parse expands environment references, decodes YAML and validates.
func Parse(data []byte, env Env) (*Config, error) {
	expanded, err := expandEnv(data, env)
	if err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(strings.NewReader(string(expanded)))
	dec.KnownFields(true)

	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(cfg)
	if err := validate(cfg, env); err != nil {
		return nil, err
	}
	return cfg, nil
}

func expandEnv(data []byte, env Env) ([]byte, error) {
	var problems []string
	out := envPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		expr := string(match[2 : len(match)-1])
		name, def, hasDefault := strings.Cut(expr, ":-")
		if name == "" {
			problems = append(problems, "empty environment reference")
			return match
		}
		if v, ok := env[name]; ok && v != "" {
			return []byte(v)
		}
		if hasDefault {
			return []byte(def)
		}
		problems = append(problems, fmt.Sprintf("environment variable %s is not set", name))
		return match
	})
	if len(problems) > 0 {
		return nil, errors.New(problems[0])
	}
	return out, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}
	if cfg.Server.ReadHeaderTimeout == 0 {
		cfg.Server.ReadHeaderTimeout = Duration(10 * time.Second)
	}
	if cfg.Server.IdleTimeout == 0 {
		cfg.Server.IdleTimeout = Duration(120 * time.Second)
	}
	if cfg.Server.ShutdownGrace == 0 {
		cfg.Server.ShutdownGrace = Duration(30 * time.Second)
	}
	if cfg.Server.MaxRequestBytes <= 0 {
		cfg.Server.MaxRequestBytes = 8 << 20
	}
	if cfg.Server.MaxResponseBytes <= 0 {
		cfg.Server.MaxResponseBytes = 32 << 20
	}
	if cfg.Server.MaxHeaderBytes <= 0 {
		cfg.Server.MaxHeaderBytes = 1 << 20
	}

	if cfg.Timeouts.Connect == 0 {
		cfg.Timeouts.Connect = Duration(10 * time.Second)
	}
	if cfg.Timeouts.ResponseHeader == 0 {
		cfg.Timeouts.ResponseHeader = Duration(60 * time.Second)
	}
	if cfg.Timeouts.StreamIdle == 0 {
		cfg.Timeouts.StreamIdle = Duration(5 * time.Minute)
	}

	if cfg.Routing.Strategy == "" {
		cfg.Routing.Strategy = StrategyLeastActive
	}
	if cfg.Routing.Cooldown.Status == nil {
		cfg.Routing.Cooldown.Status = DefaultCooldown()
	}
	if cfg.Routing.Cooldown.Max == 0 {
		cfg.Routing.Cooldown.Max = Duration(5 * time.Minute)
	}

	if cfg.Retry.MaxAttempts <= 0 {
		cfg.Retry.MaxAttempts = 1
	}
	if cfg.Fallback.MaxHops <= 0 {
		cfg.Fallback.MaxHops = 1
	}

	if cfg.Headers.Mode == "" {
		cfg.Headers.Mode = "deny"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Metrics.Path == "" {
		cfg.Metrics.Path = "/metrics"
	}
}

// validate reports every problem at once, so a startup failure is actionable.
func validate(cfg *Config, env Env) error {
	var problems []string

	if cfg.Server.Listen == "" {
		problems = append(problems, "server.listen is required")
	}
	if cfg.Server.MaxRequestBytes <= 0 {
		problems = append(problems, "server.max_request_bytes must be positive")
	}
	if cfg.Server.MaxResponseBytes <= 0 {
		problems = append(problems, "server.max_response_bytes must be positive")
	}
	if cfg.Server.ShutdownGrace <= 0 {
		problems = append(problems, "server.shutdown_grace must be positive")
	}
	if cfg.Timeouts.Connect <= 0 {
		problems = append(problems, "timeouts.connect must be positive")
	}
	if cfg.Timeouts.ResponseHeader <= 0 {
		problems = append(problems, "timeouts.response_header must be positive")
	}
	if cfg.Timeouts.StreamIdle <= 0 {
		problems = append(problems, "timeouts.stream_idle must be positive")
	}
	if cfg.Timeouts.Request < 0 {
		problems = append(problems, "timeouts.request must be zero or positive")
	}

	switch cfg.Routing.Strategy {
	case StrategyLeastActive, StrategyRoundRobin:
	default:
		problems = append(problems, fmt.Sprintf("routing.strategy %q must be least_active or round_robin", cfg.Routing.Strategy))
	}

	if cfg.Routing.Cooldown.Max <= 0 {
		problems = append(problems, "routing.cooldown.max must be positive")
	}
	for status, dur := range cfg.Routing.Cooldown.Status {
		if status < 100 || status > 599 {
			problems = append(problems, fmt.Sprintf("routing.cooldown.status key %d is not an HTTP status code", status))
		}
		if dur < 0 {
			problems = append(problems, fmt.Sprintf("routing.cooldown.status[%d] must be zero or positive", status))
		}
	}

	if cfg.Retry.Enabled && cfg.Retry.MaxAttempts > 10 {
		problems = append(problems, "retry.max_attempts must be 10 or less when retry is enabled")
	}

	switch cfg.Headers.Mode {
	case "deny", "allow":
	default:
		problems = append(problems, `headers.mode must be "deny" or "allow"`)
	}

	switch cfg.Logging.Format {
	case "json", "text":
	default:
		problems = append(problems, "logging.format must be json or text")
	}
	if !validLevel(cfg.Logging.Level) {
		problems = append(problems, "logging.level must be debug, info, warn or error")
	}
	if !strings.HasPrefix(cfg.Metrics.Path, "/") {
		problems = append(problems, "metrics.path must start with /")
	}

	if cfg.Auth.APIKeyEnv != "" {
		key := env[cfg.Auth.APIKeyEnv]
		if key == "" {
			problems = append(problems, fmt.Sprintf("auth.api_key_env %s is not set", cfg.Auth.APIKeyEnv))
		}
		cfg.Auth.APIKey = key
	}

	if len(cfg.Models) == 0 {
		problems = append(problems, "at least one model must be configured")
	}

	seenAlias := map[string]bool{}
	for alias, model := range cfg.Models {
		if alias == "" {
			problems = append(problems, "a model alias is required")
			continue
		}
		if seenAlias[alias] {
			problems = append(problems, fmt.Sprintf("duplicate model alias %q", alias))
			continue
		}
		seenAlias[alias] = true
		if model.UpstreamModel == "" {
			problems = append(problems, fmt.Sprintf("model %q: upstream_model is required", alias))
		}
		if len(model.Targets) == 0 {
			problems = append(problems, fmt.Sprintf("model %q has no targets", alias))
		}

		seenName := map[string]bool{}
		for i := range model.Targets {
			d := &model.Targets[i]
			where := fmt.Sprintf("model %q target %d", alias, i+1)
			if d.Name == "" {
				problems = append(problems, fmt.Sprintf("%s: name is required", where))
			} else if seenName[d.Name] {
				problems = append(problems, fmt.Sprintf("model %q has duplicate target name %q", alias, d.Name))
			} else {
				seenName[d.Name] = true
			}

			u, err := url.Parse(d.BaseURL)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: base_url is not a valid URL: %v", where, err))
			} else if u.Scheme != "http" && u.Scheme != "https" {
				problems = append(problems, fmt.Sprintf("%s: base_url %q must use http or https", where, d.BaseURL))
			} else if u.Host == "" {
				problems = append(problems, fmt.Sprintf("%s: base_url must include a host", where))
			} else if u.User != nil {
				problems = append(problems, fmt.Sprintf("%s: base_url must not contain credentials", where))
			} else if u.RawQuery != "" {
				problems = append(problems, fmt.Sprintf("%s: base_url must not contain a query", where))
			}

			if d.APIKeyEnv == "" {
				problems = append(problems, fmt.Sprintf("%s: api_key_env is required", where))
			} else {
				key := env[d.APIKeyEnv]
				if key == "" {
					problems = append(problems, fmt.Sprintf("%s: environment variable %s is not set", where, d.APIKeyEnv))
				}
				d.APIKey = key
			}
			if d.Path != "" && !strings.HasPrefix(d.Path, "/") {
				problems = append(problems, fmt.Sprintf("%s: path must start with /", where))
			}
			if d.MaxActive < 0 {
				problems = append(problems, fmt.Sprintf("%s: max_active must be zero or positive", where))
			}
			for name, value := range d.Headers {
				if name == "" || value == "" {
					problems = append(problems, fmt.Sprintf("%s: header %q is empty", where, name))
				}
			}
		}

		for _, target := range model.Fallbacks {
			if _, ok := cfg.Models[target]; !ok {
				problems = append(problems, fmt.Sprintf("model %q fallback %q is not a configured model", alias, target))
			}
		}
	}

	if cfg.Fallback.Enabled && len(cfg.Models) > 0 {
		if err := checkFallbackCycles(cfg.Models); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New("invalid config:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

// checkFallbackCycles refuses a fallback graph that could loop forever.
func checkFallbackCycles(models map[string]ModelConfig) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(alias string, depth int) error
	visit = func(alias string, depth int) error {
		if depth > len(models) {
			return fmt.Errorf("fallback graph reachable from %q is deeper than the number of models", alias)
		}
		color[alias] = grey
		for _, target := range models[alias].Fallbacks {
			if target == alias {
				return fmt.Errorf("model %q falls back to itself", alias)
			}
			if _, ok := models[target]; !ok {
				return fmt.Errorf("model %q falls back to unknown model %q", alias, target)
			}
			if color[target] == grey {
				return fmt.Errorf("fallback cycle: %q -> %q", alias, target)
			}
			if color[target] == white {
				if err := visit(target, depth+1); err != nil {
					return err
				}
			}
		}
		color[alias] = black
		return nil
	}
	for _, alias := range ModelNames(models) {
		if color[alias] == white {
			if err := visit(alias, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// ModelNames returns the configured aliases in a stable order, so /v1/models is
// reproducible and tests can rely on it.
func ModelNames(models map[string]ModelConfig) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// JoinPath builds the upstream request URL from a base URL and an endpoint.
func JoinPath(base, endpoint string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = path.Join(strings.TrimRight(u.Path, "/"), strings.TrimPrefix(endpoint, "/"))
	return u.String()
}

func validLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}
