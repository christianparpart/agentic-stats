package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/api"
	"github.com/christianparpart/agentic-stats/internal/seal"
)

func serve(t *testing.T, n *node) *httptest.Server {
	t.Helper()
	srv, err := api.NewServer(api.Config{Derive: n.derive, Keys: n.keys})
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path string, body io.Reader, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}

func login(t *testing.T, ts *httptest.Server, key string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"password": key})
	if err != nil {
		t.Fatalf("marshal login: %v", err)
	}
	return do(t, ts, http.MethodPost, "/v1/login", bytes.NewReader(body), nil)
}

func TestDashboardIsServedAtRoot(t *testing.T) {
	ts := serve(t, newNode(t))
	resp := do(t, ts, http.MethodGet, "/", nil, nil)
	defer func() { _ = resp.Body.Close() }() // test cleanup

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q, want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Self-contained: nothing fetched over the network at render time.
	for _, forbidden := range []string{
		`src="http`, `src='http`, `href="http`, `href='http`,
		"<script src", `<link rel="stylesheet"`, "@import",
	} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Errorf("dashboard loads an external resource (%q)", forbidden)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("dashboard must ship a Content-Security-Policy")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	ts := serve(t, newNode(t))
	resp := do(t, ts, http.MethodGet, "/no/such/page", nil, nil)
	defer func() { _ = resp.Body.Close() }() // test cleanup
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReadsRequireAuthentication(t *testing.T) {
	ts := serve(t, newNode(t))
	for _, path := range []string{"/v1/summary", "/v1/daily"} {
		resp := do(t, ts, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: status = %d, want 401", path, resp.StatusCode)
		}
		_ = resp.Body.Close()

		bogus := &http.Cookie{Name: "agentic_session", Value: "not-a-real-session"}
		resp = do(t, ts, http.MethodGet, path, nil, bogus)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with a forged cookie: status = %d, want 401", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// The mesh key is the dashboard password; nothing else opens it.
func TestMeshKeyIsTheDashboardPassword(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	n := newNodeWithKey(t, psk)
	ts := serve(t, n)

	otherKey, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, wrong := range []string{otherKey, "", "hunter2", psk + "x"} {
		resp := login(t, ts, wrong)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("login with %q: status = %d, want 401", wrong, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	resp := login(t, ts, psk)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login with the mesh key: status = %d, want 200", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "agentic_session" {
			cookie = c
		}
	}
	_ = resp.Body.Close()
	if cookie == nil {
		t.Fatal("login set no session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie must be HttpOnly so script cannot read it")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}

	for _, path := range []string{"/v1/summary", "/v1/daily"} {
		r := do(t, ts, http.MethodGet, path, nil, cookie)
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s with a session: status = %d, want 200", path, r.StatusCode)
		}
		_ = r.Body.Close()
	}

	// Logging out invalidates the session immediately.
	out := do(t, ts, http.MethodPost, "/v1/logout", nil, cookie)
	_ = out.Body.Close()
	after := do(t, ts, http.MethodGet, "/v1/summary", nil, cookie)
	if after.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout: status = %d, want 401", after.StatusCode)
	}
	_ = after.Body.Close()
}

// A retyped key with different grouping or casing must still sign in.
func TestLoginAcceptsAReformattedKey(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ts := serve(t, newNodeWithKey(t, psk))

	resp := login(t, ts, strings.ToLower(strings.ReplaceAll(psk, "-", "")))
	defer func() { _ = resp.Body.Close() }() // test cleanup
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: a reformatted key must still work", resp.StatusCode)
	}
}
