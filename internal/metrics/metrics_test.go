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
