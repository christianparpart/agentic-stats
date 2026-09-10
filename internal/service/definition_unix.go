//go:build darwin || linux

package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFile creates a definition file, making its directory first.
//
// Build-tagged rather than shared, because only launchd and systemd install
// themselves by writing a definition to disk -- Task Scheduler is driven
// through schtasks. Left in the common file it was dead code on Windows, which
// the unused check reports, and a zero-warning policy that only holds on one
// platform is not one.
func writeFile(path string, body []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("service: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, perm); err != nil {
		return fmt.Errorf("service: write %s: %w", path, err)
	}
	return nil
}
