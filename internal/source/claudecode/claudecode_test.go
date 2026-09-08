package claudecode_test

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/source"
	"github.com/christianparpart/agentic-stats/internal/source/claudecode"
	"github.com/christianparpart/agentic-stats/internal/source/sourcetest"
)

const home = "/home/user"

func discoveredPaths(t *testing.T, streams []source.Stream) []string {
	t.Helper()
	out := make([]string, len(streams))
	for i, s := range streams {
		out[i] = s.ID().Path
	}
	sort.Strings(out)
	return out
}

func TestDiscoverFindsTranscriptsIncludingSubagentsAndDesktop(t *testing.T) {
	m := sourcetest.NewMapFS()

	// A top-level session and its subagent sidechains. The sidechains carry
	// their own token usage and outnumber sessions in practice, so missing
	// them undercounts badly.
	m.Put(home+"/.claude/projects/-home-user-proj/session-a.jsonl", "{}\n")
	m.Put(home+"/.claude/projects/-home-user-proj/session-a/subagents/agent-1.jsonl", "{}\n")
	m.Put(home+"/.claude/projects/-home-user-proj/session-a/subagents/agent-2.jsonl", "{}\n")

	// Sidecar metadata is not an append-only stream and must be skipped here.
	m.Put(home+"/.claude/projects/-home-user-proj/session-a/subagents/agent-1.meta.json", "{}")

	// The archived install extends history beyond the retention window.
	m.Put(home+"/.claude.old/projects/-home-user-old/session-old.jsonl", "{}\n")

	// Prompt history outlives deleted transcripts.
	m.Put(home+"/.claude/history.jsonl", "{}\n")

	// Claude Desktop agent-mode writes a full transcript tree, several levels
	// deep, that a scraper aimed only at ~/.claude never sees.
	m.Put(home+"/Library/Application Support/Claude/local-agent-mode-sessions/"+
		"client/org/agent/local_ditto_org/.claude/projects/-enc/desktop.jsonl", "{}\n")

	src, err := claudecode.New(claudecode.Config{FS: m, Home: home, Platform: "darwin"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	streams, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := discoveredPaths(t, streams)
	want := []string{
		home + "/.claude.old/projects/-home-user-old/session-old.jsonl",
		home + "/.claude/history.jsonl",
		home + "/.claude/projects/-home-user-proj/session-a.jsonl",
		home + "/.claude/projects/-home-user-proj/session-a/subagents/agent-1.jsonl",
		home + "/.claude/projects/-home-user-proj/session-a/subagents/agent-2.jsonl",
		home + "/Library/Application Support/Claude/local-agent-mode-sessions/" +
			"client/org/agent/local_ditto_org/.claude/projects/-enc/desktop.jsonl",
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %d streams, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != filepath.Clean(want[i]) {
			t.Errorf("stream[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// ~/.claude and ~/.claude/projects overlap; a file must still appear once.
func TestDiscoverDeduplicatesOverlappingRoots(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-p/session.jsonl", "{}\n")

	src, err := claudecode.New(claudecode.Config{FS: m, Home: home, Platform: "linux"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	streams, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("discovered %d streams, want 1: %v", len(streams), discoveredPaths(t, streams))
	}
}

func TestDiscoverHonoursExclusions(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-work-secret/session.jsonl", "{}\n")
	m.Put(home+"/.claude/projects/-open-source/session.jsonl", "{}\n")

	src, err := claudecode.New(claudecode.Config{
		FS:              m,
		Home:            home,
		Platform:        "linux",
		ExcludePrefixes: []string{home + "/.claude/projects/-work-secret"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	streams, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, p := range discoveredPaths(t, streams) {
		if strings.Contains(p, "secret") {
			t.Errorf("excluded path was discovered: %s", p)
		}
	}
	if len(streams) != 1 {
		t.Fatalf("discovered %d streams, want 1", len(streams))
	}
}

// Most machines have only some roots; absent ones are not an error.
func TestDiscoverToleratesMissingRoots(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-p/session.jsonl", "{}\n")

	src, err := claudecode.New(claudecode.Config{FS: m, Home: home, Platform: "windows"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatalf("Discover over mostly-absent roots: %v", err)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := claudecode.New(claudecode.Config{Home: home, Platform: "linux"}); err == nil {
		t.Error("expected an error when FS is missing")
	}
	if _, err := claudecode.New(claudecode.Config{FS: sourcetest.NewMapFS(), Platform: "linux"}); err == nil {
		t.Error("expected an error when Home is missing")
	}
	if _, err := claudecode.New(claudecode.Config{FS: sourcetest.NewMapFS(), Home: home}); err == nil {
		t.Error("expected an error when Platform is missing")
	}
}
