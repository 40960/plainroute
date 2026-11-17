// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"encoding/json"
	"strconv"
	"time"
)

// errorEnvelope is the OpenAI-compatible error shape. PlainRoute uses it only
// for answers it produces itself; an error body from an upstream is forwarded
// byte for byte.
type errorEnvelope struct {
	Error errorFields `json:"error"`
}

type errorFields struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

func errorBody(message, typ, code string) []byte {
	// Marshal of a fixed struct cannot fail; ignoring the error keeps the writer
	// path short and panic-free.
	body, _ := json.Marshal(errorEnvelope{Error: errorFields{Message: message, Type: typ, Code: code}})
	return body
}

// parseRetryAfter turns an upstream Retry-After into a duration. It accepts
// seconds and an HTTP date, and clamps the result so a hostile or broken value
// cannot make a deployment unavailable for an unreasonable time.
func parseRetryAfter(value string, def, max time.Duration) time.Duration {
	if value == "" {
		return def
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return def
		}
		d := time.Duration(seconds) * time.Second
		if d > max {
			return max
		}
		return d
	}
	if t, err := time.Parse(time.RFC1123, value); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return def
		}
		if d > max {
			return max
		}
		return d
	}
	return def
}

// retryAfterSeconds is the value sent in our own Retry-After header: whole
// seconds, at least one.
func retryAfterSeconds(d time.Duration) string {
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}
