// Command agentic-stats is one node of an AI coding-assistant archive mesh.
//
// It collects this machine's transcripts, holds them in a local replica, and
// serves a dashboard over them. There is no server: peers find each other and
// converge.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/christianparpart/agentic-stats/internal/agentcfg"
	"github.com/christianparpart/agentic-stats/internal/api"
	"github.com/christianparpart/agentic-stats/internal/collector"
	"github.com/christianparpart/agentic-stats/internal/cursor"
	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/source"
	"github.com/christianparpart/agentic-stats/internal/source/claudecode"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// version identifies the build. Overridden at release time via -ldflags.
var version = "dev"

const usage = `agentic-stats %s

Usage:
  agentic-stats init     Generate a mesh key and write a configuration file
  agentic-stats join     Adopt an existing mesh key
  agentic-stats once     Collect once and exit
  agentic-stats run      Collect continuously and serve the dashboard
  agentic-stats status   Report what this node holds

Configuration lives in %s.
The mesh key may also be supplied as $AGENTIC_STATS_PSK.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentic-stats: %v\n", err)
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
	case "init":
		return runInit(args[1:], defaultPath, "")
	case "join":
		return runJoin(args[1:], defaultPath)
	case "once":
		return runNode(args[1:], defaultPath, modeOnce)
	case "run":
		return runNode(args[1:], defaultPath, modeRun)
	case "status":
		return runStatus(args[1:], defaultPath)
	case "-h", "--help", "help":
		fmt.Printf(usage, version, defaultPath)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// runInit generates a mesh key, or adopts the one supplied.
func runInit(args []string, defaultPath, adopt string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", defaultPath, "configuration file to write")
	force := fs.Bool("force", false, "overwrite an existing mesh key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := agentcfg.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Mesh.PSK != "" && !*force {
		return fmt.Errorf("a mesh key is already configured in %s; "+
			"pass --force to replace it, which will disconnect this node from its mesh", *configPath)
	}

	psk := adopt
	if psk == "" {
		if psk, err = seal.Generate(); err != nil {
			return err
		}
	}
	if _, err := seal.Derive(psk); err != nil {
		return err
	}
	cfg.Mesh.PSK = psk
	if err := agentcfg.Save(*configPath, cfg); err != nil {
		return err
	}

	if adopt == "" {
		fmt.Printf("mesh key generated and written to %s\n\n  %s\n\n", *configPath, psk)
		fmt.Println("This one key admits a machine to the mesh, encrypts every record, and")
		fmt.Println("unlocks the dashboard. Give it to your other machines with:")
		fmt.Println("\n  agentic-stats join --key <key>")
		fmt.Println("\nThere is no recovery if you lose it: the archive is encrypted with it.")
	} else {
		fmt.Printf("joined the mesh; configuration written to %s\n", *configPath)
	}
	return nil
}

// runJoin adopts a mesh key generated elsewhere.
func runJoin(args []string, defaultPath string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	key := fs.String("key", "", "the mesh key from another node")
	configPath := fs.String("config", defaultPath, "configuration file to write")
	force := fs.Bool("force", false, "replace an existing mesh key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" {
		return errors.New("--key is required (run `agentic-stats init` on your first machine)")
	}
	forward := []string{"--config", *configPath}
	if *force {
		forward = append(forward, "--force")
	}
	return runInit(forward, defaultPath, *key)
}

// node bundles everything a running node needs.
type node struct {
	cfg    agentcfg.Config
	keys   *seal.Keys
	db     *store.DB
	derive *derive.Service
	coll   *collector.Collector
	log    *slog.Logger
	closes []func() error
}

// openNode wires the graph. Every dependency is passed in; nothing reaches for
// a global.
func openNode(ctx context.Context, configPath, statePath string, log *slog.Logger) (*node, error) {
	cfg, err := agentcfg.Load(configPath)
	if err != nil {
		return nil, err
	}
	if cfg.Mesh.PSK == "" {
		return nil, errors.New("no mesh key configured: run `agentic-stats init`, " +
			"or `agentic-stats join --key <key>` to join an existing mesh")
	}
	keys, err := seal.Derive(cfg.Mesh.PSK)
	if err != nil {
		return nil, err
	}

	n := &node{cfg: cfg, keys: keys, log: log}

	if statePath == "" {
		statePath = filepath.Join(filepath.Dir(configPath), "archive.db")
	}
	if n.db, err = store.Open(ctx, store.Config{Path: statePath}); err != nil {
		return nil, err
	}
	n.closes = append(n.closes, n.db.Close)

	prices, err := pricing.Load()
	if err != nil {
		return nil, n.closeAll(err)
	}
	if n.derive, err = derive.NewService(n.db, prices); err != nil {
		return nil, n.closeAll(err)
	}

	writer, err := ingest.NewWriter(n.db, keys)
	if err != nil {
		return nil, n.closeAll(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, n.closeAll(fmt.Errorf("locate home directory: %w", err))
	}
	src, err := claudecode.New(claudecode.Config{
		FS:              source.NewOSFileSystem(),
		Home:            home,
		Platform:        runtime.GOOS,
		ExcludePrefixes: cfg.Privacy.ExcludePaths,
	})
	if err != nil {
		return nil, n.closeAll(err)
	}

	cursors, err := cursor.OpenBoltStore(cursor.BoltStoreConfig{
		Path: filepath.Join(filepath.Dir(statePath), "cursors.db"),
	})
	if err != nil {
		return nil, n.closeAll(err)
	}
	n.closes = append(n.closes, cursors.Close)

	if n.coll, err = collector.New(collector.Config{
		Sources:   []source.Source{src},
		Cursors:   cursors,
		Uploader:  writer,
		BatchSize: cfg.Agent.BatchMaxLines,
		Logger:    log,
	}); err != nil {
		return nil, n.closeAll(err)
	}
	return n, nil
}

// closeAll releases what has been opened so far, joining any failure to err.
func (n *node) closeAll(err error) error {
	for i := len(n.closes) - 1; i >= 0; i-- {
		err = errors.Join(err, n.closes[i]())
	}
	n.closes = nil
	return err
}

// Close releases the node's resources.
func (n *node) Close() error { return n.closeAll(nil) }

type mode uint8

const (
	modeOnce mode = iota
	modeRun
)

func runNode(args []string, defaultPath string, m mode) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	configPath := fs.String("config", defaultPath, "configuration file")
	statePath := fs.String("state", "", "archive database path (default alongside the config)")
	interval := fs.Duration("interval", 0, "override the configured poll interval")
	listen := fs.String("listen", "", "override the dashboard address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := openNode(ctx, *configPath, *statePath, log)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := n.Close(); cerr != nil {
			log.Error("close node", "error", cerr)
		}
	}()

	if m == modeOnce {
		stats, err := n.coll.CollectOnce(ctx)
		return reportPass(log, stats, err)
	}

	addr := *listen
	if addr == "" {
		addr = n.cfg.Dashboard.Listen
	}
	srv, err := api.NewServer(api.Config{Derive: n.derive, Keys: n.keys, Logger: log})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	httpErr := make(chan error, 1)
	go func() {
		log.Info("dashboard listening", "addr", addr, "origin", n.db.OriginID(), "version", version)
		log.Info(api.Describe())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	poll := *interval
	if poll <= 0 {
		if poll, err = time.ParseDuration(n.cfg.Agent.PollInterval); err != nil {
			return fmt.Errorf("parse poll_interval: %w", err)
		}
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		stats, passErr := n.coll.CollectOnce(ctx)
		if err := reportPass(log, stats, passErr); err != nil {
			// A failed pass must not kill the daemon: the cursor did not
			// advance, so the next pass retries the same data.
			log.Error("collection pass failed", "error", err)
		}
		select {
		case err := <-httpErr:
			return fmt.Errorf("dashboard: %w", err)
		case <-ctx.Done():
			log.Info("stopping")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return httpServer.Shutdown(shutdownCtx)
		case <-ticker.C:
		}
	}
}

func runStatus(args []string, defaultPath string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("config", defaultPath, "configuration file")
	statePath := fs.String("state", "", "archive database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	n, err := openNode(ctx, *configPath, *statePath, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer func() { _ = n.Close() }() // status is read-only; a close failure changes nothing

	count, err := n.db.Count(ctx)
	if err != nil {
		return err
	}
	vec, err := n.db.Vector(ctx)
	if err != nil {
		return err
	}
	quarantined, err := n.db.Quarantined(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("origin   %s\n", n.db.OriginID())
	fmt.Printf("records  %d\n", count)
	fmt.Printf("origins  %d\n", len(vec))
	for origin, seq := range vec {
		marker := ""
		if origin == n.db.OriginID() {
			marker = "  (this node)"
		}
		fmt.Printf("  %s  up to %d%s\n", origin, seq, marker)
	}
	if quarantined > 0 {
		fmt.Printf("\nWARNING: %d quarantined records.\n", quarantined)
		fmt.Println("Two machines are issuing records under the same origin id — most likely a")
		fmt.Println("cloned VM or a copied database. Their data is being kept, not merged.")
	}
	return nil
}

// reportPass logs the outcome of one collection pass.
func reportPass(log *slog.Logger, stats collector.Stats, err error) error {
	if err != nil {
		return err
	}
	log.Info("pass complete",
		"streams", stats.Streams, "lines", stats.Lines,
		"stored", stats.Stored, "duplicates", stats.Duplicates,
		"restarted", stats.Restarted, "gone", stats.Gone)
	return nil
}
