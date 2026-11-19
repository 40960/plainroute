// SPDX-License-Identifier: AGPL-3.0-only

package reload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The tests drive Tick directly. A test that waited on the ticker would be testing wall-clock
// scheduling, and it would need a sleep long enough for the watcher to look, which is a test that
// passes because of how long it waits rather than because of what the code does.

const good = `
routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: key-one
`

const bad = `
routing:
  cooldown:
    max: -1s
routes:
  demo:
    targets:
      - name: one
        model: m
        base_url: https://example.invalid/v1
        api_key: key-one
`

type drive struct {
	t       *testing.T
	w       *Watcher
	path    string
	log     *bytes.Buffer
	metrics *metrics.Metrics
	// committed is what the watcher swapped in. A reload that failed must not put anything here.
	committed []*config.Config
}

func newDrive(t *testing.T) *drive {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	d := &drive{t: t, path: path, log: &bytes.Buffer{}, metrics: metrics.New()}
	d.w = New(path, time.Second, slog.New(slog.NewJSONHandler(d.log, nil)), d.metrics, func(cfg *config.Config) {
		d.committed = append(d.committed, cfg)
	})
	return d
}

// write writes in place, the way an editor that saves does. renameOver is the other way: a temp
// file renamed on top of the inode. Both have to be seen, because which one an editor uses is not
// something this package can know.
func (d *drive) write(content string) {
	d.t.Helper()
	if err := os.WriteFile(d.path, []byte(content), 0o600); err != nil {
		d.t.Fatalf("write: %v", err)
	}
}

func (d *drive) renameOver(content string) {
	d.t.Helper()
	tmp := d.path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		d.t.Fatalf("temp write: %v", err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		d.t.Fatalf("rename: %v", err)
	}
}

func (d *drive) remove() {
	d.t.Helper()
	if err := os.Remove(d.path); err != nil {
		d.t.Fatalf("remove: %v", err)
	}
}

func (d *drive) out() string { return d.log.String() }
func (d *drive) ok() float64 { return testutil.ToFloat64(d.metrics.Reloads.WithLabelValues("ok")) }
func (d *drive) failed() float64 {
	return testutil.ToFloat64(d.metrics.Reloads.WithLabelValues("failed"))
}
func (d *drive) restartRequired() float64 {
	return testutil.ToFloat64(d.metrics.Reloads.WithLabelValues("restart_required"))
}

func TestChangedContentIsReloadedAndTheSameContentIsNot(t *testing.T) {
	d := newDrive(t)
	d.write(good)

	if !d.w.Tick() {
		t.Fatal("the first look at a file must act on it")
	}
	if len(d.committed) != 1 {
		t.Fatalf("commits: got %d, want 1", len(d.committed))
	}

	// Twelve looks at the same content: a file that is not changing must not be reloaded, and must
	// not be counted as if it were. This is the check that would fail if the watcher acted on every
	// look, which is what a watcher that polls tends to do.
	for i := 0; i < 12; i++ {
		if d.w.Tick() {
			t.Fatalf("look %d acted on content it had already acted on", i+1)
		}
	}
	if len(d.committed) != 1 {
		t.Fatalf("commits after 13 looks: got %d, want 1", len(d.committed))
	}
	if got := d.ok(); got != 1 {
		t.Errorf("reloads counted as ok: got %v, want 1", got)
	}

	d.write(strings.Replace(good, "key-one", "key-two", 1))
	if !d.w.Tick() {
		t.Fatal("changed content must be acted on")
	}
	if len(d.committed) != 2 {
		t.Fatalf("commits: got %d, want 2", len(d.committed))
	}
	if committed := d.committed[1]; committed.Routes["demo"].Targets[0].APIKey != "key-two" {
		t.Errorf("the config swapped in is not the one the file says: %q", committed.Routes["demo"].Targets[0].APIKey)
	}
}

// Startup read the file through the strict path, so the first look at the file is not a reload:
// nothing changed, and a process that counted its own startup as a reload would report a change no
// one made.
func TestTheContentStartupLoadedIsNotCountedAsAReload(t *testing.T) {
	d := newDrive(t)
	d.write(good)

	cfg, err := config.Load(d.path)
	if err != nil {
		t.Fatalf("startup load: %v", err)
	}
	d.committed = append(d.committed, cfg)
	d.w.Seed([]byte(good), cfg)

	if d.w.Tick() {
		t.Fatal("the startup content must not be acted on again")
	}
	if got := d.ok(); got != 0 {
		t.Errorf("reloads counted as ok: got %v, want 0", got)
	}
}

