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
}
