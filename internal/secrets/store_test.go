package secrets

import (
	"testing"

	"github.com/zalando/go-keyring"
)

func TestSetGetDeleteAll(t *testing.T) {
	keyring.MockInit()
	t.Setenv("OPENAI_API_KEY", "")
	if err := Set(OpenAIAPIKey, "secret-value"); err != nil {
		t.Fatal(err)
	}
	value, err := Get(OpenAIAPIKey, "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if value != "secret-value" {
		t.Fatalf("value = %q", value)
	}
	configured, err := Configured(OpenAIAPIKey, "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Fatal("stored credential was reported as unconfigured")
	}
	if err := DeleteAll(); err != nil {
		t.Fatal(err)
	}
	configured, err = Configured(OpenAIAPIKey, "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		t.Fatal("deleted credential was reported as configured")
	}
	if _, err := Get(OpenAIAPIKey, "OPENAI_API_KEY"); err == nil {
		t.Fatal("Get after DeleteAll returned no error")
	}
}

func TestConfiguredUsesEnvironmentOverride(t *testing.T) {
	keyring.MockInit()
	t.Setenv("MACAZ_TEST_SECRET", "from-environment")
	configured, err := Configured("missing-secret", "MACAZ_TEST_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Fatal("environment credential was reported as unconfigured")
	}
}