// Last-known-good is the whole rule: a bad file is not applied, not partly applied, and not a
// reason to stop. The configuration in force is the last one that was valid, and the next look
// tries again.
func TestABadFileKeepsTheLastGoodOneInForce(t *testing.T) {
	d := newDrive(t)
	d.write(good)
	d.w.Tick()

	inForce := d.committed[0]

	// An editor that saved a half-written file: content that decodes but does not validate.
	d.write(bad)
	if !d.w.Tick() {
		t.Fatal("a changed file must be acted on, even to report that it is bad")
	}
	if len(d.committed) != 1 {
		t.Fatalf("a bad file was applied: got %d commits, want 1", len(d.committed))
	}
	if d.committed[0] != inForce {
		t.Error("the configuration in force changed because of a bad file")
	}
	if !strings.Contains(d.out(), "config_reload_failed") {
		t.Errorf("the failure is not reported: %s", d.out())
	}

	// The same bad content twelve more times: one problem, reported once. A watcher that reported
	// every look would write a log line every second for one mistake an operator already made.
	for i := 0; i < 12; i++ {
		if d.w.Tick() {
			t.Errorf("look %d acted on the same content again", i+1)
		}
	}
	if got := d.failed(); got != 1 {
		t.Errorf("failures counted: got %v, want 1 for one bad content", got)
	}
	if strings.Count(d.out(), "config_reload_failed") != 1 {
		t.Errorf("the same failure was reported more than once: %s", d.out())
	}

	// Recovery. The file is written correctly again, and that is a change, so it is acted on.
	d.write(good)
	if !d.w.Tick() {
		t.Fatal("recovery to a valid file must be acted on")
	}
	if len(d.committed) != 2 {
		t.Fatalf("commits: got %d, want 2", len(d.committed))
	}
	if got := d.ok(); got != 2 {
		t.Errorf("reloads counted as ok: got %v, want 2 (the first look and the recovery)", got)
	}
}

// A file that is briefly gone is one state, reported once. A process does not crash because an
// editor moved a file away for a moment, and it does not keep saying so once a second.
func TestAFileThatIsGoneIsReportedOnceAndThenRecoveryIsActedOn(t *testing.T) {
	d := newDrive(t)
	d.write(good)
	d.w.Tick()

	d.remove()
	if !d.w.Tick() {
		t.Fatal("a file that cannot be read must be reported")
	}
	for i := 0; i < 12; i++ {
		if d.w.Tick() {
			t.Errorf("look %d reported the same unreadable file again", i+1)
		}
	}
	if got := d.failed(); got != 1 {
		t.Errorf("failures counted: got %v, want 1", got)
	}
	if len(d.committed) != 1 {
		t.Fatalf("a missing file replaced the configuration in force: got %d commits", len(d.committed))
	}
	if !strings.Contains(d.out(), "config_read_failed") {
		t.Errorf("the read failure is not reported: %s", d.out())
	}

	d.write(good)
	if !d.w.Tick() {
		t.Fatal("the file coming back is a change, and must be acted on")
	}
	if len(d.committed) != 2 {
		t.Fatalf("commits: got %d, want 2", len(d.committed))
	}
}

// Both ways an editor writes a file have to be seen: the save in place, and the temp file renamed
// over the inode. The hash is content, so it does not care which one happened.
func TestBothWaysAnEditorWritesTheFileAreSeen(t *testing.T) {
	for _, way := range []struct {
		name   string
		action func(d *drive)
	}{
		{"written in place", func(d *drive) { d.write(strings.Replace(good, "key-one", "key-two", 1)) }},
		{"written as a temp file and renamed over the inode", func(d *drive) { d.renameOver(strings.Replace(good, "key-one", "key-two", 1)) }},
	} {
		d := newDrive(t)
		d.write(good)
		d.w.Tick()

		way.action(d)
		if !d.w.Tick() {
			t.Fatalf("%s: the change was not seen", way.name)
		}
		if len(d.committed) != 2 {
			t.Fatalf("%s: commits: got %d, want 2", way.name, len(d.committed))
		}
	}
}

