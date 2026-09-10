//go:build windows

package main

import "github.com/christianparpart/agentic-stats/internal/daemonlog"

// openEventLog connects to the Windows Application log.
//
// Only when there is no console: a person running the daemon in a terminal is
// watching it, and copying every line into Event Viewer as well would be noise
// in the place an operator goes to find real problems.
func openEventLog(hasConsole bool) daemonlog.EventSink {
	if hasConsole {
		return nil
	}
	sink, err := daemonlog.OpenEventLog()
	if err != nil {
		// The file log still works, and it is the more useful of the two.
		return nil
	}
	return sink
}
