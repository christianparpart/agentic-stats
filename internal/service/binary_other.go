//go:build !windows

package service

// serviceBinary is the identity here: launchd and systemd start a program
// without giving it a terminal, so there is no window to avoid and no need for
// a second build.
func serviceBinary(exe string, _ func(string)) string { return exe }
