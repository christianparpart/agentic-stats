package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// newManager returns the systemd manager.
func newManager() (Manager, error) { return systemd{}, nil }

type systemd struct{}

// unitPath is where a user unit lives.
func (systemd) unitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("service: locate config directory: %w", err)
	}
	return filepath.Join(dir, "systemd", "user", Name+".service"), nil
}

func (s systemd) Install(cfg Config) (Status, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return Status{}, err
	}
	path, err := s.unitPath()
	if err != nil {
		return Status{}, err
	}
	if err := writeFile(path, unit(cfg), 0o644); err != nil {
		return Status{}, err
	}

	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return Status{}, fmt.Errorf("service: reload systemd: %w", err)
	}
	if err := run("systemctl", "--user", "enable", "--now", Name+".service"); err != nil {
		return Status{}, fmt.Errorf("service: enable unit: %w", err)
	}

	// Without lingering, a user service stops the moment the last session
	// ends -- which is exactly what happens on a headless VM reached over SSH,
	// the case where an always-on collector matters most. Best effort: it
	// needs privileges the user may not have, and the service still works
	// while they are logged in.
	//
	// Saying so matters. Silently swallowing this leaves someone believing
	// they have an always-on collector on the machine least likely to have a
	// session open, and they would only find out from a gap in the archive.
	if err := run("loginctl", "enable-linger"); err != nil {
		cfg.notify("Could not enable lingering, so this service will stop when " +
			"you log out and start again when you log back in. On a machine you " +
			"reach over SSH that means it only collects while you are connected. " +
			"To make it always-on: sudo loginctl enable-linger " + os.Getenv("USER"))
	}

	return s.Status()
}

func (s systemd) Uninstall() error {
	path, err := s.unitPath()
	if err != nil {
		return err
	}
	_ = run("systemctl", "--user", "disable", "--now", Name+".service")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", path, err)
	}
	_ = run("systemctl", "--user", "daemon-reload")
	return nil
}

func (s systemd) Status() (Status, error) {
	path, err := s.unitPath()
	if err != nil {
		return Status{}, err
	}
	st := Status{DefinitionPath: path}
	// A missing definition means "not installed"; anything else -- a
	// permissions problem, say -- is a real failure and must not be reported
	// as absence.
	switch _, statErr := os.Stat(path); {
	case errors.Is(statErr, os.ErrNotExist):
		st.Detail = "no systemd user unit installed"
		return st, nil
	case statErr != nil:
		return Status{}, fmt.Errorf("service: inspect %s: %w", path, statErr)
	}
	st.Installed = true

	active, err := output("systemctl", "--user", "is-active", Name+".service")
	state := strings.TrimSpace(active)
	if err != nil && state == "" {
		st.Detail = "installed; could not query systemd"
		return st, nil
	}
	st.Running = state == "active"
	st.Detail = "systemd reports " + state

	if linger, lerr := output("loginctl", "show-user", "--property=Linger"); lerr == nil {
		if strings.Contains(linger, "Linger=no") {
			st.Detail += "; lingering is off, so it stops when you log out " +
				"(enable with: loginctl enable-linger)"
		}
	}
	return st, nil
}

// unit renders the systemd user unit.
func unit(cfg Config) []byte {
	args := strings.Join(cfg.args(), " ")
	return []byte(fmt.Sprintf(`[Unit]
Description=agentic-stats — AI coding-assistant archive node
Documentation=https://github.com/christianparpart/agentic-stats
After=network-online.target

[Service]
Type=simple
ExecStart=%s %s
Restart=on-failure
RestartSec=30
# The daemon reads one user's transcripts, so it needs no privileges beyond
# their own and is confined accordingly.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-write

[Install]
WantedBy=default.target
`, cfg.Executable, args))
}
