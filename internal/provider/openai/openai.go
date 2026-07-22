package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/attachments"
	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/localagentsauth"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/provider/openresponses"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

type Mode string

const (
	ModeAPIKey            Mode = "api-key"
	ModeSubscription      Mode = "subscription"
	ModeLocalAPIKey       Mode = "local-api-key"
	ModeLocalOAuth        Mode = "local-oauth"
	ModeCompatible        Mode = "openai-compatible"
	modelsDevURL               = "https://models.dev/api.json"
	responseHeaderTimeout      = 60 * time.Second
	responseIdleTimeout        = 5 * time.Minute
)

type Provider struct {
	mode              Mode
	cfg               config.Config
	httpClient        *http.Client
	account           *accountAuth
	subscriptionMu    sync.Mutex
	subscriptionGates map[string]*generateGate
	subscriptionSlots chan struct{}
	retryBase         time.Duration
	modelMu           sync.Mutex
	models            []provider.Model
	modelsAt          time.Time
	localSource       localagentsauth.Source
}

type generateGate struct {
	token chan struct{}
	refs  int
}

func New(mode Mode, cfg config.Config) (*Provider, error) {
	timeout := time.Duration(cfg.RequestTimeoutSec) * time.Second
	transport := http.DefaultTransport
	if defaultTransport, ok := transport.(*http.Transport); ok {
		configured := defaultTransport.Clone()
		configured.ResponseHeaderTimeout = responseHeaderTimeout
		transport = configured
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	p := &Provider{
		mode:       mode,
		cfg:        cfg,
		httpClient: client,
		retryBase:  time.Second,
	}
	if p.subscription() {
		if mode != ModeLocalOAuth {
			p.account = newAccountAuth(client)
		}
		// Keep turns ordered within one Claude session/agent and bound aggregate
		// account fan-out so a burst of subagents cannot exhaust a subscription.
		p.subscriptionGates = make(map[string]*generateGate)
		limit := cfg.MaxConcurrentSubscription
		if limit < 1 {
			limit = 1
		}
		p.subscriptionSlots = make(chan struct{}, limit)
	}
	return p, nil
}

func NewLocalAgentsAuth(cfg config.Config) (*Provider, error) {
	selected := localagentsauth.Source{Agent: cfg.LocalAuthAgent, Provider: cfg.LocalAuthProvider, Path: cfg.LocalAuthPath}
	var credential localagentsauth.Source
	err := localagentsauth.WithLock(selected, func() error {
		var loadErr error
		credential, loadErr = localagentsauth.Get(selected.Agent, selected.Provider, selected.Path)
		return loadErr
	})
	if err != nil {
		return nil, err
	}
	if !localOpenAIAdapter(credential) {
		return nil, fmt.Errorf("local auth adapter for %s/%s is not implemented", credential.Agent, credential.Provider)
	}
	var mode Mode
	switch credential.Type {
	case "oauth":
		mode = ModeLocalOAuth
	case "api":
		mode = ModeLocalAPIKey
	default:
		return nil, fmt.Errorf("local credential %s/%s has unsupported type %q", credential.Agent, credential.Provider, credential.Type)
	}
	p, err := New(mode, cfg)
	if err != nil {
		return nil, err
	}
	p.localSource = credential
	if mode == ModeLocalOAuth {
		p.account = newLocalAccountAuth(p.httpClient, credential)
	}
	return p, nil
}

func (p *Provider) Name() string {
	if p.subscription() {
		if p.mode == ModeLocalOAuth {
			return "OpenAI Subscription / " + p.localSource.Agent + " auth"
		}
		return "OpenAI Subscription"
	}
	if p.mode == ModeLocalAPIKey {
		return "OpenAI API / " + p.localSource.Agent + " auth"
	}
	if p.mode == ModeCompatible {
		return "OpenAI-compatible endpoint"
	}
	return "OpenAI API"
}

func (p *Provider) Check(ctx context.Context) error {
	switch p.mode {
	case ModeAPIKey, ModeLocalAPIKey, ModeCompatible:
		if p.mode != ModeCompatible {
			if _, err := p.apiKey(); err != nil {
				return err
			}
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
		return fmt.Errorf("configured OpenAI model %q is unavailable or is not a Responses-compatible text model with tool calling", selected)
	case ModeSubscription, ModeLocalOAuth:
		models, err := p.Models(ctx)
		if err != nil {
			return err
		}
		if len(models) == 0 {
			return errors.New("OpenAI Subscription returned no available models")
		}
		return nil
	default:
		return errors.New("unsupported OpenAI mode")
	}
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	if len(p.models) > 0 && time.Since(p.modelsAt) < 5*time.Minute {
		return append([]provider.Model(nil), p.models...), nil
	}
	var (
		models []provider.Model
		err    error
	)
	if p.subscription() {
		models, err = p.subscriptionModels(ctx)
	} else {
		models, err = p.apiModels(ctx)
	}
	if err != nil {
		return nil, err
	}
	p.models = append([]provider.Model(nil), models...)
	p.modelsAt = time.Now()
	return models, nil
}

func (p *Provider) apiModels(ctx context.Context) ([]provider.Model, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("models"), nil)
	if err != nil {
		return nil, err
	}
	if err := p.authorize(ctx, httpReq); err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("list OpenAI models: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &provider.HTTPError{
			Status:  resp.StatusCode,
			Type:    "provider_error",
			Message: strings.TrimSpace(string(raw)),
			Body:    raw,
		}
	}
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode OpenAI models: %w", err)
	}
	selected := p.cfg.ResolveModel("default")
	metadata := map[string]openAIModelMetadata{}
	official := isOfficialOpenAIBaseURL(p.cfg.OpenAIBaseURL)
	if official {
		metadata = p.openAIModelMetadata(ctx)
	}
	models := make([]provider.Model, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		model, ok := openAIProviderModel(id, item.Created, selected, metadata[id], official)
		if ok {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("OpenAI returned no Responses-compatible text models with tool calling")
	}
	return models, nil
}

