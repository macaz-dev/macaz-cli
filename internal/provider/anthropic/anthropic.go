package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	anthropicVersion      = "2023-06-01"
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

type capabilitySupport struct {
	Supported bool `json:"supported"`
}

type modelCapabilities struct {
	Effort struct {
		Supported bool              `json:"supported"`
		Low       capabilitySupport `json:"low"`
		Medium    capabilitySupport `json:"medium"`
		High      capabilitySupport `json:"high"`
		XHigh     capabilitySupport `json:"xhigh"`
		Max       capabilitySupport `json:"max"`
	} `json:"effort"`
	ImageInput        capabilitySupport `json:"image_input"`
	PDFInput          capabilitySupport `json:"pdf_input"`
	StructuredOutputs capabilitySupport `json:"structured_outputs"`
	Thinking          struct {
		Supported bool `json:"supported"`
	} `json:"thinking"`
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

func (p *Provider) Name() string { return "Anthropic API" }

func (p *Provider) Check(ctx context.Context) error {
	if _, err := secrets.Get(secrets.AnthropicAPIKey, "ANTHROPIC_API_KEY"); err != nil {
		return err
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
	return fmt.Errorf("configured Anthropic model %q is unavailable", selected)
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	if len(p.models) > 0 && time.Since(p.modelsAt) < 5*time.Minute {
		return append([]provider.Model(nil), p.models...), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("models?limit=1000"), nil)
	if err != nil {
		return nil, err
	}
	if err := p.authorize(req); err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list Anthropic models: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, providerError(resp.StatusCode, resp.Header, raw)
	}
	var payload struct {
		Data []struct {
			ID             string             `json:"id"`
			DisplayName    string             `json:"display_name"`
			CreatedAt      string             `json:"created_at"`
			MaxInputTokens int64              `json:"max_input_tokens"`
			MaxTokens      int64              `json:"max_tokens"`
			Capabilities   *modelCapabilities `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode Anthropic models: %w", err)
	}
	selected := p.cfg.ResolveModel("default")
	models := make([]provider.Model, 0, len(payload.Data))
	for _, item := range payload.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		created, _ := time.Parse(time.RFC3339, item.CreatedAt)
		efforts := anthropicEfforts(item.ID)
		inputModalities := []string{"text", "image", "document"}
		thinking := true
		structuredOutputs := false
		if item.Capabilities != nil {
			efforts = capabilityEfforts(item.Capabilities)
			inputModalities = []string{"text"}
			if item.Capabilities.ImageInput.Supported {
				inputModalities = append(inputModalities, "image")
			}
			if item.Capabilities.PDFInput.Supported {
				inputModalities = append(inputModalities, "document")
			}
			thinking = item.Capabilities.Thinking.Supported
			structuredOutputs = item.Capabilities.StructuredOutputs.Supported
		}
		parameters := []string{"tools", "tool_choice", "temperature", "top_p", "top_k"}
		if thinking {
			parameters = append(parameters, "thinking")
		}
		if len(efforts) > 0 {
			parameters = append(parameters, "output_config")
		}
		if structuredOutputs {
			parameters = append(parameters, "output_format")
		}
		contextWindow := item.MaxInputTokens
		if contextWindow <= 0 {
			contextWindow = 200000
		}
		maxOutputTokens := item.MaxTokens
		if maxOutputTokens <= 0 {
			maxOutputTokens = int64(anthropicDefaultMaxTokens(item.ID))
		}
		models = append(models, provider.Model{
			ID:                  item.ID,
			DisplayName:         first(item.DisplayName, item.ID),
			Default:             item.ID == selected,
			Efforts:             efforts,
			InputModalities:     inputModalities,
			OutputModalities:    []string{"text"},
			SupportedParameters: parameters,
			ContextWindow:       contextWindow,
			MaxOutputTokens:     maxOutputTokens,
			Created:             created.Unix(),
			ToolCall:            true,
			StructuredOutput:    structuredOutputs,
			Attachment:          len(inputModalities) > 1,
		})
	}
	if len(models) == 0 {
		return nil, errors.New("Anthropic returned no models")
	}
	p.models = append([]provider.Model(nil), models...)
	p.modelsAt = time.Now()
	return models, nil
}

func (p *Provider) Generate(ctx context.Context, request *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	req, names, err := protocol.PrepareNativeToolRequest(request)
	if err != nil {
		return protocol.Result{}, err
	}
	// The local server has already resolved the client-facing model ID to the
	// exact Anthropic catalog ID. Resolving it again would turn any explicit
	// claude-* selection back into the configured default.
	req.Model = strings.TrimSpace(request.Model)
	if req.Model == "" {
		req.Model = p.cfg.ResolveModel("default")
	}
	p.configureAnthropicRequest(req)
	if req.MaxTokens <= 0 {
		if model, ok := p.modelInfo(req.Model); ok && model.MaxOutputTokens > 0 {
			req.MaxTokens = int(model.MaxOutputTokens)
		} else {
			req.MaxTokens = anthropicDefaultMaxTokens(req.Model)
		}
	}
	req.Stream = true
	raw, err := json.Marshal(req)
	if err != nil {
		return protocol.Result{}, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, p.endpoint("messages"), bytes.NewReader(raw))
	if err != nil {
		return protocol.Result{}, err
	}
	if err := p.authorize(httpReq); err != nil {
		return protocol.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return protocol.Result{}, fmt.Errorf("Anthropic request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return protocol.Result{}, providerError(resp.StatusCode, resp.Header, body)
	}
	resp.Body = openresponses.NewIdleReadCloser(resp.Body, cancelStream, responseIdleTimeout)
	defer resp.Body.Close()
	collector := newCollector(req.Model, names, request.Stream, emit)
	if err := openresponses.ReadSSE(resp.Body, collector.handle); err != nil {
		if errors.Is(err, openresponses.ErrResponseIdleTimeout) {
			return protocol.Result{}, provider.Timeout(err.Error())
		}
		return protocol.Result{}, err
	}
	return collector.finalize()
}

func (p *Provider) CountTokens(ctx context.Context, request *protocol.Request) (int, bool, error) {
	req, _, err := protocol.PrepareNativeToolRequest(request)
	if err != nil {
		return 0, false, err
	}
	req.Model = strings.TrimSpace(request.Model)
	if req.Model == "" {
		req.Model = p.cfg.ResolveModel("default")
	}
	p.configureAnthropicRequest(req)
	req.Stream = false
	raw, err := json.Marshal(req)
	if err != nil {
		return 0, false, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint("messages/count_tokens"), bytes.NewReader(raw))
	if err != nil {
		return 0, false, err
	}
	if err := p.authorize(httpReq); err != nil {
		return 0, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return 0, false, fmt.Errorf("count Anthropic tokens: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, false, providerError(resp.StatusCode, resp.Header, body)
	}
	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false, fmt.Errorf("decode Anthropic token count: %w", err)
	}
	return payload.InputTokens, false, nil
}

func (p *Provider) configureAnthropicRequest(req *protocol.Request) {
	if model, ok := p.modelInfo(req.Model); ok {
		if len(model.Efforts) == 0 {
			req.OutputConfig = nil
			return
		}
		if len(req.OutputConfig) > 0 && len(req.Thinking) == 0 && anthropicUsesAdaptiveThinking(req.Model) {
			req.Thinking = json.RawMessage(`{"type":"adaptive"}`)
		}
		return
	}
	configureAnthropicRequest(req)
}

func (p *Provider) modelInfo(id string) (provider.Model, bool) {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	for _, model := range p.models {
		if model.ID == id {
			return model, true
		}
	}
	return provider.Model{}, false
}

func capabilityEfforts(capabilities *modelCapabilities) []string {
	if capabilities == nil || !capabilities.Effort.Supported {
		return nil
	}
	levels := []struct {
		name      string
		supported bool
	}{
		{name: "low", supported: capabilities.Effort.Low.Supported},
		{name: "medium", supported: capabilities.Effort.Medium.Supported},
		{name: "high", supported: capabilities.Effort.High.Supported},
		{name: "xhigh", supported: capabilities.Effort.XHigh.Supported},
		{name: "max", supported: capabilities.Effort.Max.Supported},
	}
	result := make([]string, 0, len(levels))
	for _, level := range levels {
		if level.supported {
			result = append(result, level.name)
		}
	}
	return result
}

func configureAnthropicRequest(req *protocol.Request) {
	if len(anthropicEfforts(req.Model)) == 0 {
		req.OutputConfig = nil
		return
	}
	if len(req.OutputConfig) > 0 && len(req.Thinking) == 0 && anthropicUsesAdaptiveThinking(req.Model) {
		req.Thinking = json.RawMessage(`{"type":"adaptive"}`)
	}
}

func anthropicEfforts(model string) []string {
	model = normalizedModelID(model)
	switch {
	case containsAny(model,
		"fable-5", "mythos-5", "opus-4-8", "opus-4-7", "sonnet-5"):
		return []string{"low", "medium", "high", "xhigh", "max"}
	case containsAny(model, "opus-4-6", "sonnet-4-6"):
		return []string{"low", "medium", "high", "max"}
	case strings.Contains(model, "opus-4-5"):
		return []string{"low", "medium", "high"}
	default:
		return nil
	}
}

func anthropicUsesAdaptiveThinking(model string) bool {
	model = normalizedModelID(model)
	return containsAny(model, "opus-4-6", "sonnet-4-6", "opus-4-7", "opus-4-8")
}

func anthropicDefaultMaxTokens(model string) int {
	model = normalizedModelID(model)
	switch {
	case strings.Contains(model, "3-5"):
		return 8192
	case strings.Contains(model, "-5"), strings.Contains(model, "-4-"), strings.Contains(model, "3-7"):
		return 32000
	default:
		return 4096
	}
}

func normalizedModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.NewReplacer(".", "-", "_", "-").Replace(model)
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func (p *Provider) endpoint(path string) string {
	return strings.TrimRight(p.cfg.AnthropicBaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) authorize(req *http.Request) error {
	key, err := secrets.Get(secrets.AnthropicAPIKey, "ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("User-Agent", "macaz-cli")
	return nil
}

type collector struct {
	model      string
	names      *protocol.ToolNames
	stream     bool
	emit       protocol.EmitFunc
	id         string
	blocks     []protocol.Block
	open       map[int]bool
	arguments  map[int]string
	stopReason string
	usage      protocol.Usage
	terminal   bool
}

func newCollector(model string, names *protocol.ToolNames, stream bool, emit protocol.EmitFunc) *collector {
	return &collector{
		model:      model,
		names:      names,
		stream:     stream,
		emit:       emit,
		open:       map[int]bool{},
		arguments:  map[int]string{},
		stopReason: "end_turn",
	}
}

func (c *collector) handle(event string, data []byte) error {
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode Anthropic SSE %s: %w", event, err)
	}
	if event == "" {
		_ = json.Unmarshal(payload["type"], &event)
	}
	switch event {
	case "message_start":
		var message struct {
			ID    string         `json:"id"`
			Model string         `json:"model"`
			Usage protocol.Usage `json:"usage"`
		}
		if err := json.Unmarshal(payload["message"], &message); err != nil {
			return err
		}
		c.id = message.ID
		c.model = first(message.Model, c.model)
		c.usage = message.Usage
	case "content_block_start":
		var index int
		var block protocol.Block
		if err := json.Unmarshal(payload["index"], &index); err != nil {
			return err
		}
		if err := json.Unmarshal(payload["content_block"], &block); err != nil {
			return err
		}
		if block.Type == "tool_use" && c.names != nil {
			block.Name = c.names.Client(block.Name)
		}
		for len(c.blocks) <= index {
			c.blocks = append(c.blocks, protocol.Block{})
		}
		c.blocks[index] = block
		c.open[index] = true
		if block.Type == "tool_use" {
			c.arguments[index] = ""
			c.stopReason = "tool_use"
		}
		if c.stream && c.emit != nil {
			return c.emit(protocol.Event{Kind: protocol.EventBlockStart, Index: index, Block: block})
		}
	case "content_block_delta":
		var index int
		var delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
		}
		if err := json.Unmarshal(payload["index"], &index); err != nil {
			return err
		}
		if err := json.Unmarshal(payload["delta"], &delta); err != nil {
			return err
		}
		if index < 0 || index >= len(c.blocks) {
			return fmt.Errorf("Anthropic streamed delta for unknown block %d", index)
		}
		value := ""
		switch delta.Type {
		case "text_delta":
			value = delta.Text
			c.blocks[index].Text += value
		case "thinking_delta":
			value = delta.Thinking
			c.blocks[index].Thinking += value
		case "signature_delta":
			value = delta.Signature
			c.blocks[index].Signature += value
		case "input_json_delta":
			value = delta.PartialJSON
			c.arguments[index] += value
		default:
			return nil
		}
		if c.stream && c.emit != nil && value != "" {
			return c.emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: index, DeltaType: delta.Type, Delta: value})
		}
	case "content_block_stop":
		var index int
		if err := json.Unmarshal(payload["index"], &index); err != nil {
			return err
		}
		if index >= 0 && index < len(c.blocks) && c.blocks[index].Type == "tool_use" {
			arguments := c.arguments[index]
			if arguments == "" || !json.Valid([]byte(arguments)) {
				return fmt.Errorf("Anthropic tool %q returned invalid or empty JSON arguments", c.blocks[index].Name)
			}
			c.blocks[index].Input = json.RawMessage(arguments)
		}
		if c.open[index] {
			delete(c.open, index)
			if c.stream && c.emit != nil {
				return c.emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index, Block: c.blocks[index]})
			}
		}
	case "message_delta":
		var delta struct {
			StopReason string `json:"stop_reason"`
		}
		var usage protocol.Usage
		_ = json.Unmarshal(payload["delta"], &delta)
		_ = json.Unmarshal(payload["usage"], &usage)
		if delta.StopReason != "" {
			c.stopReason = delta.StopReason
		}
		if usage.OutputTokens != 0 {
			c.usage.OutputTokens = usage.OutputTokens
		}
		if usage.InputTokens != 0 {
			c.usage.InputTokens = usage.InputTokens
		}
		c.usage.CacheCreationInputTokens = usage.CacheCreationInputTokens
		c.usage.CacheReadInputTokens = usage.CacheReadInputTokens
	case "message_stop":
		if len(c.open) != 0 {
			return errors.New("Anthropic stream stopped with open content blocks")
		}
		c.terminal = true
	case "error":
		message := anthropicErrorMessage(data)
		if provider.IsContextWindowOverflow(message) {
			return provider.ContextWindowOverflow(message, data)
		}
		return errors.New(message)
	}
	return nil
}

func (c *collector) result() protocol.Result {
	if c.id == "" {
		c.id = "msg_macaz"
	}
	return protocol.Result{ID: c.id, Model: c.model, Blocks: c.blocks, StopReason: c.stopReason, Usage: c.usage}
}

func (c *collector) finalize() (protocol.Result, error) {
	if !c.terminal {
		return protocol.Result{}, errors.New("Anthropic stream ended without message_stop")
	}
	if len(c.open) != 0 {
		return protocol.Result{}, errors.New("Anthropic stream ended with open content blocks")
	}
	return c.result(), nil
}

func providerError(status int, header http.Header, raw []byte) error {
	message := anthropicErrorMessage(raw)
	if provider.IsContextWindowOverflow(message) {
		return provider.ContextWindowOverflow(message, raw)
	}
	return &provider.HTTPError{
		Status:     status,
		Type:       "provider_error",
		Message:    message,
		Body:       raw,
		RetryAfter: retryAfter(header.Get("Retry-After")),
	}
}

func anthropicErrorMessage(raw []byte) string {
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return payload.Error.Message
	}
	message := strings.TrimSpace(string(raw))
	if message == "" {
		return "Anthropic returned an empty error"
	}
	return message
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
