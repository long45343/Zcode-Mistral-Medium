// Command zcode-cache-gateway is a small, deliberately narrow middleware that
// sits between AxonHub and an OpenAI Responses API provider.
//
// It does exactly three things:
//   - lets the dashboard configure the provider base URL and API key
//   - adds "type": "message" to input items that omit it (fixes Mistral 3051)
//   - forwards the request and streams the response back untouched
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/long45343/Zcode-Mistral-Medium/internal/proxy"
	"github.com/long45343/Zcode-Mistral-Medium/internal/settings"
)

//go:embed web/*
var webAssets embed.FS

func main() {
	configPath := flag.String("config", "/data/settings.json", "path to the settings file")
	listen := flag.String("listen", "0.0.0.0:8787", "listen address")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-request upstream timeout")
	username := flag.String("username", firstNonEmpty(os.Getenv("DASHBOARD_USERNAME"), "admin"),
		"dashboard username")
	password := flag.String("password", firstNonEmpty(os.Getenv("DASHBOARD_PASSWORD"), "admin"),
		"dashboard password")
	rewriteReasoningDefault := flag.Bool("rewrite-reasoning", envBool("REWRITE_REASONING"),
		"initial value for the reasoning rewrite switch shown on the dashboard")
	flag.Parse()

	logger := log.New(os.Stdout, "[gateway] ", log.LstdFlags|log.Lmsgprefix)

	// Seed from the environment so a container can start configured without a
	// pre-existing settings file.
	defaults := settings.Settings{
		BaseURL:          firstNonEmpty(os.Getenv("UPSTREAM_BASE_URL"), "https://api.mistral.ai/v1"),
		APIKey:           os.Getenv("UPSTREAM_API_KEY"),
		ReasoningRewrite: *rewriteReasoningDefault,
		Username:         *username,
		Password:         *password,
	}

	store, err := settings.Open(*configPath, defaults)
	if err != nil {
		logger.Fatalf("open settings: %v", err)
	}

	server := proxy.New(store, *timeout, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", server.HandleResponses)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		cfg := store.Get()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"upstream":   cfg.ResponsesURL(),
			"configured": cfg.BaseURL != "",
		})
	})

	mux.Handle("/api/settings", guard(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg := store.Get()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"base_url":          cfg.BaseURL,
				"api_key_set":       cfg.APIKey != "",
				"api_key_hint":      maskKey(cfg.APIKey),
				"reasoning_rewrite": cfg.ReasoningRewrite,
				"username":          cfg.Username,
				"password_set":      cfg.Password != "",
			})
		case http.MethodPost:
			var in struct {
				BaseURL          string `json:"base_url"`
				APIKey           string `json:"api_key"`
				ReasoningRewrite bool   `json:"reasoning_rewrite"`
				Username         string `json:"username"`
				Password         string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			base := strings.TrimSpace(in.BaseURL)
			if !validBaseURL(base) {
				http.Error(w, "base_url must be an absolute http(s) URL", http.StatusBadRequest)
				return
			}
			next := store.Get()
			next.BaseURL = base
			next.ReasoningRewrite = in.ReasoningRewrite
			// An empty key means "keep the current one", so the dashboard never
			// has to round-trip the secret.
			if key := strings.TrimSpace(in.APIKey); key != "" {
				next.APIKey = key
			}
			// Credentials are only replaced when supplied, so saving the other
			// fields never silently clears them.
			if user := strings.TrimSpace(in.Username); user != "" {
				next.Username = user
			}
			if pass := in.Password; pass != "" {
				if len(pass) < 4 {
					http.Error(w, "password must be at least 4 characters", http.StatusBadRequest)
					return
				}
				next.Password = pass
			}
			if next.Password == "" {
				http.Error(w, "password must not be empty: an empty password disables remote access", http.StatusBadRequest)
				return
			}

			if err := store.Set(next); err != nil {
				http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
				return
			}
			logger.Printf("settings updated: base_url=%s key_set=%v rewrite_reasoning=%v user=%q",
				next.BaseURL, next.APIKey != "", next.ReasoningRewrite, next.Username)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})))

	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		logger.Fatalf("mount web assets: %v", err)
	}
	mux.Handle("/", guard(store, http.FileServer(http.FS(sub))))

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE responses legitimately run for minutes.
	}

	logger.Printf("listening on %s -> %s", *listen, store.Get().ResponsesURL())
	logger.Printf("reasoning rewrite: %v (changeable on the dashboard)", store.Get().ReasoningRewrite)
	logger.Printf("dashboard login: user=%q", *username)
	if *username == "admin" && *password == "admin" {
		logger.Printf("warning: default credentials in use, change -username/-password")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Fatalf("server error: %v", err)
	case sig := <-stop:
		logger.Printf("received %s, draining in-flight requests", sig)
	}

	// Give in-flight requests a chance to finish instead of cutting streaming
	// responses mid-flight. Anything still running after the grace period is
	// abandoned; keep this below the container's stop grace period.
	drainCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(drainCtx); err != nil {
		logger.Printf("graceful shutdown incomplete: %v", err)
		return
	}
	logger.Printf("shutdown complete")
}

// guard protects the dashboard and its settings API with HTTP Basic auth, so
// the browser shows its own login prompt and replays the credentials for every
// subsequent request.
//
// Credentials are read from the live settings on each request, so changing the
// password on the dashboard takes effect immediately without a restart.
//
// An empty password disables remote access entirely: the settings endpoint can
// rewrite the upstream URL, so exposing it without a credential would let
// anyone redirect the forwarded API key.
func guard(store *settings.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()

		if cfg.Password == "" {
			if !proxy.IsLoopback(r.RemoteAddr) {
				http.Error(w, "forbidden: set a dashboard password to allow remote access", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		user, supplied, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(cfg.Username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(supplied), []byte(cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ZCode Gateway", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validBaseURL accepts only absolute http(s) URLs, so a typo cannot silently
// point the proxy at something that is not a URL.
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// maskKey shows just enough of a key to confirm which one is stored.
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

// envBool reads a boolean toggle from the environment.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
