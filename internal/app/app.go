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
	"github.com/macaz-dev/macaz-cli/internal/provider/codexcli"
	openaiadapter "github.com/macaz-dev/macaz-cli/internal/provider/openai"
	"github.com/macaz-dev/macaz-cli/internal/provider/opencodecli"
	"github.com/macaz-dev/macaz-cli/internal/provider/openrouter"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

var Version = "dev"

type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

func Run(ctx context.Context, args []string, streams Streams) error {
	streams = withDefaultStreams(streams)
	if len(args) == 0 {
		return runClaude(ctx, nil, streams)
	}
	switch args[0] {
	case "status":
		return runStatus(ctx, streams)
	case "doctor":
		return runDoctor(ctx, streams)
	case "reset":
		return runReset(streams)
	case "legal":
		legalNotice(streams.Out)
		return nil
	case "version", "--version", "-v":
		_, _ = fmt.Fprintf(streams.Out, "macaz %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "--help", "-h":
		usage(streams.Out)
		return nil
	default:
		return runClaude(ctx, args, streams)
	}
}

func runReset(streams Streams) error {
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
	if err := secrets.DeleteAll(); err != nil {
		return fmt.Errorf("remove macaz credentials: %w", err)
	}
	_, _ = fmt.Fprintf(streams.Out, "macaz configuration removed: %s\n", path)
	_, _ = fmt.Fprintln(streams.Out, "macaz API keys and subscription tokens were removed.")
	_, _ = fmt.Fprintln(streams.Out, "The isolated macaz Claude profile and its session history were removed.")
	_, _ = fmt.Fprintln(streams.Out, "Vendor CLI credentials and Claude Code configuration were not changed.")
	return nil
}

func runClaude(ctx context.Context, args []string, streams Streams) error {
	if os.Getenv("MACAZ_ACTIVE") == "1" {
		return errors.New("macaz is already active in this process environment")
	}
	cfg, err := loadOrConfigure(ctx, streams)
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
	server, err := gateway.New(cfg, upstream)
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
	_, _ = fmt.Fprintf(streams.Err, "macaz: Claude Code → %s\n", upstream.Name())
	return launcher.Claude(ctx, cfg, launcher.Options{
		BaseURL:      server.URL(),
		Token:        server.Token(),
		Models:       catalog.IDs,
		DefaultModel: catalog.Default,
		Args:         args,
		Stdin:        streams.In,
		Stdout:       streams.Out,
		Stderr:       streams.Err,
	})
}

func runStatus(ctx context.Context, streams Streams) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
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

func runDoctor(ctx context.Context, streams Streams) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(streams.Out, "Config: %s\n", path)
	_, _ = fmt.Fprintf(streams.Out, "Provider: %s\n", cfg.Provider)
	if err := checkExecutable(ctx, cfg.ClaudeExecutable); err != nil {
		return fmt.Errorf("Claude executable: %w", err)
	}
	_, _ = fmt.Fprintf(streams.Out, "Claude executable: %s\n", cfg.ClaudeExecutable)
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

func loadOrConfigure(ctx context.Context, streams Streams) (config.Config, error) {
	cfg, err := config.Load()
	if err == nil && cfg.Provider != "" {
		return cfg, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config.Config{}, err
	}
	cfg = config.Default()
	cfg, err = wizard(ctx, cfg, streams)
	if err != nil {
		return config.Config{}, err
	}
	if err := config.Save(cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func wizard(ctx context.Context, cfg config.Config, streams Streams) (config.Config, error) {
	reader := bufio.NewReader(streams.In)
	legalNotice(streams.Out)
	_, _ = fmt.Fprintln(streams.Out)
	_, _ = fmt.Fprintln(streams.Out, "Choose the provider for `macaz`:")
	_, _ = fmt.Fprintln(streams.Out, "1. OpenAI Subscription")
	_, _ = fmt.Fprintln(streams.Out, "2. OpenAI API")
	_, _ = fmt.Fprintln(streams.Out, "3. OpenRouter API")
	_, _ = fmt.Fprintln(streams.Out, "4. Codex-CLI")
	_, _ = fmt.Fprintln(streams.Out, "5. OpenCode-CLI")
	choice, err := prompt(reader, streams.Out, "Provider [1-5]: ", "")
	if err != nil {
		return config.Config{}, err
	}
	switch strings.TrimSpace(choice) {
	case "1":
		cfg.Provider = config.ProviderOpenAISubscription
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
	case "2":
		cfg.Provider = config.ProviderOpenAIAPIKey
		key, err := promptSecret(reader, streams, "OpenAI API key: ")
		if err != nil {
			return config.Config{}, err
		}
		if err := secrets.Set(secrets.OpenAIAPIKey, key); err != nil {
			return config.Config{}, err
		}
	case "3":
		cfg.Provider = config.ProviderOpenRouterAPI
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
	case "4":
		cfg.Provider = config.ProviderCodexCLI
		value, err := prompt(reader, streams.Out, "Codex executable", cfg.CodexExecutable)
		if err != nil {
			return config.Config{}, err
		}
		cfg.CodexExecutable = value
		if _, err := exec.LookPath(value); err != nil {
			return config.Config{}, fmt.Errorf("Codex executable %q was not found: %w", value, err)
		}
	case "5":
		cfg.Provider = config.ProviderOpenCodeCLI
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
	default:
		return config.Config{}, fmt.Errorf("invalid provider choice %q", choice)
	}
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
	_, _ = fmt.Fprintln(out, `macaz - use Claude Code with your preferred AI provider

Usage:
  macaz [claude arguments...]
  macaz status
  macaz doctor
  macaz reset
  macaz legal
  macaz version

`+"`macaz` starts Claude Code directly, keeps its normal permission prompts, and routes only model inference through the selected provider.\n"+
		"Pass `--dangerously-skip-permissions` explicitly if you want Claude Code's full-permission mode.")
}

func legalNotice(out io.Writer) {
	_, _ = fmt.Fprintln(out, "macaz is an independent interoperability project. It is not affiliated with, authorized by, endorsed by, or sponsored by Anthropic or any model provider.")
	_, _ = fmt.Fprintln(out, "Claude Code must be installed separately. You are responsible for complying with Claude Code, provider, and organizational terms.")
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
