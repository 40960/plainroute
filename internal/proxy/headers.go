// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net/http"
	"strings"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/routing"
)

// hopByHop are the headers that belong to one connection, not to the message.
// They are never forwarded in either direction.
//
// The keys are lowercase because http.CanonicalHeaderKey does not produce a stable form
// for a name that is already all capitals: TE canonicalises to Te, and a lookup against
// "TE" misses. Comparing lowercase names is what makes the check actually work.
var hopByHop = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func isHopByHop(name string) bool { return hopByHop[strings.ToLower(name)] }

// copyRequestHeaders builds the header set for the upstream request.
//
// Authorization is always replaced with the key configured for the selected
// target, so a client cannot aim a request at a key it was not given.
// Content-Length is dropped because the body PlainRoute sends may differ from the
// one the client counted, and the transport sets the correct value.
func copyRequestHeaders(dst http.Header, src http.Header, cfg *config.Config, d *routing.Target, requestID string) {
	allowList := len(cfg.Headers.Allow) > 0 && cfg.Headers.Mode == "allow"
	deny := map[string]bool{}
	for _, name := range cfg.Headers.Deny {
		deny[http.CanonicalHeaderKey(name)] = true
	}

	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if isHopByHop(canonical) {
			continue
		}
		if canonical == "Content-Length" || canonical == "Host" {
			continue
		}
		if deny[canonical] {
			continue
		}
		if allowList {
			allowed := false
			for _, pattern := range cfg.Headers.Allow {
				if strings.EqualFold(pattern, canonical) {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}

	if d.APIKey != "" {
		dst.Set("Authorization", "Bearer "+d.APIKey)
	}

	for name, value := range d.Headers {
		if isHopByHop(name) {
			continue
		}
		dst.Set(name, value)
	}

	if cfg.Headers.ForwardRequestID {
		dst.Set("X-Request-Id", requestID)
	}
	for name, value := range cfg.Headers.Extra {
		if isHopByHop(name) {
			continue
		}
		dst.Set(name, value)
	}
}

// copyResponseHeaders copies an upstream response header set back to the client,
// minus the per-connection headers. Content-Length is left to the transport so a
// stream that ends early cannot be described by a wrong length.
func copyResponseHeaders(dst http.Header, src http.Header) {
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if isHopByHop(canonical) || canonical == "Content-Length" {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}
