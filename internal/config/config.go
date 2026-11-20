// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads PlainRoute's YAML configuration.
//
// PlainRoute is a single process with no database and no external service: everything it needs is
// described in one file, and the api keys are values in that file. The file a gateway actually runs
// on is therefore a secret: it is gitignored, it needs 600 permissions, and nothing in this package
// prints a value it read from it. There is no second source for a credential - no environment
// lookup, no separate secret file, no cache, no refresh subsystem - because a value that can come
// from two places is a value a reader cannot tell which one is in force.
//
// What this package returns is one snapshot, not a live view: a caller loads it and keeps it for the
// whole request. Watching the file for changes is a different package, and it swaps one whole
// snapshot for another.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

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
	Fallback FallbackConfig         `yaml:"fallbacks"`
	Headers  HeadersConfig          `yaml:"headers"`
	Logging  LoggingConfig          `yaml:"logging"`
	Metrics  MetricsConfig          `yaml:"metrics"`
	Reload   ReloadConfig           `yaml:"config_reload"`
	Auth     AuthConfig             `yaml:"auth"`
	Routes   map[string]RouteConfig `yaml:"routes"`
}

// ReloadConfig is the watcher: how often the file is read and compared against the hash of what was
// last acted on. There is no switch to turn this off, because keeping the last good config is what
// makes an operator editing a live file safe, and the interval is the only number here.
//
// One second is the default, and it is what the README states: this is an operator config, not a
// control plane, so a document that promised a change lands instantly would be a promise this code
// cannot keep. 0 or absent means the default. A value under 100ms is a startup error rather than a
// silent clamp, because a file that says 1ms and runs as a busy loop is exactly the mistake this
// guard exists for.
type ReloadConfig struct {
	Interval Duration `yaml:"interval"`
}

