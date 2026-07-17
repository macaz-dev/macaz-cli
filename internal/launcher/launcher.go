package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/macaz-dev/macaz-cli/internal/config"
)

type Options struct {
	BaseURL      string
	Token        string
	Models       []string
	DefaultModel string
	Args         []string
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
}

var gatewayModelJSON = regexp.MustCompile(`"model"\s*:\s*"claude-macaz-`)

func Claude(ctx context.Context, cfg config.Config, options Options) error {
	executable, err := exec.LookPath(cfg.ClaudeExecutable)
	if err != nil {
		return fmt.Errorf("find Claude executable %q: %w", cfg.ClaudeExecutable, err)
	}
	profileDir, selectedModel, err := prepareClaudeProfile(cfg, options.Models, options.DefaultModel)
	if err != nil {
		return fmt.Errorf("prepare isolated Claude profile: %w", err)
	}
	args, launchModel, err := gatewayArgs(options.Args, options.Models, selectedModel)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdin = firstReader(options.Stdin, os.Stdin)
	command.Stdout = firstWriter(options.Stdout, os.Stdout)
	command.Stderr = firstWriter(options.Stderr, os.Stderr)
	configureGracefulShutdown(command)
	command.Env = withEnvironment(os.Environ(), map[string]string{
		"ANTHROPIC_BASE_URL":   options.BaseURL,
		"ANTHROPIC_AUTH_TOKEN": options.Token,
		"ANTHROPIC_API_KEY":    options.Token,
		"ANTHROPIC_MODEL":      launchModel,
		// Do not set ANTHROPIC_DEFAULT_{FABLE,OPUS,SONNET,HAIKU}_MODEL.
		// Claude Code turns those variables into Custom model-picker rows and
		// deduplicates the discovered provider model they resolve to. Pin the
		// actual internal execution paths instead, keeping every discovered
		// provider model (including the selected one) visible in /model.
		"ANTHROPIC_SMALL_FAST_MODEL":                 launchModel,
		"CLAUDE_CODE_AUTO_MODE_MODEL":                launchModel,
		"CLAUDE_CODE_BG_CLASSIFIER_MODEL":            launchModel,
		"CLAUDE_CODE_SUBAGENT_MODEL":                 launchModel,
		"CLAUDE_CODE_DISABLE_LEGACY_MODEL_REMAP":     "1",
		"CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK":       "1",
		"CLAUDE_CODE_USE_GATEWAY":                    "1",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
		"CLAUDE_CODE_ALWAYS_ENABLE_EFFORT":           "1",
		"DISABLE_TELEMETRY":                          "1",
		"DISABLE_ERROR_REPORTING":                    "1",
		"DISABLE_FEEDBACK_COMMAND":                   "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":        "1",
		"DO_NOT_TRACK":                               "1",
		"CLAUDE_CONFIG_DIR":                          profileDir,
		"MACAZ_ACTIVE":                               "1",
	})
	defer stopClaudeDaemon(executable, command.Env)
	defer restoreTerminalAfterClaude(command.Stdin, command.Stdout, command.Stderr)()
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("Claude exited with code %d", exit.ExitCode())
		}
		return fmt.Errorf("run Claude: %w", err)
	}
	return nil
}

func restoreTerminalAfterClaude(stdin io.Reader, stdout, stderr io.Writer) func() {
	input, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(input.Fd())) {
		return func() {}
	}
	state, err := term.GetState(int(input.Fd()))
	if err != nil {
		return func() {}
	}
	output := terminalFile(stdout)
	if output == nil {
		output = terminalFile(stderr)
	}
	return func() {
		_ = term.Restore(int(input.Fd()), state)
		if output != nil {
			_, _ = io.WriteString(
				output,
				"\x1b[0m\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1015l",
			)
		}
	}
}

func terminalFile(writer io.Writer) *os.File {
	file, ok := writer.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil
	}
	return file
}

func prepareClaudeProfile(cfg config.Config, models []string, defaultModel string) (string, string, error) {
	profileDir, err := config.ClaudeProfileDir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return "", "", err
	}
	sourceDir, err := sourceClaudeConfigDir(profileDir)
	if err != nil {
		return "", "", err
	}
	if err := cleanupLegacyGatewayModel(sourceDir); err != nil {
		return "", "", err
	}
	settingsPath := filepath.Join(profileDir, "settings.json")
	settings, err := loadClaudeSettings(settingsPath, filepath.Join(sourceDir, "settings.json"))
	if err != nil {
		return "", "", err
	}
	scrubGatewaySettings(settings)
	providerChanged, err := profileProviderChanged(profileDir, cfg.Provider)
	if err != nil {
		return "", "", err
	}
	effortChanged, err := profileEffortChanged(profileDir, cfg.DefaultEffort)
	if err != nil {
		return "", "", err
	}
	if providerChanged {
		delete(settings, "model")
		_ = os.Remove(filepath.Join(profileDir, "cache", "gateway-models.json"))
	}
	effortApplied := false
	if providerChanged || effortChanged {
		effortApplied = configureGatewayEffort(settings, cfg.DefaultEffort)
	}
	selectedModel, err := configureGatewayModels(settings, models, defaultModel)
	if err != nil {
		return "", "", err
	}
	if err := config.WritePrivateJSON(settingsPath, settings); err != nil {
		return "", "", err
	}
	if effortApplied {
		if err := writeProfileEffort(profileDir, cfg.DefaultEffort); err != nil {
			return "", "", err
		}
	}
	for _, name := range []string{"agents", "commands", "skills", "plugins", "CLAUDE.md"} {
		if err := shareClaudeAsset(filepath.Join(sourceDir, name), filepath.Join(profileDir, name)); err != nil {
			return "", "", err
		}
	}
	if err := writeProfileProvider(profileDir, cfg.Provider); err != nil {
		return "", "", err
	}
	if err := migrateLegacyGatewaySessions(sourceDir, profileDir); err != nil {
		return "", "", err
	}
	return profileDir, selectedModel, nil
}

