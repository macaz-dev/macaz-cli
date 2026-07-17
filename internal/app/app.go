package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/macaz-dev/macaz-cli/internal/browser"
	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/gateway"
	"github.com/macaz-dev/macaz-cli/internal/launcher"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/provider/anthropic"
	"github.com/macaz-dev/macaz-cli/internal/provider/codexcli"
	openaiadapter "github.com/macaz-dev/macaz-cli/internal/provider/openai"
	"github.com/macaz-dev/macaz-cli/internal/provider/opencodecli"
	"github.com/macaz-dev/macaz-cli/internal/provider/openrouter"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
	"github.com/macaz-dev/macaz-cli/internal/updater"
)

var Version = "dev"

const (
	updateCheckTimeout = 2 * time.Second
	updateRunTimeout   = 2 * time.Minute
)

type updateClient interface {
	Check(context.Context) (updater.Release, error)
	Update(context.Context) (updater.Result, error)
}

var newUpdateClient = func(version string) updateClient {
	return updater.New(version)
}

type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

func Run(ctx context.Context, args []string, streams Streams) error {
	streams = withDefaultStreams(streams)
	_ = updater.CleanupOldExecutable()
	if shouldCheckForUpdate(args) {
		notifyAvailableUpdate(ctx, streams.Err)
	}
	if len(args) == 0 {
		usage(streams.Out)
		return nil
	}
	switch args[0] {
	case config.ClientClaude, config.ClientCodex:
		return runClient(ctx, args[0], args[1:], streams)
	case "status":
		client, err := optionalClient(args[1:])
		if err != nil {
			return err
		}
		return runStatus(ctx, client, streams)
	case "doctor":
		client, err := optionalClient(args[1:])
		if err != nil {
			return err
		}
		return runDoctor(ctx, client, streams)
	case "reset":
		return runReset(args[1:], streams)
	case "legal":
		legalNotice(streams.Out)
		return nil
	case "update":
		return runUpdate(ctx, args[1:], streams)
	case "version", "--version", "-v":
		_, _ = fmt.Fprintf(streams.Out, "macaz %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "--help", "-h":
		usage(streams.Out)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run macaz help for usage", args[0])
	}
}

func shouldCheckForUpdate(args []string) bool {
	if len(args) > 0 && args[0] == "update" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MACAZ_NO_UPDATE_CHECK"))) {
	case "1", "true", "yes":
		return false
	default:
		return true
	}
}

func notifyAvailableUpdate(ctx context.Context, out io.Writer) {
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	release, err := newUpdateClient(Version).Check(checkCtx)
	if err != nil || !release.Available {
		return
	}
	_, _ = fmt.Fprintf(out, "macaz: update available: %s (current %s). Run: macaz update\n", release.Latest, release.Current)
}

func runUpdate(ctx context.Context, args []string, streams Streams) error {
	if len(args) != 0 {
		return errors.New("usage: macaz update")
	}
	_, _ = fmt.Fprintln(streams.Out, "Checking for macaz updates…")
	updateCtx, cancel := context.WithTimeout(ctx, updateRunTimeout)
	defer cancel()
	result, err := newUpdateClient(Version).Update(updateCtx)
	if err != nil {
		return err
	}
	if !result.Updated {
		_, _ = fmt.Fprintf(streams.Out, "macaz %s is already up to date.\n", result.Current)
		return nil
	}
	_, _ = fmt.Fprintf(streams.Out, "macaz updated: %s → %s\n", result.Current, result.Latest)
	_, _ = fmt.Fprintf(streams.Out, "Binary: %s\n", result.Path)
	return nil
}

