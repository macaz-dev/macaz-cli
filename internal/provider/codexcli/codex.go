package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/attachments"
	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

const parallelToolGrace = 150 * time.Millisecond

type Provider struct {
	cfg      config.Config
	modelMu  sync.Mutex
	models   []provider.Model
	modelsAt time.Time
}

func New(cfg config.Config) *Provider {
	return &Provider{cfg: cfg}
}

func (p *Provider) Name() string {
	return "Codex-CLI"
}

func (p *Provider) Check(ctx context.Context) error {
	exe, err := exec.LookPath(p.cfg.CodexExecutable)
	if err != nil {
		return fmt.Errorf("find Codex executable %q: %w", p.cfg.CodexExecutable, err)
	}
	command := exec.CommandContext(ctx, exe, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run %s --version: %w: %s", exe, err, strings.TrimSpace(string(output)))
	}
	if !bytes.Contains(bytes.ToLower(output), []byte("codex")) {
		return fmt.Errorf("%s does not identify itself as Codex: %s", exe, strings.TrimSpace(string(output)))
	}
	_, err = p.Models(ctx)
	return err
}

func (p *Provider) CountTokens(_ context.Context, req *protocol.Request) (int, bool, error) {
	return protocol.EstimateInputTokens(req), true, nil
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	if len(p.models) > 0 && time.Since(p.modelsAt) < 5*time.Minute {
		return append([]provider.Model(nil), p.models...), nil
	}
	models, err := p.discoverModels(ctx)
	if err != nil {
		return nil, err
	}
	p.models = append([]provider.Model(nil), models...)
	p.modelsAt = time.Now()
	return models, nil
}

func (p *Provider) discoverModels(ctx context.Context) ([]provider.Model, error) {
	exe, err := exec.LookPath(p.cfg.CodexExecutable)
	if err != nil {
		return nil, fmt.Errorf("find Codex executable %q: %w", p.cfg.CodexExecutable, err)
	}
	requestDir, err := os.MkdirTemp("", "macaz-codex-models-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(requestDir)
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, exe, appServerArgs()...)
	cmd.Dir = requestDir
	cmd.Env = codexEnvironment(requestDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	var stderr boundedBuffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderr, stderrPipe)
		close(stderrDone)
	}()
	rpc := newRPC(stdin, stdout)
	defer func() {
		cancel()
		_ = stdin.Close()
		_ = cmd.Wait()
		<-stderrDone
	}()
	if _, err := rpc.request(ctx, 1, "initialize", initializeParams()); err != nil {
		return nil, withStderr(err, stderr.String())
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	var models []provider.Model
	var cursor any
	for requestID := 2; ; requestID++ {
		response, err := rpc.request(ctx, requestID, "model/list", map[string]any{
			"cursor":        cursor,
			"limit":         100,
			"includeHidden": false,
		})
		if err != nil {
			return nil, withStderr(err, stderr.String())
		}
		for _, raw := range sliceValue(response["data"]) {
			item := mapValue(raw)
			if item == nil || boolValue(item["hidden"]) {
				continue
			}
			modelID := first(stringValue(item["model"]), stringValue(item["id"]))
			if modelID == "" {
				continue
			}
			efforts := make([]string, 0)
			for _, rawEffort := range sliceValue(item["supportedReasoningEfforts"]) {
				effort := stringValue(mapValue(rawEffort)["reasoningEffort"])
				if effort != "" {
					efforts = append(efforts, effort)
				}
			}
			modalities := stringSlice(item["inputModalities"])
			models = append(models, provider.Model{
				ID:              modelID,
				DisplayName:     first(stringValue(item["displayName"]), modelID),
				Description:     stringValue(item["description"]),
				Default:         boolValue(item["isDefault"]),
				Efforts:         efforts,
				InputModalities: modalities,
			})
		}
		next := stringValue(response["nextCursor"])
		if next == "" {
			break
		}
		cursor = next
	}
	if len(models) == 0 {
		return nil, errors.New("Codex app-server returned no models")
	}
	return models, nil
}