// AuthConfig is the api key a caller presents. It is a single value, not a table of keys: a
// gateway that knows who its callers are needs accounts, and accounts are the point this project
// stays out of.
//
// Empty means off, which is also what an absent block means. There is no half state left: the value
// in the file is the value in force, so a gate that reads as on in the file is on.
type AuthConfig struct {
	APIKey string `yaml:"api_key"`
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
// from the upstream always wins, up to Max, and Max can be turned off with 0.
type CooldownConfig struct {
	Status map[int]Duration `yaml:"status"`

	// Max is the clamp for a Retry-After from the upstream. Absent means the default;
	// 0 means no clamp, so a value the upstream sent is used as written. The clamp never
	// applies to a configured duration, because a configured duration is a choice the
	// operator made and does not need protecting. A negative value is a startup error.
	Max *Duration `yaml:"max"`

	// Timeout cools a target down after a connection, response-header or stream
	// idle failure. 0 disables it. Authentication failures are handled through the
	// status map, for example 401 or 403.
	Timeout Duration `yaml:"timeout"`
}

// DefaultCooldown is the behaviour the project is built around: a 429 takes the target that
// produced it out of the pool for ten seconds, when the upstream sent no usable Retry-After.
// The ten seconds are a prior for the silent case, not a measurement of any provider window,
// and for a key that was told to slow down they are also the interval at which that key is
// asked again.
func DefaultCooldown() map[int]Duration {
	return map[int]Duration{429: Duration(10 * time.Second)}
}

// DefaultCooldownMax is the clamp used when the file does not set one: 15 minutes. A target is
// not contacted while it is cooling, so a Retry-After the upstream sent is never checked, and the
// clamp is the longest an unverified value can govern before that target is asked again and
// reality answers. 15 minutes is above what a rate limit in force can plausibly state, so the
// clamp fires on a value that cannot be true rather than on a true one.
func DefaultCooldownMax() Duration {
	return Duration(15 * time.Minute)
}

// MaxStd is the clamp as a duration, or 0 when no clamp is configured. 0 means a value the
// upstream sent is used as written.
func (c CooldownConfig) MaxStd() time.Duration {
	if c.Max == nil {
		return DefaultCooldownMax().Std()
	}
	return c.Max.Std()
}

// There is no retry block, and writing one is a startup error. The client owns backoff, and the
// gateway does not retry inside one client request, so a retry block was a config that nothing
// read: it could parse, validate and be documented while changing nothing. A field a loader can
// read but no code can use is the failure mode this project keeps out of its config, so the rule
// now is that every field has to have a reader, and this one had none.
//
// FallbackConfig is off by default. Falling back to a different model is opt-in, bounded, and only
// allowed before the first byte reaches the client. Both fields here are read by the request path;
// the logging switch this block used to carry is gone for the same reason the retry block is.
type FallbackConfig struct {
	Enabled bool `yaml:"enabled"`
	MaxHops int  `yaml:"max_hops"`
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

// RouteConfig is a name a client can send in the model field. It is not a model: the model
// belongs to each target, so one name can be served by several different models. A route owns the
// set of selectable targets and the fallback chain, and nothing else.
type RouteConfig struct {
	Fallbacks []string       `yaml:"fallbacks"`
	Targets   []TargetConfig `yaml:"targets"`
}

// TargetConfig is one upstream: a model name, an endpoint and one key. Two targets can differ by
// any of them, which is why "the same model with a different key" is one case of a target, not its
// definition. The key is a value in this file, so this file is a secret: it is gitignored and needs
// 600 permissions. A test fixture may hold an obviously fake key; a file a gateway runs on holds a
// real one, and nothing in this project prints what it reads here.
type TargetConfig struct {
	Name      string            `yaml:"name"`
	Model     string            `yaml:"model"`
	BaseURL   string            `yaml:"base_url"`
	APIKey    string            `yaml:"api_key"`
	Path      string            `yaml:"path"`
	Headers   map[string]string `yaml:"headers"`
	MaxActive int               `yaml:"max_active"`
}

const (
	StrategyLeastActive = "least_active"
	StrategyRoundRobin  = "round_robin"

	DefaultEndpoint = "/chat/completions"
)

// Load reads a configuration file and validates it. Startup stays strict: before the first config
// exists there is no last-known-good one to serve with, so a missing, unreadable or invalid file is
// a startup failure and the process does not listen.
func Load(file string) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes YAML and validates. The error text names the problem and never repeats a value it
// read from the file, because a config value can be a secret and this error is what a log line, a
// terminal and a bug report show. The yaml package reports line numbers and problems, not the text
// on the line, which is what makes it safe to print this error; the check in validate below is the
// same rule, stated as a test.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// One file is one document. A second document is never read, so accepting it
	// would let a file say one thing and silently mean another.
	var second any
	if err := dec.Decode(&second); err != io.EOF {
		if err == nil {
			return nil, errors.New("parse config: a second YAML document is not allowed")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
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
	// 0 and absent are the same number in a config file, so 0 means the default here. A negative
	// value is not a default and is reported at startup, which is what makes the check in validate
	// mean something rather than a message that can never print.
	if cfg.Server.MaxRequestBytes == 0 {
		cfg.Server.MaxRequestBytes = 8 << 20
	}
	if cfg.Server.MaxResponseBytes == 0 {
		cfg.Server.MaxResponseBytes = 32 << 20
	}
	if cfg.Server.MaxHeaderBytes == 0 {
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
	if cfg.Routing.Cooldown.Max == nil {
		max := DefaultCooldownMax()
		cfg.Routing.Cooldown.Max = &max
	}

	if cfg.Fallback.MaxHops == 0 {
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
	if cfg.Reload.Interval == 0 {
		cfg.Reload.Interval = DefaultReloadInterval()
	}
}

// DefaultReloadInterval is what the file gets when nothing is written: one second. Reading a few
// kilobytes once a second is not a cost worth measuring, and it is the bound a reader can rely on
// when the README says a change shows up within one interval.
func DefaultReloadInterval() Duration { return Duration(time.Second) }

// MinReloadInterval is the floor a configured value has to clear.
func MinReloadInterval() Duration { return Duration(100 * time.Millisecond) }

// validate reports every problem at once, so a startup failure is actionable.
func validate(cfg *Config) error {
	var problems []string

	if cfg.Server.Listen == "" {
		problems = append(problems, "server.listen is required")
	}
	if cfg.Server.MaxRequestBytes < 0 {
		problems = append(problems, "server.max_request_bytes must be positive, or 0 for the default")
	}
	if cfg.Server.MaxResponseBytes < 0 {
		problems = append(problems, "server.max_response_bytes must be positive, or 0 for the default")
	}
	if cfg.Server.MaxHeaderBytes < 0 {
		problems = append(problems, "server.max_header_bytes must be positive, or 0 for the default")
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

	if cfg.Routing.Cooldown.Max != nil && *cfg.Routing.Cooldown.Max < 0 {
		problems = append(problems, "routing.cooldown.max must be positive, or 0 for no clamp")
	}
	for status, dur := range cfg.Routing.Cooldown.Status {
		if status < 100 || status > 599 {
			problems = append(problems, fmt.Sprintf("routing.cooldown.status key %d is not an HTTP status code", status))
		}
		if dur < 0 {
			problems = append(problems, fmt.Sprintf("routing.cooldown.status[%d] must be zero or positive", status))
		}
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
	// The metrics page is an operational endpoint: it must not be shadowed by a
	// built-in route, and it is served before the API key gate so it stays open.
	switch cfg.Metrics.Path {
	case "/v1/chat/completions", "/v1/models", "/healthz", "/health/live", "/readyz", "/health/ready":
		problems = append(problems, "metrics.path collides with a built-in endpoint")
	}

	if cfg.Fallback.MaxHops < 0 {
		problems = append(problems, "fallbacks.max_hops must be zero or positive, or absent for the default")
	}
	if cfg.Reload.Interval < 0 {
		problems = append(problems, "config_reload.interval must be positive, or 0 for the default")
	} else if cfg.Reload.Interval > 0 && cfg.Reload.Interval < MinReloadInterval() {
		problems = append(problems,
			fmt.Sprintf("config_reload.interval must be %s or more, or 0 for the default, so the watcher is not a busy loop", MinReloadInterval()))
	}

	if len(cfg.Routes) == 0 {
		problems = append(problems, "at least one route must be configured")
	}

	seenRoute := map[string]bool{}
	for routeName, route := range cfg.Routes {
		if routeName == "" {
			problems = append(problems, "a route name is required")
			continue
		}
		if seenRoute[routeName] {
			problems = append(problems, fmt.Sprintf("duplicate route name %q", routeName))
			continue
		}
		seenRoute[routeName] = true
		if len(route.Targets) == 0 {
			problems = append(problems, fmt.Sprintf("route %q has no targets", routeName))
		}

		seenName := map[string]bool{}
		for i := range route.Targets {
			t := &route.Targets[i]
			// The name the operator wrote is what identifies a target once a file has more than a few
			// of them, so an error that only gives a position is not actionable. The position is kept
			// because a target with no name is one of the problems this reports.
			where := fmt.Sprintf("route %q target %d", routeName, i+1)
			if t.Name != "" {
				where = fmt.Sprintf("%s (%s)", where, t.Name)
			}
			if t.Name == "" {
				problems = append(problems, fmt.Sprintf("%s: name is required", where))
			} else if seenName[t.Name] {
				problems = append(problems, fmt.Sprintf("route %q has duplicate target name %q", routeName, t.Name))
			} else {
				seenName[t.Name] = true
			}
			if t.Model == "" {
				problems = append(problems, fmt.Sprintf("%s: model is required", where))
			}

			// A value that failed to parse can contain credentials or a query. Name the
			// field and the violation only; never echo the value or the url.Error it made.
			u, err := url.Parse(t.BaseURL)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: base_url is not a valid URL", where))
			} else if u.Scheme != "http" && u.Scheme != "https" {
				problems = append(problems, fmt.Sprintf("%s: base_url must use http or https", where))
			} else if u.Host == "" {
				problems = append(problems, fmt.Sprintf("%s: base_url must include a host", where))
			} else if u.User != nil {
				problems = append(problems, fmt.Sprintf("%s: base_url must not contain credentials", where))
			} else if u.RawQuery != "" {
				problems = append(problems, fmt.Sprintf("%s: base_url must not contain a query", where))
			}

			if t.APIKey == "" {
				problems = append(problems, fmt.Sprintf("%s: api_key is required", where))
			}
			if t.Path != "" && !strings.HasPrefix(t.Path, "/") {
				problems = append(problems, fmt.Sprintf("%s: path must start with /", where))
			}
			if t.MaxActive < 0 {
				problems = append(problems, fmt.Sprintf("%s: max_active must be zero or positive", where))
			}
			for header, value := range t.Headers {
				if msg := headerProblem(header, value); msg != "" {
					problems = append(problems, where+": "+msg)
				}
			}
		}

		// The runtime walks only Fallbacks[0]; a longer list would be parsed, validated
		// and then partly ignored, which this project does not allow.
		if len(route.Fallbacks) > 1 {
			problems = append(problems, fmt.Sprintf("route %q has more than one fallback; a chain is written as A -> B -> C", routeName))
		}
		for _, fallback := range route.Fallbacks {
			if _, ok := cfg.Routes[fallback]; !ok {
				problems = append(problems, fmt.Sprintf("route %q fallback %q is not a configured route", routeName, fallback))
			}
		}
	}

	for header, value := range cfg.Headers.Extra {
		if msg := headerProblem(header, value); msg != "" {
			problems = append(problems, "headers.extra: "+msg)
		}
	}

	if cfg.Fallback.Enabled && len(cfg.Routes) > 0 {
		if err := checkFallbackCycles(cfg.Routes); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New("invalid config:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

// headerProblem validates a configured header against what the request path can
// actually honour. A reserved or malformed name would be ignored or would fail the
// request at runtime, so it is refused here instead.
func headerProblem(name, value string) string {
	if name == "" || value == "" {
		return "header name and value are required"
	}
	if strings.ContainsAny(name+value, "\r\n\x00") {
		return "header contains an invalid control character"
	}
	for _, c := range name {
		if c <= ' ' || c >= 0x7f || !tokenChar(c) {
			return "header name is not a valid field name"
		}
	}
	switch strings.ToLower(name) {
	case "authorization", "host", "content-length", "connection", "proxy-connection",
		"keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer",
		"transfer-encoding", "upgrade":
		return "header name is reserved"
	}
	return ""
}

func tokenChar(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

// checkFallbackCycles refuses a fallback graph that could loop forever.
func checkFallbackCycles(routes map[string]RouteConfig) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(name string, depth int) error
	visit = func(name string, depth int) error {
		if depth > len(routes) {
			return fmt.Errorf("fallback graph reachable from %q is deeper than the number of routes", name)
		}
		color[name] = grey
		for _, next := range routes[name].Fallbacks {
			if next == name {
				return fmt.Errorf("route %q falls back to itself", name)
			}
			if _, ok := routes[next]; !ok {
				return fmt.Errorf("route %q falls back to unknown route %q", name, next)
			}
			if color[next] == grey {
				return fmt.Errorf("fallback cycle: %q -> %q", name, next)
			}
			if color[next] == white {
				if err := visit(next, depth+1); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}
	for _, name := range RouteNames(routes) {
		if color[name] == white {
			if err := visit(name, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// RouteNames returns the configured route names in a stable order, so /v1/models is
// reproducible and tests can rely on it. A route name is what a client sends in the
// model field; it is not necessarily a model name.
func RouteNames(routes map[string]RouteConfig) []string {
	names := make([]string, 0, len(routes))
	for name := range routes {
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
