// Command agentic-stats is one node of an AI coding-assistant archive mesh.
//
// It collects this machine's transcripts, holds them in a local replica, and
// serves a dashboard over them. There is no server: peers find each other and
// converge.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/christianparpart/agentic-stats/internal/agentcfg"
	"github.com/christianparpart/agentic-stats/internal/api"
	"github.com/christianparpart/agentic-stats/internal/bridge"
	"github.com/christianparpart/agentic-stats/internal/bridge/fsbackend"
	"github.com/christianparpart/agentic-stats/internal/certs"
	"github.com/christianparpart/agentic-stats/internal/collector"
	"github.com/christianparpart/agentic-stats/internal/cursor"
	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/mesh"
	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/resume"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/service"
	"github.com/christianparpart/agentic-stats/internal/source"
	"github.com/christianparpart/agentic-stats/internal/source/claudecode"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/supervise"
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

  agentic-stats install    Start automatically when you log in
  agentic-stats uninstall  Remove the autostart entry
  agentic-stats service    Report whether the autostart entry is running

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
	case "install":
		return runInstall(args[1:], defaultPath)
	case "uninstall":
		return runUninstall(args[1:])
	case "service":
		return runServiceStatus(args[1:])
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
	mesh   *mesh.Mesh
	bridge *bridge.Bridge
	log    *slog.Logger
	closes []func() error
}

// openNode wires the graph. Every dependency is passed in; nothing reaches for
// a global.
func openNode(ctx context.Context, configPath, statePath string, log *slog.Logger) (*node, error) {
	return openNodeWith(ctx, configPath, statePath, log, withCollector)
}

// nodeParts selects how much of the graph to build.
type nodeParts uint8

const (
	// readOnly opens the archive alone. The cursor store is a single-process
	// bbolt file that a running daemon holds, so a read-only command must not
	// reach for it.
	readOnly nodeParts = iota
	withCollector
)

