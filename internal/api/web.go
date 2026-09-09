package api

import (
	"embed"
	"net/http"
)

// dashboardFiles carries the single-page dashboard into the binary, so the
// server ships as one file with nothing to serve from disk.
//
//go:embed all:assets
var dashboardFiles embed.FS

// dashboardPath is the embedded page.
const dashboardPath = "assets/dashboard.html"

// handleDashboard serves the dashboard shell.
//
// The page itself holds no data: it authenticates and then fetches the same
// JSON endpoints any other client would, so there is one API rather than two.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := dashboardFiles.ReadFile(dashboardPath)
	if err != nil {
		s.log.Error("read embedded dashboard", "error", err)
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is self-contained: no external scripts, styles, or fonts.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if _, err := w.Write(body); err != nil {
		s.log.Error("write dashboard", "error", err)
	}
}
