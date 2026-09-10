//go:build !windows

package main

// attachConsole is a no-op where a program does not have to choose between
// having a console and having a window.
func attachConsole() {}

// hasConsole is true wherever stderr is always a usable stream, which systemd
// and launchd both arrange by redirecting it to a file.
func hasConsole() bool { return true }