func openNodeWith(ctx context.Context, configPath, statePath string, log *slog.Logger, parts nodeParts) (*node, error) {
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

	if err := n.buildMesh(keys, cfg, log); err != nil {
		return nil, n.closeAll(err)
	}
	if err := n.buildBridge(keys, cfg, log); err != nil {
		return nil, n.closeAll(err)
	}
	if parts == readOnly {
		return n, nil
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

// buildMesh wires the peering subsystem.
func (n *node) buildMesh(keys *seal.Keys, cfg agentcfg.Config, log *slog.Logger) error {
	m, err := mesh.New(mesh.Config{
		Store:             n.db,
		Keys:              keys,
		Logger:            log,
		Listen:            cfg.Mesh.Listen,
		StaticPeers:       cfg.Mesh.Peers,
		Discovery:         cfg.Mesh.Discovery,
		ExcludeInterfaces: cfg.Mesh.ExcludeInterfaces,
	})
	if err != nil {
		return err
	}
	n.mesh = m
	return nil
}

// buildBridge wires the external storage bridge, if one is configured.
func (n *node) buildBridge(keys *seal.Keys, cfg agentcfg.Config, log *slog.Logger) error {
	switch cfg.Bridge.Kind {
	case "":
		return nil
	case "filesystem":
		if cfg.Bridge.Path == "" {
			return errors.New("bridge.path is required for the filesystem backend")
		}
		be, err := fsbackend.New(cfg.Bridge.Path)
		if err != nil {
			return err
		}
		b, err := bridge.New(bridge.Config{
			Backend: be, Store: n.db, Keys: keys, Logger: log,
		})
		if err != nil {
			return err
		}
		n.bridge = b
		return nil
	default:
		return fmt.Errorf("unknown bridge.kind %q (known: filesystem)", cfg.Bridge.Kind)
	}
}

// isLoopback reports whether a listen address is loopback-only.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		// A bare ":8899" listens on every interface.
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// dashboardCertificate resolves the certificate for an off-loopback dashboard,
// generating and persisting a self-signed one when none is configured.
func dashboardCertificate(cfg agentcfg.Config, stateDir string) (tls.Certificate, error) {
	if cfg.Dashboard.TLSCert != "" && cfg.Dashboard.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.Dashboard.TLSCert, cfg.Dashboard.TLSKey)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load dashboard certificate: %w", err)
		}
		return cert, nil
	}
	// Regenerated when this machine's addresses change, not merely on expiry:
	// a stale address list is the failure people actually hit, after a VPN
	// connects or a DHCP lease moves.
	return certs.EnsureCertificate(certs.Config{
		CertPath: filepath.Join(stateDir, "dashboard-cert.pem"),
		KeyPath:  filepath.Join(stateDir, "dashboard-key.pem"),
	})
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
	// Resolve once, before supervision starts. An address that cannot be
	// parsed is operator error and must fail loudly and immediately; a bind
	// that fails later may just be a port not yet released, and is retried.
	if _, rerr := net.ResolveTCPAddr("tcp", addr); rerr != nil {
		return fmt.Errorf("dashboard.listen %q: %w", addr, rerr)
	}

	poll := *interval
	if poll <= 0 {
		if poll, err = time.ParseDuration(n.cfg.Agent.PollInterval); err != nil {
			return fmt.Errorf("parse poll_interval: %w", err)
		}
	}

	// Each subsystem is supervised independently, so a peer that cannot be
	// reached or a dashboard port briefly held by a previous run costs a
	// restart of that one thing. Collection in particular must survive
	// everything else: transcripts it misses are deleted by their owner and
	// never come back.
	subsystems := []supervise.Config{
		{Name: "collector", Run: func(ctx context.Context) error {
			return n.collectLoop(ctx, poll, log)
		}},
		{Name: "dashboard", Run: func(ctx context.Context) error {
			return n.serveDashboard(ctx, addr, *configPath, log)
		}},
	}
	if n.mesh != nil {
		subsystems = append(subsystems, supervise.Config{Name: "peering", Run: n.mesh.Run})
		// Sleeping and being suspended are the normal state of a laptop and a
		// VM. Without this the node waits out an ordinary dial interval, and
		// possibly a fifteen-minute backoff learned on a network it is no
		// longer attached to, before it tries anyone again.
		subsystems = append(subsystems, supervise.Config{Name: "resume-watch", Run: func(ctx context.Context) error {
			resume.Watch(ctx, resume.Config{Logger: log}, n.mesh.Wake)
			return nil
		}})
	}
	if n.bridge != nil {
		subsystems = append(subsystems, supervise.Config{Name: "bridge", Run: func(ctx context.Context) error {
			return n.runBridge(ctx, log)
		}})
	}

	runCtx, stopAll := context.WithCancel(ctx)
	defer stopAll()

	failed := make(chan error, len(subsystems))
	var wg sync.WaitGroup
	for _, s := range subsystems {
		s.Logger = log
		wg.Add(1)
		go func() {
			defer wg.Done()
			if serr := supervise.Run(runCtx, s); serr != nil && !errors.Is(serr, context.Canceled) {
				failed <- serr
			}
		}()
	}

	// The first unrecoverable failure takes the daemon down, but only after
	// every other subsystem has been asked to stop and has done so.
	var fatal error
	select {
	case fatal = <-failed:
	case <-ctx.Done():
		log.Info("stopping")
	}
	stopAll()
	wg.Wait()
	return fatal
}

