// SPDX-License-Identifier: AGPL-3.0-only

// Package reload watches the one config file a process started with, and swaps one whole
// configuration for another only when the whole new one is valid.
//
// There is no filesystem event framework here on purpose. The file is a few kilobytes, and reading it
// once a second and comparing a hash finds the cases that matter: an editor writing in place, an
// editor that writes a temp file and renames it over the inode, a file that is briefly half written,
// a file that is briefly gone, and a file inside a mounted directory in a container. It does not
// depend on the event semantics of a platform, so there are no events to lose and no events to
// debounce.
//
// The rule this package is built around is last-known-good. A reload that fails leaves the
// configuration in force exactly as it was: the registry is not cleared, nothing is partly applied,
// the process does not exit, and the next look at the file tries again. Startup is the one place
// where a bad file is fatal, because before the first configuration there is nothing good to keep,
// and that is why this package never waits for a file to appear.
//
// A file that changes a field the process captured at startup is refused as a whole, even when the
// rest of it is valid, because that part of the change cannot be honoured without starting the
// process again. Refusing the whole file is what keeps the file a reader reads and the process that
// is running from disagreeing about a field.
//
// The hash is internal: it is what tells two contents apart so one content is acted on once. No part
// of it is printed, because the content is a secret file. What a log line carries instead is the
// runtime generation, a counter that starts at 1 for the configuration the process started with and
// goes up by one for each content the process acted on. It says which configuration is in force
// without being derived from what the file holds.
package reload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/metrics"
)

// stateUnreadable is the state for a file that could not be read. It is not a hash, so it cannot
// collide with one, and it is one state: a file that stays missing is reported once instead of once
// per second.
const stateUnreadable = "unreadable"

// Watcher reads one file and acts on a change.
//
// Tick is called from one goroutine, which is what keeps the state below safe without a lock. The
// signal handler does not call Tick itself: it tells the goroutine that runs Run to look now, so
// there is no path where two goroutines read and write seen and good at the same time.
type Watcher struct {
	Path     string
	Interval time.Duration
	Log      *slog.Logger
	Metrics  *metrics.Metrics

	// Commit is the swap. It is called only with a configuration that parsed and validated complete,
	// so the build and the swap happen after every check that could have failed. The build itself
	// cannot fail, which is why this takes no error: a check here could never report anything, and a
	// check that cannot fail is not a check.
	Commit func(*config.Config)

	// Force is a manual look. SIGHUP sends here. A buffered channel of one is enough: a second signal
	// while a look is pending is the same look.
	Force chan struct{}

	seen string // the state last acted on, which is what makes one content one report
	// inForce is the configuration that is in force, kept so a change to a process-level field can be
	// compared with what the process was built with. Without it there is nothing to compare against,
	// and a refused change would quietly become part of the running configuration.
	inForce *config.Config
	// generation is the number of the configuration in force: 1 for the one the process started with,
	// and one more for each content acted on. It is a runtime counter, so it is not a value derived
	// from the file, and it is what a log line can carry safely.
	generation int
}

// New builds a watcher. Interval comes from the config, so the floor that config validation enforces
// is the floor this package relies on rather than a second rule written in two places.
func New(path string, interval time.Duration, log *slog.Logger, m *metrics.Metrics, commit func(*config.Config)) *Watcher {
	if interval <= 0 {
		interval = config.DefaultReloadInterval().Std()
	}
	return &Watcher{
		Path:     path,
		Interval: interval,
		Log:      log,
		Metrics:  m,
		Commit:   commit,
		Force:    make(chan struct{}, 1),
	}
}

// Seed records the content the process started with, and the configuration built from it, so the
// first look at the file is not a reload: nothing changed, and a process that counted its own
// startup as a reload would report a change that no one made. Startup read the file through the
// strict path, so this is the same content that was validated, and the configuration recorded here
// is the one the process was built with: the listener, the server object and the logger were all
// made from it, which is why a later change to a field taken from it is refused rather than applied.
//
// The number recorded here is 1, because this is the first configuration.
func (w *Watcher) Seed(data []byte, cfg *config.Config) {
	sum := sha256.Sum256(data)
	state := hex.EncodeToString(sum[:])
	w.seen = state
	w.inForce = cfg
	w.generation = 1
}

// Tick looks at the file once and reports whether it acted on a state it had not acted on before. A
// test calls this directly, so a test does not depend on wall-clock timing or on a sleep.
func (w *Watcher) Tick() bool {
	data, err := os.ReadFile(w.Path)
	if err != nil {
		if w.seen == stateUnreadable {
			return false
		}
		w.seen = stateUnreadable
		w.Metrics.Reloads.WithLabelValues("failed").Inc()
		// The path and the error, and never what the file contains: the error is the reason an
		// operator can read this line, and the file is a secret.
		w.Log.Warn("config_read_failed", "path", w.Path, "generation", w.generation, "error", err)
		return true
	}

	// The hash is the change detector, and it stays inside this function: it is what makes one
	// content one report instead of a report every second.
	sum := sha256.Sum256(data)
	state := hex.EncodeToString(sum[:])
	if state == w.seen {
		return false
	}
	w.seen = state

	cfg, err := config.Parse(data)
	if err != nil {
		w.Metrics.Reloads.WithLabelValues("failed").Inc()
		w.Log.Warn("config_reload_failed", "path", w.Path, "generation", w.generation, "error", err)
		return true
	}

	// Parsing and validating is not the whole decision. A change to a field the process captured at
	// startup is valid as a value and cannot be acted on by a running process, so the whole file is
	// refused instead of half-applied: the configuration in force stays the last one that was acted
	// on. Names, never values, because a field name is what a reader can act on and a value in this
	// file is a secret.
	if changed := config.RestartRequiredChanges(w.inForce, cfg); len(changed) > 0 {
		w.Metrics.Reloads.WithLabelValues("restart_required").Inc()
		w.Log.Warn("config_reload_failed", "path", w.Path, "generation", w.generation,
			"reason", "restart_required_field_changed", "fields", strings.Join(changed, ","))
		return true
	}

	w.Commit(cfg)
	w.inForce = cfg
	w.generation++
	w.Metrics.Reloads.WithLabelValues("ok").Inc()
	w.Log.Info("config_reloaded", "path", w.Path, "generation", w.generation, "routes", len(cfg.Routes))

	if next := cfg.Reload.Interval.Std(); next > 0 && next != w.Interval {
		// The interval is in the file, so a change to it applies from the next look. This is the only
		// way it can change while the process is running, and Run reads it from here.
		w.Interval = next
	}
	return true
}

// Run looks at the file every Interval until ctx is cancelled. The first look is immediate, so a
// change between startup and the first tick is not missed.
func (w *Watcher) Run(ctx context.Context) {
	w.Tick()

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.Force:
			w.Tick()
		case <-ticker.C:
			w.Tick()
			// The interval can have changed in the file, and the only place that learns it is a look.
			ticker.Reset(w.Interval)
		}
	}
}

// Generation is the number of the configuration in force. It is reported so a reader can tell which
// configuration a request used without any part of the file's content coming back out: the number is
// a count of the contents this process acted on, and it does not depend on what they hold.
func (w *Watcher) Generation() int { return w.generation }
