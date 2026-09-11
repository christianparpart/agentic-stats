package update_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/update"
)

// stub writes a file standing in for a binary and returns its path.
func stub(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestInstallReplacesTheRunningBinary(t *testing.T) {
	dir := t.TempDir()
	exe := stub(t, dir, "agentic-stats"+exeSuffix(), "the old build")
	staged := stub(t, t.TempDir(), "downloaded", "the new build")

	// On Windows the service runs the twin, so both must exist and both are
	// replaced; elsewhere there is only the one.
	names := []string{update.ArtifactName(runtime.GOOS, runtime.GOARCH)}
	files := map[string]string{names[0]: staged}
	if runtime.GOOS == "windows" {
		twin := stub(t, dir, "agentic-statsw.exe", "the old windowless build")
		stagedTwin := stub(t, t.TempDir(), "downloaded-w", "the new windowless build")
		files[strings.TrimSuffix(names[0], ".exe")+"w.exe"] = stagedTwin
		t.Cleanup(func() { _ = os.Remove(twin) })
	}

	inst, err := update.NewBinaryInstaller(update.InstallConfig{Executable: exe})
	if err != nil {
		t.Fatalf("NewBinaryInstaller: %v", err)
	}
	if err := inst.Install(context.Background(), files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read replaced binary: %v", err)
	}
	if string(got) != "the new build" {
		t.Errorf("binary contains %q, want the new build", got)
	}

	if runtime.GOOS == "windows" {
		twin, rerr := os.ReadFile(filepath.Join(dir, "agentic-statsw.exe"))
		if rerr != nil {
			t.Fatalf("read replaced twin: %v", rerr)
		}
		if string(twin) != "the new windowless build" {
			t.Errorf("twin contains %q, want the new windowless build", twin)
		}
	}
}

// A binary that is not executable leaves the node unable to start at all, and
// a download arrives with whatever the temp directory's umask gave it.
func TestInstallLeavesTheBinaryExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not how Windows decides what may run")
	}
	dir := t.TempDir()
	exe := stub(t, dir, "agentic-stats", "old")

	staged := filepath.Join(t.TempDir(), "downloaded")
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	inst, err := update.NewBinaryInstaller(update.InstallConfig{Executable: exe})
	if err != nil {
		t.Fatalf("NewBinaryInstaller: %v", err)
	}
	name := update.ArtifactName(runtime.GOOS, runtime.GOARCH)
	if err := inst.Install(context.Background(), map[string]string{name: staged}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	info, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the owner execute bit set", info.Mode().Perm())
	}
}

// Nothing is replaced unless every artifact the platform needs is present, or
// the command line and the service end up on different builds.
func TestInstallRefusesAnIncompleteRelease(t *testing.T) {
	dir := t.TempDir()
	exe := stub(t, dir, "agentic-stats"+exeSuffix(), "the old build")

	inst, err := update.NewBinaryInstaller(update.InstallConfig{Executable: exe})
	if err != nil {
		t.Fatalf("NewBinaryInstaller: %v", err)
	}
	if err := inst.Install(context.Background(), map[string]string{}); err == nil {
		t.Fatal("Install accepted a release with nothing in it")
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "the old build" {
		t.Errorf("binary was changed to %q despite the install failing", got)
	}
}

// The systemd case: a binary the daemon is not allowed to write. It must fail
// naming the directory rather than retrying forever or half-replacing.
func TestInstallFailsWhenTheDirectoryIsNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions are not enforced the same way here")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere, so there is nothing to refuse")
	}
	dir := t.TempDir()
	exe := stub(t, dir, "agentic-stats", "old")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	staged := stub(t, t.TempDir(), "downloaded", "new")
	inst, err := update.NewBinaryInstaller(update.InstallConfig{Executable: exe})
	if err != nil {
		t.Fatalf("NewBinaryInstaller: %v", err)
	}
	name := update.ArtifactName(runtime.GOOS, runtime.GOARCH)
	err = inst.Install(context.Background(), map[string]string{name: staged})
	if err == nil {
		t.Fatal("Install succeeded in a directory it cannot write")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %v, want it to name the directory", err)
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
