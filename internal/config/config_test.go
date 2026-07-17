package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveLoadPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := Default()
	cfg.Provider = ProviderCodexCLI
	cfg.CodexExecutable = "wcodex"
	if err := SavePath(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderCodexCLI || got.CodexExecutable != "wcodex" {
		t.Fatalf("unexpected config: %#v", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config is too permissive: %o", info.Mode().Perm())
	}
}

func TestSavePathReplacesExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first := Default()
	first.Provider = ProviderCodexCLI
	if err := SavePath(path, first); err != nil {
		t.Fatal(err)
	}
	second := Default()
	second.Provider = ProviderOpenCodeCLI
	second.OpenCodeModel = "fake/next"
	if err := SavePath(path, second); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider != ProviderOpenCodeCLI || loaded.OpenCodeModel != "fake/next" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func TestResolveModel(t *testing.T) {
	cfg := Default()
	tests := map[string]string{
		"sonnet":                      "gpt-5.6",
		"claude-sonnet-4-6":           "gpt-5.6",
		"custom-openai-model":         "custom-openai-model",
		"claude-unknown-future-model": "gpt-5.6",
	}
	for input, want := range tests {
		if got := cfg.ResolveModel(input); got != want {
			t.Fatalf("ResolveModel(%q) = %q, want %q", input, got, want)
		}
	}
	cfg.Provider = ProviderOpenRouterAPI
	cfg.ModelMap["sonnet"] = "configured/default"
	if got := cfg.ResolveModel("anthropic/claude-sonnet-4.6"); got != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("provider model was remapped: %q", got)
	}
}
