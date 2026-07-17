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
	ClientClaude = "claude"
	ClientCodex  = "codex"

	ProviderOpenAISubscription = "openai-subscription"
	ProviderOpenAIAPIKey       = "openai-api-key"
	ProviderOpenRouterAPI      = "openrouter-api"
	ProviderAnthropicAPI       = "anthropic-api"
	ProviderCodexCLI           = "codex-cli"
	ProviderOpenCodeCLI        = "opencode-cli"
)

type ClientProfile struct {
	Provider          string            `json:"provider"`
	OpenAIBaseURL     string            `json:"openai_base_url,omitempty"`
	OpenAIModel       string            `json:"openai_model,omitempty"`
	OpenRouterBaseURL string            `json:"openrouter_base_url,omitempty"`
	OpenRouterModel   string            `json:"openrouter_model,omitempty"`
	AnthropicBaseURL  string            `json:"anthropic_base_url,omitempty"`
	AnthropicModel    string            `json:"anthropic_model,omitempty"`
	OpenCodeModel     string            `json:"opencode_model,omitempty"`
	DefaultEffort     string            `json:"default_effort,omitempty"`
	ModelMap          map[string]string `json:"model_map,omitempty"`
}

type Config struct {
	Version            int                      `json:"version"`
	DefaultClient      string                   `json:"default_client,omitempty"`
	Clients            map[string]ClientProfile `json:"clients,omitempty"`
	Provider           string                   `json:"provider,omitempty"`
	ClaudeExecutable   string                   `json:"claude_executable,omitempty"`
	CodexExecutable    string                   `json:"codex_executable,omitempty"`
	OpenCodeExecutable string                   `json:"opencode_executable,omitempty"`
	CodexHome          string                   `json:"codex_home,omitempty"`
	OpenAIBaseURL      string                   `json:"openai_base_url,omitempty"`
	OpenAIModel        string                   `json:"openai_model,omitempty"`
	OpenRouterBaseURL  string                   `json:"openrouter_base_url,omitempty"`
	OpenRouterModel    string                   `json:"openrouter_model,omitempty"`
	AnthropicBaseURL   string                   `json:"anthropic_base_url,omitempty"`
	AnthropicModel     string                   `json:"anthropic_model,omitempty"`
	OpenCodeModel      string                   `json:"opencode_model,omitempty"`
	DefaultEffort      string                   `json:"default_effort,omitempty"`
	ModelMap           map[string]string        `json:"model_map,omitempty"`
	RequestTimeoutSec  int                      `json:"request_timeout_seconds,omitempty"`
	MaxConcurrentCLI   int                      `json:"max_concurrent_cli_requests,omitempty"`
	MaxBodyBytes       int64                    `json:"max_body_bytes,omitempty"`
}

func Default() Config {
	return Config{
		Version:            2,
		DefaultClient:      ClientClaude,
		Clients:            map[string]ClientProfile{},
		ClaudeExecutable:   "claude",
		CodexExecutable:    "codex",
		OpenCodeExecutable: "opencode",
		OpenAIBaseURL:      "https://api.openai.com/v1",
		OpenAIModel:        "gpt-5.6",
		OpenRouterBaseURL:  "https://openrouter.ai/api/v1",
		OpenRouterModel:    "openai/gpt-5.6-sol",
		AnthropicBaseURL:   "https://api.anthropic.com/v1",
		AnthropicModel:     "",
		DefaultEffort:      "high",
		ModelMap: map[string]string{
			"default": "gpt-5.6",
			"opus":    "gpt-5.6",
			"sonnet":  "gpt-5.6",
			"haiku":   "gpt-5.6",
		},
		RequestTimeoutSec: 7200,
		MaxConcurrentCLI:  4,
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
	if len(cfg.Clients) == 0 && strings.TrimSpace(cfg.Provider) != "" {
		cfg.SetClient(ClientClaude, cfg)
	}
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
	return ClientProfileDir(ClientClaude)
}

func CodexProfileDir() (string, error) {
	return ClientProfileDir(ClientCodex)
}

func ClientProfileDir(client string) (string, error) {
	if err := ValidateClient(client); err != nil {
		return "", err
	}
	path, err := Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), client), nil
}

func RemoveClaudeProfile() error {
	return RemoveClientProfile(ClientClaude)
}

func RemoveCodexProfile() error {
	return RemoveClientProfile(ClientCodex)
}

