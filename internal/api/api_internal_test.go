package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A caller that hangs up mid-query leaves an interrupted statement behind.
// That is expected, not a fault, and reporting it at error level is what makes
// a healthy node look broken -- which in turn teaches the reader to skip the
// log that carries the real failures.
//
// The error itself cannot be discriminated: the driver reports an interrupt as
// a bare code that neither wraps nor unwraps to context.Canceled. This pins the
// request context as the thing being consulted, so a future refactor to
// errors.Is cannot quietly pass.
func TestQueryFailedTellsAnAbandonedRequestFromAFault(t *testing.T) {
	// Exactly what the driver produced in the field, and deliberately not
	// wrapped around context.Canceled.
	interrupted := errors.New("derive: daily: sqlite3: interrupted")

	tests := []struct {
		name       string
		abandoned  bool
		wantStatus int
		wantBody   bool
		wantLevel  string
	}{
		{
			name:       "a caller that is still there gets the failure",
			wantStatus: http.StatusInternalServerError,
			wantBody:   true,
			wantLevel:  "level=ERROR",
		},
		{
			name:      "a caller that hung up is logged at debug and sent nothing",
			abandoned: true,
			// Nothing is written, so the recorder keeps its default. There is
			// nobody left to read a status line.
			wantStatus: http.StatusOK,
			wantBody:   false,
			wantLevel:  "level=DEBUG",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := &Server{log: slog.New(slog.NewTextHandler(&logged,
				&slog.HandlerOptions{Level: slog.LevelDebug}))}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.abandoned {
				cancel()
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/daily", nil).WithContext(ctx)
			w := httptest.NewRecorder()

			s.queryFailed(w, r, "daily", "daily failed", interrupted)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if wrote := w.Body.Len() > 0; wrote != tc.wantBody {
				t.Errorf("wrote a body = %v, want %v (body %q)", wrote, tc.wantBody, w.Body.String())
			}
			if got := logged.String(); !strings.Contains(got, tc.wantLevel) {
				t.Errorf("log = %q, want it to contain %s", got, tc.wantLevel)
			}
			if tc.abandoned && strings.Contains(logged.String(), "level=ERROR") {
				t.Errorf("an abandoned request was reported as a fault: %q", logged.String())
			}
		})
	}
}
