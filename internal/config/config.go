package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ProviderOpenAISubscription = "openai-subscription"
	ProviderOpenAIAPIKey       = "openai-api-key"
	ProviderOpenRouterAPI      = "openrouter-api"
	ProviderCodexCLI           = "codex-cli"
	ProviderOpenCodeCLI        = "opencode-cli"
)

type Config struct {
	Version            int               `json:"version"`
	Provider           string            `json:"provider"`
	ClaudeExecutable   string            `json:"claude_executable,omitempty"`
	CodexExecutable    string            `json:"codex_executable,omitempty"`
	OpenCodeExecutable string            `json:"opencode_executable,omitempty"`
	CodexHome          string            `json:"codex_home,omitempty"`
	OpenAIBaseURL      string            `json:"openai_base_url,omitempty"`
	OpenAIModel        string            `json:"openai_model,omitempty"`
	OpenRouterBaseURL  string            `json:"openrouter_base_url,omitempty"`
	OpenRouterModel    string            `json:"openrouter_model,omitempty"`
	OpenCodeModel      string            `json:"opencode_model,omitempty"`
	DefaultEffort      string            `json:"default_effort,omitempty"`
	ModelMap           map[string]string `json:"model_map,omitempty"`
	RequestTimeoutSec  int               `json:"request_timeout_seconds,omitempty"`
	MaxBodyBytes       int64             `json:"max_body_bytes,omitempty"`
}

func Default() Config {
	return Config{
		Version:            1,
		ClaudeExecutable:   "claude",
		CodexExecutable:    "codex",
		OpenCodeExecutable: "opencode",
		OpenAIBaseURL:      "https://api.openai.com/v1",
		OpenAIModel:        "gpt-5.6",
		OpenRouterBaseURL:  "https://openrouter.ai/api/v1",
		OpenRouterModel:    "openai/gpt-5.6-sol",
		DefaultEffort:      "high",
		ModelMap: map[string]string{
			"default": "gpt-5.6",
			"opus":    "gpt-5.6",
			"sonnet":  "gpt-5.6",
			"haiku":   "gpt-5.6",
		},
		RequestTimeoutSec: 7200,
		MaxBodyBytes:      64 << 20,
	}
}

func Path() (string, error) {
	if override := strings.TrimSpace(os.Getenv("MACAZ_CONFIG")); override != "" {
		return filepath.Clean(override), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "macaz", "config.json"), nil
}

func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	return LoadPath(path)
}

func LoadPath(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, os.ErrNotExist
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return SavePath(path, cfg)
}

func SavePath(path string, cfg Config) error {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	return WritePrivateJSON(path, cfg)
}

func WritePrivateJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode private JSON: %w", err)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temporary config: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func ClaudeProfileDir() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "claude"), nil
}

func RemoveClaudeProfile() error {
	path, err := ClaudeProfileDir()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove Claude profile: %w", err)
	}
	return nil
}

func Remove() error {
	path, err := Path()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Version == 0 {
		c.Version = d.Version
	}
	if strings.TrimSpace(c.ClaudeExecutable) == "" {
		c.ClaudeExecutable = d.ClaudeExecutable
	}
	if strings.TrimSpace(c.CodexExecutable) == "" {
		c.CodexExecutable = d.CodexExecutable
	}
	if strings.TrimSpace(c.OpenCodeExecutable) == "" {
		c.OpenCodeExecutable = d.OpenCodeExecutable
	}
	if strings.TrimSpace(c.OpenAIBaseURL) == "" {
		c.OpenAIBaseURL = d.OpenAIBaseURL
	}
	if strings.TrimSpace(c.OpenAIModel) == "" {
		c.OpenAIModel = d.OpenAIModel
	}
	if strings.TrimSpace(c.OpenRouterBaseURL) == "" {
		c.OpenRouterBaseURL = d.OpenRouterBaseURL
	}
	if strings.TrimSpace(c.OpenRouterModel) == "" {
		c.OpenRouterModel = d.OpenRouterModel
	}
	if strings.TrimSpace(c.DefaultEffort) == "" {
		c.DefaultEffort = d.DefaultEffort
	}
	if c.ModelMap == nil {
		c.ModelMap = map[string]string{}
	}
	for key, value := range d.ModelMap {
		if strings.TrimSpace(c.ModelMap[key]) == "" {
			c.ModelMap[key] = value
		}
	}
	if c.RequestTimeoutSec <= 0 {
		c.RequestTimeoutSec = d.RequestTimeoutSec
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = d.MaxBodyBytes
	}
}

func (c Config) Validate() error {
	switch c.Provider {
	case "", ProviderOpenAISubscription, ProviderOpenAIAPIKey, ProviderOpenRouterAPI, ProviderCodexCLI, ProviderOpenCodeCLI:
	default:
		return fmt.Errorf("unsupported provider %q", c.Provider)
	}
	switch c.DefaultEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("unsupported default effort %q", c.DefaultEffort)
	}
	if c.RequestTimeoutSec < 1 {
		return errors.New("request timeout must be positive")
	}
	if c.MaxBodyBytes < 1024 {
		return errors.New("max body bytes must be at least 1024")
	}
	return nil
}

func (c Config) ResolveModel(requested string) string {
	requested = strings.TrimSpace(requested)
	lower := strings.ToLower(requested)
	if model := strings.TrimSpace(c.ModelMap[lower]); model != "" {
		return model
	}
	if strings.HasPrefix(lower, "claude-") {
		for alias, model := range c.ModelMap {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias != "" && strings.Contains(lower, alias) && strings.TrimSpace(model) != "" {
				return model
			}
		}
		if model := strings.TrimSpace(c.ModelMap["default"]); model != "" {
			return model
		}
	}
	if requested != "" {
		return requested
	}
	if model := strings.TrimSpace(c.ModelMap["default"]); model != "" {
		return model
	}
	if c.Provider == ProviderOpenRouterAPI && strings.TrimSpace(c.OpenRouterModel) != "" {
		return c.OpenRouterModel
	}
	if c.Provider == ProviderOpenCodeCLI && strings.TrimSpace(c.OpenCodeModel) != "" {
		return c.OpenCodeModel
	}
	return c.OpenAIModel
}

func (c Config) ModelAliases() []string {
	keys := make([]string, 0, len(c.ModelMap))
	for key := range c.ModelMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
