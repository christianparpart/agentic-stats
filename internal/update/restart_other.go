//go:build !windows

package update

import "log/slog"

// ArrangeRestart does nothing here, because the service manager already will.
//
// launchd restarts on any exit at all (KeepAlive in the plist), and systemd on
// a non-zero one (Restart=on-failure in the unit) -- which is what the daemon
// produces when it stops for an update. Spawning anything to help would race
// the supervisor and risk a second instance against the same archive.
func ArrangeRestart(log *slog.Logger) error {
	log.Info("the service manager will start this node again when it exits")
	return nil
}