type openAIModelMetadata struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Attachment       bool   `json:"attachment"`
	ToolCall         bool   `json:"tool_call"`
	StructuredOutput bool   `json:"structured_output"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
}

func (p *Provider) openAIModelMetadata(ctx context.Context) map[string]openAIModelMetadata {
	metadataCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(metadataCtx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	var payload struct {
		OpenAI struct {
			Models map[string]openAIModelMetadata `json:"models"`
		} `json:"openai"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil
	}
	return payload.OpenAI.Models
}

func openAIProviderModel(id string, created int64, selected string, metadata openAIModelMetadata, official bool) (provider.Model, bool) {
	if official && metadata.ID != "" {
		if !metadata.ToolCall || !containsString(metadata.Modalities.Output, "text") {
			return provider.Model{}, false
		}
		efforts := openAIMetadataEfforts(metadata)
		if len(efforts) == 0 {
			efforts = openAIEfforts(id)
		}
		input := normalizeOpenAIInputModalities(metadata.Modalities.Input)
		return provider.Model{
			ID:               id,
			DisplayName:      first(metadata.Name, id),
			Description:      metadata.Description,
			Default:          id == selected,
			Efforts:          efforts,
			InputModalities:  input,
			OutputModalities: append([]string(nil), metadata.Modalities.Output...),
			ContextWindow:    metadata.Limit.Context,
			MaxOutputTokens:  metadata.Limit.Output,
			Created:          created,
			ToolCall:         true,
			StructuredOutput: metadata.StructuredOutput,
			Attachment: metadata.Attachment ||
				containsString(input, "image") ||
				containsString(input, "file"),
		}, true
	}
	if official && !likelyOpenAIResponsesModel(id) {
		return provider.Model{}, false
	}
	return provider.Model{
		ID:               id,
		DisplayName:      id,
		Default:          id == selected,
		Efforts:          openAIEfforts(id),
		InputModalities:  []string{"text"},
		OutputModalities: []string{"text"},
		Created:          created,
		ToolCall:         true,
	}, true
}

