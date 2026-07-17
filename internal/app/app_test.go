package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
	"github.com/macaz-dev/macaz-cli/internal/updater"
)

type fakeUpdateClient struct {
	checkResult  updater.Release
	checkErr     error
	updateResult updater.Result
	updateErr    error
	checks       int
	updates      int
}

func (client *fakeUpdateClient) Check(context.Context) (updater.Release, error) {
	client.checks++
	return client.checkResult, client.checkErr
}

func (client *fakeUpdateClient) Update(context.Context) (updater.Result, error) {
	client.updates++
	return client.updateResult, client.updateErr
}

func init() {
	keyring.MockInit()
}

func TestResetRemovesOnlyMacazConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("MACAZ_CONFIG", path)
	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	profileDir, err := config.ClaudeProfileDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "settings.json"), []byte(`{"model":"claude-macaz-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(secrets.OpenAIAPIKey, "secret-to-remove"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"reset"}, Streams{Out: &output, Err: &output}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("isolated Claude profile still exists or stat failed: %v", err)
	}
	if !strings.Contains(output.String(), "Vendor CLI credentials") {
		t.Fatalf("output = %q", output.String())
	}
	if _, err := secrets.Get(secrets.OpenAIAPIKey, ""); err == nil {
		t.Fatal("macaz reset retained a macaz credential")
	}
}

func TestClientResetRemovesOnlySelectedClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("MACAZ_CONFIG", path)
	cfg := config.Default()
	claude := cfg
	claude.Provider = config.ProviderOpenAIAPIKey
	claude.OpenAIModel = "gpt-claude"
	claude.ModelMap = map[string]string{"default": "gpt-claude"}
	cfg.SetClient(config.ClientClaude, claude)
	codex := cfg
	codex.Provider = config.ProviderAnthropicAPI
	codex.AnthropicModel = "claude-codex"
	codex.ModelMap = map[string]string{"default": "claude-codex"}
	cfg.SetClient(config.ClientCodex, codex)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	claudeProfile, _ := config.ClaudeProfileDir()
	codexProfile, _ := config.CodexProfileDir()
	for _, dir := range []string{claudeProfile, codexProfile} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.Set(secrets.AnthropicAPIKey, "shared-secret"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"reset", "codex"}, Streams{Out: &output, Err: &output}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.HasClient(config.ClientClaude) || loaded.HasClient(config.ClientCodex) {
		t.Fatalf("clients after reset = %#v", loaded.Clients)
	}
	if _, err := os.Stat(claudeProfile); err != nil {
		t.Fatalf("Claude profile was removed: %v", err)
	}
	if _, err := os.Stat(codexProfile); !os.IsNotExist(err) {
		t.Fatalf("Codex profile still exists or stat failed: %v", err)
	}
	if value, err := secrets.Get(secrets.AnthropicAPIKey, ""); err != nil || value != "shared-secret" {
		t.Fatalf("shared Anthropic credential = %q, error = %v", value, err)
	}
}

func TestProviderModelByID(t *testing.T) {
	models := []provider.Model{{ID: "first"}, {ID: "provider/model", Efforts: []string{"high"}}}
	model, ok := providerModelByID(models, "provider/model")
	if !ok || model.ID != "provider/model" || len(model.Efforts) != 1 {
		t.Fatalf("model = %#v, ok = %t", model, ok)
	}
	if _, ok := providerModelByID(models, "missing"); ok {
		t.Fatal("missing model was reported as available")
	}
}

func TestActiveProviderModelFallsBackToLiveDefault(t *testing.T) {
	models := []provider.Model{
		{ID: "gpt-5.6-sol", Default: true},
		{ID: "gpt-5.6-terra"},
	}
	model, ok := activeProviderModel(models, "gpt-5.6")
	if !ok || model.ID != "gpt-5.6-sol" {
		t.Fatalf("model = %#v, ok = %t", model, ok)
	}
	model, ok = activeProviderModel(models, "gpt-5.6-terra")
	if !ok || model.ID != "gpt-5.6-terra" {
		t.Fatalf("explicit model = %#v, ok = %t", model, ok)
	}
	if _, ok := activeProviderModel(nil, "gpt-5.6"); ok {
		t.Fatal("empty catalog returned an active model")
	}
}

func TestHelpKeepsConfigurationSurfaceMinimal(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"help"}, Streams{Out: &output}); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"macaz provider", "macaz model", "macaz models", "macaz effort"} {
		if strings.Contains(output.String(), removed) {
			t.Fatalf("help still advertises removed command %q: %s", removed, output.String())
		}
	}
	if !strings.Contains(output.String(), "macaz update") {
		t.Fatalf("help does not advertise self-update: %s", output.String())
	}
}

func TestRunWithoutCommandShowsHelpInsteadOfStartingAClient(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), nil, Streams{Out: &output, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "macaz claude") || !strings.Contains(output.String(), "macaz codex") {
		t.Fatalf("missing explicit client commands: %s", output.String())
	}
	if strings.Contains(output.String(), "backward-compatible") {
		t.Fatalf("help still advertises the removed implicit alias: %s", output.String())
	}
}

func TestUnknownCommandDoesNotStartClaude(t *testing.T) {
	err := Run(context.Background(), []string{"not-a-command"}, Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v", err)
	}
}

func TestWizardOnlyOffersUsefulProvidersForEachClient(t *testing.T) {
	tests := []struct {
		client  string
		present []string
		absent  []string
	}{
		{
			client:  config.ClientClaude,
			present: []string{"OpenAI Subscription", "OpenAI API", "OpenRouter API", "Codex-CLI", "OpenCode-CLI"},
			absent:  []string{"Anthropic API"},
		},
		{
			client:  config.ClientCodex,
			present: []string{"OpenRouter API", "Anthropic API", "OpenCode-CLI"},
			absent:  []string{"OpenAI Subscription", "OpenAI API", "Codex-CLI"},
		},
	}
	for _, test := range tests {
		t.Run(test.client, func(t *testing.T) {
			var output bytes.Buffer
			_, _ = wizard(context.Background(), test.client, config.Default(), Streams{
				In: strings.NewReader("invalid\n"), Out: &output, Err: &output,
			})
			for _, value := range test.present {
				if !strings.Contains(output.String(), value) {
					t.Fatalf("wizard is missing %q: %s", value, output.String())
				}
			}
			for _, value := range test.absent {
				if strings.Contains(output.String(), value) {
					t.Fatalf("wizard still offers %q: %s", value, output.String())
				}
			}
		})
	}
}

func TestAnthropicWizardSelectsDefaultFromLiveCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "wizard-anthropic-key" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-current-one","display_name":"Claude Current One"},{"id":"claude-current-two","display_name":"Claude Current Two"}]}`))
	}))
	defer server.Close()
	input := strings.Join([]string{"2", "wizard-anthropic-key", server.URL + "/v1", "2", ""}, "\n")
	var output bytes.Buffer
	selected, err := wizard(context.Background(), config.ClientCodex, config.Default(), Streams{
		In: strings.NewReader(input), Out: &output, Err: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider != config.ProviderAnthropicAPI || selected.AnthropicModel != "claude-current-two" || selected.ResolveModel("default") != "claude-current-two" {
		t.Fatalf("selected config = %#v", selected)
	}
	for _, value := range []string{"Claude Current One", "Claude Current Two"} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("live catalog is missing %q: %s", value, output.String())
		}
	}
}

