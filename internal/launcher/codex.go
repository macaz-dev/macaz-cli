package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

func Codex(ctx context.Context, cfg config.Config, options Options) error {
	executable, err := exec.LookPath(cfg.CodexExecutable)
	if err != nil {
		return fmt.Errorf("find Codex executable %q: %w", cfg.CodexExecutable, err)
	}
	profileDir, err := prepareCodexProfile(cfg, options)
	if err != nil {
		return fmt.Errorf("prepare isolated Codex profile: %w", err)
	}
	catalogPath := filepath.Join(profileDir, "macaz-models.json")
	args, err := codexGatewayArgs(options.Args, options, catalogPath, cfg.DefaultEffort)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdin = firstReader(options.Stdin, os.Stdin)
	command.Stdout = firstWriter(options.Stdout, os.Stdout)
	command.Stderr = firstWriter(options.Stderr, os.Stderr)
	configureGracefulShutdown(command)
	command.Env = withEnvironment(os.Environ(), map[string]string{
		"CODEX_HOME":          profileDir,
		"MACAZ_GATEWAY_TOKEN": options.Token,
		"MACAZ_ACTIVE":        "1",
	})
	defer restoreTerminalAfterClaude(command.Stdin, command.Stdout, command.Stderr)()
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("Codex exited with code %d", exit.ExitCode())
		}
		return fmt.Errorf("run Codex: %w", err)
	}
	return nil
}