func openAIMetadataEfforts(metadata openAIModelMetadata) []string {
	for _, option := range metadata.ReasoningOptions {
		if strings.EqualFold(strings.TrimSpace(option.Type), "effort") {
			return append([]string(nil), option.Values...)
		}
	}
	return nil
}

func normalizeOpenAIInputModalities(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "pdf" {
			value = "file"
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	if len(result) == 0 {
		return []string{"text"}
	}
	return result
}

func isOfficialOpenAIBaseURL(raw string) bool {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(endpoint.Hostname())
	return host == "api.openai.com" || strings.HasSuffix(host, ".api.openai.com")
}

func likelyOpenAIResponsesModel(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	for _, marker := range []string{
		"audio", "embedding", "image", "moderation", "realtime", "search-preview",
		"sora", "transcribe", "tts", "whisper", "dall-e", "babbage", "davinci",
		"instruct",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	if strings.HasPrefix(lower, "gpt-") ||
		strings.HasPrefix(lower, "codex-") ||
		strings.HasPrefix(lower, "ft:gpt-") ||
		strings.HasPrefix(lower, "ft:o") {
		return true
	}
	return len(lower) > 1 && lower[0] == 'o' && lower[1] >= '1' && lower[1] <= '9'
}

func openAIEfforts(model string) []string {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "gpt-5") || strings.Contains(lower, "o1") ||
		strings.Contains(lower, "o3") || strings.Contains(lower, "o4") {
		return []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	}
	return nil
}

func (p *Provider) subscriptionModels(ctx context.Context) ([]provider.Model, error) {
	endpoint, err := url.Parse(accountModelsEndpoint)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("client_version", accountClientVersion)
	endpoint.RawQuery = query.Encode()
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		if err := p.authorize(ctx, req); err != nil {
			return nil, err
		}
		resp, err = p.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list OpenAI Subscription models: %w", err)
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			break
		}
		usedAccess := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		_ = resp.Body.Close()
		if _, err := p.account.forceRefresh(ctx, usedAccess); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &provider.HTTPError{
			Status:  resp.StatusCode,
			Type:    "provider_error",
			Message: strings.TrimSpace(string(raw)),
			Body:    raw,
		}
	}
	var payload struct {
		Models []struct {
			Slug                    string `json:"slug"`
			DisplayName             string `json:"display_name"`
			Description             string `json:"description"`
			DefaultReasoningLevel   string `json:"default_reasoning_level"`
			SupportedReasoningLevel []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			Visibility      string   `json:"visibility"`
			Priority        int      `json:"priority"`
			InputModalities []string `json:"input_modalities"`
			ContextWindow   int64    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode OpenAI Subscription models: %w", err)
	}
	sort.SliceStable(payload.Models, func(i, j int) bool {
		return payload.Models[i].Priority < payload.Models[j].Priority
	})
	models := make([]provider.Model, 0, len(payload.Models))
	for _, item := range payload.Models {
		if strings.TrimSpace(item.Slug) == "" || !strings.EqualFold(item.Visibility, "list") {
			continue
		}
		efforts := make([]string, 0, len(item.SupportedReasoningLevel))
		for _, supported := range item.SupportedReasoningLevel {
			if effort := strings.TrimSpace(supported.Effort); effort != "" {
				efforts = append(efforts, effort)
			}
		}
		modalities := append([]string(nil), item.InputModalities...)
		if len(modalities) == 0 {
			modalities = []string{"text"}
		}
		models = append(models, provider.Model{
			ID:              item.Slug,
			DisplayName:     first(item.DisplayName, item.Slug),
			Description:     item.Description,
			Efforts:         efforts,
			InputModalities: modalities,
			ContextWindow:   item.ContextWindow,
			ToolCall:        true,
			Attachment:      containsString(modalities, "image") || containsString(modalities, "file"),
		})
	}
	if len(models) == 0 {
		return nil, errors.New("OpenAI Subscription returned no visible models")
	}
	models[0].Default = true
	return models, nil
}