func runReset(args []string, streams Streams) error {
	if len(args) > 1 {
		return errors.New("usage: macaz reset [claude|codex]")
	}
	if len(args) == 1 {
		client := strings.ToLower(strings.TrimSpace(args[0]))
		if err := config.ValidateClient(client); err != nil {
			return errors.New("usage: macaz reset [claude|codex]")
		}
		cfg, err := config.Load()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		cfg.RemoveClient(client)
		if err == nil {
			if err := config.Save(cfg); err != nil {
				return err
			}
		}
		if err := config.RemoveClientProfile(client); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(streams.Out, "macaz %s configuration and isolated profile were removed.\n", client)
		_, _ = fmt.Fprintln(streams.Out, "Shared provider credentials and the other client configuration were not changed.")
		return nil
	}
	path, err := config.Path()
	if err != nil {
		return err
	}
	if err := config.Remove(); err != nil {
		return fmt.Errorf("remove macaz configuration: %w", err)
	}
	if err := config.RemoveClaudeProfile(); err != nil {
		return err
	}
	if err := config.RemoveCodexProfile(); err != nil {
		return err
	}
	if err := secrets.DeleteAll(); err != nil {
		return fmt.Errorf("remove macaz credentials: %w", err)
	}
	_, _ = fmt.Fprintf(streams.Out, "macaz configuration removed: %s\n", path)
	_, _ = fmt.Fprintln(streams.Out, "macaz API keys and subscription tokens were removed.")
	_, _ = fmt.Fprintln(streams.Out, "The isolated macaz Claude and Codex profiles and their session history were removed.")
	_, _ = fmt.Fprintln(streams.Out, "Vendor CLI credentials and normal client configuration were not changed.")
	return nil
}

func runClient(ctx context.Context, client string, args []string, streams Streams) error {
	if os.Getenv("MACAZ_ACTIVE") == "1" {
		return errors.New("macaz is already active in this process environment")
	}
	if err := config.ValidateClient(client); err != nil {
		return err
	}
	cfg, err := loadOrConfigure(ctx, client, streams)
	if err != nil {
		return err
	}
	upstream, err := makeProvider(cfg)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := upstream.Check(checkCtx); err != nil {
		return fmt.Errorf("%s is not ready: %w", upstream.Name(), err)
	}
	server, err := gateway.NewForClient(cfg, upstream, client)
	if err != nil {
		return err
	}
	modelCtx, modelCancel := context.WithTimeout(ctx, 60*time.Second)
	catalog, err := server.PrimeModels(modelCtx)
	if err != nil {
		modelCancel()
		return fmt.Errorf("%s model discovery failed: %w", upstream.Name(), err)
	}
	modelCancel()
	if err := server.Start(); err != nil {
		return err
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = server.Close(closeCtx)
	}()
	label := "Claude Code"
	if client == config.ClientCodex {
		label = "Codex CLI"
	}
	_, _ = fmt.Fprintf(streams.Err, "macaz: %s → %s\n", label, upstream.Name())
	options := launcher.Options{
		BaseURL:      server.URL(),
		Token:        server.Token(),
		Models:       catalog.IDs,
		ModelDetails: catalog.Models,
		DefaultModel: catalog.Default,
		Args:         args,
		Stdin:        streams.In,
		Stdout:       streams.Out,
		Stderr:       streams.Err,
	}
	if client == config.ClientCodex {
		return launcher.Codex(ctx, cfg, options)
	}
	return launcher.Claude(ctx, cfg, options)
}

func runStatus(ctx context.Context, client string, streams Streams) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	root, err := config.Load()
	if err != nil {
		return err
	}
	if client == "" {
		client = root.DefaultClient
	}
	cfg, err := root.ForClient(client)
	if err != nil {
		return err
	}
	if cfg.Provider == "" {
		return fmt.Errorf("%s is not configured; run `macaz %s`", client, client)
	}
	upstream, err := makeProvider(cfg)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := upstream.Check(checkCtx); err != nil {
		return fmt.Errorf("%s: %w", upstream.Name(), err)
	}
	models, err := upstream.Models(checkCtx)
	if err != nil {
		return err
	}
	activeModel, ok := activeProviderModel(models, cfg.ResolveModel("default"))
	if !ok {
		return errors.New("provider returned no models")
	}
	_, _ = fmt.Fprintf(streams.Out, "Config: %s\n", path)
	_, _ = fmt.Fprintf(streams.Out, "Client: %s\n", client)
	_, _ = fmt.Fprintf(streams.Out, "Provider: %s (OK)\n", upstream.Name())
	_, _ = fmt.Fprintf(streams.Out, "Model: %s\n", activeModel.ID)
	_, _ = fmt.Fprintf(streams.Out, "Effort: %s\n", cfg.DefaultEffort)
	_, _ = fmt.Fprintf(streams.Out, "Discovered models: %d\n", len(models))
	return nil
}

