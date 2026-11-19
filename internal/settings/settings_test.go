package settings

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptFileFallsBackToDefaults ensures a hand-edited typo cannot stop the
// process from starting.
func TestCorruptFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store, err := Open(path, Settings{BaseURL: "https://default.example/v1", ReasoningRewrite: true})
	if err != nil {
		t.Fatalf("Open returned an error for a corrupt file: %v", err)
	}

	got := store.Get()
	if got.BaseURL != "https://default.example/v1" {
		t.Errorf("base_url = %q, want the default", got.BaseURL)
	}
	if !got.ReasoningRewrite {
		t.Error("reasoning_rewrite should keep the default on a corrupt file")
	}
}

// TestMissingFieldKeepsDefault is the regression for the switch silently
// turning off after an upgrade.
func TestMissingFieldKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://a.example/v1","api_key":"k"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store, err := Open(path, Settings{ReasoningRewrite: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !store.Get().ReasoningRewrite {
		t.Error("absent reasoning_rewrite must keep the default true")
	}
}

// TestExplicitFalseIsHonored checks the other direction: an explicit false in
// the file must win over the default.
func TestExplicitFalseIsHonored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://a.example/v1","reasoning_rewrite":false}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store, err := Open(path, Settings{ReasoningRewrite: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if store.Get().ReasoningRewrite {
		t.Error("explicit false must override the default")
	}
}

// TestSetPersistsAndReloads covers the dashboard write path.
func TestSetPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	store, err := Open(path, Settings{BaseURL: "https://a.example/v1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	want := Settings{BaseURL: "https://b.example/v1", APIKey: "secret", ReasoningRewrite: true}
	if err := store.Set(want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	reloaded, err := Open(path, Settings{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reloaded.Get(); got != want {
		t.Errorf("reloaded = %+v, want %+v", got, want)
	}
}

// TestResponsesURL joins the path without doubling slashes.
func TestResponsesURL(t *testing.T) {
	cases := map[string]string{
		"https://api.mistral.ai/v1":  "https://api.mistral.ai/v1/responses",
		"https://api.mistral.ai/v1/": "https://api.mistral.ai/v1/responses",
		"http://127.0.0.1:8787/v1":   "http://127.0.0.1:8787/v1/responses",
	}
	for in, want := range cases {
		if got := (Settings{BaseURL: in}).ResponsesURL(); got != want {
			t.Errorf("ResponsesURL(%q) = %q, want %q", in, got, want)
		}
	}
}
