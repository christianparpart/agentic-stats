package service

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// commandTimeout bounds a platform tool invocation.
//
// launchctl, systemctl and schtasks are all capable of blocking indefinitely
// when the thing they manage is wedged, and an install command that hangs
// forever is worse than one that fails.
const commandTimeout = 30 * time.Second

// run executes a platform command, discarding its output.
func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// output executes a platform command and returns its standard output.
func output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
