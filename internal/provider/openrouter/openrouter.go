package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/provider/openresponses"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

const (
	responseHeaderTimeout = 60 * time.Second
	responseIdleTimeout   = 5 * time.Minute
)

type Provider struct {
	cfg        config.Config
	httpClient *http.Client
	modelMu    sync.Mutex
	models     []provider.Model
	modelsAt   time.Time
}

func New(cfg config.Config) *Provider {
	transport := http.DefaultTransport
	if defaultTransport, ok := transport.(*http.Transport); ok {
		configured := defaultTransport.Clone()
		configured.ResponseHeaderTimeout = responseHeaderTimeout
		transport = configured
	}
	return &Provider{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.RequestTimeoutSec) * time.Second,
		},
	}
}

func (p *Provider) Name() string {
	return "OpenRouter API"
}

func (p *Provider) Check(ctx context.Context) error {
	if _, err := secrets.Get(secrets.OpenRouterAPIKey, "OPENROUTER_API_KEY"); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("key"), nil)
	if err != nil {
		return err
	}
	if err := p.authorize(req); err != nil {
		return err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("validate OpenRouter API key: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return providerError(resp.StatusCode, resp.Header, raw)
	}
	models, err := p.Models(ctx)
	if err != nil {
		return err
	}
	selected := p.cfg.ResolveModel("default")
	for _, model := range models {
		if model.ID == selected {
			return nil
		}
	}
	return fmt.Errorf("configured OpenRouter model %q is unavailable or does not support tool calling", selected)
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	if len(p.models) > 0 && time.Since(p.modelsAt) < 5*time.Minute {
		return append([]provider.Model(nil), p.models...), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("models")+"?output_modalities=text", nil)
	if err != nil {
		return nil, err
	}
	if key, err := secrets.Get(secrets.OpenRouterAPIKey, "OPENROUTER_API_KEY"); err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list OpenRouter models: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, providerError(resp.StatusCode, resp.Header, raw)
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Description   string `json:"description"`
			Created       int64  `json:"created"`
			ContextLength int64  `json:"context_length"`
			Architecture  struct {
				InputModalities  []string `json:"input_modalities"`
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			TopProvider struct {
				MaxCompletionTokens int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
			Reasoning           struct {
				SupportedEfforts []string `json:"supported_efforts"`
			} `json:"reasoning"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode OpenRouter models: %w", err)
	}
	selected := p.cfg.ResolveModel("default")
	models := make([]provider.Model, 0, len(payload.Data))
	for _, item := range payload.Data {
		parameters := stringSet(item.SupportedParameters)
		if strings.TrimSpace(item.ID) == "" || !parameters["tools"] {
			continue
		}
		if !contains(item.Architecture.OutputModalities, "text") {
			continue
		}
		efforts := append([]string(nil), item.Reasoning.SupportedEfforts...)
		sort.Strings(efforts)
		input := append([]string(nil), item.Architecture.InputModalities...)
		sort.Strings(input)
		models = append(models, provider.Model{
			ID:                  item.ID,
			DisplayName:         first(item.Name, item.ID),
			Description:         item.Description,
			Default:             item.ID == selected,
			Efforts:             efforts,
			InputModalities:     input,
			OutputModalities:    item.Architecture.OutputModalities,
			SupportedParameters: item.SupportedParameters,
			ContextWindow:       item.ContextLength,
			MaxOutputTokens:     item.TopProvider.MaxCompletionTokens,
			Created:             item.Created,
			ToolCall:            true,
			StructuredOutput:    parameters["structured_outputs"] || parameters["response_format"],
			Attachment:          contains(input, "file") || contains(input, "image"),
		})
	}
	if len(models) == 0 {
		return nil, errors.New("OpenRouter returned no text models with tool calling")
	}
	p.models = append([]provider.Model(nil), models...)
	p.modelsAt = time.Now()
	return models, nil
}

func (p *Provider) Generate(ctx context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	model := p.cfg.ResolveModel(req.Model)
	translated, err := protocol.ToResponses(req, model, p.cfg.DefaultEffort)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	translated.Body["stream"] = true
	if sessionID := strings.TrimSpace(req.PromptCacheKey); sessionID != "" {
		// OpenRouter uses session_id as a sticky routing key across providers,
		// improving prompt-cache locality without changing the selected model.
		translated.Body["session_id"] = sessionID
	}
	raw, err := json.Marshal(translated.Body)
	if err != nil {
		return protocol.Result{}, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, p.endpoint("responses"), bytes.NewReader(raw))
	if err != nil {
		return protocol.Result{}, err
	}
	if err := p.authorize(httpReq); err != nil {
		return protocol.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("HTTP-Referer", "https://github.com/macaz-dev/macaz-cli")
	httpReq.Header.Set("X-Title", "macaz")
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return protocol.Result{}, fmt.Errorf("OpenRouter request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return protocol.Result{}, providerError(resp.StatusCode, resp.Header, body)
	}
	resp.Body = openresponses.NewIdleReadCloser(resp.Body, cancelStream, responseIdleTimeout)
	defer resp.Body.Close()
	collector := openresponses.NewCollector(model, translated.ToolNames, req.Stream, emit)
	if err := openresponses.ReadSSE(resp.Body, collector.Handle); err != nil && !errors.Is(err, openresponses.ErrToolCallReady) {
		if errors.Is(err, openresponses.ErrResponseIdleTimeout) {
			return protocol.Result{}, provider.Timeout(err.Error())
		}
		if provider.IsContextWindowOverflow(err.Error()) {
			return protocol.Result{}, provider.ContextWindowOverflow(err.Error(), nil)
		}
		var streamErr *openresponses.StreamError
		if errors.As(err, &streamErr) {
			return protocol.Result{}, provider.StreamFailure(first(streamErr.Type, streamErr.Code), streamErr.Message)
		}
		return protocol.Result{}, err
	}
	result, err := collector.Finalize()
	if err == nil && result.Usage.Estimated && result.Usage.InputTokens == 0 {
		result.Usage.InputTokens = int64(protocol.EstimateInputTokens(req))
	}
	return result, err
}

func (p *Provider) CountTokens(_ context.Context, req *protocol.Request) (int, bool, error) {
	return protocol.EstimateInputTokens(req), true, nil
}

func (p *Provider) endpoint(path string) string {
	return strings.TrimRight(p.cfg.OpenRouterBaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) authorize(req *http.Request) error {
	key, err := secrets.Get(secrets.OpenRouterAPIKey, "OPENROUTER_API_KEY")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return nil
}

func providerError(status int, header http.Header, raw []byte) error {
	message := strings.TrimSpace(string(raw))
	var payload struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Error.Message != "" {
		message = payload.Error.Message
	}
	if provider.IsContextWindowOverflow(message) {
		return provider.ContextWindowOverflow(message, raw)
	}
	return &provider.HTTPError{
		Status:     status,
		Type:       "provider_error",
		Message:    message,
		Body:       raw,
		RetryAfter: openRouterRetryAfter(header),
	}
}

func openRouterRetryAfter(header http.Header) time.Duration {
	if milliseconds, err := strconv.ParseInt(strings.TrimSpace(header.Get("retry-after-ms")), 10, 64); err == nil && milliseconds > 0 {
		return time.Duration(milliseconds) * time.Millisecond
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[strings.ToLower(strings.TrimSpace(value))] = true
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