func (p *Provider) Generate(ctx context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	model := p.cfg.ResolveModel(req.Model)
	translated, err := protocol.ToResponses(req, model, p.cfg.DefaultEffort)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	translated.Body["stream"] = true
	if p.subscription() {
		if err := normalizeSubscriptionDocuments(ctx, translated.Body, p.cfg.MaxBodyBytes); err != nil {
			return protocol.Result{}, provider.InvalidRequest(err)
		}
		sanitizeSubscription(translated.Body)
	}
	raw, err := json.Marshal(translated.Body)
	if err != nil {
		return protocol.Result{}, err
	}
	release, err := p.acquireGenerate(ctx, req)
	if err != nil {
		return protocol.Result{}, err
	}
	defer release()
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	resp, err := p.sendResponsesWithRetry(streamCtx, raw)
	if err != nil {
		return protocol.Result{}, err
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

func (p *Provider) acquireGenerate(ctx context.Context, req *protocol.Request) (func(), error) {
	if p.subscriptionGates == nil {
		return func() {}, nil
	}
	key := "legacy"
	if req != nil && strings.TrimSpace(req.PromptCacheKey) != "" {
		key = strings.TrimSpace(req.PromptCacheKey)
	}
	p.subscriptionMu.Lock()
	gate := p.subscriptionGates[key]
	if gate == nil {
		gate = &generateGate{token: make(chan struct{}, 1)}
		p.subscriptionGates[key] = gate
	}
	gate.refs++
	p.subscriptionMu.Unlock()
	releaseGate := func(held bool) {
		if held {
			<-gate.token
		}
		p.subscriptionMu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(p.subscriptionGates, key)
		}
		p.subscriptionMu.Unlock()
	}
	select {
	case gate.token <- struct{}{}:
	case <-ctx.Done():
		releaseGate(false)
		return nil, ctx.Err()
	}
	select {
	case p.subscriptionSlots <- struct{}{}:
		return func() {
			<-p.subscriptionSlots
			releaseGate(true)
		}, nil
	case <-ctx.Done():
		releaseGate(true)
		return nil, ctx.Err()
	}
}

func (p *Provider) sendResponsesWithRetry(ctx context.Context, body []byte) (*http.Response, error) {
	// Claude Code also retries retryable gateway responses. Keep the local
	// subscription loop deliberately small so nested retry policies cannot
	// amplify one rate limit into dozens of hidden upstream requests.
	const subscriptionAttempts = 3
	attempts := 1
	if p.subscription() {
		attempts = subscriptionAttempts
	}
	var lastRetry time.Duration
	authRefreshed := false
	for attempt := 0; attempt < attempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			p.endpoint("responses"),
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}
		if err := p.authorize(ctx, httpReq); err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("%s request failed: %w", p.Name(), err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return resp, nil
		}
		usedAccess := strings.TrimPrefix(httpReq.Header.Get("Authorization"), "Bearer ")
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if p.subscription() && resp.StatusCode == http.StatusUnauthorized && !authRefreshed && attempt+1 < attempts {
			authRefreshed = true
			if _, err := p.account.forceRefresh(ctx, usedAccess); err != nil {
				return nil, err
			}
			continue
		}
		lastRetry = responseRetryDelay(resp.Header, attempt, p.retryBase)
		if p.subscription() && retryableSubscriptionStatus(resp.StatusCode) && attempt+1 < attempts {
			timer := time.NewTimer(lastRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
				continue
			}
		}
		message := strings.TrimSpace(string(raw))
		if provider.IsContextWindowOverflow(message) {
			return nil, provider.ContextWindowOverflow(message, raw)
		}
		return nil, &provider.HTTPError{
			Status:     resp.StatusCode,
			Type:       "provider_error",
			Message:    message,
			Body:       raw,
			RetryAfter: lastRetry,
		}
	}
	return nil, errors.New("OpenAI Subscription retry loop ended unexpectedly")
}

func retryableSubscriptionStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func responseRetryDelay(header http.Header, attempt int, base time.Duration) time.Duration {
	if milliseconds, err := strconv.ParseInt(strings.TrimSpace(header.Get("retry-after-ms")), 10, 64); err == nil && milliseconds > 0 {
		return min(time.Duration(milliseconds)*time.Millisecond, 30*time.Second)
	}
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
			return min(time.Duration(seconds)*time.Second, 30*time.Second)
		}
		if when, err := http.ParseTime(value); err == nil {
			if delay := time.Until(when); delay > 0 {
				return min(delay, 30*time.Second)
			}
		}
	}
	if base <= 0 {
		base = time.Second
	}
	delay := base << min(attempt, 5)
	return min(delay, 30*time.Second)
}