func prepareCodexProfile(cfg config.Config, options Options) (string, error) {
	profileDir, err := config.CodexProfileDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return "", err
	}
	sourceDir, err := sourceCodexHome(profileDir)
	if err != nil {
		return "", err
	}
	if err := syncCodexBaseConfig(sourceDir, profileDir); err != nil {
		return "", err
	}
	for _, name := range []string{"skills", "plugins", "rules", "prompts", "AGENTS.md", "instructions.md"} {
		if err := shareClaudeAsset(filepath.Join(sourceDir, name), filepath.Join(profileDir, name)); err != nil {
			return "", err
		}
	}
	catalogPath := filepath.Join(profileDir, "macaz-models.json")
	if err := writeCodexModelCatalog(catalogPath, options.Models, options.ModelDetails, options.DefaultModel, cfg.DefaultEffort); err != nil {
		return "", err
	}
	// Runtime overrides make the selected catalog survive client wrappers that
	// replace CODEX_HOME. Remove the obsolete named profile from older builds.
	if err := os.Remove(filepath.Join(profileDir, "macaz.config.toml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return profileDir, nil
}

func sourceCodexHome(profileDir string) (string, error) {
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		clean := filepath.Clean(value)
		if clean != filepath.Clean(profileDir) {
			return clean, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func syncCodexBaseConfig(sourceDir, profileDir string) error {
	source := filepath.Join(sourceDir, "config.toml")
	destination := filepath.Join(profileDir, "config.toml")
	raw, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		if removeErr := os.Remove(destination); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(destination, raw, 0o600)
}

func writeCodexModelCatalog(path string, models []string, details []provider.Model, defaultModel, effort string) error {
	if len(models) == 0 {
		return errors.New("Codex model catalog cannot be empty")
	}
	items := make([]map[string]any, 0, len(models))
	for index, model := range models {
		if strings.TrimSpace(model) == "" {
			continue
		}
		detail := provider.Model{ID: model}
		if index < len(details) && details[index].ID == model {
			detail = details[index]
		}
		items = append(items, codexCatalogModel(detail, model == defaultModel, index, effort))
	}
	if len(items) == 0 {
		return errors.New("Codex model catalog cannot be empty")
	}
	return config.WritePrivateJSON(path, map[string]any{"models": items})
}

func codexCatalogModel(model provider.Model, isDefault bool, priority int, effort string) map[string]any {
	levels := codexReasoningLevels(model.Efforts)
	if len(levels) == 0 {
		levels = codexReasoningLevels([]string{"high"})
	}
	contextWindow := model.ContextWindow
	if contextWindow <= 0 {
		contextWindow = 200000
	}
	displayName := strings.TrimSpace(model.DisplayName)
	if displayName == "" {
		displayName = model.ID
	}
	description := strings.TrimSpace(model.Description)
	if description == "" {
		description = "Available through Macaz."
	}
	inputModalities := codexInputModalities(model.InputModalities)
	if len(inputModalities) == 0 {
		inputModalities = []string{"text"}
	}
	if !isDefault {
		priority++
	} else {
		priority = 0
	}
	return map[string]any{
		"slug":                             model.ID,
		"display_name":                     displayName,
		"description":                      description,
		"default_reasoning_level":          codexDefaultReasoningLevel(levels, effort),
		"supported_reasoning_levels":       levels,
		"shell_type":                       "shell_command",
		"visibility":                       "list",
		"supported_in_api":                 true,
		"priority":                         priority,
		"additional_speed_tiers":           []string{},
		"service_tiers":                    []map[string]any{},
		"availability_nux":                 map[string]any{"message": ""},
		"upgrade":                          nil,
		"base_instructions":                "",
		"features":                         nil,
		"apply_patch_tool_type":            "freeform",
		"web_search_tool_type":             "text_and_image",
		"supports_reasoning_summaries":     true,
		"default_reasoning_summary":        "none",
		"support_verbosity":                true,
		"default_verbosity":                "low",
		"supports_parallel_tool_calls":     true,
		"supports_image_detail_original":   true,
		"context_window":                   contextWindow,
		"max_context_window":               contextWindow,
		"effective_context_window_percent": 95,
		"experimental_supported_tools":     []string{},
		"input_modalities":                 inputModalities,
		"supports_search_tool":             false,
		"use_responses_lite":               false,
		"truncation_policy":                map[string]any{"mode": "tokens", "limit": 10000},
	}
}

func codexDefaultReasoningLevel(levels []map[string]string, configured string) string {
	configured = codexEffort(configured)
	first := "high"
	for index, level := range levels {
		value := level["effort"]
		if index == 0 && value != "" {
			first = value
		}
		if value == configured {
			return configured
		}
	}
	for _, level := range levels {
		if level["effort"] == "high" {
			return "high"
		}
	}
	return first
}

func codexInputModalities(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, 2)
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if (value == "text" || value == "image") && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func codexReasoningLevels(efforts []string) []map[string]string {
	seen := map[string]bool{}
	result := make([]map[string]string, 0, len(efforts))
	for _, effort := range efforts {
		normalized := codexEffort(effort)
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		description := "Provider-specific reasoning level"
		switch normalized {
		case "low":
			description = "Fast responses with lighter reasoning"
		case "medium":
			description = "Balanced reasoning for everyday tasks"
		case "high":
			description = "Greater reasoning depth for complex work"
		case "xhigh":
			description = "Maximum provider reasoning where available"
		}
		result = append(result, map[string]string{"effort": normalized, "description": description})
	}
	return result
}

func codexEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "xhigh", "max", "ultra":
		return "xhigh"
	default:
		return "high"
	}
}

func codexGatewayArgs(args []string, options Options, catalogPath, effort string) ([]string, error) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--profile" || arg == "-p" || strings.HasPrefix(arg, "--profile="):
			return nil, errors.New("macaz owns the Codex configuration profile")
		case arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model="):
			return nil, errors.New("select the routed model with Codex /model")
		case arg == "--oss" || arg == "--local-provider" || strings.HasPrefix(arg, "--local-provider="):
			return nil, errors.New("macaz owns the Codex model provider")
		case arg == "--config" || arg == "-c":
			if index+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			if codexConfigOverridesGateway(args[index+1]) {
				return nil, errors.New("macaz owns Codex model, provider, authentication, and model catalog settings")
			}
			index++
		case strings.HasPrefix(arg, "--config="):
			if codexConfigOverridesGateway(strings.TrimPrefix(arg, "--config=")) {
				return nil, errors.New("macaz owns Codex model, provider, authentication, and model catalog settings")
			}
		}
	}
	baseURL := strings.TrimRight(options.BaseURL, "/") + "/v1"
	overrides := []string{
		fmt.Sprintf("model=%q", options.DefaultModel),
		`model_provider="macaz"`,
		fmt.Sprintf("model_catalog_json=%q", catalogPath),
		fmt.Sprintf("model_reasoning_effort=%q", codexEffort(effort)),
		`web_search="disabled"`,
		`model_providers.macaz.name="Macaz local connection"`,
		fmt.Sprintf("model_providers.macaz.base_url=%q", baseURL),
		`model_providers.macaz.env_key="MACAZ_GATEWAY_TOKEN"`,
		`model_providers.macaz.wire_api="responses"`,
		`model_providers.macaz.request_max_retries=1`,
		`model_providers.macaz.stream_max_retries=1`,
		`model_providers.macaz.stream_idle_timeout_ms=300000`,
	}
	result := make([]string, 0, len(args)+len(overrides)*2)
	for _, override := range overrides {
		result = append(result, "-c", override)
	}
	result = append(result, args...)
	return result, nil
}

func codexConfigOverridesGateway(value string) bool {
	key, _, _ := strings.Cut(strings.TrimSpace(value), "=")
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "model" || key == "model_provider" || key == "model_catalog_json" ||
		key == "openai_base_url" || strings.HasPrefix(key, "model_providers.macaz")
}
