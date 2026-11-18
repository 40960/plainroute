// SPDX-License-Identifier: AGPL-3.0-only

// Package metrics exposes PlainRoute's counters and histograms on /metrics.
//
// The pair requests_total / upstream_attempts_total is the point of this
// package: with retry disabled they must match, so anyone can prove the gateway
// does not amplify one client request into several upstream calls.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NoTarget is the label used when a request never reached a target, for
// example a name that is not a configured route, or a request answered from cooldown
// state alone.
const NoTarget = "none"

// NoRoute is the label for a request the gateway answered on its own, with no route in
// play, such as a health endpoint.
const NoRoute = "none"

// Metrics is the set of collectors PlainRoute exposes. Everything is a plain
// Prometheus collector, so the values are safe to update from many goroutines.
type Metrics struct {
	Registry          *prometheus.Registry
	RequestsTotal     *prometheus.CounterVec
	UpstreamAttempts  *prometheus.CounterVec
	ActiveRequests    *prometheus.GaugeVec
	CooldownEvents    *prometheus.CounterVec
	CooldownActive    *prometheus.GaugeVec
	Latency           *prometheus.HistogramVec
	TTFT              *prometheus.HistogramVec
	StreamDuration    *prometheus.HistogramVec
	BytesStreamed     *prometheus.CounterVec
	UpstreamErrors    *prometheus.CounterVec
	ClientDisconnects *prometheus.CounterVec
	StreamAborts      *prometheus.CounterVec
	GatewayRejected   *prometheus.CounterVec
	Reloads           *prometheus.CounterVec
	InFlight          prometheus.Gauge
}

var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600,
}

func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_requests_total",
			Help: "Client requests handled, labelled by route, target and status class.",
		}, []string{"route", "target", "status_class"}),
		UpstreamAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_upstream_attempts_total",
			Help: "Upstream requests actually sent. With retry disabled this matches requests_total for proxied requests.",
		}, []string{"route", "target", "status_class"}),
		ActiveRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "plainroute_active_requests",
			Help: "Requests currently in flight per target.",
		}, []string{"route", "target"}),
		CooldownEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_cooldown_events_total",
			Help: "Times a target was put into cooldown, by reason.",
		}, []string{"route", "target", "reason"}),
		CooldownActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "plainroute_target_in_cooldown",
			Help: "1 while a target is cooling down, 0 otherwise.",
		}, []string{"route", "target"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "plainroute_request_duration_seconds",
			Help:    "Time from request start to response end, per request.",
			Buckets: latencyBuckets,
		}, []string{"route", "target", "stream"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "plainroute_ttft_seconds",
			Help:    "Time to the first upstream response byte.",
			Buckets: latencyBuckets,
		}, []string{"route", "target"}),
		StreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "plainroute_stream_duration_seconds",
			Help:    "Duration of a streamed response.",
			Buckets: latencyBuckets,
		}, []string{"route", "target"}),
		BytesStreamed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_bytes_streamed_total",
			Help: "Response bytes copied to clients, per target.",
		}, []string{"route", "target"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_upstream_errors_total",
			Help: "Upstream failures by kind: build_request, transport, timeout, stream_idle, client_gone.",
		}, []string{"route", "target", "kind"}),
		ClientDisconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_client_disconnects_total",
			Help: "Clients that went away before the response finished.",
		}, []string{"route", "target"}),
		StreamAborts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_stream_aborts_total",
			Help: "Streams ended by a gateway condition instead of a normal close.",
		}, []string{"route", "target", "reason"}),
		GatewayRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_gateway_rejected_total",
			Help: "Requests answered by the gateway itself, without an upstream call.",
		}, []string{"route", "code"}),
		Reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "plainroute_config_reloads_total",
			Help: "Configuration reloads by outcome.",
		}, []string{"outcome"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "plainroute_in_flight_requests",
			Help: "Requests currently being handled.",
		}),
	}

	for _, collector := range []prometheus.Collector{
		m.RequestsTotal, m.UpstreamAttempts, m.ActiveRequests, m.CooldownEvents,
		m.CooldownActive, m.Latency, m.TTFT, m.StreamDuration, m.BytesStreamed,
		m.UpstreamErrors, m.ClientDisconnects, m.StreamAborts, m.GatewayRejected,
		m.Reloads, m.InFlight,
	} {
		reg.MustRegister(collector)
	}

	// The process itself is part of what the page reports, so a load test can read
	// memory and goroutine counts from the same place as the routing numbers.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return m
}

// Handler serves the exposition page.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// StatusClass reduces a status code to the class used as a label, so the label
// set stays small and the numbers stay readable.
func StatusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

// Observe records one finished request.
func (m *Metrics) Observe(model, target string, status int, stream bool, duration, ttft time.Duration, bytes int64) {
	m.RequestsTotal.WithLabelValues(model, target, StatusClass(status)).Inc()
	m.Latency.WithLabelValues(model, target, boolLabel(stream)).Observe(duration.Seconds())
	if ttft > 0 {
		m.TTFT.WithLabelValues(model, target).Observe(ttft.Seconds())
	}
	if bytes > 0 {
		m.BytesStreamed.WithLabelValues(model, target).Add(float64(bytes))
	}
	if stream {
		m.StreamDuration.WithLabelValues(model, target).Observe(duration.Seconds())
	}
}

func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
