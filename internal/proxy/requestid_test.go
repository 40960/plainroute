// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net/http"
	"testing"

	"github.com/40960/plainroute/internal/fakeupstream"
)

func TestGatewayRequestIDWinsOverAnUpstreamHeader(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)
	h.fake.Set("key-a", fakeupstream.Behavior{
		Status:  http.StatusOK,
		Headers: map[string]string{"X-Request-Id": "from-upstream"},
		Body:    "ok",
	})

	resp, _ := h.post(chatBody("m"))
	got := resp.Header.Values("X-Request-Id")
	if len(got) != 1 {
		t.Fatalf("X-Request-Id values: got %v, want exactly the gateway id", got)
	}
	if got[0] == "from-upstream" {
		t.Errorf("the upstream id replaced the gateway id")
	}
}