func (p *Provider) Generate(ctx context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	exe, err := exec.LookPath(p.cfg.CodexExecutable)
	if err != nil {
		return protocol.Result{}, fmt.Errorf("find Codex executable %q: %w", p.cfg.CodexExecutable, err)
	}
	selectedModel, err := p.selectModel(ctx, req.Model)
	if err != nil {
		return protocol.Result{}, err
	}
	transcript, requestAttachments, err := protocol.TranscriptWithAttachments(req)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	system, err := protocol.SystemText(req.System)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	allClientTools, err := protocol.ClientTools(req.Tools)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	toolPolicy, err := protocol.ParseToolPolicy(req.ToolChoice)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	clientTools, err := protocol.ApplyToolPolicy(allClientTools, toolPolicy)
	if err != nil {
		return protocol.Result{}, provider.InvalidRequest(err)
	}
	names := protocol.NewToolNames(allClientTools)
	dynamicTools := make([]map[string]any, 0, len(clientTools))
	for _, tool := range clientTools {
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		dynamicTools = append(dynamicTools, map[string]any{
			"type":        "function",
			"name":        names.Provider(tool.Name),
			"description": tool.Description,
			"inputSchema": schema,
		})
	}
	requestDir, err := os.MkdirTemp("", "macaz-codex-*")
	if err != nil {
		return protocol.Result{}, err
	}
	defer os.RemoveAll(requestDir)
	input, err := codexInputs(ctx, requestDir, transcript, requestAttachments)
	if err != nil {
		return protocol.Result{}, err
	}
	allowedImageViews := codexImagePaths(input, requestDir)

	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, exe, appServerArgs()...)
	cmd.Dir = requestDir
	cmd.Env = codexEnvironment(requestDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return protocol.Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return protocol.Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return protocol.Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return protocol.Result{}, fmt.Errorf("start Codex app-server: %w", err)
	}
	var stderrBuffer boundedBuffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuffer, stderr)
		close(stderrDone)
	}()

	rpc := newRPC(stdin, stdout)
	defer func() {
		cancel()
		_ = stdin.Close()
		_ = cmd.Wait()
		<-stderrDone
	}()

	if _, err := rpc.request(ctx, 1, "initialize", initializeParams()); err != nil {
		return protocol.Result{}, withStderr(err, stderrBuffer.String())
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return protocol.Result{}, err
	}

	threadParams := threadStartParams(selectedModel, requestDir, system, dynamicTools)
	threadResponse, err := rpc.request(ctx, 2, "thread/start", threadParams)
	if err != nil {
		return protocol.Result{}, withStderr(err, stderrBuffer.String())
	}
	threadID := nestedString(threadResponse, "thread", "id")
	if threadID == "" {
		return protocol.Result{}, fmt.Errorf("Codex thread/start returned no thread id")
	}
	turnResponse, err := rpc.request(ctx, 3, "turn/start", map[string]any{
		"threadId": threadID,
		"effort":   protocol.Effort(req, p.cfg.DefaultEffort),
		"input":    input,
	})
	if err != nil {
		return protocol.Result{}, withStderr(err, stderrBuffer.String())
	}
	turnID := nestedString(turnResponse, "turn", "id")
	if turnID == "" {
		return protocol.Result{}, fmt.Errorf("Codex turn/start returned no turn id")
	}

	result := protocol.Result{
		ID:         "msg_" + threadID,
		Model:      selectedModel,
		StopReason: "end_turn",
	}
	var textIndex = -1
	var toolTimer <-chan time.Time
	var timer *time.Timer
	finishTools := func() {
		if timer != nil {
			timer.Stop()
		}
	}
	defer finishTools()

	for {
		select {
		case <-ctx.Done():
			return protocol.Result{}, ctx.Err()
		case <-toolTimer:
			_ = rpc.sendRequest(1000, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
			for i := range result.Blocks {
				if req.Stream && emit != nil {
					_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: i})
				}
			}
			result.StopReason = "tool_use"
			return result, nil
		case envelope, ok := <-rpc.events:
			if !ok {
				if len(result.Blocks) > 0 {
					return result, nil
				}
				return protocol.Result{}, withStderr(errors.New("Codex app-server closed its output"), stderrBuffer.String())
			}
			if envelope.Error != nil {
				return protocol.Result{}, withStderr(envelope.Error, stderrBuffer.String())
			}
			switch envelope.Method {
			case "item/agentMessage/delta":
				delta := stringValue(envelope.Params["delta"])
				if delta == "" {
					continue
				}
				if textIndex < 0 {
					textIndex = len(result.Blocks)
					result.Blocks = append(result.Blocks, protocol.Block{Type: "text"})
					if req.Stream && emit != nil {
						if err := emit(protocol.Event{Kind: protocol.EventBlockStart, Index: textIndex, Block: protocol.Block{Type: "text"}}); err != nil {
							return protocol.Result{}, err
						}
					}
				}
				result.Blocks[textIndex].Text += delta
				if req.Stream && emit != nil {
					if err := emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: textIndex, DeltaType: "text_delta", Delta: delta}); err != nil {
						return protocol.Result{}, err
					}
				}
			case "item/tool/call":
				if textIndex >= 0 && req.Stream && emit != nil {
					if err := emit(protocol.Event{Kind: protocol.EventBlockStop, Index: textIndex}); err != nil {
						return protocol.Result{}, err
					}
					textIndex = -1
				}
				rawArgs, err := json.Marshal(envelope.Params["arguments"])
				if err != nil {
					return protocol.Result{}, err
				}
				block := protocol.Block{
					Type:  "tool_use",
					ID:    first(stringValue(envelope.Params["callId"]), "toolu_codex"),
					Name:  names.Client(stringValue(envelope.Params["tool"])),
					Input: rawArgs,
				}
				index := len(result.Blocks)
				result.Blocks = append(result.Blocks, block)
				if req.Stream && emit != nil {
					if err := emit(protocol.Event{Kind: protocol.EventBlockStart, Index: index, Block: block}); err != nil {
						return protocol.Result{}, err
					}
					if err := emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: index, DeltaType: "input_json_delta", Delta: string(rawArgs)}); err != nil {
						return protocol.Result{}, err
					}
				}
				if toolPolicy.DisableParallel {
					_ = rpc.sendRequest(1000, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
					if req.Stream && emit != nil {
						_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index})
					}
					result.StopReason = "tool_use"
					return result, nil
				}
				if timer == nil {
					timer = time.NewTimer(parallelToolGrace)
					toolTimer = timer.C
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(parallelToolGrace)
				}
			case "thread/tokenUsage/updated":
				if usage := mapValue(envelope.Params["tokenUsage"]); usage != nil {
					if last := mapValue(usage["last"]); last != nil {
						result.Usage.InputTokens = int64Value(last["inputTokens"])
						result.Usage.OutputTokens = int64Value(last["outputTokens"])
						result.Usage.CacheReadInputTokens = int64Value(last["cachedInputTokens"])
						result.Usage.ReasoningOutputTokens = int64Value(last["reasoningOutputTokens"])
					}
				}
			case "item/started":
				item := mapValue(envelope.Params["item"])
				itemType := stringValue(item["type"])
				if itemType == "imageView" {
					if !allowedCodexImageView(item, allowedImageViews, requestDir) {
						return protocol.Result{}, fmt.Errorf(
							"Codex attempted image view outside request attachments: %#v",
							item,
						)
					}
					continue
				}
				if isNativeTool(itemType) {
					return protocol.Result{}, fmt.Errorf("Codex attempted forbidden native tool %q", itemType)
				}
			case "turn/completed":
				if timer != nil {
					continue
				}
				turn := mapValue(envelope.Params["turn"])
				switch status := stringValue(turn["status"]); status {
				case "failed":
					turnError := mapValue(turn["error"])
					message := first(
						stringValue(turnError["message"]),
						stringValue(turnError["additionalDetails"]),
						"Codex turn failed",
					)
					return protocol.Result{}, fmt.Errorf("%s", message)
				case "interrupted":
					return protocol.Result{}, errors.New("Codex turn was interrupted before returning a result")
				case "", "completed":
				}
				if textIndex >= 0 && req.Stream && emit != nil {
					if err := emit(protocol.Event{Kind: protocol.EventBlockStop, Index: textIndex}); err != nil {
						return protocol.Result{}, err
					}
				}
				if len(result.Blocks) == 0 {
					return protocol.Result{}, errors.New("Codex returned no text or client tool call")
				}
				return result, nil
			case "error":
				return protocol.Result{}, fmt.Errorf("Codex app-server error: %v", envelope.Params)
			}
		}
	}
}