// collectLoop collects on an interval until ctx is done.
func (n *node) collectLoop(ctx context.Context, poll time.Duration, log *slog.Logger) error {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		func() {
			// One bad transcript must not end collection for the others, and a
			// restart of this loop would re-scan everything to reach the same
			// place. Contained here rather than left to the supervisor.
			defer supervise.Recover(log, "collection pass")
			stats, passErr := n.coll.CollectOnce(ctx)
			if err := reportPass(log, stats, passErr); err != nil {
				// A failed pass must not kill the daemon: the cursor did not
				// advance, so the next pass retries the same data.
				log.Error("collection pass failed", "error", err)
			}
		}()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// serveDashboard serves the dashboard until ctx is done.
//
// The server is built per attempt because an http.Server that has been shut
// down cannot be served again, and this may be restarted.
func (n *node) serveDashboard(ctx context.Context, addr, configPath string, log *slog.Logger) error {
	srv, err := api.NewServer(api.Config{Derive: n.derive, Keys: n.keys, Logger: log})
	if err != nil {
		return supervise.Permanent(err)
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	// Loopback stays plain HTTP on purpose: http://127.0.0.1 is already a
	// secure context in every browser, so TLS there would buy nothing and cost
	// a certificate warning. Anywhere else, encrypt.
	useTLS := !isLoopback(addr)
	if useTLS {
		cert, cerr := dashboardCertificate(n.cfg, filepath.Dir(configPath))
		if cerr != nil {
			// A certificate we cannot build is configuration, not weather.
			return supervise.Permanent(cerr)
		}
		httpServer.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
	}

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Info("dashboard listening",
		"url", scheme+"://"+addr, "origin", n.db.OriginID(), "version", version)
	log.Info(api.Describe())
	if useTLS && n.cfg.Dashboard.TLSCert == "" {
		log.Warn("using a self-signed certificate; your browser will warn once " +
			"unless you supply dashboard.tls_cert")
	}

	served := make(chan error, 1)
	go func() {
		var serr error
		if useTLS {
			serr = httpServer.ListenAndServeTLS("", "")
		} else {
			serr = httpServer.ListenAndServe()
		}
		if errors.Is(serr, http.ErrServerClosed) {
			serr = nil
		}
		served <- serr
	}()

	select {
	case serr := <-served:
		return serr
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
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
	n, err := openNodeWith(ctx, *configPath, *statePath, slog.New(slog.DiscardHandler), readOnly)
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
	peers, err := n.mesh.KnownPeers(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("peers    %d\n", len(peers))
	for _, p := range peers {
		seen := p.LastSeen
		if seen == "" {
			seen = "never"
		}
		fmt.Printf("  %-34s %v  last seen %s\n", p.ID, p.Addrs, seen)
	}

	if quarantined > 0 {
		fmt.Printf("\nWARNING: %d quarantined records.\n", quarantined)
		fmt.Println("Two machines are issuing records under the same origin id — most likely a")
		fmt.Println("cloned VM or a copied database. Their data is being kept, not merged.")
	}
	return nil
}

// runBridge publishes to and fetches from external storage on an interval.
//
// A failure is logged and retried rather than fatal: a bridge folder living on
// a network share or a sync client is expected to be intermittently absent.
func (n *node) runBridge(ctx context.Context, log *slog.Logger) error {
	every := 5 * time.Minute
	if n.cfg.Bridge.Interval != "" {
		if parsed, err := time.ParseDuration(n.cfg.Bridge.Interval); err == nil {
			every = parsed
		} else {
			log.Warn("parse bridge.interval; using the default", "error", err)
		}
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		stats, err := n.bridge.Sync(ctx)
		switch {
		case err != nil:
			log.Warn("bridge sync failed", "error", err)
		case stats.Published > 0 || stats.Stored > 0 || stats.Forked > 0:
			log.Info("bridge sync",
				"published", stats.Published, "fetched", stats.Fetched,
				"stored", stats.Stored, "forked", stats.Forked)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runInstall registers the daemon to start at login.
func runInstall(args []string, defaultPath string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	configPath := fs.String("config", defaultPath, "configuration file the service should use")
	statePath := fs.String("state", "", "archive database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Refuse to install a node that cannot start: a service that fails at
	// every login is worse than no service, because nothing surfaces it.
	cfg, err := agentcfg.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Mesh.PSK == "" {
		return fmt.Errorf("no mesh key configured in %s: run `agentic-stats init` "+
			"(or `join --key <key>`) before installing the service", *configPath)
	}

	mgr, err := service.New()
	if err != nil {
		return err
	}
	st, err := mgr.Install(service.Config{ConfigPath: *configPath, StatePath: *statePath})
	if err != nil {
		return err
	}
	fmt.Printf("installed: %s\n", st.DefinitionPath)
	fmt.Printf("status:    %s\n", st.Detail)
	fmt.Printf("\nIt will start automatically when you log in.\n")
	fmt.Printf("Dashboard: http://%s\n", cfg.Dashboard.Listen)
	return nil
}

// runUninstall removes the autostart entry.
func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	mgr, err := service.New()
	if err != nil {
		return err
	}
	if err := mgr.Uninstall(); err != nil {
		return err
	}
	fmt.Println("removed; the archive and configuration are untouched")
	return nil
}

// runServiceStatus reports what the platform knows about the service.
func runServiceStatus(args []string) error {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	mgr, err := service.New()
	if err != nil {
		return err
	}
	st, err := mgr.Status()
	if err != nil {
		return err
	}
	fmt.Printf("installed  %v\n", st.Installed)
	fmt.Printf("running    %v\n", st.Running)
	fmt.Printf("definition %s\n", st.DefinitionPath)
	if st.Detail != "" {
		fmt.Printf("detail     %s\n", st.Detail)
	}
	if !st.Installed {
		fmt.Println("\nInstall it with: agentic-stats install")
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