// A half-written file is the case the rule exists for: content that is on its way to being valid,
// read while it is not. It is reported, not applied, and the completed file is acted on.
func TestAHalfWrittenFileIsReportedAndTheCompletedFileIsApplied(t *testing.T) {
	d := newDrive(t)
	d.write(good)
	d.w.Tick()

	// A writer that put the first part down and has not finished: it decodes as YAML, and it is not
	// a configuration, so it cannot become the one in force.
	d.write("routes:\n  demo:\n    targets:\n      - name: one\n")
	if !d.w.Tick() {
		t.Fatal("the half-written content is a change and must be acted on")
	}
	if len(d.committed) != 1 {
		t.Fatalf("half-written content was applied: got %d commits, want 1", len(d.committed))
	}
	if !strings.Contains(d.out(), "config_reload_failed") {
		t.Errorf("the half-written file is not reported: %s", d.out())
	}

	d.write(good)
	if !d.w.Tick() {
		t.Fatal("the completed file must be acted on")
	}
	if len(d.committed) != 2 {
		t.Fatalf("commits: got %d, want 2", len(d.committed))
	}
}

// The interval is in the file, so the only way it can change while the process is running is a look
// that reads it. A value under the floor is a startup error, so this package never has to handle a
// file that would make it a busy loop — but a file that arrived after startup is not startup, and a
// watcher that accepted 1ms would be a busy loop.
func TestTheIntervalInTheFileAppliesFromTheNextLook(t *testing.T) {
	d := newDrive(t)
	d.write(good)
	if d.w.Interval != time.Second {
		t.Fatalf("default interval: got %s, want 1s", d.w.Interval)
	}
	d.w.Tick()

	fast := strings.Replace(good, "routes:", "config_reload:\n  interval: 250ms\nroutes:", 1)
	d.write(fast)
	if !d.w.Tick() {
		t.Fatal("the file that sets the interval is a change")
	}
	if d.w.Interval != 250*time.Millisecond {
		t.Errorf("interval after that look: got %s, want 250ms", d.w.Interval)
	}

	// Under the floor: refused, and the interval that is in force stays the one that was working.
	under := strings.Replace(fast, "250ms", "10ms", 1)
	d.write(under)
	if !d.w.Tick() {
		t.Fatal("a change must be acted on, even to refuse it")
	}
	if d.w.Interval != 250*time.Millisecond {
		t.Errorf("a refused value changed the interval in force: got %s", d.w.Interval)
	}
	if len(d.committed) != 2 {
		t.Fatalf("a refused file was applied: got %d commits, want 2", len(d.committed))
	}
}

// SIGHUP asks for a look now. It does not call Tick itself, because the state is kept by one
// goroutine; it sends here, and a buffered channel of one is enough: a second signal while a look
// is pending is the same look.
func TestForceIsOnePendingLookAndASignalThatChangesNothingIsNotAReload(t *testing.T) {
	d := newDrive(t)
	d.write(good)
	d.w.Tick()

	// Two signals with nobody reading. The first is the pending look; the second cannot fit, which is
	// the point of a channel of one: a signal that arrives while a look is pending is the same look,
	// and a signal handler that could queue work would be a worker rather than a nudge.
	if !signal(d.w.Force) {
		t.Fatal("the first signal did not fit")
	}
	if signal(d.w.Force) {
		t.Error("a second pending look fitted, so a signal can queue work")
	}

	// The pending look is read, which is what Run does. The file has not changed, so this is a look
	// that finds nothing: not a reload, and not a count.
	<-d.w.Force
	if d.w.Tick() {
		t.Error("a signal on a file that has not changed is not a reload")
	}

	// A look was read, so the next signal fits. A signal that changes the file is a look that acts:
	// this is what makes the signal an immediate reload rather than a restart.
	d.write(strings.Replace(good, "key-one", "key-two", 1))
	if !signal(d.w.Force) {
		t.Fatal("after a look was read the next signal must fit")
	}
	<-d.w.Force
	if !d.w.Tick() {
		t.Fatal("a signal that changes the file must be acted on")
	}
}

