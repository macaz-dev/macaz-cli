package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

type fakeClaudeReport struct {
	Args        []string          `json:"args"`
	Environment map[string]string `json:"environment"`
}

func TestMain(m *testing.M) {
	if os.Getenv("MACAZ_FAKE_CODEX") == "1" {
		report := fakeClaudeReport{
			Args: append([]string(nil), os.Args[1:]...),
			Environment: map[string]string{
				"CODEX_HOME":          os.Getenv("CODEX_HOME"),
				"MACAZ_GATEWAY_TOKEN": os.Getenv("MACAZ_GATEWAY_TOKEN"),
				"MACAZ_ACTIVE":        os.Getenv("MACAZ_ACTIVE"),
			},
		}
		raw, _ := json.Marshal(report)
		_ = os.WriteFile(os.Getenv("MACAZ_FAKE_CODEX_REPORT"), raw, 0o600)
		os.Exit(0)
	}
	if os.Getenv("MACAZ_FAKE_CLAUDE") == "1" {
		if len(os.Args) >= 3 && os.Args[1] == "daemon" && os.Args[2] == "stop" {
			os.Exit(0)
		}
		report := fakeClaudeReport{
			Args:        append([]string(nil), os.Args[1:]...),
			Environment: map[string]string{},
		}
		for _, key := range []string{
			"ANTHROPIC_BASE_URL",
			"ANTHROPIC_AUTH_TOKEN",
			"ANTHROPIC_API_KEY",
			"ANTHROPIC_MODEL",
			"ANTHROPIC_DEFAULT_FABLE_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL",
			"ANTHROPIC_DEFAULT_OPUS_MODEL",
			"ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_SMALL_FAST_MODEL",
			"CLAUDE_CODE_AUTO_MODE_MODEL",
			"CLAUDE_CODE_BG_CLASSIFIER_MODEL",
			"CLAUDE_CODE_SUBAGENT_MODEL",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
			"CLAUDE_CODE_DISABLE_LEGACY_MODEL_REMAP",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
			"CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK",
			"CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK",
			"CLAUDE_CODE_USE_GATEWAY",
			"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY",
			"CLAUDE_CODE_ALWAYS_ENABLE_EFFORT",
			"DISABLE_TELEMETRY",
			"DISABLE_ERROR_REPORTING",
			"DISABLE_FEEDBACK_COMMAND",
			"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY",
			"DO_NOT_TRACK",
			"CLAUDE_CONFIG_DIR",
			"MACAZ_ACTIVE",
		} {
			report.Environment[key] = os.Getenv(key)
		}
		raw, _ := json.Marshal(report)
		var interrupt chan os.Signal
		if os.Getenv("MACAZ_FAKE_CLAUDE_WAIT") == "1" {
			interrupt = make(chan os.Signal, 1)
			signal.Notify(interrupt, os.Interrupt)
		}
		_ = os.WriteFile(os.Getenv("MACAZ_FAKE_CLAUDE_REPORT"), raw, 0o600)
		if interrupt != nil {
			<-interrupt
			_ = os.WriteFile(os.Getenv("MACAZ_FAKE_CLAUDE_REPORT")+".interrupted", []byte("yes\n"), 0o600)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCodexUsesIsolatedOfficialProfileAndMappedModelCatalog(t *testing.T) {
	root := t.TempDir()
	reportPath := filepath.Join(root, "report.json")
	sourceProfile := filepath.Join(root, "source-codex")
	if err := os.MkdirAll(filepath.Join(sourceProfile, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceProfile, "config.toml"), []byte("personality = \"pragmatic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceProfile, "skills", "example.md"), []byte("skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
	t.Setenv("CODEX_HOME", sourceProfile)
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)
	cfg := config.Default()
	cfg.CodexExecutable = os.Args[0]
	models := []string{"macaz-primary-a1", "macaz-secondary-b2"}
	if err := Codex(context.Background(), cfg, Options{
		BaseURL:      "http://127.0.0.1:54321",
		Token:        "loopback-secret",
		Models:       models,
		DefaultModel: models[0],
		ModelDetails: []provider.Model{
			{ID: models[0], DisplayName: "Primary", Description: "Primary routed model", Default: true, Efforts: []string{"low", "medium", "high"}, InputModalities: []string{"text", "image"}, ContextWindow: 128000},
			{ID: models[1], DisplayName: "Secondary", Efforts: []string{"medium"}, InputModalities: []string{"text"}, ContextWindow: 64000},
		},
		Args: []string{"exec", "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeClaudeReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Args) < 2 || !slices.Equal(report.Args[len(report.Args)-2:], []string{"exec", "hello"}) {
		t.Fatalf("client args = %#v", report.Args)
	}
	joinedArgs := strings.Join(report.Args, "\n")
	for _, required := range []string{
		`model="macaz-primary-a1"`, `model_provider="macaz"`,
		`model_catalog_json=`, `model_reasoning_effort="medium"`,
		`model_providers.macaz.base_url="http://127.0.0.1:54321/v1"`,
		`model_providers.macaz.wire_api="responses"`, `web_search="disabled"`,
	} {
		if !strings.Contains(joinedArgs, required) {
			t.Fatalf("runtime arguments are missing %q: %#v", required, report.Args)
		}
	}
	if strings.Contains(joinedArgs, "--profile") {
		t.Fatalf("Codex still depends on a named profile: %#v", report.Args)
	}
	if report.Environment["MACAZ_GATEWAY_TOKEN"] != "loopback-secret" || report.Environment["MACAZ_ACTIVE"] != "1" {
		t.Fatalf("environment = %#v", report.Environment)
	}
	profileDir := report.Environment["CODEX_HOME"]
	if profileDir == "" || profileDir == sourceProfile {
		t.Fatalf("isolated CODEX_HOME = %q", profileDir)
	}
	if strings.Contains(joinedArgs, "loopback-secret") {
		t.Fatalf("local token leaked into arguments: %#v", report.Args)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "macaz.config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete named profile still exists: %v", err)
	}
	if copied, err := os.ReadFile(filepath.Join(profileDir, "config.toml")); err != nil || string(copied) != "personality = \"pragmatic\"\n" {
		t.Fatalf("base config copy = %q, error = %v", copied, err)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "skills", "example.md")); err != nil {
		t.Fatalf("shared Codex skills: %v", err)
	}
	catalogRaw, err := os.ReadFile(filepath.Join(profileDir, "macaz-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(catalogRaw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 || catalog.Models[0]["slug"] != models[0] || catalog.Models[0]["display_name"] != "Primary" || catalog.Models[0]["context_window"] != float64(128000) {
		t.Fatalf("catalog = %#v", catalog.Models)
	}
	if got := catalog.Models[0]["input_modalities"]; !slices.Equal(got.([]any), []any{"text", "image"}) {
		t.Fatalf("input modalities = %#v", got)
	}
}

func TestCodexInputModalitiesExcludeUnsupportedProviderKinds(t *testing.T) {
	got := codexInputModalities([]string{"text", "document", "image", "audio", "IMAGE"})
	if !slices.Equal(got, []string{"text", "image"}) {
		t.Fatalf("modalities = %#v", got)
	}
}

func TestCodexGatewayArgsFailClosed(t *testing.T) {
	options := Options{BaseURL: "http://127.0.0.1:1234", DefaultModel: "macaz-default"}
	for _, args := range [][]string{
		{"--model", "gpt-native"},
		{"--profile", "other"},
		{"--oss"},
		{"-c", "model_provider=other"},
		{"--config=model_catalog_json=/tmp/other.json"},
	} {
		if _, err := codexGatewayArgs(args, options, "/tmp/macaz-models.json", "high"); err == nil {
			t.Fatalf("gateway override was accepted: %#v", args)
		}
	}
	args, err := codexGatewayArgs([]string{"--dangerously-bypass-approvals-and-sandbox", "exec", "hello"}, options, "/tmp/macaz-models.json", "high")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("explicit permission override was dropped: %#v", args)
	}
}

func TestClaudeUsesNormalPermissionsByDefaultAndReturnsWhenItExits(t *testing.T) {
	root := t.TempDir()
	reportPath := filepath.Join(root, "report.json")
	sourceProfile := filepath.Join(root, "source-claude")
	if err := os.MkdirAll(filepath.Join(sourceProfile, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceProfile, "skills", "example.md"), []byte("skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sourceProfile, "settings.json"),
		[]byte(`{"model":"claude-macaz-old","theme":"dark","env":{"ANTHROPIC_API_KEY":"secret","SAFE":"yes"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", sourceProfile)
	t.Setenv("MACAZ_FAKE_CLAUDE", "1")
	t.Setenv("MACAZ_FAKE_CLAUDE_REPORT", reportPath)
	t.Setenv("CLAUDE_CODE_ALWAYS_ENABLE_EFFORT", "1")
	cfg := config.Default()
	cfg.ClaudeExecutable = os.Args[0]
	if err := Claude(context.Background(), cfg, Options{
		BaseURL:      "http://127.0.0.1:54321",
		Token:        "loopback-secret",
		Models:       []string{"claude-macaz-fake-a1b2c3d4", "claude-macaz-other-e5f6a7b8"},
		ModelDetails: []provider.Model{{ID: "claude-macaz-fake-a1b2c3d4", ContextWindow: 272000}},
		DefaultModel: "claude-macaz-fake-a1b2c3d4",
		Args:         []string{"--model", "sonnet"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeClaudeReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Args) != 4 || !slices.Equal(report.Args[:2], []string{
		"--model",
		"claude-macaz-fake-a1b2c3d4",
	}) {
		t.Fatalf("args = %#v", report.Args)
	}
	if slices.Contains(report.Args, "--dangerously-skip-permissions") {
		t.Fatalf("full permissions were enabled without an explicit user flag: %#v", report.Args)
	}
	if report.Args[2] != "--managed-settings" {
		t.Fatalf("managed settings flag missing: %#v", report.Args)
	}
	var managed map[string]any
	if err := json.Unmarshal([]byte(report.Args[3]), &managed); err != nil {
		t.Fatalf("decode managed settings: %v", err)
	}
	if managed["model"] != "claude-macaz-fake-a1b2c3d4" || managed["enforceAvailableModels"] != true {
		t.Fatalf("managed settings = %#v", managed)
	}
	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                         "http://127.0.0.1:54321",
		"ANTHROPIC_AUTH_TOKEN":                       "loopback-secret",
		"ANTHROPIC_API_KEY":                          "loopback-secret",
		"ANTHROPIC_MODEL":                            "claude-macaz-fake-a1b2c3d4",
		"ANTHROPIC_SMALL_FAST_MODEL":                 "claude-macaz-fake-a1b2c3d4",
		"CLAUDE_CODE_AUTO_MODE_MODEL":                "claude-macaz-fake-a1b2c3d4",
		"CLAUDE_CODE_BG_CLASSIFIER_MODEL":            "claude-macaz-fake-a1b2c3d4",
		"CLAUDE_CODE_SUBAGENT_MODEL":                 "claude-macaz-fake-a1b2c3d4",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":            "272000",
		"CLAUDE_CODE_DISABLE_LEGACY_MODEL_REMAP":     "1",
		"CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK":  "1",
		"CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK":       "1",
		"CLAUDE_CODE_USE_GATEWAY":                    "1",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
		"DISABLE_ERROR_REPORTING":                    "1",
		"DISABLE_FEEDBACK_COMMAND":                   "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":        "1",
		"DO_NOT_TRACK":                               "1",
		"MACAZ_ACTIVE":                               "1",
	} {
		if report.Environment[key] != want {
			t.Fatalf("%s = %q, want %q", key, report.Environment[key], want)
		}
	}
	if report.Environment["CLAUDE_CODE_ALWAYS_ENABLE_EFFORT"] != "" {
		t.Fatalf("CLAUDE_CODE_ALWAYS_ENABLE_EFFORT should be unset: %#v", report.Environment)
	}
	if report.Environment["DISABLE_TELEMETRY"] != "" || report.Environment["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "" {
		t.Fatalf("feature-flag-disabling environment should be unset: %#v", report.Environment)
	}
	for _, key := range []string{
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
	} {
		if report.Environment[key] != "" {
			t.Fatalf("%s unexpectedly created a Claude family alias row: %q", key, report.Environment[key])
		}
	}
	profileDir := report.Environment["CLAUDE_CONFIG_DIR"]
	if profileDir == "" || profileDir == sourceProfile {
		t.Fatalf("isolated Claude profile = %q", profileDir)
	}
	raw, err = os.ReadFile(filepath.Join(profileDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["model"] != "claude-macaz-fake-a1b2c3d4" || settings["theme"] != "dark" {
		t.Fatalf("isolated settings = %#v", settings)
	}
	if settings["enforceAvailableModels"] != true {
		t.Fatalf("isolated model enforcement = %#v", settings)
	}
	if settings["effortLevel"] != "medium" {
		t.Fatalf("isolated default effort = %#v", settings)
	}
	env, _ := settings["env"].(map[string]any)
	if _, exists := env["ANTHROPIC_API_KEY"]; exists || env["SAFE"] != "yes" {
		t.Fatalf("isolated settings env = %#v", env)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "skills", "example.md")); err != nil {
		t.Fatalf("shared Claude skills: %v", err)
	}
	raw, err = os.ReadFile(filepath.Join(sourceProfile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	settings = nil
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if _, exists := settings["model"]; exists {
		t.Fatalf("legacy global gateway model was retained: %#v", settings)
	}
}

func TestConfiguredEffortUpdatesOnceThenPreservesClaudeSelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "source-claude"))
	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	models := []string{"claude-macaz-primary-a1"}
	profileDir, _, err := prepareClaudeProfile(cfg, models, models[0])
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(profileDir, "settings.json")
	var settings map[string]any
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["effortLevel"] != "medium" {
		t.Fatalf("initial effort = %#v", settings)
	}

	settings["effortLevel"] = "low"
	if err := config.WritePrivateJSON(settingsPath, settings); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareClaudeProfile(cfg, models, models[0]); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["effortLevel"] != "low" {
		t.Fatalf("Claude-selected effort was overwritten: %#v", settings)
	}

	cfg.DefaultEffort = "high"
	if _, _, err := prepareClaudeProfile(cfg, models, models[0]); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["effortLevel"] != "high" {
		t.Fatalf("updated configured effort was not applied: %#v", settings)
	}
}

func TestClaudePinsInternalModelsWithoutCreatingFamilyAliasRows(t *testing.T) {
	root := t.TempDir()
	reportPath := filepath.Join(root, "report.json")
	t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "source-claude"))
	t.Setenv("MACAZ_FAKE_CLAUDE", "1")
	t.Setenv("MACAZ_FAKE_CLAUDE_REPORT", reportPath)
	cfg := config.Default()
	cfg.ClaudeExecutable = os.Args[0]
	models := []string{"claude-macaz-sol", "claude-macaz-terra"}
	if err := Claude(context.Background(), cfg, Options{
		BaseURL: "http://127.0.0.1:54321",
		Token:   "loopback-secret",
		Models:  models,
		ModelDetails: []provider.Model{
			{ID: models[0], ContextWindow: 272000},
			{ID: models[1], ContextWindow: 272000},
		},
		DefaultModel: models[0],
		Args:         []string{"--model", models[1]},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeClaudeReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
	} {
		if report.Environment[key] != "" {
			t.Fatalf("%s unexpectedly created a Claude family alias row: %q", key, report.Environment[key])
		}
	}
	for _, key := range []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_AUTO_MODE_MODEL",
		"CLAUDE_CODE_BG_CLASSIFIER_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
	} {
		if report.Environment[key] != models[1] {
			t.Fatalf("%s = %q, want selected public model", key, report.Environment[key])
		}
	}
	if report.Environment["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "272000" {
		t.Fatalf("auto compact window = %q", report.Environment["CLAUDE_CODE_AUTO_COMPACT_WINDOW"])
	}
	if report.Environment["CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK"] != "1" {
		t.Fatalf(
			"CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK = %q",
			report.Environment["CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK"],
		)
	}
}

func TestGatewayArgsFailClosed(t *testing.T) {
	models := []string{"claude-macaz-primary-a1", "claude-macaz-secondary-b2"}
	args, selected, err := gatewayArgs(
		[]string{"--model", "opus", "--print", "hello"},
		models,
		models[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected != models[0] || args[1] != models[0] {
		t.Fatalf("resolved args = %#v, selected = %q", args, selected)
	}
	withBypass, _, err := gatewayArgs(
		[]string{"--dangerously-skip-permissions", "--model", "sonnet"},
		models,
		models[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(withBypass, "--dangerously-skip-permissions") {
		t.Fatalf("explicit permission bypass was not forwarded: %#v", withBypass)
	}
	if _, _, err := gatewayArgs([]string{"--model", "claude-opus-4-8"}, models, models[0]); err == nil {
		t.Fatal("official Claude model was accepted inside macaz")
	}
	if _, _, err := gatewayArgs([]string{"--fallback-model", "opus"}, models, models[0]); err == nil {
		t.Fatal("client-side provider fallback was accepted inside macaz")
	}
}

func TestMigrateLegacyGatewaySessionsSkipsActiveSessions(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	profile := filepath.Join(root, "profile")
	project := filepath.Join(source, "projects", "-work")
	if err := os.MkdirAll(filepath.Join(project, "inactive", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "session-env", "inactive"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(project, "inactive.jsonl"):                       `{"message":{"model":"claude-macaz-gpt-a1"}}` + "\n",
		filepath.Join(project, "inactive", "subagents", "agent.jsonl"): `{"type":"user"}` + "\n",
		filepath.Join(source, "session-env", "inactive", "session.sh"): "export TEST=1\n",
		filepath.Join(project, "active.jsonl"):                         `{"message":{"model":"claude-macaz-gpt-a1"}}` + "\n",
		filepath.Join(project, "native.jsonl"):                         `{"message":{"model":"claude-opus-4-8"}}` + "\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(source, "daemon"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(source, "daemon", "roster.json"),
		[]byte(`{"workers":{"active":{"sessionId":"active"}}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyGatewaySessions(source, profile); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(profile, "projects", "-work", "inactive.jsonl"),
		filepath.Join(profile, "projects", "-work", "inactive", "subagents", "agent.jsonl"),
		filepath.Join(profile, "session-env", "inactive", "session.sh"),
		filepath.Join(source, "projects", "-work", "active.jsonl"),
		filepath.Join(source, "projects", "-work", "native.jsonl"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected preserved or migrated path %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "projects", "-work", "inactive.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inactive legacy session was not removed from global history: %v", err)
	}
}
