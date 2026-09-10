package derive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The frozen fixture set.
//
// Written once and never edited, which is the point: a refactor of the
// extraction must not quietly change how records already in an archive read. If
// a format grows a case, add a fixture; if this build reads an old fixture
// differently than its golden says, that is a re-derivation bug, and the years
// of history that fixture stands for would be re-read wrongly.
//
// The lines come from internal/testfixture via `go run
// ./internal/testfixture/cmd/fixturegen`, so they are synthetic by
// construction. The goldens beside them are written by hand: one produced by
// the code under test would agree with it whatever it did, and assert nothing.
func TestIdentifyReadsTheFrozenFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "synthetic", "claudecode")
	lines, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}

	for _, path := range lines {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			goldenPath := path[:len(path)-len(".jsonl")] + ".want.json"
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			var want Identity
			if err := json.Unmarshal(golden, &want); err != nil {
				t.Fatalf("parse golden: %v", err)
			}

			got := Identify(raw)
			gotJSON, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			wantJSON, err := json.MarshalIndent(want, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("this build reads the fixture differently:\n got %s\nwant %s",
					gotJSON, wantJSON)
			}
		})
	}
}

// The working directory is on the envelope, not in the usage, so it must be
// read from lines that carry no usage at all -- a pr-link line binds a session
// to a pull request and is the one whose columns nothing else can repair.
func TestIdentifyReadsProvenanceFromEveryLineThatHasIt(t *testing.T) {
	for _, tc := range []struct {
		name, raw, cwd, branch string
	}{
		{
			name:   "an assistant line",
			raw:    `{"type":"assistant","requestId":"r1","sessionId":"s","cwd":"D:\\fastcached","gitBranch":"master","message":{"model":"claude-opus-5","usage":{"input_tokens":1}}}`,
			cwd:    `D:\fastcached`,
			branch: "master",
		},
		{
			name:   "a user line, which carries no usage",
			raw:    `{"type":"user","uuid":"u1","sessionId":"s","cwd":"/home/chris/endo","gitBranch":"feature/1"}`,
			cwd:    "/home/chris/endo",
			branch: "feature/1",
		},
		{
			name:   "a pr-link line",
			raw:    `{"type":"pr-link","sessionId":"s","cwd":"D:\\endo","gitBranch":"master","prRepository":"acme/endo","prNumber":3}`,
			cwd:    `D:\endo`,
			branch: "master",
		},
		{
			name: "an empty branch, which is unknown",
			raw:  `{"type":"user","uuid":"u1","sessionId":"s","cwd":"D:\\endo","gitBranch":""}`,
			cwd:  `D:\endo`,
		},
		{
			// docs/design.md is explicit that this means unknown. Left alone it
			// becomes a category on the chart, merging every detached checkout
			// in every project into one plausible-looking row.
			name: "a detached head, which is not a branch named HEAD",
			raw:  `{"type":"user","uuid":"u1","sessionId":"s","cwd":"D:\\endo","gitBranch":"HEAD"}`,
			cwd:  `D:\endo`,
		},
		{
			name: "a sidecar line with no provenance at all",
			raw:  `{"type":"mode","sessionId":"s","mode":"default"}`,
		},
		{
			name: "whitespace, which is not a directory",
			raw:  `{"type":"user","uuid":"u1","sessionId":"s","cwd":"  ","gitBranch":" "}`,
		},
		{
			name: "a line that is not JSON at all",
			raw:  `not json`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Identify([]byte(tc.raw))
			if got.CWD != tc.cwd {
				t.Errorf("CWD = %q, want %q", got.CWD, tc.cwd)
			}
			if got.GitBranch != tc.branch {
				t.Errorf("GitBranch = %q, want %q", got.GitBranch, tc.branch)
			}
		})
	}
}
