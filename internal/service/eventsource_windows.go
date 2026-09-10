//go:build windows

package service

import "github.com/christianparpart/agentic-stats/internal/daemonlog"

// registerEventSource makes the Application log recognise the daemon.
//
// Called from Install, which by then either started elevated or has just been
// re-run elevated -- registering a source writes under HKLM and needs those
// rights. Best effort: without it the daemon still logs, and Event Viewer just
// wraps each message in a complaint about a missing description.
func registerEventSource(cfg Config) {
	if err := daemonlog.RegisterEventSource(); err != nil {
		cfg.notify("Installed, but could not register the Windows event log source, " +
			"so entries in Event Viewer will carry a 'description not found' note. " +
			"The full log is in the logs folder beside your archive.")
	}
}

// unregisterEventSource removes it again, for uninstall.
func unregisterEventSource() {
	// Needs elevation, which uninstall does not ask for. A leftover registry
	// key is inert, so this is not worth a prompt.
	_ = daemonlog.RemoveEventSource()
}