func (p *Provider) selectModel(ctx context.Context, requested string) (string, error) {
	desired := p.cfg.ResolveModel(requested)
	models, err := p.Models(ctx)
	if err != nil {
		return "", err
	}
	for _, model := range models {
		if model.ID == desired {
			return model.ID, nil
		}
	}
	if desired != "" {
		return "", provider.InvalidRequest(fmt.Errorf("Codex model %q is not present in the live catalog", desired))
	}
	for _, model := range models {
		if model.Default {
			return model.ID, nil
		}
	}
	if len(models) > 0 {
		return models[0].ID, nil
	}
	return "", errors.New("Codex has no available model")
}

func appServerArgs() []string {
	return []string{
		"app-server", "--stdio",
		"--disable", "apps",
		"--disable", "browser_use",
		"--disable", "browser_use_external",
		"--disable", "browser_use_full_cdp_access",
		"--disable", "computer_use",
		"--disable", "image_generation",
		"--disable", "in_app_browser",
		"--disable", "multi_agent",
		"--disable", "plugins",
		"--disable", "shell_tool",
		"--disable", "unified_exec",
		"--disable", "code_mode_host",
		"--disable", "tool_suggest",
		"-c", `web_search="disabled"`,
		"-c", `mcp_servers={}`,
		"-c", `project_doc_max_bytes=0`,
		"-c", `include_permissions_instructions=false`,
		"-c", `include_apps_instructions=false`,
		"-c", `include_collaboration_mode_instructions=false`,
		"-c", `skills.include_instructions=false`,
		"-c", `orchestrator_skills_enabled=false`,
		"-c", `orchestrator_mcp_enabled=false`,
		"-c", `include_environment_context=false`,
	}
}

