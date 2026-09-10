//go:build windows

package service

import (
	"os"
	"path/filepath"
	"strings"
)

// WindowlessSuffix marks the binary built for the GUI subsystem.
//
// The same convention as python/pythonw and node/nodew, and for the same
// reason: one program needs to be two things that Windows will not let a single
// executable be.
const WindowlessSuffix = "w"

// serviceBinary picks the executable the service should run.
//
// Windows decides whether a program gets a console from its subsystem, not from
// how it is launched, so a console binary registered as a logon task puts a
// window on screen at every login. Linking the whole program for the GUI
// subsystem removes that, but then a shell does not wait for it, and every
// command-line invocation returns before its own output -- which reads as the
// prompt being overwritten and the terminal hanging.
//
// Neither is acceptable, and no single binary is both. So there are two, built
// from the same source with different link flags: the console one is the
// command line, and the windowless one beside it is what the service runs.
//
// Falling back to the console binary is deliberate. Someone who built by hand
// with `go build` has only that one, and a service with a console window is
// much better than an install that refuses.
func serviceBinary(exe string, notify func(string)) string {
	windowless := windowlessPath(exe)
	if windowless == exe {
		return exe
	}
	if _, err := os.Stat(windowless); err == nil {
		return windowless
	}
	notify("Could not find " + filepath.Base(windowless) + " next to the command, so the " +
		"service will run the console build and show a window at each login. " +
		"`make build` produces both.")
	return exe
}

// windowlessPath inserts WindowlessSuffix before the extension.
func windowlessPath(exe string) string {
	ext := filepath.Ext(exe)
	base := strings.TrimSuffix(exe, ext)
	if strings.HasSuffix(base, WindowlessSuffix) {
		return exe // already the windowless build
	}
	return base + WindowlessSuffix + ext
}
