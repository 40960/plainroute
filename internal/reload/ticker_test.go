// SPDX-License-Identifier: AGPL-3.0-only

package reload

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/metrics"
)

// A forced look can change config_reload.interval. The ticker must start the new
// cadence immediately; otherwise 60s -> 100ms still waits almost a minute. Driving
// handleForce directly keeps the test independent of when Run's first tick lands.
func TestForcedIntervalChangeResetsTheTicker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := `config_reload:
  interval: %s
routes:
  %s:
    targets:
      - name: a
        model: m
        base_url: https://example.invalid/v1
        api_key: key-a
`
	write := func(interval, route string) []byte {
		data := []byte(fmt.Sprintf(base, interval, route))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := write("1h", "one")
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	w := New(path, time.Hour, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), metrics.New(), func(*config.Config) {})
	w.Seed(data, cfg)

	write("100ms", "two")
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	w.handleForce(ticker)
	if w.Generation() != 2 {
		t.Fatalf("forced look did not apply the new config: generation %d", w.Generation())
	}

	select {
	case <-ticker.C:
	case <-time.After(2 * time.Second):
		t.Fatal("ticker kept the old interval after a forced look changed it")
	}
}
