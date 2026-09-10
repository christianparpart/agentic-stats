package service

import (
	"errors"
	"fmt"
	"strings"
)

// newManager returns the Task Scheduler manager.
func newManager() (Manager, error) { return scheduler{}, nil }

// scheduler installs a logon task.
//
// A Windows *service* proper would need administrator rights and a service
// control handler. A scheduled task triggered at logon needs neither, runs as
// the user whose transcripts are being collected, and is what a per-user
// background agent should be.
type scheduler struct{}

// taskName is how the task appears in Task Scheduler.
const taskName = "agentic-stats"

func (s scheduler) Install(cfg Config) (Status, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return Status{}, err
	}
	// Quote the whole command: paths under Program Files contain spaces, and
	// schtasks takes the command as a single argument.
	command := quote(cfg.Executable)
	for _, a := range cfg.args() {
		command += " " + quote(a)
	}

	// /F replaces an existing task, so install is repeatable.
	err = run("schtasks", "/Create", "/F",
		"/SC", "ONLOGON",
		"/TN", taskName,
		"/TR", command,
		"/RL", "LIMITED")
	switch {
	case err == nil:
	case isElevated():
		// Already administrator, so rights are not what is wrong.
		return Status{}, fmt.Errorf("service: create scheduled task: %w", err)
	default:
		// Almost certainly the root folder's permissions. Retrying elevated
		// costs one dialog; guessing from the error text would not survive a
		// non-English Windows, and schtasks reports too many things as exit
		// status 1 to discriminate on the code.
		if eerr := elevateInstall(cfg); eerr != nil {
			return Status{}, errors.Join(
				fmt.Errorf("service: create scheduled task: %w", err), eerr)
		}
		// The elevated child ran the whole install, including starting it.
		return s.Status()
	}
	if err := run("schtasks", "/Run", "/TN", taskName); err != nil {
		// The task exists and will start at the next logon even if starting
		// it now failed.
		return s.Status()
	}
	return s.Status()
}

func (s scheduler) Uninstall() error {
	// Deleting a task that is not there is not a failure worth reporting.
	_ = run("schtasks", "/End", "/TN", taskName)
	_ = run("schtasks", "/Delete", "/F", "/TN", taskName)
	return nil
}

// presence says whether the scheduled task exists.
//
// A named type rather than an error, because "there is no task" is a state to
// report and not a failure to report state -- schtasks simply exits non-zero
// for it. Modelling it as an error meant Status returned a nil error on an
// error path, which reads as a swallowed failure to both a linter and a person.
type presence uint8

const (
	// taskAbsent is the zero value: nothing installed.
	taskAbsent presence = iota
	taskPresent
)

// queryTask returns the task definition, or reports that there is none.
func queryTask() (string, presence) {
	out, err := output("schtasks", "/Query", "/TN", taskName, "/FO", "LIST")
	if err != nil {
		return "", taskAbsent
	}
	return out, taskPresent
}

func (s scheduler) Status() (Status, error) {
	st := Status{DefinitionPath: `Task Scheduler\` + taskName}
	out, found := queryTask()
	if found == taskAbsent {
		st.Detail = "no scheduled task installed"
		return st, nil
	}
	st.Installed = true
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Status:") {
			continue
		}
		state := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Status:"))
		st.Running = strings.EqualFold(state, "Running")
		st.Detail = "Task Scheduler reports " + state
		return st, nil
	}
	st.Detail = "installed"
	return st, nil
}

// quote wraps a value for schtasks, which takes one command string.
func quote(s string) string {
	if !strings.ContainsAny(s, ` "`) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
