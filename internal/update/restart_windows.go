//go:build windows

package update

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"golang.org/x/sys/windows"

	"github.com/christianparpart/agentic-stats/internal/service"
)

// restartDelaySeconds gives this process time to exit before the task is
// started again. schtasks refuses to run a task whose instance is still
// present, and the daemon is still shutting down when this is called.
const restartDelaySeconds = 5

// ArrangeRestart asks Task Scheduler to start this node again after it exits.
//
// Windows registers the daemon as an ONLOGON task, which starts it once and
// never restarts it -- so unlike launchd and systemd, nothing here brings the
// node back on its own. A detached child waits for this process to go and then
// runs the task.
//
// Deliberately not os.StartProcess of the new binary: that instance would live
// outside Task Scheduler, and the next logon would start a second one. Two
// daemons against one archive means two writers on a cursor store that only
// admits a single process.
//
// Running an existing task needs no elevation, unlike creating one, so this
// never prompts.
func ArrangeRestart(log *slog.Logger) error {
	// timeout is used rather than a sleep loop because it is present on every
	// Windows install and needs no shell of its own.
	script := fmt.Sprintf("timeout /t %d /nobreak >nul & schtasks /Run /TN %s",
		restartDelaySeconds, service.Name)

	// context.Background, not the daemon's: this child exists to outlive this
	// process, so propagating cancellation into it would kill the very thing
	// that brings the node back.
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", script)
	// Detached and in its own process group, or it dies with the daemon it is
	// waiting for -- which is the one thing it must outlive.
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("update: arrange a restart: %w", err)
	}
	// Released rather than waited for: waiting is the opposite of the point.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("update: release the restart helper: %w", err)
	}
	log.Info("a restart is scheduled", "in_seconds", restartDelaySeconds, "task", service.Name)
	return nil
}
