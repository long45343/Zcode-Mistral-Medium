package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/long45343/Zcode-Mistral-Medium/internal/settings"
)

// newTestServer builds a Server backed by a temporary settings file.
func newTestServer(t *testing.T, baseURL string) *Server {
	t.Helper()
	store, err := settings.Open(filepath.Join(t.TempDir(), "settings.json"), settings.Settings{
		BaseURL: baseURL,
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatalf("open settings: %v", err)
	}
	return New(store, 30*time.Second, log.New(io.Discard, "", 0))
}

// TestRejectsNonLoopbackClients is the guard that keeps an exposed port from
// driving the configured upstream credential.
func TestRejectsNonLoopbackClients(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:54321"

	rec := httptest.NewRecorder()
	srv.HandleResponses(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "loopback") {
		t.Errorf("body should explain the restriction, got %q", rec.Body.String())
	}
}

// TestRejectsOversizedBody bounds memory use for a single request.
func TestRejectsOversizedBody(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")

	big := bytes.Repeat([]byte("a"), maxRequestBody+1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5000"

	rec := httptest.NewRecorder()
	srv.HandleResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestRejectsMissingCredential keeps the no-key path explicit.
func TestRejectsMissingCredential(t *testing.T) {
	store, err := settings.Open(filepath.Join(t.TempDir(), "settings.json"),
		settings.Settings{BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatalf("open settings: %v", err)
	}
	srv := New(store, 30*time.Second, log.New(io.Discard, "", 0))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5000"

	rec := httptest.NewRecorder()
	srv.HandleResponses(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestMethodNotAllowed rejects non-POST verbs before doing any work.
func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.RemoteAddr = "127.0.0.1:5000"

	rec := httptest.NewRecorder()
	srv.HandleResponses(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestIsLoopback covers the address forms the guard must accept and reject.
func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080":  true,
		"[::1]:8080":      true,
		"127.0.0.5:1":     true,
		"192.0.2.10:8080": false, // TEST-NET-1, reserved for documentation
		"203.0.113.7:1":   false,
		"example.com:80":  false,
		"":                false,
		"not-an-ip":       false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