func activeProviderModel(models []provider.Model, configured string) (provider.Model, bool) {
	if model, ok := providerModelByID(models, configured); ok {
		return model, true
	}
	for _, model := range models {
		if model.Default {
			return model, true
		}
	}
	if len(models) > 0 {
		return models[0], true
	}
	return provider.Model{}, false
}

func runDoctor(ctx context.Context, client string, streams Streams) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	root, err := config.Load()
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if client == "" {
		client = root.DefaultClient
	}
	cfg, err := root.ForClient(client)
	if err != nil {
		return err
	}
	if cfg.Provider == "" {
		return fmt.Errorf("%s is not configured; run `macaz %s`", client, client)
	}
	_, _ = fmt.Fprintf(streams.Out, "Config: %s\n", path)
	_, _ = fmt.Fprintf(streams.Out, "Client: %s\n", client)
	_, _ = fmt.Fprintf(streams.Out, "Provider: %s\n", cfg.Provider)
	executable := cfg.ClaudeExecutable
	label := "Claude"
	if client == config.ClientCodex {
		executable = cfg.CodexExecutable
		label = "Codex"
	}
	if err := checkExecutable(ctx, executable); err != nil {
		return fmt.Errorf("%s executable: %w", label, err)
	}
	_, _ = fmt.Fprintf(streams.Out, "%s executable: %s\n", label, executable)
	upstream, err := makeProvider(cfg)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := upstream.Check(checkCtx); err != nil {
		return fmt.Errorf("%s: %w", upstream.Name(), err)
	}
	_, _ = fmt.Fprintf(streams.Out, "%s: OK\n", upstream.Name())
	return nil
}

func loadOrConfigure(ctx context.Context, client string, streams Streams) (config.Config, error) {
	root, err := config.Load()
	if err == nil && root.HasClient(client) {
		return root.ForClient(client)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config.Config{}, err
	}
	if errors.Is(err, os.ErrNotExist) {
		root = config.Default()
	}
	selected, selectErr := root.ForClient(client)
	if selectErr != nil {
		return config.Config{}, selectErr
	}
	selected.Provider = ""
	selected, err = wizard(ctx, client, selected, streams)
	if err != nil {
		return config.Config{}, err
	}
	root.SetClient(client, selected)
	if err := config.Save(root); err != nil {
		return config.Config{}, err
	}
	return selected, nil
}

