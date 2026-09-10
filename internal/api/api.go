// Package api is the dashboard and read API each node serves for itself.
//
// There are no accounts. The pre-shared key that admits a node to the mesh is
// also the dashboard password, so anyone who can read the archive over the
// network already holds the key that decrypts it.
package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/mesh"
	"github.com/christianparpart/agentic-stats/internal/seal"
)

// maxLoginBody bounds the login request, which is small by construction.
const maxLoginBody = 64 << 10

// sessionCookie is the dashboard's session cookie name.
const sessionCookie = "agentic_session"

// sessionTTL is how long a dashboard login lasts. Sessions live in memory, so
// a daemon restart signs you out — acceptable for a local dashboard, and it
// means no session state to replicate between peers.
const sessionTTL = 12 * time.Hour

// loginDelay is applied to every failed login, so guessing the PSK over the
// network is rate-limited by wall clock rather than by CPU.
const loginDelay = 500 * time.Millisecond

// Config is everything the API needs.
type Config struct {
	Derive *derive.Service
	Keys   *seal.Keys
	Logger *slog.Logger
	// Now supplies time. Zero uses the system clock.
	Now func() time.Time
	// Health reports convergence with peers. Zero disables /v1/health, which
	// is correct for a node with no mesh configured.
	//
	// A function rather than the Mesh itself: the dashboard needs one report
	// out of peering and has no business holding the thing that does it.
	Health func(context.Context) (mesh.Health, error)
}

// Server routes and handles HTTP requests.
type Server struct {
	derive *derive.Service
	keys   *seal.Keys
	log    *slog.Logger
	now    func() time.Time
	health func(context.Context) (mesh.Health, error)

	mu       sync.Mutex
	sessions map[string]time.Time
}

// NewServer returns a Server wired to the supplied services.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Derive == nil {
		return nil, errors.New("api: a derive service is required")
	}
	if cfg.Keys == nil {
		return nil, errors.New("api: keys are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Server{
		derive:   cfg.Derive,
		keys:     cfg.Keys,
		log:      log,
		now:      now,
		health:   cfg.Health,
		sessions: make(map[string]time.Time),
	}, nil
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /healthz", s.handleLiveness)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("POST /v1/logout", s.handleLogout)
	// Behind authentication on purpose. /healthz stays a bare liveness check
	// because a node bound off loopback answers it to anyone; this reports peer
	// identities, addresses and how far behind each one is, which is a map of
	// the fleet and not something to hand out.
	mux.HandleFunc("GET /v1/health", s.authenticated(s.handleHealth))
	mux.HandleFunc("GET /v1/summary", s.authenticated(s.handleSummary))
	mux.HandleFunc("GET /v1/daily", s.authenticated(s.handleDaily))
	mux.HandleFunc("GET /v1/delivery", s.authenticated(s.handleDelivery))
	return mux
}

// authenticated gates a handler on a valid dashboard session.
func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" || !s.validSession(c.Value) {
			s.fail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

// validSession reports whether a session token is live, expiring it if not.
func (s *Server) validSession(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}
	if s.now().After(expiry) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// handleLiveness answers "is this process up" and nothing more.
//
// Deliberately uninformative: a node bound off loopback answers this to
// anyone, so it must not leak whether peering works or who the peers are.
func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

// loginRequest carries the pre-shared key.
type loginRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "malformed request")
		return
	}

	// Derive the offered secret and compare the resulting key rather than the
	// text, so the comparison is fixed-width and constant-time regardless of
	// how long a guess is.
	offered, err := seal.Derive(req.Password)
	if err != nil || !seal.Equal(offered.DashboardKey(), s.keys.DashboardKey()) {
		time.Sleep(loginDelay)
		s.fail(w, http.StatusUnauthorized, "invalid key")
		return
	}

	token, err := newSessionToken()
	if err != nil {
		s.log.Error("mint session", "error", err)
		s.fail(w, http.StatusInternalServerError, "login failed")
		return
	}
	s.mu.Lock()
	s.sessions[token] = s.now().Add(sessionTTL)
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(r),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r), MaxAge: -1,
	})
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

