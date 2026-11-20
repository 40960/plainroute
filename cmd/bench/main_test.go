// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// The baseline and the final page differ by four upstream attempts, three client requests,
// one disconnect and no gateway rejection. A client that issued four requests is within the
// bound; one that issued three is not, even though both see the same gateway counters.
const metricsBaseline = `# HELP plainroute_requests_total Client requests handled.
# TYPE plainroute_requests_total counter
plainroute_requests_total{route="demo",target="a",status_class="2xx"} 3
plainroute_upstream_attempts_total{route="demo",target="a",status_class="2xx"} 3
plainroute_gateway_rejected_total{route="none",code="invalid_api_key"} 1
plainroute_client_disconnects_total{route="demo",target="a"} 0
plainroute_active_requests{route="demo",target="a"} 0
plainroute_in_flight_requests 0
`

const metricsFinal = `# HELP plainroute_requests_total Client requests handled.
# TYPE plainroute_requests_total counter
plainroute_requests_total{route="demo",target="a",status_class="2xx"} 6
plainroute_upstream_attempts_total{route="demo",target="a",status_class="2xx"} 7
plainroute_gateway_rejected_total{route="none",code="invalid_api_key"} 1
plainroute_client_disconnects_total{route="demo",target="a"} 1
plainroute_active_requests{route="demo",target="a"} 0
plainroute_in_flight_requests 0
`

func captureGateway(t *testing.T, baseline, final string, issued int) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	printGateway(baseline, final, issued)
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestPrintGatewayUsesBaselineDeltasAndIssuedRequests(t *testing.T) {
	out := captureGateway(t, metricsBaseline, metricsFinal, 4)
	for _, want := range []string{
		"plainroute_upstream_attempts_total",
		"7 (+4)",
		"no amplification",
		"4 attempts since baseline for 4 client requests issued: ok",
		"gateway rejected",
		"+0 (auxiliary; answered from gateway state)",
		"client disconnects",
		"+1 (auxiliary; client left before the response)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("with four requests issued, output missing %q:\n%s", want, out)
		}
	}

	out = captureGateway(t, metricsBaseline, metricsFinal, 3)
	failLine := "4 attempts since baseline for 3 client requests issued: FAIL: " +
		"more upstream attempts than client requests issued"
	if !strings.Contains(out, failLine) {
		t.Errorf("with three requests issued, output missing the FAIL verdict:\n%s", out)
	}
}