func wizard(ctx context.Context, client string, cfg config.Config, streams Streams) (config.Config, error) {
	type providerOption struct {
		label    string
		provider string
	}
	reader := bufio.NewReader(streams.In)
	legalNotice(streams.Out)
	_, _ = fmt.Fprintln(streams.Out)
	_, _ = fmt.Fprintf(streams.Out, "Choose the provider for `macaz %s`:\n", client)
	var options []providerOption
	if client == config.ClientClaude {
		options = []providerOption{
			{label: "OpenAI Subscription", provider: config.ProviderOpenAISubscription},
			{label: "OpenAI API", provider: config.ProviderOpenAIAPIKey},
			{label: "OpenRouter API", provider: config.ProviderOpenRouterAPI},
			{label: "Codex-CLI (experimental)", provider: config.ProviderCodexCLI},
			{label: "OpenCode-CLI (experimental)", provider: config.ProviderOpenCodeCLI},
		}
	} else {
		options = []providerOption{
			{label: "OpenRouter API", provider: config.ProviderOpenRouterAPI},
			{label: "Anthropic API", provider: config.ProviderAnthropicAPI},
			{label: "OpenCode-CLI (experimental)", provider: config.ProviderOpenCodeCLI},
		}
	}
	for index, option := range options {
		_, _ = fmt.Fprintf(streams.Out, "%d. %s\n", index+1, option.label)
	}
	choice, err := prompt(reader, streams.Out, fmt.Sprintf("Provider [1-%d]: ", len(options)), "")
	if err != nil {
		return config.Config{}, err
	}
	selected := -1
	for index := range options {
		if strings.TrimSpace(choice) == fmt.Sprint(index+1) {
			selected = index
			break
		}
	}
	if selected < 0 {
		return config.Config{}, fmt.Errorf("invalid provider choice %q", choice)
	}
	cfg.Provider = options[selected].provider
	switch cfg.Provider {
	case config.ProviderOpenAISubscription:
		authCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		client := &http.Client{Timeout: 30 * time.Second}
		err := openaiadapter.AuthorizeSubscription(authCtx, client, func(device openaiadapter.DeviceAuthorization) error {
			_, _ = fmt.Fprintf(streams.Out, "\nOpenAI authorization code: %s\n", device.UserCode)
			_, _ = fmt.Fprintf(streams.Out, "Open: %s\n", device.URL)
			if err := browser.Open(device.URL); err != nil {
				_, _ = fmt.Fprintf(streams.Err, "Could not open the browser automatically: %v\n", err)
			} else {
				_, _ = fmt.Fprintln(streams.Out, "The authorization page was opened in your browser.")
			}
			_, _ = fmt.Fprintln(streams.Out, "Waiting for OpenAI to confirm authorization…")
			return nil
		})
		if err != nil {
			return config.Config{}, fmt.Errorf("connect OpenAI Subscription: %w", err)
		}
		_, _ = fmt.Fprintln(streams.Out, "OpenAI Subscription connected.")
	case config.ProviderOpenAIAPIKey:
		key, err := promptSecret(reader, streams, "OpenAI API key: ")
		if err != nil {
			return config.Config{}, err
		}
		if err := secrets.Set(secrets.OpenAIAPIKey, key); err != nil {
			return config.Config{}, err
		}
	case config.ProviderOpenRouterAPI:
		key, err := promptSecret(reader, streams, "OpenRouter API key: ")
		if err != nil {
			return config.Config{}, err
		}
		if err := secrets.Set(secrets.OpenRouterAPIKey, key); err != nil {
			return config.Config{}, err
		}
		baseURL, err := prompt(reader, streams.Out, "OpenRouter base URL", cfg.OpenRouterBaseURL)
		if err != nil {
			return config.Config{}, err
		}
		cfg.OpenRouterBaseURL = baseURL
		model, err := prompt(reader, streams.Out, "OpenRouter model (provider/model)", cfg.OpenRouterModel)
		if err != nil {
			return config.Config{}, err
		}
		if !strings.Contains(model, "/") {
			return config.Config{}, fmt.Errorf("OpenRouter model must use provider/model format, got %q", model)
		}
		cfg.OpenRouterModel = model
		for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
			cfg.ModelMap[alias] = model
		}
	case config.ProviderAnthropicAPI:
		key, err := promptSecret(reader, streams, "Anthropic API key: ")
		if err != nil {
			return config.Config{}, err
		}
		if err := secrets.Set(secrets.AnthropicAPIKey, key); err != nil {
			return config.Config{}, err
		}
		baseURL, err := prompt(reader, streams.Out, "Anthropic base URL", cfg.AnthropicBaseURL)
		if err != nil {
			return config.Config{}, err
		}
		cfg.AnthropicBaseURL = baseURL
		modelCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		models, err := anthropic.New(cfg).Models(modelCtx)
		if err != nil {
			return config.Config{}, fmt.Errorf("list Anthropic models: %w", err)
		}
		_, _ = fmt.Fprintln(streams.Out, "Available Anthropic models:")
		for index, model := range models {
			label := strings.TrimSpace(model.DisplayName)
			if label == "" {
				label = model.ID
			}
			_, _ = fmt.Fprintf(streams.Out, "%d. %s (%s)\n", index+1, label, model.ID)
		}
		choice, err := prompt(reader, streams.Out, fmt.Sprintf("Anthropic model [1-%d]", len(models)), "1")
		if err != nil {
			return config.Config{}, err
		}
		selectedModel := ""
		for index, model := range models {
			if strings.TrimSpace(choice) == fmt.Sprint(index+1) {
				selectedModel = model.ID
				break
			}
		}
		if selectedModel == "" {
			return config.Config{}, fmt.Errorf("invalid Anthropic model choice %q", choice)
		}
		cfg.AnthropicModel = selectedModel
		for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
			cfg.ModelMap[alias] = selectedModel
		}
	case config.ProviderCodexCLI:
		value, err := prompt(reader, streams.Out, "Codex executable", cfg.CodexExecutable)
		if err != nil {
			return config.Config{}, err
		}
		cfg.CodexExecutable = value
		if _, err := exec.LookPath(value); err != nil {
			return config.Config{}, fmt.Errorf("Codex executable %q was not found: %w", value, err)
		}
	case config.ProviderOpenCodeCLI:
		return configureOpenCode(cfg, reader, streams)
	}
	return cfg, nil
}

