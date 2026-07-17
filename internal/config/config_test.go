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
	claude, err := got.ForClient(ClientClaude)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Provider != ProviderCodexCLI || got.CodexExecutable != "wcodex" {
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
	claude, err := loaded.ForClient(ClientClaude)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Provider != ProviderOpenCodeCLI || claude.OpenCodeModel != "fake/next" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func TestClientProfilesAreIndependentAndLegacyConfigMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 1,
  "provider": "openrouter-api",
  "openrouter_model": "legacy/model",
  "model_map": {"default":"legacy/model"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadPath(path)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := cfg.ForClient(ClientClaude)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 2 || claude.Provider != ProviderOpenRouterAPI || claude.ResolveModel("default") != "legacy/model" {
		t.Fatalf("migrated config = %#v", cfg)
	}
	codex := cfg
	codex.Provider = ProviderAnthropicAPI
	codex.AnthropicModel = "claude-test"
	codex.CodexExecutable = "codex-custom"
	codex.OpenCodeExecutable = "opencode-custom"
	codex.ModelMap = map[string]string{"default": "claude-test"}
	cfg.SetClient(ClientCodex, codex)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	selected, err := cfg.ForClient(ClientCodex)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider != ProviderAnthropicAPI || selected.ResolveModel("default") != "claude-test" {
		t.Fatalf("Codex config = %#v", selected)
	}
	if cfg.CodexExecutable != "codex-custom" || cfg.OpenCodeExecutable != "opencode-custom" {
		t.Fatalf("client executables were not persisted: %#v", cfg)
	}
	claude, _ = cfg.ForClient(ClientClaude)
	if claude.Provider != ProviderOpenRouterAPI || claude.ResolveModel("default") != "legacy/model" {
		t.Fatalf("Claude config changed = %#v", claude)
	}
}

func TestCodexClientRejectsRecursiveCodexCLIProvider(t *testing.T) {
	cfg := Default()
	selected := cfg
	selected.Provider = ProviderCodexCLI
	cfg.SetClient(ClientCodex, selected)
	if err := cfg.Validate(); err == nil {
		t.Fatal("Codex client accepted Codex CLI as its upstream")
	}
}

func TestClientProfilesRejectRedundantDirectProviders(t *testing.T) {
	tests := []struct {
		client   string
		provider string
	}{
		{client: ClientClaude, provider: ProviderAnthropicAPI},
		{client: ClientCodex, provider: ProviderOpenAISubscription},
		{client: ClientCodex, provider: ProviderOpenAIAPIKey},
	}
	for _, test := range tests {
		t.Run(test.client+"-"+test.provider, func(t *testing.T) {
			cfg := Default()
			selected := cfg
			selected.Provider = test.provider
			cfg.SetClient(test.client, selected)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s client accepted redundant provider %s", test.client, test.provider)
			}
		})
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