func cleanupLegacyGatewayModel(sourceDir string) error {
	settingsPath := filepath.Join(sourceDir, "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("decode %s: %w", settingsPath, err)
	}
	model, _ := settings["model"].(string)
	if !strings.HasPrefix(strings.TrimSpace(model), "claude-macaz-") {
		return nil
	}
	delete(settings, "model")
	return config.WritePrivateJSON(settingsPath, settings)
}

func sourceClaudeConfigDir(profileDir string) (string, error) {
	if value := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); value != "" {
		clean := filepath.Clean(value)
		if clean != filepath.Clean(profileDir) {
			return clean, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func scrubGatewaySettings(settings map[string]any) {
	delete(settings, "availableModels")
	delete(settings, "enforceAvailableModels")
	delete(settings, "fallbackModel")
	delete(settings, "fallbackModels")
	delete(settings, "modelOverrides")
	if values, ok := settings["env"].(map[string]any); ok {
		for key := range values {
			upper := strings.ToUpper(strings.TrimSpace(key))
			if strings.HasPrefix(upper, "ANTHROPIC_") ||
				strings.HasPrefix(upper, "MACAZ_") ||
				upper == "CLAUDE_CODE_USE_GATEWAY" ||
				upper == "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY" {
				delete(values, key)
			}
		}
		if len(values) == 0 {
			delete(settings, "env")
		}
	}
}

func loadClaudeSettings(profilePath, sourcePath string) (map[string]any, error) {
	settings := map[string]any{}
	path := profilePath
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		path = sourcePath
		raw, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return settings, nil
}

func profileProviderChanged(profileDir, selectedProvider string) (bool, error) {
	markerPath := filepath.Join(profileDir, ".macaz-provider")
	prior, err := os.ReadFile(markerPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return strings.TrimSpace(string(prior)) != strings.TrimSpace(selectedProvider), nil
}

func writeProfileProvider(profileDir, selectedProvider string) error {
	return os.WriteFile(
		filepath.Join(profileDir, ".macaz-provider"),
		[]byte(strings.TrimSpace(selectedProvider)+"\n"),
		0o600,
	)
}

func profileEffortChanged(profileDir, selectedEffort string) (bool, error) {
	markerPath := filepath.Join(profileDir, ".macaz-effort")
	prior, err := os.ReadFile(markerPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return strings.TrimSpace(string(prior)) != strings.TrimSpace(selectedEffort), nil
}

func writeProfileEffort(profileDir, selectedEffort string) error {
	return os.WriteFile(
		filepath.Join(profileDir, ".macaz-effort"),
		[]byte(strings.TrimSpace(selectedEffort)+"\n"),
		0o600,
	)
}

func configureGatewayEffort(settings map[string]any, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "low", "medium", "high", "xhigh":
		settings["effortLevel"] = effort
		return true
	default:
		return false
	}
}

func configureGatewayModels(settings map[string]any, models []string, defaultModel string) (string, error) {
	allowed := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		allowed = append(allowed, model)
	}
	if len(allowed) == 0 {
		return "", errors.New("macaz provider returned no public Claude model IDs")
	}
	defaultModel = strings.TrimSpace(defaultModel)
	if !seen[defaultModel] {
		defaultModel = allowed[0]
	}
	selected, _ := settings["model"].(string)
	selected = strings.TrimSpace(selected)
	if !seen[selected] {
		selected = defaultModel
	}
	settings["model"] = selected
	settings["availableModels"] = allowed
	settings["enforceAvailableModels"] = true
	delete(settings, "modelOverrides")
	return selected, nil
}

func shareClaudeAsset(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Symlink(source, destination); err == nil {
		return nil
	}
	if info.IsDir() {
		return copyClaudeDirectory(source, destination)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, raw, 0o600)
}

func copyClaudeDirectory(source, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err == nil {
				return nil
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}

func gatewayArgs(args, models []string, selected string) ([]string, string, error) {
	allowed := map[string]bool{}
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			allowed[model] = true
		}
	}
	launchModel := selected
	result := append([]string(nil), args...)
	for index := 0; index < len(result); index++ {
		arg := result[index]
		switch {
		case arg == "--managed-settings" || strings.HasPrefix(arg, "--managed-settings="):
			return nil, "", errors.New("macaz owns Claude managed model settings")
		case arg == "--fallback-model" || strings.HasPrefix(arg, "--fallback-model="):
			return nil, "", errors.New("macaz owns provider retries and does not allow Claude to fall back outside the active provider")
		case arg == "--model":
			if index+1 >= len(result) {
				return nil, "", errors.New("--model requires a value")
			}
			model, err := resolveLaunchModel(result[index+1], allowed, selected)
			if err != nil {
				return nil, "", err
			}
			result[index+1] = model
			launchModel = model
			index++
		case strings.HasPrefix(arg, "--model="):
			model, err := resolveLaunchModel(strings.TrimPrefix(arg, "--model="), allowed, selected)
			if err != nil {
				return nil, "", err
			}
			result[index] = "--model=" + model
			launchModel = model
		}
	}
	managed, err := json.Marshal(map[string]any{
		"model":                  launchModel,
		"availableModels":        models,
		"enforceAvailableModels": true,
	})
	if err != nil {
		return nil, "", err
	}
	result = append(result, "--managed-settings", string(managed))
	return result, launchModel, nil
}

func resolveLaunchModel(requested string, allowed map[string]bool, selected string) (string, error) {
	requested = strings.TrimSpace(requested)
	if allowed[requested] {
		return requested, nil
	}
	switch strings.ToLower(requested) {
	case "default", "inherit", "sonnet", "opus", "opusplan", "haiku", "fable":
		return selected, nil
	default:
		return "", fmt.Errorf("model %q is not available in the active macaz provider", requested)
	}
}

func stopClaudeDaemon(executable string, environment []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "daemon", "stop", "--any")
	command.Env = environment
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	_ = command.Run()
}

