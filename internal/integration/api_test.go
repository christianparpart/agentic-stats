package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/api"
	"github.com/christianparpart/agentic-stats/internal/auth"
	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/storetest"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// doRequest issues a request carrying a context, which is what every caller
// should do and what the linter enforces.
func doRequest(t *testing.T, ts *httptest.Server, method, path, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}

// newTestServer builds the real handler stack over the test database.
func newTestServer(t *testing.T, db *store.DB) *httptest.Server {
	t.Helper()
	prices, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	authSvc, err := auth.NewService(db)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	ingestSvc, err := ingest.NewService(db)
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	deriveSvc, err := derive.NewService(db, prices)
	if err != nil {
		t.Fatalf("derive.NewService: %v", err)
	}
	srv, err := api.NewServer(api.Config{Auth: authSvc, Ingest: ingestSvc, Derive: deriveSvc})
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthNeedsNoCredential(t *testing.T) {
	db := storetest.Open(t)
	ts := newTestServer(t, db)

	resp := doRequest(t, ts, http.MethodGet, "/healthz", "", nil)
	defer func() { _ = resp.Body.Close() }() // test cleanup
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestProtectedRoutesRejectBadCredentials(t *testing.T) {
	db := storetest.Open(t)
	ts := newTestServer(t, db)

	cases := []struct {
		name, method, path, authHeader string
	}{
		{"ingest without a token", http.MethodPost, "/v1/ingest", ""},
		{"ingest with a bogus token", http.MethodPost, "/v1/ingest", "Bearer not-a-token"},
		{"ingest with a malformed header", http.MethodPost, "/v1/ingest", "token-without-bearer"},
		{"summary without a token", http.MethodGet, "/v1/summary", ""},
		{"daily with a bogus token", http.MethodGet, "/v1/daily", "Bearer nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(),
				tc.method, ts.URL+tc.path, http.NoBody)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			// Set verbatim: these cases deliberately include a malformed header.
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }() // test cleanup
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestEnrollRejectsAnUnknownCode(t *testing.T) {
	db := storetest.Open(t)
	ts := newTestServer(t, db)

	body, err := json.Marshal(map[string]any{
		"code":   "definitely-not-issued",
		"device": wire.Device{Hostname: "h", OS: "linux", Arch: "amd64"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := doRequest(t, ts, http.MethodPost, "/v1/devices/enroll", "", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }() // test cleanup
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// The whole path a real machine takes: enroll over HTTP, upload over HTTP,
// then read back a summary whose numbers reflect the fold.
func TestFullHTTPRoundTrip(t *testing.T) {
	db := storetest.Open(t)
	ts := newTestServer(t, db)
	ctx := context.Background()

	authSvc, err := auth.NewService(db)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	userID, err := authSvc.CreateUser(ctx, "http@example.invalid", "pw", auth.RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	code, err := authSvc.CreateEnrollmentCode(ctx, userID, time.Hour)
	if err != nil {
		t.Fatalf("CreateEnrollmentCode: %v", err)
	}

	// Enroll.
	enrollBody, err := json.Marshal(map[string]any{
		"code":   code,
		"device": wire.Device{Hostname: "laptop", OS: "darwin", Arch: "arm64", Timezone: "Europe/Berlin"},
	})
	if err != nil {
		t.Fatalf("marshal enroll: %v", err)
	}
	enrollResp := doRequest(t, ts, http.MethodPost, "/v1/devices/enroll", "", bytes.NewReader(enrollBody))
	var enrolled struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(enrollResp.Body).Decode(&enrolled); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	_ = enrollResp.Body.Close() // response fully read
	if enrolled.Token == "" {
		t.Fatal("enrollment returned no token")
	}

	// Upload, using the real client so the wire format is exercised too.
	client, err := wire.NewClient(wire.ClientConfig{
		BaseURL: ts.URL,
		Token:   enrolled.Token,
		Device:  wire.Device{Hostname: "laptop", OS: "darwin", Arch: "arm64"},
	})
	if err != nil {
		t.Fatalf("wire.NewClient: %v", err)
	}
	batch := records(
		assistantLine("http-req", "claude-opus-5", 300, 9000),
		assistantLine("http-req", "claude-opus-5", 300, 9000),
	)
	result, err := client.Ingest(ctx, batch)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.Stored != 2 {
		t.Errorf("stored = %d, want 2 (two distinct lines)", result.Stored)
	}

	// Read the summary back over HTTP.
	sumResp := doRequest(t, ts, http.MethodGet, "/v1/summary", enrolled.Token, nil)
	var summary derive.Summary
	if err := json.NewDecoder(sumResp.Body).Decode(&summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	_ = sumResp.Body.Close() // response fully read

	// Two lines, one request: the fold happened.
	if summary.Requests != 1 {
		t.Errorf("requests = %d, want 1", summary.Requests)
	}
	if summary.AssistantLines != 2 {
		t.Errorf("assistant lines = %d, want 2", summary.AssistantLines)
	}
	if len(summary.Models) != 1 || summary.Models[0].OutputTokens != 300 {
		t.Errorf("output tokens = %+v, want a single model with 300", summary.Models)
	}
	if summary.TotalCostUSD <= 0 {
		t.Errorf("cost = %v, want positive", summary.TotalCostUSD)
	}
}
