// SPDX-License-Identifier: AGPL-3.0-only

// Package fakeupstream is a controllable OpenAI-compatible upstream for tests and
// the end-to-end script.
//
// Behaviour is chosen by the API key the gateway presents, because a key is what
// identifies a target. A sequence of behaviours can be configured per key, so
// a test can say: first request 429, second request 200. The server counts every
// request it sees, which is how the tests prove that client requests and upstream
// attempts are the same number.
package fakeupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Behavior is one response the fake upstream can produce. The JSON tags are for the
// standalone binary's config file; the tests build these in Go.
type Behavior struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`

	// Body is sent as one write for a non-stream response.
	Body string `json:"body,omitempty"`

	// SSE is written chunk by chunk. Each entry is written exactly as stored, so a
	// test can compare bytes byte for byte.
	SSE []string `json:"sse,omitempty"`

	// Delay waits before the first byte. Gap waits between chunks, which is the case
	// where a stream goes silent in the middle and the idle watchdog should fire.
	// Stall stays silent after the last chunk, which is the case where a stream stops
	// producing data.
	Delay time.Duration `json:"-"`
	Gap   time.Duration `json:"-"`
	Stall time.Duration `json:"-"`

	// CloseEarly writes headers and some chunks, then drops the connection.
	CloseEarly bool `json:"close_early,omitempty"`

	// WaitForCancel writes the chunks and then blocks until the request is cancelled,
	// recording that it happened. It proves a client disconnect reaches the upstream.
	WaitForCancel bool `json:"wait_for_cancel,omitempty"`

	// Sequence is consumed one entry at a time; the last entry repeats when the
	// sequence runs out.
	Sequence []Behavior `json:"sequence,omitempty"`

	// Cancelled is set by the server when the request context was cancelled while
	// this behaviour was running. It is runtime state, not part of the config file.
	Cancelled bool `json:"-"`
}

// fileBehavior is the JSON shape used by the standalone binary's config file.
// Durations are strings such as "2s", which is why this is a separate type rather than
// tags on Behavior.
type fileBehavior struct {
	Status        int               `json:"status"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          string            `json:"body,omitempty"`
	SSE           []string          `json:"sse,omitempty"`
	Delay         string            `json:"delay,omitempty"`
	Gap           string            `json:"gap,omitempty"`
	Stall         string            `json:"stall,omitempty"`
	CloseEarly    bool              `json:"close_early,omitempty"`
	WaitForCancel bool              `json:"wait_for_cancel,omitempty"`
	Sequence      []fileBehavior    `json:"sequence,omitempty"`
}

// Server is a fake OpenAI-compatible upstream.
type Server struct {
	mu        sync.Mutex
	behaviors map[string]*Behavior
	attempts  map[string]int
	lastBody  map[string]string
	total     int
}

func New() *Server {
	return &Server{
		behaviors: map[string]*Behavior{},
		attempts:  map[string]int{},
		lastBody:  map[string]string{},
	}
}

// Set registers the behaviour for one key.
func (s *Server) Set(key string, b Behavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.behaviors[key] = &b
}

// Attempts is how many chat requests the fake upstream received for a key.
func (s *Server) Attempts(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[key]
}

// TotalAttempts is how many chat requests the fake upstream received overall.
func (s *Server) TotalAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// LastBody is the body the fake upstream received for a key on its last request,
// so a test can check what the gateway actually forwarded.
func (s *Server) LastBody(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBody[key]
}

// Cancelled reports whether a request for this key was cancelled while the fake
// upstream was serving it.
func (s *Server) Cancelled(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.behaviors[key]
	return b != nil && b.Cancelled
}

// Stats is what the end-to-end script reads to check the counts: attempts per key,
// the total, and which keys reported a cancellation.
func (s *Server) Stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	attempts := make(map[string]int, len(s.attempts))
	for key, n := range s.attempts {
		attempts[key] = n
	}
	cancelled := make([]string, 0)
	for key, b := range s.behaviors {
		if b.Cancelled {
			cancelled = append(cancelled, key)
		}
	}
	return map[string]any{"total": s.total, "attempts": attempts, "cancelled": sortedStrings(cancelled)}
}

// LoadBehaviors reads a JSON file mapping an API key value to a behaviour, which is
// how the standalone binary is configured.
func LoadBehaviors(path string) (map[string]Behavior, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	file := map[string]fileBehavior{}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	out := make(map[string]Behavior, len(file))
	for key, f := range file {
		behavior, err := convert(f)
		if err != nil {
			return nil, fmt.Errorf("key %s: %w", key, err)
		}
		out[key] = behavior
	}
	return out, nil
}

