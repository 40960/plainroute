// SPDX-License-Identifier: AGPL-3.0-only

package fakeupstream

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fake upstream is the thing every other test trusts, so it needs its own checks.
// These tests use the fixed paths the project requires, exactly as a deployment uses them:
// the base URL is the path part, and the endpoint is appended to it.

func TestRequiredPathsProduceTheShapesTheyPromise(t *testing.T) {
	server := httptest.NewServer(New())
	defer server.Close()

	cases := []struct {
		path        string
		status      int
		stream      bool
		needle      string
		retryAfter  string
		contentType string
	}{
		{"/success", 200, false, `"object":"chat.completion"`, "", "application/json"},
		{"/stream", 200, true, "data: [DONE]", "", "text/event-stream"},
		{"/slow-stream", 200, true, "thinking", "", "text/event-stream"},
		{"/429", 429, false, `"type":"rate_limit_error"`, "", "application/json"},
		{"/429-retry-after", 429, false, `"type":"rate_limit_error"`, "2", "application/json"},
		{"/500", 500, false, `"message":"server error"`, "", "application/json"},
		{"/503", 503, false, `"message":"unavailable"`, "", "application/json"},
		{"/timeout", 504, false, `"message":"upstream timeout"`, "", "application/json"},
		{"/tool-call-stream", 200, true, `"tool_calls"`, "", "text/event-stream"},
		{"/empty", 200, false, "", "", ""},
		{"/malformed", 200, true, "data: not json", "", "text/event-stream"},
	}

	for _, c := range cases {
		// A deployment path is the base URL plus the endpoint, which is how the gateway
		// builds the URL it requests.
		url := server.URL + c.path + "/chat/completions"
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"model":"m","stream":true}`))
		if err != nil {
			t.Fatalf("%s: new request: %v", c.path, err)
		}
		req.Header.Set("Authorization", "Bearer test-key")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: request: %v", c.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != c.status {
			t.Errorf("%s: status got %d, want %d", c.path, resp.StatusCode, c.status)
		}
		if c.contentType != "" && resp.Header.Get("Content-Type") != c.contentType {
			t.Errorf("%s: content type got %q, want %q", c.path, resp.Header.Get("Content-Type"), c.contentType)
		}
		if c.retryAfter != "" && resp.Header.Get("Retry-After") != c.retryAfter {
			t.Errorf("%s: Retry-After got %q, want %q", c.path, resp.Header.Get("Retry-After"), c.retryAfter)
		}
		if c.needle != "" && !strings.Contains(string(body), c.needle) {
			t.Errorf("%s: body does not contain %q: %s", c.path, c.needle, body)
		}
		if c.stream && !strings.HasSuffix(string(body), "\n\n") {
			t.Errorf("%s: an SSE body must end with the frame separator: %q", c.path, body)
		}
	}
}

// TestStreamFramesArriveInOrderAndSeparately is the property the gateway depends on: the
// fake upstream writes each frame on its own, so a test that compares bytes is comparing
// the frames as a provider would send them, not as the test assembled them.
func TestStreamFramesArriveInOrderAndSeparately(t *testing.T) {
	server := httptest.NewServer(New())
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/stream/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	frames := []string{}
	for {
		line, err := reader.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, line)
		}
		if err != nil {
			break
		}
	}

	if len(frames) != 3 {
		t.Fatalf("frames: got %d, want 3: %v", len(frames), frames)
	}
	if !strings.Contains(frames[0], `"role":"assistant"`) {
		t.Errorf("first frame is not the opening role delta: %s", frames[0])
	}
	if !strings.Contains(frames[1], `"content"`) {
		t.Errorf("second frame is not a content delta: %s", frames[1])
	}
	if frames[2] != "data: [DONE]" {
		t.Errorf("closing frame: got %q, want the upstream's own [DONE]", frames[2])
	}
}

// TestSequenceIsConsumedOneEntryAtATime is what lets a test say "first request 429, second
// request 200" and still count what the upstream saw.
func TestSequenceIsConsumedOneEntryAtATime(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()

	fake.Set("test-key", Behavior{Sequence: []Behavior{
		{Status: 429, Body: `{"error":{"message":"limited"}}`},
		{Status: 200, Body: `{"ok":true}`},
	}})

	statuses := []int{}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer test-key")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}

	// The last entry repeats once the sequence runs out, which is what a test that sends
	// more requests than entries needs.
	if statuses[0] != 429 || statuses[1] != 200 || statuses[2] != 200 {
		t.Errorf("sequence: got %v, want [429 200 200]", statuses)
	}
	if fake.Attempts("test-key") != 3 {
		t.Errorf("attempts: got %d, want 3", fake.Attempts("test-key"))
	}

	stats := fake.Stats()
	if stats["total"] != 3 {
		t.Errorf("stats total: got %v, want 3", stats["total"])
	}
	reported, ok := stats["attempts"].(map[string]int)
	if !ok || reported["test-key"] != 3 {
		t.Errorf("stats attempts: got %v, want 3 for test-key", stats["attempts"])
	}
}

// TestDisconnectDropsTheConnectionMidStream checks the case the gateway has to survive: the
// upstream writes some frames and then closes the socket without a closing frame.
func TestDisconnectDropsTheConnectionMidStream(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()

	fake.Set("test-key", Behavior{Status: 200, CloseEarly: true, SSE: []string{
		"data: {\"delta\":{\"role\":\"assistant\"}}\n\n",
		"data: {\"delta\":{\"content\":\"half\"}}\n\n",
	}})

	req, err := http.NewRequest(http.MethodPost, server.URL+"/disconnect/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if !strings.Contains(line, "assistant") {
		t.Errorf("first frame: got %q", line)
	}

	// The socket is gone: reading must fail rather than report a clean end.
	if _, err := io.ReadAll(reader); err == nil {
		t.Error("the connection closed cleanly; a test that expects an abrupt end cannot tell it apart")
	}
	resp.Body.Close()
}

// TestCancelIsRecordedForAStreamStillOpen proves the fake upstream can report that a
// cancellation reached it, which is the only way a client-disconnect test can assert
// anything about the upstream side.
func TestCancelIsRecordedForAStreamStillOpen(t *testing.T) {
	fake := New()
	server := httptest.NewServer(fake)
	defer server.Close()

	fake.Set("test-key", Behavior{Status: 200, WaitForCancel: true, SSE: []string{
		"data: {\"delta\":{\"role\":\"assistant\"}}\n\n",
	}})

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !fake.Cancelled("test-key") {
		time.Sleep(10 * time.Millisecond)
	}
	if !fake.Cancelled("test-key") {
		t.Error("the cancellation was not recorded")
	}

	stats := fake.Stats()
	list, _ := stats["cancelled"].([]string)
	if len(list) != 1 || list[0] != "test-key" {
		t.Errorf("stats cancelled: got %v, want test-key", stats["cancelled"])
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(fake.LastBody("test-key")), &body); err != nil {
		t.Errorf("the received body is not JSON: %v", err)
	}
	if body["model"] != "m" {
		t.Errorf("the received body does not report the model: %s", fake.LastBody("test-key"))
	}
}
