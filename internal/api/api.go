// Package api is the dashboard and read API each node serves for itself.
//
// There are no accounts. The pre-shared key that admits a node to the mesh is
// also the dashboard password, so anyone who can read the archive over the
// network already holds the key that decrypts it.
package api

import (
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
}

// Server routes and handles HTTP requests.
type Server struct {
	derive *derive.Service
	keys   *seal.Keys
	log    *slog.Logger
	now    func() time.Time

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
		sessions: make(map[string]time.Time),
	}, nil
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("POST /v1/logout", s.handleLogout)
	mux.HandleFunc("GET /v1/summary", s.authenticated(s.handleSummary))
	mux.HandleFunc("GET /v1/daily", s.authenticated(s.handleDaily))
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

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
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

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.derive.Summarize(r.Context())
	if err != nil {
		s.log.Error("summarize", "error", err)
		s.fail(w, http.StatusInternalServerError, "summary failed")
		return
	}
	s.respond(w, http.StatusOK, summary)
}

func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request) {
	days, err := s.derive.Daily(r.Context())
	if err != nil {
		s.log.Error("daily", "error", err)
		s.fail(w, http.StatusInternalServerError, "daily failed")
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"days": days})
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
		"GET /v1/summary", "GET /v1/daily",
	}, ", ")
}
