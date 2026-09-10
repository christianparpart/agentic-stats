package ingest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// fakeArchive is an archive in memory, so the walk can be tested without
// SQLite and without key material.
type fakeArchive struct {
	records map[string][]store.Record
	marks   map[string]int64
	saves   int
	passes  int
	// onPass runs at the start of each pass, so a test can stop Run between
	// two of them rather than in the middle of one.
	onPass func(pass int)
}

func (a *fakeArchive) Vector(context.Context) (store.VersionVector, error) {
	a.passes++
	if a.onPass != nil {
		a.onPass(a.passes)
	}
	v := make(store.VersionVector, len(a.records))
	for origin, recs := range a.records {
		v[origin] = recs[len(recs)-1].Seq
	}
	return v, nil
}

func (a *fakeArchive) Since(_ context.Context, origin string, after int64, limit int) ([]store.Record, error) {
	var out []store.Record
	for _, r := range a.records[origin] {
		if r.Seq > after {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (a *fakeArchive) ExtractionMarks(context.Context, int) (map[string]int64, error) {
	out := make(map[string]int64, len(a.marks))
	for k, v := range a.marks {
		out[k] = v
	}
	return out, nil
}

func (a *fakeArchive) SaveExtracted(_ context.Context, _ int, origin string, upTo int64, rows []store.Extracted) error {
	a.saves++
	if a.marks == nil {
		a.marks = make(map[string]int64)
	}
	a.marks[origin] = upTo
	for _, w := range rows {
		for i, r := range a.records[origin] {
			if r.Seq == w.Seq {
				a.records[origin][i].CWD = w.CWD
				a.records[origin][i].GitBranch = w.GitBranch
				a.records[origin][i].PRRepo = w.PRRepo
				a.records[origin][i].PRNumber = w.PRNumber
			}
		}
	}
	return nil
}

// plaintextOpener stands in for the seal, so a test needs no key.
type plaintextOpener struct{}

func (plaintextOpener) OpenPayload(sealed []byte) ([]byte, error) { return sealed, nil }

// The log is written to a file beside the archive and is not sealed. So
// progress may be counted but never described: a working directory in there
// would put the project names this archive exists to protect into plaintext,
// right next to the database that went to the trouble of encrypting them.
//
// Through Run rather than Pass, because Run is what logs.
func TestReprocessingNeverLogsAWorkingDirectory(t *testing.T) {
	const secret = `D:\customer-name-in-the-path`

	var logged bytes.Buffer
	archive := &fakeArchive{records: map[string][]store.Record{
		"origin-a": {{
			OriginID: "origin-a", Seq: 1,
			Sealed: []byte(`{"type":"assistant","requestId":"r1","sessionId":"s",` +
				`"cwd":"D:\\customer-name-in-the-path","gitBranch":"secret/branch-name",` +
				`"message":{"model":"claude-opus-5","usage":{"input_tokens":1}}}`),
		}},
	}}

	// Let one pass finish and stop Run before the next, so the log holds
	// exactly what a complete pass writes and nothing is left racing. Stopping
	// mid-pass would prove nothing: a cancelled pass deliberately logs no
	// summary, because it did not finish.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// One tick, already waiting, so Run reaches its second pass without a
	// timer and the test does not wait on wall-clock time.
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	archive.onPass = func(pass int) {
		if pass == 2 {
			cancel()
		}
	}

	redo, err := NewReprocessor(ReprocessConfig{
		Archive: archive,
		Keys:    plaintextOpener{},
		Logger:  slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Tick:    ticks,
	})
	if err != nil {
		t.Fatalf("NewReprocessor: %v", err)
	}
	if err := redo.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := archive.records["origin-a"][0].CWD; got != secret {
		t.Fatalf("the working directory was not extracted (%q), so the log proves nothing", got)
	}

	out := logged.String()
	if out == "" {
		t.Fatal("nothing was logged at all; the test cannot show what is absent")
	}
	for _, leaked := range []string{"customer-name-in-the-path", "secret/branch-name", `D:`} {
		if strings.Contains(out, leaked) {
			t.Errorf("the log names %q:\n%s", leaked, out)
		}
	}
}

// A constructed object is a usable object: neither dependency has a sensible
// default, so a missing one is a construction error and not a pass that walks
// the archive and quietly writes nothing.
func TestAReprocessorNeedsAnArchiveAndAKey(t *testing.T) {
	if _, err := NewReprocessor(ReprocessConfig{Keys: plaintextOpener{}}); err == nil {
		t.Error("a reprocessor was built with no archive")
	}
	if _, err := NewReprocessor(ReprocessConfig{Archive: &fakeArchive{}}); err == nil {
		t.Error("a reprocessor was built with no key")
	}
}

// A body that will not unseal is skipped, not fatal.
//
// A node can hold whole origins it has no key for. Treating one as an error
// stopped the pass dead, which meant a single unreadable origin denied every
// readable one its columns -- and which origin happened to be walked first
// decided how much of the archive worked at all.
func TestAnUnreadableBodyIsSkippedRatherThanFatal(t *testing.T) {
	archive := &fakeArchive{records: map[string][]store.Record{
		"origin-a": {
			{OriginID: "origin-a", Seq: 1, Sealed: []byte("ciphertext")},
			{OriginID: "origin-a", Seq: 2, Sealed: []byte(`{"type":"assistant","requestId":"r","sessionId":"s","cwd":"/w/endo","message":{"model":"m","usage":{"input_tokens":1}}}`)},
		},
	}}
	redo, err := NewReprocessor(ReprocessConfig{
		Archive: archive,
		Keys:    openerFailingOn("ciphertext"),
	})
	if err != nil {
		t.Fatalf("NewReprocessor: %v", err)
	}
	stats, err := redo.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.Unreadable != 1 {
		t.Errorf("reported %d unreadable records, want 1", stats.Unreadable)
	}
	// The readable record past it still got its columns, which is the whole
	// point: one origin without a key must not cost the others theirs.
	if got := archive.records["origin-a"][1].CWD; got != "/w/endo" {
		t.Errorf("the record after the unreadable one has CWD %q", got)
	}
	// And the walk finished, so the next pass has nothing to redo.
	if archive.marks["origin-a"] != 2 {
		t.Errorf("the mark stopped at %d, want 2", archive.marks["origin-a"])
	}
}

// openerFailingOn unseals everything except the one body it is told to refuse,
// standing in for an origin this node has no key for.
type openerFailingOn string

func (o openerFailingOn) OpenPayload(sealed []byte) ([]byte, error) {
	if string(sealed) == string(o) {
		return nil, errors.New("seal: cannot open")
	}
	return sealed, nil
}
