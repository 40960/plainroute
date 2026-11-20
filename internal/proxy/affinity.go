// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net/http"
	"strings"
)

// The three names OpenCode is verified to send, all carrying the same session
// identity. Nothing else is recognised: a routing hint must not become an
// unbounded header allow list.
var affinityHeaders = []string{"X-Session-Affinity", "X-Session-Id", "X-Opencode-Session"}

const maxAffinityLength = 128

// sessionAffinity returns one usable key and an empty code, or an empty key and
// the code of a local rejection. Multiple supported headers with the same value
// are normal (OpenCode sends three); conflicting values are refused rather than
// picking one at random. The value is never logged or used as a metric label.
func sessionAffinity(h http.Header) (string, string) {
	value := ""
	for _, name := range affinityHeaders {
		for _, candidate := range h.Values(name) {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			if len(candidate) > maxAffinityLength || !validAffinityValue(candidate) {
				return "", "invalid_session_affinity"
			}
			if value != "" && value != candidate {
				return "", "conflicting_session_affinity"
			}
			value = candidate
		}
	}
	return value, ""
}

func validAffinityValue(value string) bool {
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}
