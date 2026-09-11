//go:build !windows

package service

// serviceBinary is the identity here: launchd and systemd start a program
// without giving it a terminal, so there is no window to avoid and no need for
// a second build.
func serviceBinary(exe string, _ func(string)) string { return exe }

// WindowlessPath is the identity here: there is only ever one binary.
func WindowlessPath(exe string) string { return exe }

// ConsolePath is the identity here, for the same reason.
func ConsolePath(exe string) string { return exe }