func TestLegalNoticeIsAvailableFromCLI(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"legal"}, Streams{Out: &output}); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"independent interoperability project", "not affiliated", "authorized by", "sponsored by", "installed separately", "responsible for complying"} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("legal notice is missing %q: %s", required, output.String())
		}
	}
}

func TestRunNotifiesWhenUpdateIsAvailable(t *testing.T) {
	fake := &fakeUpdateClient{checkResult: updater.Release{
		Current:   "v1.2.3",
		Latest:    "v1.2.4",
		Available: true,
	}}
	restoreUpdaterGlobals(t, "v1.2.3", fake)
	var output bytes.Buffer
	var errorOutput bytes.Buffer
	if err := Run(context.Background(), []string{"help"}, Streams{Out: &output, Err: &errorOutput}); err != nil {
		t.Fatal(err)
	}
	if fake.checks != 1 || fake.updates != 0 {
		t.Fatalf("checks = %d, updates = %d", fake.checks, fake.updates)
	}
	for _, required := range []string{"update available", "v1.2.4", "current v1.2.3", "macaz update"} {
		if !strings.Contains(errorOutput.String(), required) {
			t.Fatalf("notification is missing %q: %s", required, errorOutput.String())
		}
	}
}

func TestAutomaticUpdateCheckCanBeDisabled(t *testing.T) {
	fake := &fakeUpdateClient{}
	restoreUpdaterGlobals(t, "v1.2.3", fake)
	t.Setenv("MACAZ_NO_UPDATE_CHECK", "1")
	if err := Run(context.Background(), []string{"help"}, Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if fake.checks != 0 || fake.updates != 0 {
		t.Fatalf("checks = %d, updates = %d", fake.checks, fake.updates)
	}
}

func TestAutomaticUpdateCheckFailureIsSilent(t *testing.T) {
	fake := &fakeUpdateClient{checkErr: errors.New("network unavailable")}
	restoreUpdaterGlobals(t, "v1.2.3", fake)
	var errorOutput bytes.Buffer
	if err := Run(context.Background(), []string{"help"}, Streams{Out: &bytes.Buffer{}, Err: &errorOutput}); err != nil {
		t.Fatal(err)
	}
	if fake.checks != 1 || errorOutput.Len() != 0 {
		t.Fatalf("checks = %d, error output = %q", fake.checks, errorOutput.String())
	}
}

func TestUpdateCommandReplacesWithoutDuplicateCheck(t *testing.T) {
	fake := &fakeUpdateClient{updateResult: updater.Result{
		Current: "v1.2.3",
		Latest:  "v1.2.4",
		Path:    "/usr/local/bin/macaz",
		Updated: true,
	}}
	restoreUpdaterGlobals(t, "v1.2.3", fake)
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"update"}, Streams{Out: &output, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if fake.checks != 0 || fake.updates != 1 {
		t.Fatalf("checks = %d, updates = %d", fake.checks, fake.updates)
	}
	for _, required := range []string{"Checking for macaz updates", "v1.2.3 → v1.2.4", "/usr/local/bin/macaz"} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("update output is missing %q: %s", required, output.String())
		}
	}
}

func TestUpdateCommandRejectsArguments(t *testing.T) {
	fake := &fakeUpdateClient{}
	restoreUpdaterGlobals(t, "v1.2.3", fake)
	err := Run(context.Background(), []string{"update", "unexpected"}, Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "usage: macaz update") {
		t.Fatalf("error = %v", err)
	}
	if fake.checks != 0 || fake.updates != 0 {
		t.Fatalf("checks = %d, updates = %d", fake.checks, fake.updates)
	}
}

func restoreUpdaterGlobals(t *testing.T, version string, fake updateClient) {
	t.Helper()
	previousVersion := Version
	previousFactory := newUpdateClient
	Version = version
	newUpdateClient = func(string) updateClient { return fake }
	t.Cleanup(func() {
		Version = previousVersion
		newUpdateClient = previousFactory
	})
}
