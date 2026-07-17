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
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/provider/openresponses"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

type Provider struct {
	cfg        config.Config
	httpClient *http.Client
	modelMu    sync.Mutex
	models     []provider.Model
	modelsAt   time.Time
}

func New(cfg config.Config) *Provider {
	return &Provider{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.RequestTimeoutSec) * time.Second,
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
		return providerError(resp.StatusCode, raw)
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
		return nil, providerError(resp.StatusCode, raw)
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
	raw, err := json.Marshal(translated.Body)
	if err != nil {
		return protocol.Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint("responses"), bytes.NewReader(raw))
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return protocol.Result{}, providerError(resp.StatusCode, body)
	}
	collector := openresponses.NewCollector(model, translated.ToolNames, req.Stream, emit)
	if err := openresponses.ReadSSE(resp.Body, collector.Handle); err != nil {
		return protocol.Result{}, err
	}
	return collector.Result(), nil
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

func providerError(status int, raw []byte) error {
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
	return &provider.HTTPError{
		Status:  status,
		Type:    "provider_error",
		Message: message,
		Body:    raw,
	}
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
