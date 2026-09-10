// Command fixturegen writes the frozen synthetic fixture sets.
//
// It refuses to overwrite a fixture that already exists. That is the whole
// point of the command rather than an inconvenience: a parser's fixtures are
// written once and never edited, so that a refactor cannot quietly change how
// years of archived records are read. A format that grows a case gets a new
// fixture beside the old ones.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/christianparpart/agentic-stats/internal/testfixture"
)

func main() {
	dir := flag.String("dir", filepath.Join("testdata", "synthetic"),
		"where the fixture sets live")
	flag.Parse()

	if err := run(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "fixturegen: %v\n", err)
		os.Exit(1)
	}
}

// project and worktree are the two shapes a working directory takes: a
// repository, and a worktree the assistant created inside it.
const (
	project  = `D:\example-project`
	worktree = `D:\example-project\.claude\worktrees\agent-a0000000000000001`
	posix    = "/home/example/example-project"
)

func run(dir string) error {
	claudecode := filepath.Join(dir, "claudecode")
	if err := os.MkdirAll(claudecode, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", claudecode, err)
	}

	onBranch := testfixture.Provenance{CWD: project, Branch: "master"}
	onFeature := testfixture.Provenance{CWD: project, Branch: "feature/12-something"}
	full := testfixture.Usage{
		Input: 11, Output: 22, Thinking: 44,
		CacheRead: 33, CacheWrite5m: 55, CacheWrite1h: 66,
	}

	fixtures := []struct{ name, line string }{
		{"assistant-billable", testfixture.Assistant(
			"sess-0001", "uuid-0001", "req-0001", "claude-opus-5",
			"2026-01-02T03:04:05.000Z", onBranch, full)},

		// The same response's second content block: one line per block, each
		// repeating the whole usage object. Summing these is the 2.9x
		// over-count the fold exists to prevent.
		{"assistant-repeated-usage", testfixture.Assistant(
			"sess-0001", "uuid-0002", "req-0001", "claude-opus-5",
			"2026-01-02T03:04:05.100Z", onBranch, full)},

		{"assistant-synthetic", testfixture.Synthetic(
			"sess-0001", "uuid-0003", "req-0002", "2026-01-02T03:05:00.000Z", onBranch)},

		{"user-prompt", testfixture.User(
			"sess-0001", "uuid-0004", "2026-01-02T03:03:00.000Z",
			testfixture.Provenance{CWD: posix, Branch: "feature/12-something"})},

		{"pr-link", testfixture.PRLink(
			"sess-0001", "uuid-0005", "2026-01-02T03:06:00.000Z",
			"example-org/example-project", 12, onFeature)},

		// Inside a worktree the assistant made, which folds back to the project.
		{"assistant-in-worktree", testfixture.Assistant(
			"sess-0002", "uuid-0006", "req-0003", "claude-sonnet-5",
			"2026-01-03T10:00:00.000Z",
			testfixture.Provenance{CWD: worktree, Branch: "claude/something-123"},
			testfixture.Usage{Input: 7, Output: 8})},

		// A detached HEAD, which means no branch rather than a branch called
		// HEAD.
		{"assistant-detached-head", testfixture.Assistant(
			"sess-0002", "uuid-0007", "req-0004", "claude-opus-5",
			"2026-01-03T11:00:00.000Z",
			testfixture.Provenance{CWD: project, Branch: "HEAD"},
			testfixture.Usage{Input: 3, Output: 4})},

		{"sidecar-mode", testfixture.Sidecar(
			"sess-0002", "mode", map[string]any{"mode": "default"})},
	}

	var wrote, kept int
	for _, f := range fixtures {
		path := filepath.Join(claudecode, f.name+".jsonl")
		switch _, err := os.Stat(path); {
		case err == nil:
			kept++
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if err := os.WriteFile(path, []byte(f.line+"\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		wrote++
	}

	fmt.Printf("%s: wrote %d fixtures, left %d frozen ones alone\n", claudecode, wrote, kept)
	if wrote > 0 {
		fmt.Println("each new fixture needs a .want.json beside it, written by hand:")
		fmt.Println("a golden produced by the code under test would assert nothing")
	}
	return nil
}
