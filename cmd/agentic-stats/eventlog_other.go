//go:build !windows

package main

import "github.com/christianparpart/agentic-stats/internal/daemonlog"

// openEventLog has nothing to connect to: systemd and launchd capture the
// service's own output, so the file log is the whole story here.
func openEventLog(bool) daemonlog.EventSink { return nil }
