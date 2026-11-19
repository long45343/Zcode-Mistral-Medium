// Package settings holds the single piece of runtime configuration the
// dashboard exposes: where to send requests, and the key to send with them.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Settings is the whole configuration surface.
type Settings struct {
	// BaseURL is the provider base URL, e.g. "https://api.mistral.ai/v1".
	BaseURL string `json:"base_url"`
	// APIKey is the bearer token sent upstream.
	APIKey string `json:"api_key"`
	// ReasoningRewrite retypes raw reasoning events into summary events so
	// clients built on the Vercel AI SDK can render the thinking process.
	ReasoningRewrite bool `json:"reasoning_rewrite"`
	// Username and Password guard the dashboard. An empty password disables
	// remote access entirely.
	Username string `json:"username"`
	Password string `json:"password"`
	// ImageTrim replaces images beyond MaxImages with a text placeholder so a
	// long session keeps working past the provider's 20-image ceiling.
	ImageTrim bool `json:"image_trim"`
	// MaxImages is the ceiling applied when ImageTrim is enabled.
	MaxImages int `json:"max_images"`
}

// Store persists settings to a JSON file and serves them concurrency-safely.
type Store struct {
	mu   sync.RWMutex
	path string
	cur  Settings
}

// Open loads settings from path, creating the file with defaults when it does
// not exist yet.
func Open(path string, defaults Settings) (*Store, error) {
	s := &Store{path: path, cur: defaults}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// Materialize the defaults so the file is easy to edit by hand.
		if saveErr := s.write(defaults); saveErr != nil {
			return nil, saveErr
		}
		return s, nil
	}

	// Pointers distinguish "absent from the file" from "explicitly false", so a
	// file written by an older version keeps the current default instead of
	// silently turning features off.
	var loaded struct {
		BaseURL          string `json:"base_url"`
		APIKey           string `json:"api_key"`
		ReasoningRewrite *bool  `json:"reasoning_rewrite"`
		Username         string `json:"username"`
		Password         string `json:"password"`
		ImageTrim        *bool  `json:"image_trim"`
		MaxImages        int    `json:"max_images"`
	}
	if err := json.Unmarshal(raw, &loaded); err != nil {
		// A hand-edited file with a typo must not stop the process from
		// starting; fall back to defaults and let the dashboard rewrite it.
		return s, nil
	}
	if loaded.BaseURL != "" {
		s.cur.BaseURL = loaded.BaseURL
	}
	if loaded.APIKey != "" {
		s.cur.APIKey = loaded.APIKey
	}
	if loaded.ReasoningRewrite != nil {
		s.cur.ReasoningRewrite = *loaded.ReasoningRewrite
	}
	if loaded.Username != "" {
		s.cur.Username = loaded.Username
	}
	if loaded.Password != "" {
		s.cur.Password = loaded.Password
	}
	if loaded.ImageTrim != nil {
		s.cur.ImageTrim = *loaded.ImageTrim
	}
	if loaded.MaxImages > 0 {
		s.cur.MaxImages = loaded.MaxImages
	}

	return s, nil
}

// Get returns the current settings.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Set replaces the settings and persists them.
func (s *Store) Set(next Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.write(next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// write persists atomically so a crash cannot leave a half-written file.
func (s *Store) write(v Settings) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ResponsesURL returns the endpoint the proxy forwards to.
func (v Settings) ResponsesURL() string {
	return trimTrailingSlash(v.BaseURL) + "/responses"
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
