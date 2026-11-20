// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type stuckDrain struct {
	drained bool
	mu      sync.Mutex
}

func (d *stuckDrain) Drain() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.drained = true
}

// Wait never finishes before the deadline, which is what an endless stream would do.
func (d *stuckDrain) Wait(deadline time.Time) bool {
	time.Sleep(time.Until(deadline) + time.Millisecond)
	return false
}

func TestShutdownGraceIsABound(t *testing.T) {
	// A handler that never returns keeps Shutdown waiting unless its context has
	// the same deadline as the handler wait.
	started := make(chan struct{})
	block := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-make(chan struct{})
	})
	ts := httptest.NewUnstartedServer(block)
	ts.Start()
	go func() {
		resp, err := http.Get(ts.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the blocking handler was not reached")
	}

	d := &stuckDrain{}
	var mu sync.Mutex
	warnings := []string{}
	warn := func(msg string, _ ...any) {
		mu.Lock()
		warnings = append(warnings, msg)
		mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		shutdownWithGrace(ts.Config, d, 50*time.Millisecond, warn)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown ignored the grace deadline")
	}

	d.mu.Lock()
	drained := d.drained
	d.mu.Unlock()
	if !drained {
		t.Error("Drain was not called")
	}
	mu.Lock()
	got := append([]string(nil), warnings...)
	mu.Unlock()
	found := false
	for _, msg := range got {
		if msg == "shutdown_grace_exceeded" {
			found = true
		}
	}
	if !found {
		t.Errorf("no grace-exceeded warning: %v", got)
	}
}
