// Package proxy forwards Responses API calls from AxonHub to the provider,
// fixing message types on the way through.
package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/long45343/Zcode-Mistral-Medium/internal/normalize"
	"github.com/long45343/Zcode-Mistral-Medium/internal/reasoning"
	"github.com/long45343/Zcode-Mistral-Medium/internal/settings"
)

// maxRequestBody bounds how much of a request body is read into memory. Real
// Responses payloads run around 1-2 MB; this leaves generous headroom while
// preventing an oversized body from exhausting memory.
const maxRequestBody = 64 << 20

// hopHeaders must not cross a hop.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Host", "Content-Length", "Accept-Encoding",
}

// Server proxies requests to the configured provider.
type Server struct {
	store   *settings.Store
	client  *http.Client
	timeout time.Duration
	logger  *log.Logger
}

// New builds a Server.
func New(store *settings.Store, timeout time.Duration, logger *log.Logger) *Server {
	return &Server{
		store:   store,
		timeout: timeout,
		logger:  logger,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 60 * time.Second,
				}).DialContext,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
				// Hand SSE bytes through exactly as the provider sent them.
				DisableCompression: true,
				ForceAttemptHTTP2:  true,
			},
			// Generation can run for minutes; the request context bounds it.
		},
	}
}

// HandleResponses proxies POST /v1/responses.
func (s *Server) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Only the co-located proxy may forward through this endpoint. Without
	// this check the port would let anyone on the network drive the configured
	// upstream credential.
	if !IsLoopback(r.RemoteAddr) {
		http.Error(w, "forbidden: this endpoint only accepts loopback clients", http.StatusForbidden)
		return
	}

	cfg := s.store.Get()
	if cfg.BaseURL == "" {
		http.Error(w, "not configured: set the base url on the dashboard", http.StatusServiceUnavailable)
		return
	}

	// The dashboard key wins when set. Otherwise fall back to whatever
	// credential AxonHub forwarded, so the gateway works without storing a key.
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = bearerToken(r.Header.Get("Authorization"))
	}
	if apiKey == "" {
		http.Error(w, "no api key: configure one on the dashboard or send an Authorization header", http.StatusServiceUnavailable)
		return
	}

	limited := http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "failed to read request body (or it exceeded the size limit)", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	body, st := normalize.Rewrite(body)

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ResponsesURL(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
		return
	}

	copyHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		s.logger.Printf("upstream error: %v", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	out := w.Header()
	copyHeaders(out, resp.Header)
	for _, h := range hopHeaders {
		out.Del(h)
	}

	w.WriteHeader(resp.StatusCode)

	n, copyErr := s.pipe(w, resp.Body, cfg.ReasoningRewrite)
	if copyErr != nil {
		s.logger.Printf("stream interrupted after %d bytes: %v", n, copyErr)
	}

	if st.Changed() {
		s.logger.Printf("fixed %d message type(s), status=%d bytes=%d", st.Typed, resp.StatusCode, n)
	}
}

// pipe copies the upstream body to the client, flushing after every write so
// SSE frames reach the caller immediately.
//
// When reasoning rewriting is enabled the bytes pass through an SSE frame
// buffer first, so an event split across reads is reassembled before it is
// inspected.
func (s *Server) pipe(w http.ResponseWriter, src io.Reader, rewriteReasoning bool) (int64, error) {
	flusher, _ := w.(http.Flusher)

	var rewriter *reasoning.Rewriter
	if rewriteReasoning {
		rewriter = reasoning.New(true)
	}

	write := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	var written int64
	buf := make([]byte, 32*1024)

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			out := buf[:n]
			if rewriter != nil {
				out = rewriter.Write(out)
			}
			written += int64(len(out))
			if err := write(out); err != nil {
				return written, err
			}
		}
		if readErr != nil {
			if rewriter != nil {
				tail := rewriter.Flush()
				written += int64(len(tail))
				if err := write(tail); err != nil {
					return written, err
				}
				if st := rewriter.Stats(); st.Changed() {
					s.logger.Printf("rewrote reasoning stream: %d deltas, %d parts", st.Deltas, st.Parts)
				}
			}
			if readErr == io.EOF {
				return written, nil
			}
			return written, readErr
		}
	}
}

// copyHeaders copies all values from src into dst.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// IsLoopback reports whether the request came from the local machine.
func IsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerToken extracts the credential from an Authorization header, accepting
// both "Bearer <token>" and a bare token.
func bearerToken(header string) string {
	h := strings.TrimSpace(header)
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}
