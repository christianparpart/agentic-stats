// Command server ingests collected transcripts and serves the analytics API.
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
	"strings"
	"syscall"
	"time"

	"github.com/christianparpart/agentic-stats/internal/api"
	"github.com/christianparpart/agentic-stats/internal/auth"
	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// version identifies the build. Overridden at release time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentic-stats-server: %v\n", err)
		os.Exit(1)
	}
}

const usage = `agentic-stats-server %s

Usage:
  server serve            Run the ingest and analytics API
  server create-user      Register a user
  server enroll-code      Mint a one-time device enrollment code

The database is taken from --database-url or $AGENTIC_STATS_DATABASE_URL.
`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Printf(usage, version)
		return errors.New("no command given")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "create-user":
		return runCreateUser(args[1:])
	case "enroll-code":
		return runEnrollCode(args[1:])
	case "-h", "--help", "help":
		fmt.Printf(usage, version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// databaseURL resolves the connection string from a flag or the environment.
func databaseURL(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("AGENTIC_STATS_DATABASE_URL"); env != "" {
		return env, nil
	}
	return "", errors.New("no database configured: pass --database-url or set AGENTIC_STATS_DATABASE_URL")
}

// openDB connects and brings the schema up to date.
func openDB(ctx context.Context, url string) (*store.DB, error) {
	db, err := store.Open(ctx, store.Config{URL: url})
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	dbURL := fs.String("database-url", "", "Postgres connection string")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url, err := databaseURL(*dbURL)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()

	server, err := buildAPI(db, log)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// Generous: an ingest body can be a large gzipped backlog.
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 2 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "version", version)
		log.Info(api.Describe())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// buildAPI wires the service graph. Every dependency is passed in; nothing
// reaches for a global (AGENT.md: dependency injection).
func buildAPI(db *store.DB, log *slog.Logger) (*api.Server, error) {
	prices, err := pricing.Load()
	if err != nil {
		return nil, err
	}
	authSvc, err := auth.NewService(db)
	if err != nil {
		return nil, err
	}
	ingestSvc, err := ingest.NewService(db)
	if err != nil {
		return nil, err
	}
	deriveSvc, err := derive.NewService(db, prices)
	if err != nil {
		return nil, err
	}
	return api.NewServer(api.Config{
		Auth:   authSvc,
		Ingest: ingestSvc,
		Derive: deriveSvc,
		Logger: log,
	})
}

func runCreateUser(args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := fs.String("email", "", "email address")
	password := fs.String("password", "", "password (or $AGENTIC_STATS_PASSWORD)")
	admin := fs.Bool("admin", false, "grant the admin role")
	dbURL := fs.String("database-url", "", "Postgres connection string")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("--email is required")
	}
	pass := *password
	if pass == "" {
		pass = os.Getenv("AGENTIC_STATS_PASSWORD")
	}
	if pass == "" {
		return errors.New("a password is required: pass --password or set AGENTIC_STATS_PASSWORD")
	}

	url, err := databaseURL(*dbURL)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := openDB(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()

	authSvc, err := auth.NewService(db)
	if err != nil {
		return err
	}
	role := auth.RoleUser
	if *admin {
		role = auth.RoleAdmin
	}
	id, err := authSvc.CreateUser(ctx, strings.TrimSpace(*email), pass, role)
	if err != nil {
		return err
	}
	fmt.Printf("created user %s (%s) with role %s\n", *email, id, role)
	return nil
}

func runEnrollCode(args []string) error {
	fs := flag.NewFlagSet("enroll-code", flag.ExitOnError)
	email := fs.String("email", "", "user to enroll a device for")
	ttl := fs.Duration("ttl", time.Hour, "how long the code stays valid")
	dbURL := fs.String("database-url", "", "Postgres connection string")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("--email is required")
	}
	url, err := databaseURL(*dbURL)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := openDB(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()

	userID, err := lookupUserID(ctx, db, *email)
	if err != nil {
		return err
	}
	authSvc, err := auth.NewService(db)
	if err != nil {
		return err
	}
	code, err := authSvc.CreateEnrollmentCode(ctx, userID, *ttl)
	if err != nil {
		return err
	}
	fmt.Printf("enrollment code (valid for %s):\n%s\n", *ttl, code)
	return nil
}

// lookupUserID resolves an email to a user id. This runs before any tenant is
// known, which is why it uses the auth transaction.
func lookupUserID(ctx context.Context, db *store.DB, email string) (string, error) {
	var id string
	err := db.InAuthTx(ctx, func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email = $1`, email).Scan(&id)
	})
	if err != nil {
		return "", fmt.Errorf("look up user %s: %w", email, err)
	}
	return id, nil
}