func migrateLegacyGatewaySessions(sourceDir, profileDir string) error {
	sourceProjects := filepath.Join(sourceDir, "projects")
	active, err := activeClaudeSessions(sourceDir)
	if err != nil {
		return err
	}
	var sessions []string
	err = filepath.WalkDir(sourceProjects, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			return nil
		}
		relative, err := filepath.Rel(sourceProjects, path)
		if err != nil {
			return err
		}
		if strings.Contains(relative, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
			return nil
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if active[sessionID] {
			return nil
		}
		marked, err := fileContainsGatewayModel(path)
		if err != nil {
			return err
		}
		if marked {
			sessions = append(sessions, path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, source := range sessions {
		relative, err := filepath.Rel(sourceDir, source)
		if err != nil {
			return err
		}
		destination := filepath.Join(profileDir, relative)
		if err := moveLegacyPath(source, destination); err != nil {
			return err
		}
		sessionID := strings.TrimSuffix(filepath.Base(source), ".jsonl")
		sourceSubagents := filepath.Join(filepath.Dir(source), sessionID)
		destinationSubagents := filepath.Join(filepath.Dir(destination), sessionID)
		if err := moveLegacyPath(sourceSubagents, destinationSubagents); err != nil {
			return err
		}
		if err := moveLegacyPath(
			filepath.Join(sourceDir, "session-env", sessionID),
			filepath.Join(profileDir, "session-env", sessionID),
		); err != nil {
			return err
		}
	}
	return nil
}

func activeClaudeSessions(sourceDir string) (map[string]bool, error) {
	active := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(sourceDir, "daemon", "roster.json"))
	if errors.Is(err, os.ErrNotExist) {
		return active, nil
	}
	if err != nil {
		return nil, err
	}
	var roster struct {
		Workers map[string]struct {
			SessionID string `json:"sessionId"`
			Dispatch  struct {
				Launch struct {
					SessionID string `json:"sessionId"`
				} `json:"launch"`
			} `json:"dispatch"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(raw, &roster); err != nil {
		return nil, fmt.Errorf("decode Claude daemon roster: %w", err)
	}
	for _, worker := range roster.Workers {
		if session := strings.TrimSpace(worker.SessionID); session != "" {
			active[session] = true
		}
		if session := strings.TrimSpace(worker.Dispatch.Launch.SessionID); session != "" {
			active[strings.TrimSuffix(filepath.Base(session), ".jsonl")] = true
		}
	}
	return active, nil
}

func fileContainsGatewayModel(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	buffer := make([]byte, 64<<10)
	tail := make([]byte, 0, 128)
	for {
		read, readErr := file.Read(buffer)
		if read > 0 {
			window := append(tail, buffer[:read]...)
			if gatewayModelJSON.Match(window) {
				return true, nil
			}
			keep := min(len(window), 128)
			tail = append(tail[:0], window[len(window)-keep:]...)
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
	}
}

func moveLegacyPath(source, destination string) error {
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return os.Rename(source, destination)
}

func withEnvironment(current []string, overrides map[string]string) []string {
	result := make([]string, 0, len(current)+len(overrides))
	for _, item := range current {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if _, replace := overrides[key]; !replace {
			result = append(result, item)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func firstReader(values ...io.Reader) io.Reader {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstWriter(values ...io.Writer) io.Writer {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