func configureOpenCode(cfg config.Config, reader *bufio.Reader, streams Streams) (config.Config, error) {
	value, err := prompt(reader, streams.Out, "OpenCode executable", cfg.OpenCodeExecutable)
	if err != nil {
		return config.Config{}, err
	}
	cfg.OpenCodeExecutable = value
	if _, err := exec.LookPath(value); err != nil {
		return config.Config{}, fmt.Errorf("OpenCode executable %q was not found: %w", value, err)
	}
	model, err := prompt(reader, streams.Out, "OpenCode model (provider/model, blank uses OpenCode default)", cfg.OpenCodeModel)
	if err != nil {
		return config.Config{}, err
	}
	cfg.OpenCodeModel = model
	return cfg, nil
}

func promptSecret(reader *bufio.Reader, streams Streams, label string) (string, error) {
	return promptHidden(reader, streams, label, "API key cannot be empty")
}

func promptHidden(reader *bufio.Reader, streams Streams, label, emptyMessage string) (string, error) {
	_, _ = fmt.Fprint(streams.Out, label)
	if file, ok := streams.In.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		raw, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(streams.Out)
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", errors.New(emptyMessage)
		}
		return value, nil
	}
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New(emptyMessage)
	}
	return value, nil
}

func prompt(reader *bufio.Reader, out io.Writer, label, fallback string) (string, error) {
	if fallback == "" {
		_, _ = fmt.Fprint(out, label)
	} else {
		_, _ = fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	}
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	return value, nil
}

func makeProvider(cfg config.Config) (provider.Provider, error) {
	switch cfg.Provider {
	case config.ProviderOpenAIAPIKey:
		return openaiadapter.New(openaiadapter.ModeAPIKey, cfg)
	case config.ProviderOpenAISubscription:
		return openaiadapter.New(openaiadapter.ModeSubscription, cfg)
	case config.ProviderCodexCLI:
		return codexcli.New(cfg), nil
	case config.ProviderOpenCodeCLI:
		return opencodecli.New(cfg), nil
	case config.ProviderOpenRouterAPI:
		return openrouter.New(cfg), nil
	case config.ProviderAnthropicAPI:
		return anthropic.New(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported configured provider %q", cfg.Provider)
	}
}

func checkExecutable(ctx context.Context, name string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, path, "--version")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func usage(out io.Writer) {
	_, _ = fmt.Fprintln(out, `macaz - use your favorite models and providers with your favorite coding agents

Usage:
  macaz claude [arguments...]
  macaz codex [arguments...]
  macaz status [claude|codex]
  macaz doctor [claude|codex]
  macaz reset [claude|codex]
  macaz legal
  macaz update
  macaz version

`+"`macaz` starts the selected client directly, keeps its normal permission prompts, and routes only model inference through the selected provider.\n"+
		"Any client-specific permission bypass must be passed explicitly by the user.")
}

func legalNotice(out io.Writer) {
	_, _ = fmt.Fprintln(out, "macaz is an independent interoperability project. It is not affiliated with, authorized by, endorsed by, or sponsored by Anthropic, OpenAI, or any other client, model, or service provider.")
	_, _ = fmt.Fprintln(out, "Third-party clients and models are not included and must be obtained and installed separately from authorized sources. You are responsible for complying with each applicable client, provider, account, and organizational agreement.")
	_, _ = fmt.Fprintln(out, "See LEGAL.md and PRIVACY.md in the macaz source repository for the complete notices.")
}

func optionalClient(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) != 1 || config.ValidateClient(args[0]) != nil {
		return "", errors.New("expected zero arguments or one client: claude|codex")
	}
	return strings.ToLower(strings.TrimSpace(args[0])), nil
}

func withDefaultStreams(streams Streams) Streams {
	if streams.In == nil {
		streams.In = os.Stdin
	}
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	return streams
}

func providerModelByID(models []provider.Model, id string) (provider.Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return provider.Model{}, false
}
