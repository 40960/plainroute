// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net/http"
	"testing"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/routing"
)

func TestConfiguredHeadersCannotReplaceTheTargetKey(t *testing.T) {
	cfg := &config.Config{Headers: config.HeadersConfig{
		Extra: map[string]string{"Authorization": "Bearer from-config", "Host": "from-config"},
	}}
	target := &routing.Target{
		APIKey: "the-target-key",
		Headers: map[string]string{
			"Authorization":  "Bearer from-target",
			"Content-Length": "1",
		},
	}
	dst := http.Header{}
	copyRequestHeaders(dst, http.Header{}, cfg, target, "id")
	if got := dst.Get("Authorization"); got != "Bearer the-target-key" {
		t.Errorf("Authorization: got %q, want the target key", got)
	}
	if got := dst.Get("Host"); got != "" {
		t.Errorf("Host: got %q, want it ignored", got)
	}
	if got := dst.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length: got %q, want it ignored", got)
	}
}

func TestConnectionNominatedHeadersAreDroppedInBothDirections(t *testing.T) {
	srcReq := http.Header{
		"Connection": {"keep-alive, X-Foo, X-Bar"},
		"X-Foo":      {"one"},
		"X-Bar":      {"two"},
		"X-Keep":     {"three"},
	}
	cfg := &config.Config{}
	dstReq := http.Header{}
	copyRequestHeaders(dstReq, srcReq, cfg, &routing.Target{}, "id")
	for _, name := range []string{"X-Foo", "X-Bar", "Connection"} {
		if dstReq.Get(name) != "" {
			t.Errorf("request forwarded connection-nominated %s: %v", name, dstReq)
		}
	}
	if dstReq.Get("X-Keep") != "three" {
		t.Errorf("request dropped an ordinary header: %v", dstReq)
	}

	srcRes := http.Header{
		"Connection": {"X-Foo"},
		"X-Foo":      {"one"},
		"X-Keep":     {"three"},
	}
	dstRes := http.Header{}
	copyResponseHeaders(dstRes, srcRes)
	if dstRes.Get("X-Foo") != "" || dstRes.Get("Connection") != "" {
		t.Errorf("response forwarded connection-nominated header: %v", dstRes)
	}
	if dstRes.Get("X-Keep") != "three" {
		t.Errorf("response dropped an ordinary header: %v", dstRes)
	}
}
