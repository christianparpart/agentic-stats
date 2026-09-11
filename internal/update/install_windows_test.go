//go:build windows

package update_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/update"
)

// Windows will not let a running image be deleted, so a replacement renames it
// aside and sweeps it at the next start. This covers that bookkeeping.
//
// It deliberately does not hold the file open to simulate "running". A mapped
// executable image and an ordinary open handle are not the same condition: an
// image permits rename and refuses delete, while a handle opened without
// FILE_SHARE_DELETE refuses both. Faking one with the other asserts a stricter
// rule than Windows actually applies, and would fail against a replacement
// that works perfectly on a live process.
//
// The live case has its own evidence: the Go linker does exactly this, and
// building over this project's own running service leaves an "exe~" beside it
// rather than failing.
func TestSweepRemovesAnImageLeftByAnEarlierUpdate(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentic-stats.exe")
	for _, p := range []string{exe, filepath.Join(dir, "agentic-statsw.exe")} {
		if err := os.WriteFile(p, []byte("current"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		// What a previous update could not delete while it was mapped.
		if err := os.WriteFile(p+".old", []byte("previous"), 0o755); err != nil {
			t.Fatalf("write %s.old: %v", p, err)
		}
	}

	inst, err := update.NewBinaryInstaller(update.InstallConfig{Executable: exe})
	if err != nil {
		t.Fatalf("NewBinaryInstaller: %v", err)
	}
	inst.SweepStale()

	for _, p := range []string{exe, filepath.Join(dir, "agentic-statsw.exe")} {
		if _, serr := os.Stat(p + ".old"); serr == nil {
			t.Errorf("%s.old survived a sweep", p)
		}
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("the sweep removed the live binary %s: %v", p, serr)
		}
	}
}

// Sweeping when there is nothing to sweep must be silent and harmless: it runs
// at every start, and most starts follow no update at all.
func TestSweepIsHarmlessWithNothingToRemove(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentic-stats.exe")
	if err := os.WriteFile(exe, []byte("current"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	inst, err := update.NewBinaryInstaller(update.InstallConfig{Executable: exe})
	if err != nil {
		t.Fatalf("NewBinaryInstaller: %v", err)
	}
	inst.SweepStale()

	if _, err := os.Stat(exe); err != nil {
		t.Errorf("the binary went missing: %v", err)
	}
}
