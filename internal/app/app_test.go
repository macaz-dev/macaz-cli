package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

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