func threadStartParams(model, cwd, system string, dynamicTools []map[string]any) map[string]any {
	return map[string]any{
		"model":                      model,
		"cwd":                        cwd,
		"approvalPolicy":             "never",
		"sandbox":                    "danger-full-access",
		"ephemeral":                  true,
		"baseInstructions":           system,
		"developerInstructions":      "",
		"dynamicTools":               dynamicTools,
		"allowProviderModelFallback": false,
	}
}

func initializeParams() map[string]any {
	return map[string]any{
		"clientInfo": map[string]any{"name": "macaz", "version": "dev"},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}
}

func codexInputs(ctx context.Context, dir, transcript string, items []protocol.Attachment) ([]map[string]any, error) {
	var enriched strings.Builder
	enriched.WriteString(transcript)
	var images []protocol.Attachment
	for _, item := range items {
		if item.Kind == "image" {
			images = append(images, item)
			continue
		}
		text, err := attachments.Text(
			ctx,
			item,
			attachments.DefaultMaxBytes,
			attachments.DefaultMaxTextBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("prepare document attachment %q: %w", item.Filename, err)
		}
		fmt.Fprintf(
			&enriched,
			"\n<document_attachment name=%q media_type=%q>\n%s\n</document_attachment>\n",
			item.Filename,
			item.MediaType,
			text,
		)
	}
	input := []map[string]any{
		{"type": "text", "text": enriched.String(), "text_elements": []any{}},
	}
	paths, err := attachments.Materialize(ctx, dir, images, attachments.DefaultMaxBytes)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		input = append(input, map[string]any{
			"type": "localImage",
			"path": path,
		})
	}
	return input, nil
}