// queryFailed reports a failed dashboard query, telling a genuine fault apart
// from a caller that hung up.
//
// A caller that goes away cancels the request context, which interrupts the
// statement in flight. The driver surfaces that as a bare INTERRUPT code: it
// neither wraps nor unwraps to context.Canceled, so errors.Is against that
// would never match and the error alone cannot be discriminated. The request
// context is the witness instead.
//
// An abandoned request is not a fault. Nobody is left to read a status line,
// and logging a closed tab at error level makes a healthy node look broken --
// which teaches the reader to skip the log that is supposed to carry the real
// failures.
func (s *Server) queryFailed(w http.ResponseWriter, r *http.Request, op, msg string, err error) {
	if r.Context().Err() != nil {
		s.log.Debug("query abandoned by caller", "query", op, "error", err)
		return
	}
	s.log.Error(op, "error", err)
	s.fail(w, http.StatusInternalServerError, msg)
}

// handleHealth reports whether this node is actually converging with its peers.
//
// Always 200 when the node itself is working, with the verdict in the body. A
// peer being switched off is the normal state of a laptop and must not read as
// a failure -- if this returned non-200 for that, the signal would be ignored
// within a week and would then be useless for the case that matters.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		// Peering is disabled on this node. Saying so beats a 404, which a
		// caller cannot tell apart from an old build.
		s.respond(w, http.StatusOK, map[string]any{
			"status": statusNoPeering,
			"reason": "this node has no mesh configured",
		})
		return
	}
	report, err := s.health(r.Context())
	if err != nil {
		s.queryFailed(w, r, "health", "health failed", err)
		return
	}
	s.respond(w, http.StatusOK, map[string]any{
		"status": verdict(report),
		"health": report,
	})
}

// nodeStatus is the one-word verdict a monitor can alert on.
type nodeStatus string

const (
	statusOK        nodeStatus = "ok"
	statusDegraded  nodeStatus = "degraded"
	statusNoPeering nodeStatus = "no-peering"
)

// verdict reduces the report to something worth alerting on.
//
// Quarantined records mean two machines are issuing under one origin id, which
// is silent data loss and always degraded. A peer that is failing after having
// worked is degraded too. A peer that has simply never been reached is not:
// that is an address someone configured for a machine they have not switched on
// yet, and calling it degraded would make the whole signal noise.
func verdict(h mesh.Health) nodeStatus {
	if h.Quarantined > 0 {
		return statusDegraded
	}
	for _, p := range h.Peers {
		if p.Reach == mesh.ReachFailing || p.Reach == mesh.ReachStale {
			return statusDegraded
		}
	}
	return statusOK
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.derive.Summarize(r.Context())
	if err != nil {
		s.queryFailed(w, r, "summarize", "summary failed", err)
		return
	}
	s.respond(w, http.StatusOK, summary)
}

func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request) {
	activity, err := s.derive.Daily(r.Context())
	if err != nil {
		s.queryFailed(w, r, "daily", "daily failed", err)
		return
	}
	// Activity carries the same "days" key it always did, plus the legends
	// that read its stacks. A dashboard served by an older node in the mesh
	// sees the days and ignores the rest.
	s.respond(w, http.StatusOK, activity)
}

func (s *Server) handleDelivery(w http.ResponseWriter, r *http.Request) {
	delivery, err := s.derive.Deliveries(r.Context())
	if err != nil {
		s.queryFailed(w, r, "deliveries", "delivery failed", err)
		return
	}
	s.respond(w, http.StatusOK, delivery)
}

// newSessionToken returns an unguessable session identifier.
func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("api: generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// isSecure reports whether the request reached us over TLS, directly or via a
// terminating proxy. The cookie must not be marked Secure over plain HTTP or
// the loopback dashboard would silently fail to authenticate.
func isSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// respond writes a JSON body.
func (s *Server) respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this can only be logged.
		s.log.Error("write response", "error", err)
	}
}

// fail writes a JSON error body. Messages stay coarse deliberately.
func (s *Server) fail(w http.ResponseWriter, status int, message string) {
	s.respond(w, status, map[string]string{"error": message})
}

// Describe returns a one-line description of the routes, for startup logging.
func Describe() string {
	return "routes: " + strings.Join([]string{
		"GET /", "GET /healthz", "POST /v1/login", "POST /v1/logout",
		"GET /v1/health", "GET /v1/summary", "GET /v1/daily", "GET /v1/delivery",
	}, ", ")
}