func convert(f fileBehavior) (Behavior, error) {
	b := Behavior{
		Status:        f.Status,
		Headers:       f.Headers,
		Body:          f.Body,
		SSE:           f.SSE,
		CloseEarly:    f.CloseEarly,
		WaitForCancel: f.WaitForCancel,
	}

	for _, entry := range []struct {
		name   string
		value  string
		target *time.Duration
	}{
		{"delay", f.Delay, &b.Delay},
		{"gap", f.Gap, &b.Gap},
		{"stall", f.Stall, &b.Stall},
	} {
		if entry.value == "" {
			continue
		}
		d, err := time.ParseDuration(entry.value)
		if err != nil {
			return Behavior{}, fmt.Errorf("%s: %w", entry.name, err)
		}
		*entry.target = d
	}

	for _, next := range f.Sequence {
		converted, err := convert(next)
		if err != nil {
			return Behavior{}, err
		}
		b.Sequence = append(b.Sequence, converted)
	}
	return b, nil
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ServeHTTP serves the fake upstream. A path may be one of the named test paths,
// or the OpenAI endpoint /v1/chat/completions where behaviour comes from the key.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/stats" {
		out, err := json.Marshal(s.Stats())
		if err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}

	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[]}`)
		return
	}

	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	s.mu.Lock()
	behavior := s.behaviors[key]
	if behavior == nil {
		behavior = &Behavior{Status: http.StatusOK, Body: `{"error":{"message":"no behaviour configured"}}`}
		s.behaviors[key] = behavior
	}
	if len(behavior.Sequence) > 0 {
		next := behavior.Sequence[0]
		// The remaining entries have to travel with the entry being served, otherwise the
		// stored behaviour is the one just consumed and the sequence never advances. The
		// documented rule is: consume one entry, and the last entry repeats afterwards.
		next.Sequence = behavior.Sequence[1:]
		behavior = &next
		s.behaviors[key] = behavior
	}
	if r.URL.Path != "/v1/chat/completions" {
		// A target's base URL is the path part that selects the behaviour, so
		// http://host/429 answers the /429 case even though the endpoint is appended.
		base := strings.TrimSuffix(r.URL.Path, "/chat/completions")
		if byPath, ok := pathBehaviour(base); ok {
			behavior = &byPath
		}
	}
	s.attempts[key]++
	s.total++
	s.mu.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.lastBody[key] = string(body)
	s.mu.Unlock()

	if behavior.Delay > 0 {
		select {
		case <-time.After(behavior.Delay):
		case <-r.Context().Done():
			s.markCancelled(key)
			return
		}
	}

	status := behavior.Status
	if status == 0 {
		status = http.StatusOK
	}

	for name, value := range behavior.Headers {
		w.Header().Set(name, value)
	}

	if behavior.CloseEarly {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		// The response is framed as chunked, the way a streaming provider frames it. Without
		// framing, a socket that is dropped mid-body looks like a clean end to the reader, so
		// a test that is meant to check truncation cannot tell truncation from completion.
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n",
			status, http.StatusText(status))
		for _, chunk := range behavior.SSE {
			_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(chunk), chunk)
			_ = buf.Flush()
		}
		// Closing here, without the terminating zero-length chunk, is the abrupt end.
		_ = conn.Close()
		return
	}

	if len(behavior.SSE) > 0 {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)

	flusher, canFlush := w.(http.Flusher)

	for i, chunk := range behavior.SSE {
		if r.Context().Err() != nil {
			s.markCancelled(key)
			return
		}
		if _, err := io.WriteString(w, chunk); err != nil {
			return
		}
		if canFlush {
			flusher.Flush()
		}
		if behavior.Gap > 0 && i < len(behavior.SSE)-1 {
			select {
			case <-time.After(behavior.Gap):
			case <-r.Context().Done():
				s.markCancelled(key)
				return
			}
		}
	}

	// WaitForCancel keeps the stream open until the client or the gateway gives up,
	// so a test can prove the cancellation reached the upstream.
	if behavior.WaitForCancel {
		<-r.Context().Done()
		s.markCancelled(key)
		return
	}

	// Stall stays silent after the last chunk, which is the case where a stream
	// stops producing data.
	if behavior.Stall > 0 {
		select {
		case <-time.After(behavior.Stall):
		case <-r.Context().Done():
			s.markCancelled(key)
			return
		}
	}

	if len(behavior.SSE) == 0 && behavior.Body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, behavior.Body)
	}
}

func (s *Server) markCancelled(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.behaviors[key]; ok {
		b.Cancelled = true
	}
}

// pathBehaviour is the list of fixed test paths the brief asks for.
func pathBehaviour(path string) (Behavior, bool) {
	switch path {
	case "/success":
		return Behavior{Status: 200, Body: `{"object":"chat.completion","choices":[]}`}, true
	case "/stream":
		return Behavior{Status: 200, SSE: []string{
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: [DONE]\n\n",
		}}, true
	case "/slow-stream":
		return Behavior{Status: 200, Delay: 250 * time.Millisecond, SSE: []string{
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"thinking\"}}]}\n\n",
			"data: [DONE]\n\n",
		}}, true
	case "/429":
		return Behavior{Status: 429, Body: `{"error":{"message":"rate limited","type":"rate_limit_error"}}`}, true
	case "/429-retry-after":
		return Behavior{Status: 429, Headers: map[string]string{"Retry-After": "2"},
			Body: `{"error":{"message":"rate limited","type":"rate_limit_error"}}`}, true
	case "/500":
		return Behavior{Status: 500, Body: `{"error":{"message":"server error"}}`}, true
	case "/503":
		return Behavior{Status: 503, Body: `{"error":{"message":"unavailable"}}`}, true
	case "/timeout":
		return Behavior{Status: 504, Body: `{"error":{"message":"upstream timeout"}}`}, true
	case "/disconnect":
		return Behavior{Status: 200, CloseEarly: true, SSE: []string{
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n",
		}}, true
	case "/tool-call-stream":
		return Behavior{Status: 200, SSE: []string{
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\"}}}]}]}\n\n",
			"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n",
			"data: [DONE]\n\n",
		}}, true
	case "/empty":
		return Behavior{Status: 200}, true
	case "/malformed":
		return Behavior{Status: 200, SSE: []string{"data: not json\n\n", "data: [DONE]\n\n"}}, true
	}
	return Behavior{}, false
}
