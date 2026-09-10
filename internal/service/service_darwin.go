package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// newManager returns the launchd manager.
func newManager() (Manager, error) { return launchd{}, nil }

type launchd struct{}

// plistPath is where a per-user agent lives.
func (launchd) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

func (l launchd) Install(cfg Config) (Status, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return Status{}, err
	}
	path, err := l.plistPath()
	if err != nil {
		return Status{}, err
	}
	logDir := cfg.LogDir
	if logDir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return Status{}, fmt.Errorf("service: locate home directory: %w", herr)
		}
		logDir = filepath.Join(home, "Library", "Logs", Name)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return Status{}, fmt.Errorf("service: create %s: %w", logDir, err)
	}

	body, err := plist(cfg, logDir)
	if err != nil {
		return Status{}, err
	}
	// Remove any existing registration first, or launchd keeps the old
	// definition and the new plist appears to do nothing.
	_ = run("launchctl", "bootout", domainTarget())
	if err := writeFile(path, body, 0o644); err != nil {
		return Status{}, err
	}

	// bootstrap/kickstart rather than the legacy load. `launchctl load` still
	// accepts the plist on current macOS but leaves the job registered and
	// never started -- launchd reports runs = 0 and "never exited", with no
	// error anywhere to explain it.
	if err := run("launchctl", "bootstrap", domain(), path); err != nil {
		// Older systems have no bootstrap verb; fall back rather than fail.
		if lerr := run("launchctl", "load", path); lerr != nil {
			return Status{}, fmt.Errorf("service: register agent: %w", err)
		}
	}
	if err := run("launchctl", "kickstart", "-k", domainTarget()); err != nil {
		// RunAtLoad still starts it at the next login even if starting it
		// right now failed. Worth saying, though: the difference between
		// "installed and collecting" and "installed, collecting after you next
		// log in" is a gap in the archive that nothing else would explain.
		cfg.notify("Installed, but it could not be started right now. " +
			"It will start when you next log in, or run: launchctl kickstart -k " +
			domainTarget())
		return l.Status()
	}
	return l.Status()
}

func (l launchd) Uninstall() error {
	path, err := l.plistPath()
	if err != nil {
		return err
	}
	_ = run("launchctl", "bootout", domainTarget())
	_ = run("launchctl", "unload", path)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", path, err)
	}
	return nil
}

func (l launchd) Status() (Status, error) {
	path, err := l.plistPath()
	if err != nil {
		return Status{}, err
	}
	st := Status{DefinitionPath: path}
	// A missing definition means "not installed"; anything else -- a
	// permissions problem, say -- is a real failure and must not be reported
	// as absence.
	switch _, statErr := os.Stat(path); {
	case errors.Is(statErr, os.ErrNotExist):
		st.Detail = "no launch agent installed"
		return st, nil
	case statErr != nil:
		return Status{}, fmt.Errorf("service: inspect %s: %w", path, statErr)
	}
	st.Installed = true
	st.Running, st.Detail = launchctlState()
	return st, nil
}

// domain is the per-user launchd domain.
func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// domainTarget names this service inside that domain.
func domainTarget() string { return domain() + "/" + Label }

// launchctlState asks launchd what it is doing with our label.
//
// It answers in words rather than an error: the agent is installed either way,
// and "installed but I could not ask launchctl" is a useful thing to print,
// not a failure of the status command.
func launchctlState() (running bool, detail string) {
	out, err := output("launchctl", "print", domainTarget())
	if err != nil {
		return false, "installed but not registered with launchd " +
			"(run `agentic-stats install` again)"
	}
	// Take the first occurrence of each field. launchctl prints the job's own
	// properties first and then nested sub-structures that reuse the same
	// names -- reading the last "state" picks up a child's and reports a
	// running daemon as stopped.
	var state, pid string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "=" {
			continue
		}
		// Join the remainder: launchd reports multi-word states such as
		// "not running", and taking one field turns that into "not".
		value := strings.Join(fields[2:], " ")
		switch {
		case fields[0] == "state" && state == "":
			state = value
		case fields[0] == "pid" && pid == "":
			pid = value
		}
	}
	switch {
	case state == "running" && pid != "":
		return true, "running as pid " + pid
	case state == "running":
		return true, "running"
	case state != "":
		return false, "registered with launchd, " + state
	default:
		return false, "registered with launchd"
	}
}

// plist renders the launch agent definition.
//
// Modelled on the conventions already in use on this machine: restart on
// crash, a throttle so a crash loop does not spin, and logs under
// ~/Library/Logs. ProcessType is Background rather than Interactive, so macOS
// is free to deprioritise a collector that is not user-facing.
func plist(cfg Config, logDir string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" ` +
		`"http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	write := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	write("    <key>Label</key>\n    <string>%s</string>\n", escape(Label))
	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	write("        <string>%s</string>\n", escape(cfg.Executable))
	for _, a := range cfg.args() {
		write("        <string>%s</string>\n", escape(a))
	}
	b.WriteString("    </array>\n")
	b.WriteString("    <key>RunAtLoad</key>\n    <true/>\n")
	// Plain KeepAlive, not a dict keyed on Crashed. The dict form marks the
	// job on-demand, and launchd then declines to keep a collector running --
	// which is the entire point of installing it.
	b.WriteString("    <key>KeepAlive</key>\n    <true/>\n")
	b.WriteString("    <key>ProcessType</key>\n    <string>Background</string>\n")
	b.WriteString("    <key>ThrottleInterval</key>\n    <integer>30</integer>\n")
	b.WriteString("    <key>ExitTimeOut</key>\n    <integer>30</integer>\n")
	write("    <key>StandardOutPath</key>\n    <string>%s</string>\n",
		escape(filepath.Join(logDir, Name+".out.log")))
	write("    <key>StandardErrorPath</key>\n    <string>%s</string>\n",
		escape(filepath.Join(logDir, Name+".err.log")))
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

// escape makes a value safe inside an XML element.
func escape(s string) string {
	var out strings.Builder
	if err := xml.EscapeText(&out, []byte(s)); err != nil {
		// EscapeText only fails if the writer does, and a strings.Builder
		// never does.
		return s
	}
	return out.String()
}