func codexImagePaths(input []map[string]any, cwd string) map[string]struct{} {
	paths := make(map[string]struct{})
	for _, item := range input {
		if item["type"] != "localImage" {
			continue
		}
		path := canonicalCodexPath(stringValue(item["path"]), cwd)
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	return paths
}

func allowedCodexImageView(item map[string]any, allowed map[string]struct{}, cwd string) bool {
	path := canonicalCodexPath(stringValue(item["path"]), cwd)
	if path == "" {
		return false
	}
	_, ok := allowed[path]
	return ok
}

func canonicalCodexPath(path, cwd string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

func isNativeTool(itemType string) bool {
	switch itemType {
	case "", "userMessage", "agentMessage", "reasoning", "plan", "dynamicToolCall":
		return false
	default:
		// Fail closed when a newer Codex release introduces another thread item.
		// macaz permits only passive model output and client-owned dynamic tools;
		// every provider-side execution/context item is forbidden.
		return true
	}
}

func codexEnvironment(requestDir string) []string {
	current := os.Environ()
	result := make([]string, 0, len(current)+1)
	for _, item := range current {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if strings.EqualFold(key, "PWD") {
			continue
		}
		result = append(result, item)
	}
	return append(result, "PWD="+requestDir)
}

type rpcEnvelope struct {
	ID     json.RawMessage
	Method string
	Params map[string]any
	Result map[string]any
	Error  error
}

type rpcClient struct {
	writer      io.Writer
	mu          sync.Mutex
	events      chan rpcEnvelope
	waitMu      sync.Mutex
	wait        map[string]chan rpcEnvelope
	diagnostics boundedBuffer
}

func newRPC(writer io.Writer, reader io.Reader) *rpcClient {
	client := &rpcClient{
		writer: writer,
		events: make(chan rpcEnvelope, 128),
		wait:   map[string]chan rpcEnvelope{},
	}
	go client.read(reader)
	return client
}

func (r *rpcClient) request(ctx context.Context, id int, method string, params any) (map[string]any, error) {
	key := fmt.Sprint(id)
	ch := make(chan rpcEnvelope, 1)
	r.waitMu.Lock()
	r.wait[key] = ch
	r.waitMu.Unlock()
	defer func() {
		r.waitMu.Lock()
		delete(r.wait, key)
		r.waitMu.Unlock()
	}()
	if err := r.sendRequest(id, method, params); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case envelope, ok := <-ch:
		if !ok {
			return nil, errors.New("Codex app-server closed before responding")
		}
		if envelope.Error != nil {
			return nil, envelope.Error
		}
		return envelope.Result, nil
	}
}

func (r *rpcClient) sendRequest(id int, method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (r *rpcClient) notify(method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (r *rpcClient) write(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.writer.Write(raw)
	return err
}

func (r *rpcClient) read(reader io.Reader) {
	defer func() {
		r.waitMu.Lock()
		for _, ch := range r.wait {
			close(ch)
		}
		r.wait = map[string]chan rpcEnvelope{}
		r.waitMu.Unlock()
		close(r.events)
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' {
			_, _ = r.diagnostics.Write(append(append([]byte(nil), line...), '\n'))
			continue
		}
		var raw struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result map[string]any  `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			r.events <- rpcEnvelope{Error: fmt.Errorf("decode Codex app-server JSONL: %w", err)}
			continue
		}
		envelope := rpcEnvelope{ID: raw.ID, Method: raw.Method, Params: raw.Params, Result: raw.Result}
		if raw.Error != nil {
			envelope.Error = fmt.Errorf("Codex RPC %d: %s", raw.Error.Code, raw.Error.Message)
		}
		if len(raw.ID) > 0 && raw.Method == "" {
			var key any
			_ = json.Unmarshal(raw.ID, &key)
			r.waitMu.Lock()
			ch := r.wait[fmt.Sprint(key)]
			r.waitMu.Unlock()
			if ch != nil {
				ch <- envelope
				continue
			}
		}
		r.events <- envelope
	}
	if err := scanner.Err(); err != nil {
		r.events <- rpcEnvelope{Error: fmt.Errorf("read Codex app-server JSONL: %w", err)}
	}
}

func (r *rpcClient) Diagnostics() string {
	return r.diagnostics.String()
}

type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const max = 64 << 10
	if b.buf.Len()+len(p) > max {
		drop := b.buf.Len() + len(p) - max
		current := b.buf.Bytes()
		if drop < len(current) {
			kept := append([]byte(nil), current[drop:]...)
			b.buf.Reset()
			_, _ = b.buf.Write(kept)
		} else {
			b.buf.Reset()
		}
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func withStderr(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

func nestedString(value map[string]any, path ...string) string {
	var current any = value
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = next[key]
	}
	return stringValue(current)
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func int64Value(value any) int64 {
	if n, ok := value.(float64); ok {
		return int64(n)
	}
	return 0
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func sliceValue(value any) []any {
	result, _ := value.([]any)
	return result
}

func stringSlice(value any) []string {
	raw := sliceValue(value)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text := stringValue(item); text != "" {
			result = append(result, text)
		}
	}
	return result
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
