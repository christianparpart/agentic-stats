// Command agent collects AI coding-assistant transcripts from this machine and
// ships them to an agentic-stats server.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/christianparpart/agentic-stats/internal/agentcfg"
	"github.com/christianparpart/agentic-stats/internal/collector"
	"github.com/christianparpart/agentic-stats/internal/cursor"
	"github.com/christianparpart/agentic-stats/internal/source"
	"github.com/christianparpart/agentic-stats/internal/source/claudecode"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// version identifies the build. Overridden at release time via -ldflags.
var version = "dev"

const usage = `agentic-stats-agent %s

Usage:
  agent enroll --server URL --code CODE   Register this machine
  agent once                              Collect once and exit
  agent run                               Collect continuously

Configuration is read from %s, overridable with
$AGENTIC_STATS_SERVER and $AGENTIC_STATS_TOKEN.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentic-stats-agent: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	defaultPath, err := agentcfg.DefaultPath()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Printf(usage, version, defaultPath)
		return errors.New("no command given")
	}
	switch args[0] {
	case "enroll":
		return runEnroll(args[1:], defaultPath)
	case "once":
		return runCollect(args[1:], defaultPath, collectOnce)
	case "run":
		return runCollect(args[1:], defaultPath, collectForever)
	case "-h", "--help", "help":
		fmt.Printf(usage, version, defaultPath)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// describeDevice reports what this machine is, so the server can label it and
// render times in the machine's own zone.
func describeDevice() wire.Device {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	zone := time.Local.String()
	return wire.Device{
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Timezone:     zone,
		AgentVersion: version,
	}
}

func runEnroll(args []string, defaultPath string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	serverURL := fs.String("server", "", "server base URL")
	code := fs.String("code", "", "one-time enrollment code")
	configPath := fs.String("config", defaultPath, "configuration file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serverURL == "" || *code == "" {
		return errors.New("--server and --code are both required")
	}

	body, err := json.Marshal(map[string]any{"code": *code, "device": describeDevice()})
	if err != nil {
		return fmt.Errorf("build enrollment request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		*serverURL+"/v1/devices/enroll", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build enrollment request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // response already consumed below

	var payload struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode enrollment response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enrollment rejected: %s", payload.Error)
	}

	cfg, err := agentcfg.Load(*configPath)
	if err != nil {
		return err
	}
	cfg.Server.URL = *serverURL
	cfg.Server.Token = payload.Token
	if err := agentcfg.Save(*configPath, cfg); err != nil {
		return err
	}
	fmt.Printf("enrolled; token written to %s\n", *configPath)
	return nil
}

// collectMode is how many passes to make.
type collectMode uint8

const (
	collectOnce collectMode = iota
	collectForever
)

func runCollect(args []string, defaultPath string, mode collectMode) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	configPath := fs.String("config", defaultPath, "configuration file")
	statePath := fs.String("state", "", "cursor database path (default alongside the config)")
	interval := fs.Duration("interval", 0, "override the configured poll interval")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := agentcfg.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Server.URL == "" || cfg.Server.Token == "" {
		return errors.New("not enrolled: run `agent enroll --server URL --code CODE` first")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}

	src, err := claudecode.New(claudecode.Config{
		FS:              source.NewOSFileSystem(),
		Home:            home,
		Platform:        runtime.GOOS,
		ExcludePrefixes: cfg.Privacy.ExcludePaths,
	})
	if err != nil {
		return err
	}

	cursors, err := openCursors(*statePath, *configPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := cursors.Close(); cerr != nil {
			log.Error("close cursor store", "error", cerr)
		}
	}()

	client, err := wire.NewClient(wire.ClientConfig{
		BaseURL: cfg.Server.URL,
		Token:   cfg.Server.Token,
		Device:  describeDevice(),
	})
	if err != nil {
		return err
	}

	c, err := collector.New(collector.Config{
		Sources:   []source.Source{src},
		Cursors:   cursors,
		Uploader:  client,
		BatchSize: cfg.Agent.BatchMaxLines,
		Logger:    log,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mode == collectOnce {
		stats, err := c.CollectOnce(ctx)
		return reportPass(log, stats, err)
	}

	poll := *interval
	if poll <= 0 {
		poll, err = time.ParseDuration(cfg.Agent.PollInterval)
		if err != nil {
			return fmt.Errorf("parse poll_interval: %w", err)
		}
	}
	log.Info("collecting", "interval", poll.String(), "server", cfg.Server.URL, "version", version)

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		stats, passErr := c.CollectOnce(ctx)
		if err := reportPass(log, stats, passErr); err != nil {
			// A failed pass must not kill the daemon: the cursor did not
			// advance, so the next pass retries the same data.
			log.Error("collection pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// openCursors places the cursor database next to the configuration by default.
func openCursors(statePath, configPath string) (cursor.Store, error) {
	path := statePath
	if path == "" {
		path = configPath + ".cursors.db"
	}
	return cursor.OpenBoltStore(cursor.BoltStoreConfig{Path: path})
}

// reportPass logs the outcome of one collection pass.
func reportPass(log *slog.Logger, stats collector.Stats, err error) error {
	if err != nil {
		return err
	}
	log.Info("pass complete",
		"streams", stats.Streams,
		"lines", stats.Lines,
		"batches", stats.Batches,
		"stored", stats.Stored,
		"duplicates", stats.Duplicates,
		"restarted", stats.Restarted,
		"gone", stats.Gone)
	return nil
}