// signal models what the signal handler does: a send that never blocks. It reports whether the look
// fitted. A test that received from Force after a send that was expected not to fit would wait for
// something that can never arrive.
func signal(ch chan struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// The log carries the path and the runtime generation, and never a value read from the file,
// because the file is a secret. This is the same rule as the parse error, on the path a log line
// takes. The hash is what the watcher uses to tell one content from another, and no part of it is
// printed: a hash of a secret file is still something a reader can compare against a file.
func TestTheReloadLogNamesTheFileAndTheGenerationAndNotAValueOrTheHash(t *testing.T) {
	d := newDrive(t)
	secret := "PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645"
	content := strings.Replace(good, "key-one", secret, 1)
	d.write(content)
	d.w.Tick()

	if strings.Contains(d.out(), secret) {
		t.Errorf("a key appeared in the reload log: %s", d.out())
	}
	if !strings.Contains(d.out(), d.path) {
		t.Errorf("the log must name the file: %s", d.out())
	}

	// Both forms the fingerprint used to take: the whole hash and the ten characters of it. A check
	// that only looked for the short form would pass on a build that printed the whole one.
	sum := sha256.Sum256([]byte(content))
	full := hex.EncodeToString(sum[:])
	if strings.Contains(d.out(), full) {
		t.Errorf("the log printed the content hash: %s", d.out())
	}
	if strings.Contains(d.out(), full[:10]) {
		t.Errorf("the log printed part of the content hash: %s", d.out())
	}
	if !strings.Contains(d.out(), `"generation":1`) {
		t.Errorf("the log must name the configuration in force: %s", d.out())
	}

	d.write(bad)
	d.w.Tick()
	if strings.Contains(d.out(), secret) {
		t.Errorf("a key from the file in force appeared in the failure line: %s", d.out())
	}
	if strings.Contains(d.out(), full) || strings.Contains(d.out(), full[:10]) {
		t.Errorf("a failure line printed part of the content hash: %s", d.out())
	}
}

// The number the log carries is the count of contents this process acted on, and it is the check
// that the number is not a value derived from the file: the two contents below are written in an
// order that the hashes put the other way round, so a generation that came from the content would
// not come out as 2 and then 3. Re-reading content that was already acted on does not move it.
func TestTheGenerationIsTheCountOfContentsActedOnAndNotAValueFromTheFile(t *testing.T) {
	d := newDrive(t)

	// The hash of this content starts with a hex digit greater than the hash of the content written
	// second, which is what makes the order below the order the process acted on them and not an order
	// the content itself would give. Running this test shows the two hashes, so the claim is measured
	// and not taken on faith.
	first := strings.Replace(good, "key-one", "key-a", 1)
	second := strings.Replace(good, "key-one", "key-b", 1)
	sumFirst := sha256.Sum256([]byte(first))
	sumSecond := sha256.Sum256([]byte(second))
	hashFirst := hex.EncodeToString(sumFirst[:])
	hashSecond := hex.EncodeToString(sumSecond[:])
	if hashFirst <= hashSecond {
		t.Fatalf("the fixtures no longer order the way this check needs, so it cannot answer: %s vs %s",
			hashFirst, hashSecond)
	}

	d.write(first)
	d.w.Tick()
	if d.w.Generation() != 1 {
		t.Fatalf("generation after the first content: got %d, want 1", d.w.Generation())
	}

	d.write(second)
	if !d.w.Tick() {
		t.Fatal("the second content is a change")
	}
	if d.w.Generation() != 2 {
		t.Errorf("generation after the content with the smaller hash: got %d, want 2", d.w.Generation())
	}

	// The same content again: nothing to act on, so the number does not move.
	if d.w.Tick() {
		t.Fatal("the same content was acted on twice")
	}
	if d.w.Generation() != 2 {
		t.Errorf("reading content that was already acted on moved the generation: got %d", d.w.Generation())
	}

	// A refusal does not make a new generation: the configuration in force is still the one the
	// process was built with, and the number names that one.
	d.write(bad)
	if !d.w.Tick() {
		t.Fatal("a bad file is acted on, even to refuse it")
	}
	if d.w.Generation() != 2 {
		t.Errorf("a refused file made a new generation: got %d, want 2", d.w.Generation())
	}
}

// The transactional half: a file that changes a field the running process cannot act on is refused
// as a whole, including the fields in the same file that the process could have acted on. Nothing is
// partly applied, the configuration in force is the last one that was acted on, the process is not
// stopped, and the next look tries again.
//
// The comparison is against the configuration in force and not against the file the last look read,
// which is what the last part of this test checks: a content that keeps a refused value and changes
// only a hot field is still refused. A watcher that compared against the file it had just read would
// see no change to the listener here and apply the key.
func TestAChangeToAProcessLevelFieldIsRefusedAsAWholeAndSurvivesAsTheLastKnownGood(t *testing.T) {
	d := newDrive(t)
	secret := "PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645"

	d.write(strings.Replace(good, "key-one", secret, 1))
	d.w.Tick()
	inForce := d.committed[0]

	// A listener the process already built, together with a key a new request would have read.
	refused := strings.Replace(good, "key-one", "key-two", 1) + "server:\n  listen: 127.0.0.1:9999\n"
	d.write(refused)
	if !d.w.Tick() {
		t.Fatal("a changed file must be acted on, even to refuse it")
	}
	if len(d.committed) != 1 {
		t.Fatalf("a refused file was applied: got %d commits, want 1", len(d.committed))
	}
	if d.committed[0] != inForce {
		t.Error("the configuration in force changed because of a refused file")
	}
	if got := d.restartRequired(); got != 1 {
		t.Errorf("refusals counted: got %v, want 1", got)
	}

	line := d.out()
	if !strings.Contains(line, "restart_required_field_changed") {
		t.Errorf("the refusal does not say why: %s", line)
	}
	if !strings.Contains(line, "server.listen") {
		t.Errorf("the refusal does not name the field it refused: %s", line)
	}

	// Names, not values: the address the file asked for and the key the file holds.
	if strings.Contains(line, "9999") {
		t.Errorf("the refusal printed the value it refused: %s", line)
	}
	if strings.Contains(line, "key-two") {
		t.Errorf("the refusal printed a key: %s", line)
	}
	if d.w.Generation() != 1 {
		t.Errorf("the generation in force moved because of a refused file: got %d", d.w.Generation())
	}

	// One refusal per content, not one per second.
	for i := 0; i < 12; i++ {
		if d.w.Tick() {
			t.Errorf("look %d reported the same refused content again", i+1)
		}
	}
	if got := d.restartRequired(); got != 1 {
		t.Errorf("refusals counted after 13 looks: got %v, want 1", got)
	}

	// The refused value is still in the file, and this content changes only a hot field. It is still
	// refused, because the process cannot act on the listener it was built with.
	d.write(strings.Replace(refused, "key-two", "key-three", 1))
	if !d.w.Tick() {
		t.Fatal("new content is a change, even when it is refused")
	}
	if len(d.committed) != 1 {
		t.Fatalf("a file that still asked for a new listener was applied: got %d commits", len(d.committed))
	}
	if got := d.restartRequired(); got != 2 {
		t.Errorf("refusals counted: got %v, want 2, because the comparison is against the file in force", got)
	}

	// The file comes back to what the process was built with, plus a key a new request can read.
	// That is applied: the refusal did not freeze the watcher.
	d.write(strings.Replace(good, "key-one", "key-four", 1))
	if !d.w.Tick() {
		t.Fatal("a file that only changes a field the process can act on must be applied")
	}
	if len(d.committed) != 2 {
		t.Fatalf("commits: got %d, want 2", len(d.committed))
	}
	if committed := d.committed[1]; committed.Routes["demo"].Targets[0].APIKey != "key-four" {
		t.Errorf("the configuration swapped in is not the one the file says: %q",
			committed.Routes["demo"].Targets[0].APIKey)
	}
	if got := d.restartRequired(); got != 2 {
		t.Errorf("the applied reload was counted as a refusal: got %v, want 2 refusals", got)
	}
	if got := d.ok(); got != 2 {
		t.Errorf("reloads counted as ok: got %v, want 2 (the first look and this one)", got)
	}
	if d.w.Generation() != 2 {
		t.Errorf("generation after the applied reload: got %d, want 2", d.w.Generation())
	}
}

// The two sides of the boundary, read from the code and not from a document: a request-level field
// is what a new request can see, and a process-level field is what it cannot. The test writes both
// in one file and reads back which one the running process acted on, so a table that claims a field
// is hot while the code reads it only at startup cannot pass here.
func TestAProcessLevelFieldAndARequestLevelFieldInOneFileAnswerFromTheCode(t *testing.T) {
	d := newDrive(t)

	d.write(good)
	d.w.Tick()

	// One file, two kinds of change: the logger was built at startup, and the request cap is read on
	// every request.
	mixed := strings.Replace(good, "key-one", "key-two", 1) + "logging:\n  level: debug\n"
	d.write(mixed)
	if !d.w.Tick() {
		t.Fatal("the mixed file must be acted on, even to refuse it")
	}
	if got := d.restartRequired(); got != 1 {
		t.Fatalf("refusals counted: got %v, want 1", got)
	}

	// The refusal names the field that a running process cannot act on, and not the key, which a new
	// request would have read from the generation in force.
	line := d.out()
	if !strings.Contains(line, "logging.level") {
		t.Errorf("the refusal does not name the field it refused: %s", line)
	}
	if strings.Contains(line, "key-two") {
		t.Errorf("the refusal printed a value from the file: %s", line)
	}
	if len(d.committed) != 1 {
		t.Fatalf("part of a refused file was applied: got %d commits", len(d.committed))
	}
}
