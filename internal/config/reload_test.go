// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
	"time"
)

// The interval is the only number in the reload block, and the floor is what makes the watcher safe:
// a file that says 1ms is refused at startup rather than quietly clamped, because a watcher that polls
// every millisecond is exactly the mistake the guard exists for. 0 and absent are the same value in
// YAML, so both mean the default, and the default is the bound a reader can rely on when the docs say
// a change shows up within one interval.
func TestTheReloadIntervalDefaultsAndRefusesABusyLoop(t *testing.T) {
	const route = `routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: test-key
`

	cfg, err := Parse([]byte(route))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Reload.Interval.Std() != time.Second {
		t.Errorf("interval when nothing is written: got %s, want 1s", cfg.Reload.Interval)
	}

	cfg, err = Parse([]byte("config_reload:\n  interval: 0s\n" + route))
	if err != nil {
		t.Fatalf("parse with 0: %v", err)
	}
	if cfg.Reload.Interval.Std() != time.Second {
		t.Errorf("interval written as 0: got %s, want the default 1s, because 0 and absent are the same value in YAML", cfg.Reload.Interval)
	}

	cfg, err = Parse([]byte("config_reload:\n  interval: 250ms\n" + route))
	if err != nil {
		t.Fatalf("parse with 250ms: %v", err)
	}
	if cfg.Reload.Interval.Std() != 250*time.Millisecond {
		t.Errorf("interval: got %s, want 250ms", cfg.Reload.Interval)
	}

	_, err = Parse([]byte("config_reload:\n  interval: 1ms\n" + route))
	if err == nil {
		t.Fatal("a value under the floor is accepted, so the guard is a silent clamp instead of a startup error")
	}
	if !strings.Contains(err.Error(), "config_reload.interval must be 100ms or more") {
		t.Errorf("the startup error says: %v", err)
	}

	_, err = Parse([]byte("config_reload:\n  interval: -1s\n" + route))
	if err == nil {
		t.Fatal("a negative interval is accepted")
	}
	if !strings.Contains(err.Error(), "config_reload.interval must be positive, or 0 for the default") {
		t.Errorf("the startup error says: %v", err)
	}
}