func (p *Provider) CountTokens(ctx context.Context, req *protocol.Request) (int, bool, error) {
	if p.subscription() || p.mode == ModeCompatible {
		return protocol.EstimateInputTokens(req), true, nil
	}
	model := p.cfg.ResolveModel(req.Model)
	translated, err := protocol.ToResponses(req, model, p.cfg.DefaultEffort)
	if err != nil {
		return 0, false, provider.InvalidRequest(err)
	}
	delete(translated.Body, "stream")
	delete(translated.Body, "store")
	delete(translated.Body, "max_output_tokens")
	raw, err := json.Marshal(translated.Body)
	if err != nil {
		return 0, false, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint("responses/input_tokens"), bytes.NewReader(raw))
	if err != nil {
		return 0, false, err
	}
	if err := p.authorize(ctx, httpReq); err != nil {
		return 0, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, false, &provider.HTTPError{Status: resp.StatusCode, Type: "provider_error", Message: strings.TrimSpace(string(body)), Body: body}
	}
	var result struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, false, err
	}
	if result.InputTokens < 1 {
		return 0, false, errors.New("OpenAI input token count was empty")
	}
	return result.InputTokens, false, nil
}

func (p *Provider) endpoint(path string) string {
	if p.subscription() {
		return accountResponsesEndpoint
	}
	base := strings.TrimRight(p.cfg.OpenAIBaseURL, "/")
	return base + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) authorize(ctx context.Context, req *http.Request) error {
	if !p.subscription() {
		if p.mode == ModeCompatible {
			return nil
		}
		key, err := p.apiKey()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		return nil
	}
	creds, err := p.account.credentials(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.Access)
	req.Header.Set("originator", "macaz")
	req.Header.Set("version", accountClientVersion)
	req.Header.Set("User-Agent", "macaz/"+accountClientVersion)
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	return nil
}

func (p *Provider) subscription() bool {
	return p.mode == ModeSubscription || p.mode == ModeLocalOAuth
}

func (p *Provider) apiKey() (string, error) {
	if p.mode != ModeLocalAPIKey {
		return secrets.Get(secrets.OpenAIAPIKey, "OPENAI_API_KEY")
	}
	selected := localagentsauth.Source{Agent: p.cfg.LocalAuthAgent, Provider: p.cfg.LocalAuthProvider, Path: p.cfg.LocalAuthPath}
	var credential localagentsauth.Source
	err := localagentsauth.WithLock(selected, func() error {
		var loadErr error
		credential, loadErr = localagentsauth.Get(selected.Agent, selected.Provider, selected.Path)
		return loadErr
	})
	if err != nil {
		return "", err
	}
	if credential.Type != "api" || strings.TrimSpace(credential.Key) == "" {
		return "", fmt.Errorf("local credential %s/%s is not a usable API key", credential.Agent, credential.Provider)
	}
	return strings.TrimSpace(credential.Key), nil
}

func localOpenAIAdapter(source localagentsauth.Source) bool {
	return source.Provider == "openai" || source.Agent == "pi" && source.Provider == "openai-codex"
}

