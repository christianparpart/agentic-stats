// Package api exposes the server's HTTP surface.
//
// Every endpoint but health requires a device token, and the tenant is always
// taken from that token server-side. A user id in a request body is never
// trusted.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/christianparpart/agentic-stats/internal/auth"
	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// maxEnrollBody bounds the enrollment request, which is small by construction.
const maxEnrollBody = 64 << 10

// Config is everything the API needs.
type Config struct {
	Auth   *auth.Service
	Ingest *ingest.Service
	Derive *derive.Service
	Logger *slog.Logger
}

// Server routes and handles HTTP requests.
type Server struct {
	auth   *auth.Service
	ingest *ingest.Service
	derive *derive.Service
	log    *slog.Logger
}

// NewServer returns a Server wired to the supplied services.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Auth == nil || cfg.Ingest == nil || cfg.Derive == nil {
		return nil, errors.New("api: Auth, Ingest and Derive services are all required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{auth: cfg.Auth, ingest: cfg.Ingest, derive: cfg.Derive, log: log}, nil
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("POST /v1/logout", s.handleLogout)
	mux.HandleFunc("POST /v1/devices/enroll", s.handleEnroll)
	// Ingest is device-only: a browser session must never be able to write
	// into the archive.
	mux.HandleFunc("POST /v1/ingest", s.withDevice(s.handleIngest))
	// Reads accept either a device token or a dashboard session.
	mux.HandleFunc("GET /v1/summary", s.withTenant(s.handleSummary))
	mux.HandleFunc("GET /v1/daily", s.withTenant(s.handleDaily))
	return mux
}

// deviceHandler is a handler that runs with an authenticated device.
type deviceHandler func(http.ResponseWriter, *http.Request, auth.Device)

// withDevice authenticates the bearer token and establishes the tenant.
func (s *Server) withDevice(next deviceHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			s.fail(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		dev, err := s.auth.AuthenticateDevice(r.Context(), token)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidCredential) {
				s.fail(w, http.StatusUnauthorized, "invalid device token")
				return
			}
			s.log.Error("authenticate device", "error", err)
			s.fail(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		next(w, r, dev)
	}
}

// bearerToken extracts a bearer credential from the Authorization header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

// enrollRequest redeems a one-time code for a durable device token.
type enrollRequest struct {
	Code   string      `json:"code"`
	Device wire.Device `json:"device"`
}

type enrollResponse struct {
	Token string `json:"token"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "malformed request")
		return
	}
	token, err := s.auth.RedeemEnrollment(r.Context(), req.Code, req.Device)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredential) {
			// Deliberately identical for expired, redeemed and unknown codes.
			s.fail(w, http.StatusUnauthorized, "invalid or expired enrollment code")
			return
		}
		s.log.Error("redeem enrollment", "error", err)
		s.fail(w, http.StatusInternalServerError, "enrollment failed")
		return
	}
	s.respond(w, http.StatusOK, enrollResponse{Token: token})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	var records []wire.Record
	header, err := wire.DecodeBatch(r.Body, func(rec wire.Record) error {
		records = append(records, rec)
		return nil
	})
	if err != nil {
		s.log.Warn("decode batch", "device", dev.Hostname, "error", err)
		s.fail(w, http.StatusBadRequest, "malformed batch")
		return
	}

	result, err := s.ingest.Store(r.Context(), dev, header, records)
	if err != nil {
		s.log.Error("store batch", "device", dev.Hostname, "error", err)
		s.fail(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	s.log.Info("ingested",
		"device", dev.Hostname,
		"received", result.Received, "stored", result.Stored, "duplicates", result.Duplicates)
	s.respond(w, http.StatusOK, result)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request, userID string) {
	summary, err := s.derive.Summarize(r.Context(), userID)
	if err != nil {
		s.log.Error("summarize", "error", err)
		s.fail(w, http.StatusInternalServerError, "summary failed")
		return
	}
	s.respond(w, http.StatusOK, summary)
}

func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request, userID string) {
	days, err := s.derive.Daily(r.Context(), userID)
	if err != nil {
		s.log.Error("daily", "error", err)
		s.fail(w, http.StatusInternalServerError, "daily failed")
		return
	}
	s.respond(w, http.StatusOK, map[string]any{"days": days})
}

// sessionCookie is the dashboard's session cookie name.
const sessionCookie = "agentic_session"

// tenantHandler is a handler that runs with a resolved tenant.
type tenantHandler func(http.ResponseWriter, *http.Request, string)

// withTenant resolves the tenant from either a device token or a dashboard
// session, so the dashboard and the collector share one set of read endpoints.
func (s *Server) withTenant(next tenantHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := bearerToken(r); token != "" {
			dev, err := s.auth.AuthenticateDevice(r.Context(), token)
			if err == nil {
				next(w, r, dev.UserID)
				return
			}
			if !errors.Is(err, auth.ErrInvalidCredential) {
				s.log.Error("authenticate device", "error", err)
				s.fail(w, http.StatusInternalServerError, "authentication failed")
				return
			}
		}
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			user, serr := s.auth.AuthenticateSession(r.Context(), c.Value)
			if serr == nil {
				next(w, r, user.ID)
				return
			}
			if !errors.Is(serr, auth.ErrInvalidCredential) {
				s.log.Error("authenticate session", "error", serr)
				s.fail(w, http.StatusInternalServerError, "authentication failed")
				return
			}
		}
		s.fail(w, http.StatusUnauthorized, "authentication required")
	}
}

// loginRequest carries dashboard credentials.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "malformed request")
		return
	}
	token, err := s.auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredential) {
			// Identical for an unknown email and a wrong password.
			s.fail(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		s.log.Error("login", "error", err)
		s.fail(w, http.StatusInternalServerError, "login failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(r),
		MaxAge:   int(auth.SessionTTL.Seconds()),
	})
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.auth.Logout(r.Context(), c.Value); err != nil {
			s.log.Error("logout", "error", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r), MaxAge: -1,
	})
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

// isSecure reports whether the request reached us over TLS, directly or via a
// terminating proxy. The cookie must not be marked Secure over plain HTTP or a
// local install would silently fail to authenticate.
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

// fail writes a JSON error body.
//
// Messages are intentionally coarse: the server must not help an attacker
// distinguish an unknown credential from an expired one.
func (s *Server) fail(w http.ResponseWriter, status int, message string) {
	s.respond(w, status, map[string]string{"error": message})
}

// Describe returns a one-line description of the routes, for startup logging.
func Describe() string {
	return fmt.Sprintf("routes: %s", strings.Join([]string{
		"GET /",
		"GET /healthz",
		"POST /v1/login",
		"POST /v1/logout",
		"POST /v1/devices/enroll",
		"POST /v1/ingest",
		"GET /v1/summary",
		"GET /v1/daily",
	}, ", "))
}
