// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A rename that misses one collector is invisible to a test built on WithLabelValues: that
// call matches on values only, so it cannot tell whether a label is named route or model.
// Only the collected text names the label, and the text is what a reader on the metrics page
// depends on. This is the check that catches a label left behind by a rename.
func TestCollectedTextNamesTheLabels(t *testing.T) {
	m := New()

	m.RequestsTotal.WithLabelValues(NoRoute, NoTarget, "4xx").Inc()
	if err := testutil.CollectAndCompare(m.RequestsTotal, strings.NewReader(`# HELP plainroute_requests_total Client requests handled, labelled by route, target and status class.
# TYPE plainroute_requests_total counter
plainroute_requests_total{route="none",status_class="4xx",target="none"} 1
`), "plainroute_requests_total"); err != nil {
		t.Errorf("requests_total: %v", err)
	}

	m.GatewayRejected.WithLabelValues(NoRoute, "model_not_found").Inc()
	if err := testutil.CollectAndCompare(m.GatewayRejected, strings.NewReader(`# HELP plainroute_gateway_rejected_total Requests answered by the gateway itself, without an upstream call.
# TYPE plainroute_gateway_rejected_total counter
plainroute_gateway_rejected_total{code="model_not_found",route="none"} 1
`), "plainroute_gateway_rejected_total"); err != nil {
		t.Errorf("gateway_rejected: %v", err)
	}

	m.UpstreamAttempts.WithLabelValues(NoRoute, NoTarget, "other").Inc()
	if err := testutil.CollectAndCompare(m.UpstreamAttempts, strings.NewReader(`# HELP plainroute_upstream_attempts_total Upstream requests actually sent, including attempts whose transport failed. At most one per client request by default; a configured fallback is the bounded exception.
# TYPE plainroute_upstream_attempts_total counter
plainroute_upstream_attempts_total{route="none",status_class="other",target="none"} 1
`), "plainroute_upstream_attempts_total"); err != nil {
		t.Errorf("upstream_attempts: %v", err)
	}

	// The help text lists the kinds this collector can actually be incremented with. Two
	// values people expect are not among them: an upstream that answered with an error status
	// is counted by upstream_attempts_total with its status class, and a response cap is a
	// stream abort reason, not an upstream failure.
	m.UpstreamErrors.WithLabelValues(NoRoute, NoTarget, "transport").Inc()
	if err := testutil.CollectAndCompare(m.UpstreamErrors, strings.NewReader(`# HELP plainroute_upstream_errors_total Upstream failures by kind: build_request, transport, timeout, stream_idle, client_gone.
# TYPE plainroute_upstream_errors_total counter
plainroute_upstream_errors_total{kind="transport",route="none",target="none"} 1
`), "plainroute_upstream_errors_total"); err != nil {
		t.Errorf("upstream_errors: %v", err)
	}
}

// The help text has to name the outcomes the reload path can actually put on the label: a page that
// names fewer outcomes than the code produces is a page a reader misreads. Only the collected text
// names the help line and the label, so this reads both. Each of the three outcomes is produced by
// the reload path and proven in internal/reload: a reload that applied, a content it could not read
// or validate, and a valid file that changed a field the running process cannot act on.
//
// The outcome is the only label, which is what keeps the cardinality low: a field name, a value from
// the file or a hash has no place in it.
func TestTheReloadHelpTextNamesTheOutcomesTheCodeProduces(t *testing.T) {
	m := New()

	m.Reloads.WithLabelValues("restart_required").Inc()
	if err := testutil.CollectAndCompare(m.Reloads, strings.NewReader(`# HELP plainroute_config_reloads_total Configuration reloads that acted on changed content, by outcome: ok, failed, restart_required. One count per distinct content, so a file that stays bad is counted once and a signal that changes nothing is not a reload. The outcome is the only label, and no value read from the file is part of it.
# TYPE plainroute_config_reloads_total counter
plainroute_config_reloads_total{outcome="restart_required"} 1
`), "plainroute_config_reloads_total"); err != nil {
		t.Errorf("reloads: %v", err)
	}

	// A help text that named an outcome the code never produces would still pass a test that only
	// looked for the string. This is the half that reads the code: the label the reload path writes.
	if got := testutil.ToFloat64(m.Reloads.WithLabelValues("restart_required")); got != 1 {
		t.Errorf("the outcome label: got %v, want 1", got)
	}
}
