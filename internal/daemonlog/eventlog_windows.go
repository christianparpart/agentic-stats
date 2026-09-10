//go:build windows

package daemonlog

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

// Source is the name this daemon logs under in the Windows Application log.
const Source = "agentic-stats"

// Event ids, so Event Viewer can filter by severity rather than by reading.
//
// Small integers with no meaning beyond these three: this is not an API, and
// inventing a numbered catalogue of every message would be a maintenance burden
// nobody would keep accurate.
const (
	eventInfo    = 1
	eventWarning = 2
	eventError   = 3
)

// windowsEvents writes to the Windows Application event log.
type windowsEvents struct{ log *eventlog.Log }

// OpenEventLog connects to the Application log as Source.
//
// The source should already have been registered by `install`, which needs
// administrator rights to do it. Opening an unregistered source still works --
// Event Viewer then shows the message wrapped in a complaint about a missing
// description, which is ugly but strictly better than logging nowhere.
func OpenEventLog() (EventSink, error) {
	l, err := eventlog.Open(Source)
	if err != nil {
		return nil, fmt.Errorf("daemonlog: open event log: %w", err)
	}
	return windowsEvents{log: l}, nil
}

func (w windowsEvents) Info(msg string) error    { return w.log.Info(eventInfo, msg) }
func (w windowsEvents) Warning(msg string) error { return w.log.Warning(eventWarning, msg) }
func (w windowsEvents) Error(msg string) error   { return w.log.Error(eventError, msg) }
func (w windowsEvents) Close() error             { return w.log.Close() }

// RegisterEventSource makes the Application log recognise Source.
//
// Writes under HKLM, so it needs administrator rights -- which is why it is
// called from the elevated half of `install` rather than from the daemon. An
// already-registered source is success, so installing twice is not an error.
func RegisterEventSource() error {
	// Checked rather than inferred from the error. InstallAsEventCreate reports
	// an existing registration as a plain errors.New whose text is the only
	// discriminator, and a re-install must stay idempotent -- reading the key
	// says the same thing without matching on a message.
	if eventSourceRegistered() {
		return nil
	}
	err := eventlog.InstallAsEventCreate(Source,
		eventlog.Info|eventlog.Warning|eventlog.Error)
	if err == nil {
		return nil
	}
	// Something registered it between the check and the create. That is the
	// state we wanted, however it got there.
	if eventSourceRegistered() {
		return nil
	}
	return fmt.Errorf("daemonlog: register event source: %w", err)
}

// sourceKey is where the Application log records its known sources.
const sourceKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + Source

// eventSourceRegistered reports whether the Application log knows Source.
func eventSourceRegistered() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, sourceKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = k.Close() // read-only probe
	return true
}

// RemoveEventSource unregisters Source, for uninstall. Also needs elevation.
func RemoveEventSource() error {
	if err := eventlog.Remove(Source); err != nil {
		return fmt.Errorf("daemonlog: remove event source: %w", err)
	}
	return nil
}
