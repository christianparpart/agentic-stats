//go:build windows

package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// elevationWait bounds how long we wait for the elevated helper to finish.
//
// Generous, because most of it is a person reading a dialog. It is a bound
// against a helper that died without registering anything, not against a slow
// reader.
const elevationWait = 2 * time.Minute

// elevationPoll is how often the task is checked for while waiting.
const elevationPoll = 500 * time.Millisecond

// isElevated reports whether this process may write the Task Scheduler root.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// elevateInstall re-runs this command elevated and waits for the task to exist.
//
// Registering a logon task writes to the root Task Scheduler folder, which is
// administrator-only. That is a property of the folder, not of the daemon: the
// task is created with a limited run level, so what eventually runs is still
// unprivileged. Only the registration needs the rights, which is exactly the
// shape of thing UAC exists for -- so ask, once, rather than making someone
// open a second prompt and retype the command.
//
// The child is elevated and so takes the other branch, which is what stops
// this recursing.
func elevateInstall(cfg Config) error {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return fmt.Errorf("service: encode verb: %w", err)
	}
	exe, err := windows.UTF16PtrFromString(cfg.Executable)
	if err != nil {
		return fmt.Errorf("service: encode executable: %w", err)
	}
	args, err := windows.UTF16PtrFromString(installArgs(cfg))
	if err != nil {
		return fmt.Errorf("service: encode arguments: %w", err)
	}

	cfg.notify("Installing the logon task needs administrator rights once. " +
		"Windows will ask you to confirm.")

	// SW_HIDE: the child does one thing and exits, and a console window
	// flashing up would only be noise.
	if err := windows.ShellExecute(0, verb, exe, args, nil, windows.SW_HIDE); err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return errors.New("service: the administrator prompt was declined, " +
				"so nothing was installed")
		}
		return fmt.Errorf("service: request elevation: %w", err)
	}

	// ShellExecute does not wait, and gives no exit code. Waiting for the task
	// to appear is a better test anyway: it checks the thing that was actually
	// wanted rather than a number the helper returned.
	deadline := time.Now().Add(elevationWait)
	for time.Now().Before(deadline) {
		time.Sleep(elevationPoll)
		if _, found := queryTask(); found == taskPresent {
			return nil
		}
	}
	return errors.New("service: the elevated helper did not register the task in time")
}

// installArgs renders the install command for the elevated child.
//
// It must match the flags runInstall accepts, since the child re-enters the
// same command.
func installArgs(cfg Config) string {
	args := []string{"install"}
	if cfg.ConfigPath != "" {
		args = append(args, "--config", cfg.ConfigPath)
	}
	if cfg.StatePath != "" {
		args = append(args, "--state", cfg.StatePath)
	}
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, quote(a))
	}
	return strings.Join(quoted, " ")
}
