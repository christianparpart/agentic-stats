package daemonlog_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/daemonlog"
)

// fakeEvents stands in for the operating system's log.
type fakeEvents struct {
	mu                    sync.Mutex
	info, warning, errors []string
	closed                bool
	fail                  bool
}

func (f *fakeEvents) record(dst *[]string, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*dst = append(*dst, msg)
	if f.fail {
		return os.ErrClosed
	}
	return nil
}

func (f *fakeEvents) Info(m string) error    { return f.record(&f.info, m) }
func (f *fakeEvents) Warning(m string) error { return f.record(&f.warning, m) }
func (f *fakeEvents) Error(m string) error   { return f.record(&f.errors, m) }
func (f *fakeEvents) Close() error           { f.mu.Lock(); defer f.mu.Unlock(); f.closed = true; return nil }

func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "agentic-stats.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// A service with no terminal must still leave a durable record.
func TestRecordsReachTheFile(t *testing.T) {
	dir := t.TempDir()
	l, err := daemonlog.New(daemonlog.Config{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("collection pass", "lines", 42)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := readLog(t, dir)
	for _, want := range []string{"collection pass", "lines=42"} {
		if !strings.Contains(got, want) {
			t.Errorf("log = %q, missing %q", got, want)
		}
	}
}

// In a terminal the log has to keep printing. A log that vanishes when you run
// the thing by hand is the wrong kind of quiet.
func TestRecordsAlsoReachTheConsole(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l, err := daemonlog.New(daemonlog.Config{Dir: dir, Console: &console})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Warn("peer unreachable")
	_ = l.Close()

	if !strings.Contains(console.String(), "peer unreachable") {
		t.Errorf("console = %q, missing the record", console.String())
	}
	if !strings.Contains(readLog(t, dir), "peer unreachable") {
		t.Error("the console copy replaced the file copy instead of adding to it")
	}
}

// Severity has to survive the trip, since that is what an operator filters on.
func TestSeverityIsMappedForTheEventLog(t *testing.T) {
	dir := t.TempDir()
	events := &fakeEvents{}
	l, err := daemonlog.New(daemonlog.Config{
		Dir: dir, EventLog: events, EventLevel: slog.LevelInfo,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("started")
	l.Warn("peer failing")
	l.Error("archive unreadable")
	_ = l.Close()

	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.info) != 1 || !strings.Contains(events.info[0], "started") {
		t.Errorf("info entries = %v", events.info)
	}
	if len(events.warning) != 1 || !strings.Contains(events.warning[0], "peer failing") {
		t.Errorf("warning entries = %v", events.warning)
	}
	if len(events.errors) != 1 || !strings.Contains(events.errors[0], "archive unreadable") {
		t.Errorf("error entries = %v", events.errors)
	}
	if !events.closed {
		t.Error("the event log was not closed")
	}
}

// Structured fields have to be flattened into the message, because the event
// log takes prose. Losing them would make the two logs disagree about what
// happened.
func TestAttributesSurviveIntoTheEventLog(t *testing.T) {
	dir := t.TempDir()
	events := &fakeEvents{}
	l, err := daemonlog.New(daemonlog.Config{
		Dir: dir, EventLog: events, EventLevel: slog.LevelInfo,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("converged with peer", "peer", "abc123", "stored", 7)
	_ = l.Close()

	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.info) != 1 {
		t.Fatalf("info entries = %v", events.info)
	}
	for _, want := range []string{"converged with peer", "peer=abc123", "stored=7"} {
		if !strings.Contains(events.info[0], want) {
			t.Errorf("event %q missing %q", events.info[0], want)
		}
	}
}

// A failing event log must not cost the file line that explains why it failed.
func TestAFailingEventLogDoesNotLoseTheFileRecord(t *testing.T) {
	dir := t.TempDir()
	l, err := daemonlog.New(daemonlog.Config{Dir: dir, EventLog: &fakeEvents{fail: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Error("something broke")
	_ = l.Close()

	if !strings.Contains(readLog(t, dir), "something broke") {
		t.Error("a failing event log swallowed the file record")
	}
}

// A daemon runs for months. Without rotation its log is unbounded, which on a
// laptop eventually matters more than the log does.
func TestTheLogRotatesAndKeepsABoundedHistory(t *testing.T) {
	dir := t.TempDir()
	l, err := daemonlog.New(daemonlog.Config{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Comfortably past the threshold, several times over.
	big := strings.Repeat("x", 64*1024)
	for range (daemonlog.MaxFileBytes/len(big) + 2) * (daemonlog.KeptFiles + 2) {
		l.Info("filler", "pad", big)
	}
	_ = l.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var rotated int
	for _, e := range entries {
		if e.Name() != "agentic-stats.log" {
			rotated++
		}
		info, ierr := e.Info()
		if ierr != nil {
			t.Fatalf("stat %s: %v", e.Name(), ierr)
		}
		// Each file is closed at the threshold, so none should be far past it.
		if info.Size() > 2*daemonlog.MaxFileBytes {
			t.Errorf("%s is %d bytes, well past the %d rotation threshold",
				e.Name(), info.Size(), daemonlog.MaxFileBytes)
		}
	}
	if rotated == 0 {
		t.Error("nothing rotated despite writing far past the threshold")
	}
	if rotated > daemonlog.KeptFiles {
		t.Errorf("kept %d rotated files, want at most %d", rotated, daemonlog.KeptFiles)
	}
}

func TestNewRequiresADirectory(t *testing.T) {
	if _, err := daemonlog.New(daemonlog.Config{}); err == nil {
		t.Error("New accepted a configuration with no directory")
	}
}

// The level has to be honoured, or a debug-heavy daemon fills the disk.
func TestLevelIsRespected(t *testing.T) {
	dir := t.TempDir()
	l, err := daemonlog.New(daemonlog.Config{Dir: dir, Level: slog.LevelWarn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("should not appear")
	l.Warn("should appear")
	_ = l.Close()

	got := readLog(t, dir)
	if strings.Contains(got, "should not appear") {
		t.Error("a record below the configured level was written")
	}
	if !strings.Contains(got, "should appear") {
		t.Error("a record at the configured level was dropped")
	}
}

// The Application log is where someone looks to find out whether anything is
// wrong, so routine progress must not reach it. Teeing everything put a
// "pass complete" in Event Viewer every poll interval, which buries the
// entries that matter -- caught by looking at Event Viewer rather than at a
// test, which is why this one exists.
func TestRoutineProgressStaysOutOfTheEventLog(t *testing.T) {
	dir := t.TempDir()
	events := &fakeEvents{}
	l, err := daemonlog.New(daemonlog.Config{Dir: dir, EventLog: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 20 {
		l.Info("pass complete", "lines", 3)
	}
	l.Warn("peer failing")
	l.Error("archive unreadable")
	_ = l.Close()

	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.info) != 0 {
		t.Errorf("%d routine records reached the event log, want 0: %v",
			len(events.info), events.info)
	}
	if len(events.warning) != 1 || len(events.errors) != 1 {
		t.Errorf("the entries that matter did not reach the event log: warnings=%v errors=%v",
			events.warning, events.errors)
	}
	// All of it still has to be in the file.
	got := readLog(t, dir)
	if !strings.Contains(got, "pass complete") {
		t.Error("routine progress was dropped from the file as well")
	}
}
