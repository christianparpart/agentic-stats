//go:build windows

package service

import (
	"os"
	"path/filepath"
	"testing"
)

// The service must run the windowless build when one is there, because a
// console binary registered as a logon task puts a window on screen at every
// login -- and the console binary must stay the command line, because a shell
// does not wait for a GUI-subsystem program.
func TestTheServiceRunsTheWindowlessBuildWhenPresent(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "agentic-stats.exe")
	windowless := filepath.Join(dir, "agentic-statsw.exe")
	for _, p := range []string{cli, windowless} {
		if err := os.WriteFile(p, []byte("stub"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	got := serviceBinary(cli, func(string) {
		t.Error("chose the console build even though the windowless one exists")
	})
	if got != windowless {
		t.Errorf("serviceBinary() = %s, want %s", got, windowless)
	}
}

// Someone who ran `go build` by hand has only the console binary. A service
// with a window is much better than an install that refuses, so it falls back
// and says why.
func TestItFallsBackToTheConsoleBuildAndExplains(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "agentic-stats.exe")
	if err := os.WriteFile(cli, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var told string
	got := serviceBinary(cli, func(msg string) { told = msg })
	if got != cli {
		t.Errorf("serviceBinary() = %s, want the console build %s", got, cli)
	}
	if told == "" {
		t.Error("fell back silently; the window at every login needs explaining")
	}
}

// Re-resolving must be stable, or an install run from the windowless binary
// would look for agentic-statswW.exe.
func TestResolvingTheWindowlessBuildIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	windowless := filepath.Join(dir, "agentic-statsw.exe")
	if err := os.WriteFile(windowless, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := serviceBinary(windowless, func(string) {}); got != windowless {
		t.Errorf("serviceBinary() = %s, want it unchanged at %s", got, windowless)
	}
}

func TestWindowlessPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{`C:\bin\agentic-stats.exe`, `C:\bin\agentic-statsw.exe`},
		{`C:\bin\agentic-statsw.exe`, `C:\bin\agentic-statsw.exe`},
		{`C:\Program Files\as\agentic-stats.exe`, `C:\Program Files\as\agentic-statsw.exe`},
		// No extension: still has to produce something distinct, or the
		// fallback check compares a path to itself and never fires.
		{`C:\bin\agentic-stats`, `C:\bin\agentic-statsw`},
	}
	for _, tc := range tests {
		if got := windowlessPath(tc.in); got != tc.want {
			t.Errorf("windowlessPath(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// The names a release actually publishes must resolve to each other, or
// `install` silently registers the console build and puts a window on screen
// at every login -- which is the whole reason the second binary exists.
//
// This pins the release artifact naming to what windowlessPath looks for: the
// "w" goes before the extension, not after the program name.
func TestReleasedArtifactNamesResolveToTheirWindowlessTwin(t *testing.T) {
	tests := []struct{ console, windowless string }{
		{`C:\dl\agentic-stats-windows-amd64.exe`, `C:\dl\agentic-stats-windows-amd64w.exe`},
		{`C:\dl\agentic-stats-windows-arm64.exe`, `C:\dl\agentic-stats-windows-arm64w.exe`},
	}
	for _, tc := range tests {
		if got := windowlessPath(tc.console); got != tc.windowless {
			t.Errorf("windowlessPath(%s) = %s, want %s", tc.console, got, tc.windowless)
		}
		// And idempotent, so running install with the twin does not look for
		// a third binary that will never exist.
		if got := windowlessPath(tc.windowless); got != tc.windowless {
			t.Errorf("windowlessPath(%s) = %s, want it unchanged", tc.windowless, got)
		}
	}
}

// The end-to-end shape: a user downloads both artifacts into one directory and
// runs the console one. serviceBinary must find the twin beside it.
func TestServiceBinaryFindsTheReleasedTwinInADownloadDirectory(t *testing.T) {
	dir := t.TempDir()
	console := filepath.Join(dir, "agentic-stats-windows-amd64.exe")
	twin := filepath.Join(dir, "agentic-stats-windows-amd64w.exe")
	for _, p := range []string{console, twin} {
		if err := os.WriteFile(p, []byte("stub"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	notified := ""
	got := serviceBinary(console, func(m string) { notified = m })
	if got != twin {
		t.Errorf("serviceBinary() = %s, want the windowless twin %s", got, twin)
	}
	if notified != "" {
		t.Errorf("fell back with a warning (%q) despite the twin being present", notified)
	}
}