func RemoveClientProfile(client string) error {
	path, err := ClientProfileDir(client)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s profile: %w", client, err)
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
	if strings.TrimSpace(c.DefaultClient) == "" {
		c.DefaultClient = d.DefaultClient
	}
	if c.Clients == nil {
		c.Clients = map[string]ClientProfile{}
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
	if strings.TrimSpace(c.AnthropicBaseURL) == "" {
		c.AnthropicBaseURL = d.AnthropicBaseURL
	}
	if strings.TrimSpace(c.AnthropicModel) == "" {
		c.AnthropicModel = d.AnthropicModel
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
	if c.MaxConcurrentCLI <= 0 {
		c.MaxConcurrentCLI = d.MaxConcurrentCLI
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = d.MaxBodyBytes
	}
}

func (c Config) Validate() error {
	if err := ValidateClient(c.DefaultClient); err != nil {
		return err
	}
	if err := validateProvider(c.Provider); err != nil {
		return err
	}
	if strings.TrimSpace(c.Provider) != "" {
		if err := validateClientProvider(ClientClaude, c.Provider); err != nil {
			return err
		}
	}
	for client, profile := range c.Clients {
		if err := ValidateClient(client); err != nil {
			return err
		}
		if err := validateProvider(profile.Provider); err != nil {
			return fmt.Errorf("%s client: %w", client, err)
		}
		if err := validateClientProvider(client, profile.Provider); err != nil {
			return err
		}
	}
	switch c.DefaultEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("unsupported default effort %q", c.DefaultEffort)
	}
	if c.RequestTimeoutSec < 1 {
		return errors.New("request timeout must be positive")
	}
	if c.MaxConcurrentCLI < 1 || c.MaxConcurrentCLI > 64 {
		return errors.New("max concurrent CLI requests must be between 1 and 64")
	}
	if c.MaxBodyBytes < 1024 {
		return errors.New("max body bytes must be at least 1024")
	}
	return nil
}

func validateClientProvider(client, provider string) error {
	switch {
	case client == ClientClaude && provider == ProviderAnthropicAPI:
		return errors.New("claude client does not need macaz to use the Anthropic API")
	case client == ClientCodex && (provider == ProviderOpenAISubscription || provider == ProviderOpenAIAPIKey):
		return errors.New("codex client does not need macaz to use OpenAI")
	case client == ClientCodex && provider == ProviderCodexCLI:
		return errors.New("codex client cannot use Codex CLI as its upstream provider")
	default:
		return nil
	}
}

func ValidateClient(client string) error {
	switch strings.ToLower(strings.TrimSpace(client)) {
	case ClientClaude, ClientCodex:
		return nil
	default:
		return fmt.Errorf("unsupported client %q", client)
	}
}

func validateProvider(name string) error {
	switch name {
	case "", ProviderOpenAISubscription, ProviderOpenAIAPIKey, ProviderOpenRouterAPI, ProviderAnthropicAPI, ProviderCodexCLI, ProviderOpenCodeCLI:
		return nil
	default:
		return fmt.Errorf("unsupported provider %q", name)
	}
}

func (c Config) HasClient(client string) bool {
	client = strings.ToLower(strings.TrimSpace(client))
	if profile, ok := c.Clients[client]; ok && strings.TrimSpace(profile.Provider) != "" {
		return true
	}
	return client == ClientClaude && strings.TrimSpace(c.Provider) != ""
}

func (c Config) ForClient(client string) (Config, error) {
	client = strings.ToLower(strings.TrimSpace(client))
	if err := ValidateClient(client); err != nil {
		return Config{}, err
	}
	result := c
	profile, ok := c.Clients[client]
	if !ok {
		if client == ClientClaude && strings.TrimSpace(c.Provider) != "" {
			return result, nil
		}
		result.Provider = ""
		return result, nil
	}
	result.Provider = profile.Provider
	// A client profile is an isolation boundary. Do not fall back to the
	// legacy/root model fields here: those belong to the other client and can
	// leak a Macaz-selected model into the user's normal Codex installation.
	result.OpenAIBaseURL = firstNonEmpty(profile.OpenAIBaseURL, Default().OpenAIBaseURL)
	result.OpenAIModel = firstNonEmpty(profile.OpenAIModel, Default().OpenAIModel)
	result.OpenRouterBaseURL = firstNonEmpty(profile.OpenRouterBaseURL, Default().OpenRouterBaseURL)
	result.OpenRouterModel = firstNonEmpty(profile.OpenRouterModel, Default().OpenRouterModel)
	result.AnthropicBaseURL = firstNonEmpty(profile.AnthropicBaseURL, Default().AnthropicBaseURL)
	result.AnthropicModel = profile.AnthropicModel
	result.OpenCodeModel = profile.OpenCodeModel
	result.DefaultEffort = firstNonEmpty(profile.DefaultEffort, Default().DefaultEffort)
	if profile.ModelMap != nil {
		result.ModelMap = cloneStringMap(profile.ModelMap)
	}
	return result, nil
}

func (c *Config) SetClient(client string, selected Config) {
	client = strings.ToLower(strings.TrimSpace(client))
	if c.Clients == nil {
		c.Clients = map[string]ClientProfile{}
	}
	c.Version = 2
	c.ClaudeExecutable = selected.ClaudeExecutable
	c.CodexExecutable = selected.CodexExecutable
	c.OpenCodeExecutable = selected.OpenCodeExecutable
	c.CodexHome = selected.CodexHome
	c.Clients[client] = ClientProfile{
		Provider:          selected.Provider,
		OpenAIBaseURL:     selected.OpenAIBaseURL,
		OpenAIModel:       selected.OpenAIModel,
		OpenRouterBaseURL: selected.OpenRouterBaseURL,
		OpenRouterModel:   selected.OpenRouterModel,
		AnthropicBaseURL:  selected.AnthropicBaseURL,
		AnthropicModel:    selected.AnthropicModel,
		OpenCodeModel:     selected.OpenCodeModel,
		DefaultEffort:     selected.DefaultEffort,
		ModelMap:          cloneStringMap(selected.ModelMap),
	}
	if client == ClientClaude {
		c.Provider = ""
	}
}

func (c *Config) RemoveClient(client string) {
	client = strings.ToLower(strings.TrimSpace(client))
	delete(c.Clients, client)
	if client == ClientClaude {
		c.Provider = ""
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
	if c.Provider == ProviderAnthropicAPI && strings.TrimSpace(c.AnthropicModel) != "" {
		return c.AnthropicModel
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
