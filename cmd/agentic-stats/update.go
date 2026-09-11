package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/christianparpart/agentic-stats/internal/release"
	"github.com/christianparpart/agentic-stats/internal/supervise"
	"github.com/christianparpart/agentic-stats/internal/update"
)

// downloadTimeout bounds fetching one file of a release.
//
// Generous because an artifact is tens of megabytes and some of these machines
// are on domestic uplinks, but bounded because a stalled download must not hold
// the subsystem open until the daemon stops.
const downloadTimeout = 15 * time.Minute

// httpFetcher retrieves release files over HTTPS.
//
// The only outbound request this daemon makes that is not to a peer, which is
// why it is written down here in the wiring rather than buried in the updater:
// the updater states that it fetches, and this is where that becomes network
// traffic.
type httpFetcher struct{ client *http.Client }

func (f httpFetcher) Fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Drained and closed: an unread body keeps the connection out of the
		// pool, and the next attempt pays for a new one.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// buildUpdater wires self-updating, or returns nil when it is switched off.
//
// A nil Updater and a nil error means the node was told not to, which is not a
// failure and must not be logged as one.
func (n *node) buildUpdater(log *slog.Logger) (*update.Updater, *update.BinaryInstaller, error) {
	if !n.cfg.Update.Enabled {
		log.Info("self-update is disabled; this node will stay on its current build")
		return nil, nil, nil
	}

	interval := update.DefaultInterval
	if n.cfg.Update.Interval != "" {
		parsed, err := time.ParseDuration(n.cfg.Update.Interval)
		if err != nil {
			return nil, nil, fmt.Errorf("update.interval: %w", err)
		}
		interval = parsed
	}

	verifier, err := release.Official()
	if err != nil {
		return nil, nil, err
	}
	installer, err := update.NewBinaryInstaller(update.InstallConfig{Logger: log})
	if err != nil {
		return nil, nil, err
	}

	updater, err := update.New(update.Config{
		Verifier:  verifier,
		Fetcher:   httpFetcher{client: &http.Client{Timeout: downloadTimeout}},
		Peers:     n.db,
		Installer: installer,
		Current:   buildVersion,
		Interval:  interval,
		Logger:    log,
	})
	if err != nil {
		return nil, nil, err
	}
	return updater, installer, nil
}

// runUpdate converges this node onto the newest release its peers report.
//
// Turns the updater's "a new build is in place" into the daemon stopping. It is
// marked Permanent so the supervisor takes the process down rather than
// restarting this one subsystem, which would install the same build again on a
// loop.
//
// The reason is logged before any of that begins. A daemon that exits without
// saying why is indistinguishable from one that crashed, and this is the one
// exit that is deliberate.
func runUpdate(ctx context.Context, updater *update.Updater, log *slog.Logger) error {
	err := updater.Run(ctx)
	if !errors.Is(err, update.ErrRestartPending) {
		return err
	}

	log.Info("stopping to run the build that was just installed",
		"reason", "a peer reported a newer release and it is now in place")
	if rerr := update.ArrangeRestart(log); rerr != nil {
		// Worth a warning rather than a failure: the binary is already
		// replaced, so the new build starts whenever this node next does --
		// late rather than lost.
		log.Warn("could not arrange a restart; the new build starts at the next launch",
			"error", rerr)
	}
	return supervise.Permanent(err)
}
