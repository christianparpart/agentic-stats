//go:build !windows

package agentcfg_test

import (
	"os"
	"testing"
)

// assertOwnerOnly checks that nobody but the owner can read the file.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %#o, want 0600: it holds the mesh key", perm)
	}
}