func containsString(values []string, target string) bool {
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

func sanitizeSubscription(body map[string]any) {
	body["store"] = false
	body["stream"] = true
	// Claude Code can expose recursive Agent tools. A subscription account has
	// tight burst limits, and parallel agent calls can fan out exponentially.
	// Keep every client tool available but make the model request them one at a
	// time; Claude still executes each tool with its normal local permissions.
	body["parallel_tool_calls"] = false
	// The ChatGPT Codex Responses surface is narrower than the public
	// Responses API and rejects the public request attribution field.
	// Claude Code supplies metadata.user_id on normal turns, which ToResponses
	// maps to user for API providers; it must not cross this account endpoint.
	delete(body, "user")
	delete(body, "max_output_tokens")
	delete(body, "truncation")
	delete(body, "previous_response_id")
}

func normalizeSubscriptionDocuments(ctx context.Context, body map[string]any, maxInputBytes int64) error {
	if maxInputBytes <= 0 || maxInputBytes > attachments.DefaultMaxBytes {
		maxInputBytes = attachments.DefaultMaxBytes
	}
	normalized, err := normalizeSubscriptionInputValue(ctx, body["input"], maxInputBytes)
	if err != nil {
		return err
	}
	body["input"] = normalized
	return nil
}

func normalizeSubscriptionInputValue(ctx context.Context, value any, maxInputBytes int64) (any, error) {
	switch value := value.(type) {
	case []any:
		normalized := make([]any, len(value))
		for index, item := range value {
			next, err := normalizeSubscriptionInputValue(ctx, item, maxInputBytes)
			if err != nil {
				return nil, err
			}
			normalized[index] = next
		}
		return normalized, nil
	case []map[string]any:
		normalized := make([]map[string]any, len(value))
		for index, item := range value {
			next, err := normalizeSubscriptionInputMap(ctx, item, maxInputBytes)
			if err != nil {
				return nil, err
			}
			normalized[index] = next
		}
		return normalized, nil
	case map[string]any:
		return normalizeSubscriptionInputMap(ctx, value, maxInputBytes)
	default:
		return value, nil
	}
}

func normalizeSubscriptionInputMap(
	ctx context.Context,
	value map[string]any,
	maxInputBytes int64,
) (map[string]any, error) {
	if value["type"] == "input_file" {
		return subscriptionDocumentText(ctx, value, maxInputBytes)
	}
	normalized := make(map[string]any, len(value))
	for key, item := range value {
		next, err := normalizeSubscriptionInputValue(ctx, item, maxInputBytes)
		if err != nil {
			return nil, err
		}
		normalized[key] = next
	}
	return normalized, nil
}

func subscriptionDocumentText(
	ctx context.Context,
	file map[string]any,
	maxInputBytes int64,
) (map[string]any, error) {
	attachment := protocol.Attachment{
		Kind:     "document",
		Filename: stringField(file, "filename"),
	}
	if attachment.Filename == "" {
		attachment.Filename = "document.pdf"
	}
	switch {
	case stringField(file, "file_data") != "":
		raw := stringField(file, "file_data")
		header, data, ok := strings.Cut(raw, ",")
		if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
			return nil, errors.New("OpenAI Subscription document has an invalid base64 data URL")
		}
		attachment.MediaType = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		attachment.Data = data
	case stringField(file, "file_url") != "":
		attachment.URL = stringField(file, "file_url")
		attachment.MediaType = documentMediaType(attachment.Filename)
	default:
		return nil, errors.New("OpenAI Subscription document has no supported data or URL source")
	}
	text, err := attachments.Text(
		ctx,
		attachment,
		maxInputBytes,
		attachments.DefaultMaxTextBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("prepare OpenAI Subscription document %q: %w", attachment.Filename, err)
	}
	return map[string]any{
		"type": "input_text",
		"text": fmt.Sprintf(
			"<macaz_document filename=%q media_type=%q>\n%s\n</macaz_document>",
			attachment.Filename,
			attachment.MediaType,
			text,
		),
	}, nil
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func documentMediaType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".txt", ".md", ".markdown", ".log",
		".go", ".py", ".rs", ".java", ".c", ".h", ".cpp", ".hpp", ".cs",
		".rb", ".php", ".swift", ".kt", ".kts", ".sh", ".bash", ".zsh",
		".fish", ".sql", ".css", ".html", ".htm", ".ts", ".tsx", ".jsx",
		".vue", ".svelte", ".ini", ".conf", ".env":
		return "text/plain"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".js", ".mjs", ".cjs":
		return "application/javascript"
	case ".toml":
		return "application/toml"
	case ".xml":
		return "application/xml"
	case ".yaml", ".yml":
		return "application/yaml"
	default:
		return "application/pdf"
	}
}
