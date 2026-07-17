package opencodecli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/attachments"
	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

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
	return "OpenCode-CLI"
}

func (p *Provider) Check(ctx context.Context) error {
	exe, err := exec.LookPath(p.cfg.OpenCodeExecutable)
	if err != nil {
		return fmt.Errorf("find OpenCode executable %q: %w", p.cfg.OpenCodeExecutable, err)
	}
	command := exec.CommandContext(ctx, exe, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run %s --version: %w: %s", exe, err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) == "" {
		return fmt.Errorf("%s returned an empty version", exe)
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
	exe, err := exec.LookPath(p.cfg.OpenCodeExecutable)
	if err != nil {
		return nil, fmt.Errorf("find OpenCode executable %q: %w", p.cfg.OpenCodeExecutable, err)
	}
	tempDir, err := os.MkdirTemp("", "macaz-opencode-models-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	command := exec.CommandContext(ctx, exe, "models", "--pure", "--verbose")
	command.Dir = tempDir
	command.Env = mergeEnvironment(os.Environ(), isolatedEnvironment(tempDir, ""))
	var stderr boundedBuffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list OpenCode models: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	models := parseModels(output, p.cfg.OpenCodeModel)
	if len(models) == 0 {
		return nil, errors.New("OpenCode returned no models")
	}
	p.models = append([]provider.Model(nil), models...)
	p.modelsAt = time.Now()
	return models, nil
}

func (p *Provider) Generate(ctx context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	exe, err := exec.LookPath(p.cfg.OpenCodeExecutable)
	if err != nil {
		return protocol.Result{}, fmt.Errorf("find OpenCode executable %q: %w", p.cfg.OpenCodeExecutable, err)
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
	configDir, err := os.MkdirTemp("", "macaz-opencode-*")
	if err != nil {
		return protocol.Result{}, err
	}
	defer os.RemoveAll(configDir)
	toolsDir := filepath.Join(configDir, "tools")
	if err := os.MkdirAll(toolsDir, 0o700); err != nil {
		return protocol.Result{}, err
	}
	permissions := orderedPermissions(clientTools, names)
	for _, tool := range clientTools {
		name := names.Provider(tool.Name)
		source, err := toolSource(tool)
		if err != nil {
			return protocol.Result{}, provider.InvalidRequest(fmt.Errorf("generate OpenCode tool %q: %w", tool.Name, err))
		}
		if err := os.WriteFile(filepath.Join(toolsDir, name+".js"), []byte(source), 0o600); err != nil {
			return protocol.Result{}, err
		}
	}
	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		return protocol.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "macaz-context.js"), []byte(contextPluginSource(system)), 0o600); err != nil {
		return protocol.Result{}, err
	}
	inlineConfig := `{
  "share": "disabled",
  "permission": ` + permissions + `,
  "agent": {
    "macaz-provider": {
      "mode": "primary",
      "description": "macaz request-scoped inference provider",
      "prompt": " ",
      "permission": ` + permissions + `
    }
  }
}`
	selectedModel := p.cfg.ResolveModel(req.Model)
	args := []string{
		"run",
		"--format", "json",
		"--thinking",
		"--agent", "macaz-provider",
		"--title", "macaz provider request",
	}
	if selectedModel != "" {
		args = append(args, "--model", selectedModel)
	}
	if effort := protocol.Effort(req, p.cfg.DefaultEffort); effort != "" {
		args = append(args, "--variant", effort)
	}
	attachmentPaths, err := attachments.Materialize(ctx, configDir, requestAttachments, attachments.DefaultMaxBytes)
	if err != nil {
		return protocol.Result{}, err
	}
	for _, path := range attachmentPaths {
		args = append(args, "--file", path)
	}

	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processCtx, exe, args...)
	command.Dir = configDir
	command.Stdin = strings.NewReader(transcript)
	command.Env = mergeEnvironment(os.Environ(), isolatedEnvironment(configDir, inlineConfig))
	stdout, err := command.StdoutPipe()
	if err != nil {
		return protocol.Result{}, err
	}
	var stderr boundedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return protocol.Result{}, fmt.Errorf("start OpenCode: %w", err)
	}

	result := protocol.Result{
		ID:         "msg_opencode_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Model:      selectedModel,
		StopReason: "end_turn",
	}
	var eventError error
	var rawEvents boundedBuffer
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		_, _ = rawEvents.Write(line)
		_, _ = rawEvents.Write([]byte{'\n'})
		var event struct {
			Type  string `json:"type"`
			Error struct {
				Name string `json:"name"`
				Data struct {
					Message      string          `json:"message"`
					StatusCode   int             `json:"statusCode"`
					IsRetryable  bool            `json:"isRetryable"`
					ResponseBody json.RawMessage `json:"responseBody"`
				} `json:"data"`
			} `json:"error"`
			Part struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				CallID string `json:"callID"`
				Tool   string `json:"tool"`
				State  struct {
					Status string         `json:"status"`
					Input  map[string]any `json:"input"`
					Error  string         `json:"error"`
				} `json:"state"`
				Tokens struct {
					Input     int64 `json:"input"`
					Output    int64 `json:"output"`
					Reasoning int64 `json:"reasoning"`
					Cache     struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
				Reason string `json:"reason"`
			} `json:"part"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		switch event.Type {
		case "text":
			if event.Part.Text == "" {
				continue
			}
			index := len(result.Blocks)
			block := protocol.Block{Type: "text", Text: event.Part.Text}
			result.Blocks = append(result.Blocks, block)
			if req.Stream && emit != nil {
				if err := emit(protocol.Event{Kind: protocol.EventBlockStart, Index: index, Block: protocol.Block{Type: "text"}}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
				if err := emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: index, DeltaType: "text_delta", Delta: block.Text}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
				if err := emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
			}
		case "reasoning":
			if event.Part.Text == "" {
				continue
			}
			index := len(result.Blocks)
			block := protocol.Block{
				Type:      "thinking",
				Thinking:  event.Part.Text,
				Signature: "macaz-opencode-reasoning-summary",
			}
			result.Blocks = append(result.Blocks, block)
			if req.Stream && emit != nil {
				if err := emit(protocol.Event{Kind: protocol.EventBlockStart, Index: index, Block: protocol.Block{Type: "thinking", Signature: block.Signature}}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
				if err := emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: index, DeltaType: "thinking_delta", Delta: block.Thinking}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
				_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index})
			}
		case "tool_use":
			rawInput, _ := json.Marshal(event.Part.State.Input)
			block := protocol.Block{
				Type:  "tool_use",
				ID:    first(event.Part.CallID, "toolu_opencode"),
				Name:  names.Client(event.Part.Tool),
				Input: rawInput,
			}
			index := len(result.Blocks)
			result.Blocks = append(result.Blocks, block)
			result.StopReason = "tool_use"
			if req.Stream && emit != nil {
				if err := emit(protocol.Event{Kind: protocol.EventBlockStart, Index: index, Block: block}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
				if err := emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: index, DeltaType: "input_json_delta", Delta: string(rawInput)}); err != nil {
					cancel()
					_ = command.Wait()
					return protocol.Result{}, err
				}
				_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index})
			}
			cancel()
			_ = command.Wait()
			return result, nil
		case "step_finish":
			result.Usage.InputTokens = event.Part.Tokens.Input
			result.Usage.OutputTokens = event.Part.Tokens.Output
			result.Usage.ReasoningOutputTokens = event.Part.Tokens.Reasoning
			result.Usage.CacheReadInputTokens = event.Part.Tokens.Cache.Read
			result.Usage.CacheCreationInputTokens = event.Part.Tokens.Cache.Write
			if event.Part.Reason == "length" {
				result.StopReason = "max_tokens"
			}
		case "error":
			eventError = openCodeEventError(
				event.Error.Name,
				event.Error.Data.Message,
				event.Error.Data.StatusCode,
				event.Error.Data.IsRetryable,
				event.Error.Data.ResponseBody,
			)
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if scanErr != nil {
		return protocol.Result{}, fmt.Errorf("read OpenCode events: %w", scanErr)
	}
	if eventError != nil {
		if strings.Contains(eventError.Error(), "Unexpected server error") {
			return protocol.Result{}, fmt.Errorf(
				"%w%s",
				eventError,
				openCodeDiagnostics(stderr.String(), rawEvents.String()),
			)
		}
		return protocol.Result{}, eventError
	}
	if waitErr != nil && len(result.Blocks) == 0 {
		return protocol.Result{}, fmt.Errorf(
			"OpenCode failed: %w%s",
			waitErr,
			openCodeDiagnostics(stderr.String(), rawEvents.String()),
		)
	}
	if len(result.Blocks) == 0 {
		return protocol.Result{}, fmt.Errorf(
			"OpenCode returned no text or tool call%s",
			openCodeDiagnostics(stderr.String(), rawEvents.String()),
		)
	}
	return result, nil
}

func isolatedEnvironment(configDir, inlineConfig string) []string {
	values := []string{
		"OPENCODE_CONFIG_DIR=" + configDir,
		"OPENCODE_DB=" + filepath.Join(configDir, "opencode.db"),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_AUTO_SHARE=false",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_CLAUDE_CODE_PROMPT=1",
		"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_DISABLE_AUTOCOMPACT=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_LSP_DOWNLOAD=1",
		"XDG_CONFIG_HOME=" + filepath.Join(configDir, "xdg-config"),
		"XDG_CACHE_HOME=" + filepath.Join(configDir, "xdg-cache"),
		"XDG_STATE_HOME=" + filepath.Join(configDir, "xdg-state"),
		"PWD=" + configDir,
		"TMPDIR=" + configDir,
		"TMP=" + configDir,
		"TEMP=" + configDir,
	}
	if inlineConfig != "" {
		values = append(values, "OPENCODE_CONFIG_CONTENT="+inlineConfig)
	}
	return values
}

func openCodeEventError(name, message string, status int, retryable bool, responseBody json.RawMessage) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = strings.TrimSpace(string(responseBody))
	}
	if message == "" {
		message = first(strings.TrimSpace(name), "OpenCode API error")
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	retryAfter := time.Duration(0)
	if retryable && (status == http.StatusTooManyRequests || status >= http.StatusInternalServerError) {
		retryAfter = time.Second
	}
	return &provider.HTTPError{
		Status:     status,
		Type:       "provider_error",
		Message:    message,
		Body:       append([]byte(nil), responseBody...),
		RetryAfter: retryAfter,
	}
}

func openCodeDiagnostics(stderr, events string) string {
	stderr = strings.TrimSpace(stderr)
	events = strings.TrimSpace(events)
	switch {
	case stderr != "" && events != "":
		return ": stderr: " + stderr + "; events: " + events
	case stderr != "":
		return ": stderr: " + stderr
	case events != "":
		return ": events: " + events
	default:
		return ""
	}
}

func mergeEnvironment(current, overrides []string) []string {
	replacements := make(map[string]string, len(overrides))
	for _, item := range overrides {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		replacements[key] = item
	}
	result := make([]string, 0, len(current)+len(replacements))
	for _, item := range current {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if _, replace := replacements[key]; !replace {
			result = append(result, item)
		}
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, replacements[key])
	}
	return result
}

func contextPluginSource(system string) string {
	return `export const MacazContext = async () => ({
  "experimental.chat.system.transform": async (_input, output) => {
    output.system.splice(0, output.system.length)
    const clientSystem = ` + strconv.Quote(system) + `
    if (clientSystem.length > 0) output.system.push(clientSystem)
  }
})
`
}

func parseModels(output []byte, selected string) []provider.Model {
	lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	models := make([]provider.Model, 0)
	for index := 0; index < len(lines); {
		id := strings.TrimSpace(lines[index])
		index++
		if id == "" || strings.HasPrefix(id, "{") || !strings.Contains(id, "/") {
			continue
		}
		var metadata map[string]any
		if index < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index]), "{") {
			var raw strings.Builder
			depth := 0
			started := false
			for index < len(lines) {
				line := lines[index]
				index++
				raw.WriteString(line)
				raw.WriteByte('\n')
				depth += strings.Count(line, "{") - strings.Count(line, "}")
				started = true
				if started && depth <= 0 {
					break
				}
			}
			_ = json.Unmarshal([]byte(raw.String()), &metadata)
		}
		efforts := []string(nil)
		if variants, ok := metadata["variants"].(map[string]any); ok {
			efforts = make([]string, 0, len(variants))
			for effort := range variants {
				efforts = append(efforts, effort)
			}
			sort.Strings(efforts)
		}
		modalities := []string{"text"}
		if capabilities, ok := metadata["capabilities"].(map[string]any); ok {
			if input, ok := capabilities["input"].(map[string]any); ok {
				for _, modality := range []string{"image", "audio", "video", "pdf"} {
					if enabled, _ := input[modality].(bool); enabled {
						modalities = append(modalities, modality)
					}
				}
			}
		}
		models = append(models, provider.Model{
			ID:              id,
			DisplayName:     openCodeModelDisplayName(id, first(stringValue(metadata["name"]), id)),
			Default:         selected != "" && id == selected,
			Efforts:         efforts,
			InputModalities: modalities,
		})
	}
	if len(models) > 0 && strings.TrimSpace(selected) == "" {
		models[0].Default = true
	}
	return models
}

func openCodeModelDisplayName(id, name string) string {
	providerID, _, found := strings.Cut(id, "/")
	if !found || strings.TrimSpace(providerID) == "" {
		return name
	}
	label := providerID
	switch strings.ToLower(providerID) {
	case "openai":
		label = "OpenAI"
	case "opencode":
		label = "OpenCode Zen"
	case "anthropic":
		label = "Anthropic"
	case "github-copilot":
		label = "GitHub Copilot"
	case "openrouter":
		label = "OpenRouter"
	case "ollama":
		label = "Ollama"
	}
	return label + " / " + name
}

func orderedPermissions(tools []protocol.Tool, names *protocol.ToolNames) string {
	var out strings.Builder
	out.WriteString(`{"*":"deny"`)
	sorted := append([]protocol.Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool {
		return names.Provider(sorted[i].Name) < names.Provider(sorted[j].Name)
	})
	for _, tool := range sorted {
		out.WriteByte(',')
		out.WriteString(strconv.Quote(names.Provider(tool.Name)))
		out.WriteString(`:"allow"`)
	}
	out.WriteByte('}')
	return out.String()
}

func toolSource(tool protocol.Tool) (string, error) {
	args, err := schemaArgs(tool.InputSchema)
	if err != nil {
		return "", err
	}
	return `import { z } from "../node_modules/zod/index.js"

export default {
  description: ` + strconv.Quote(tool.Description) + `,
  args: ` + args + `,
  async execute(args) {
    throw new Error("MACAZ_CAPTURED_TOOL_CALL")
  }
}
`, nil
}

func schemaArgs(schema map[string]any) (string, error) {
	if schema == nil {
		return "{}", nil
	}
	if stringValue(schema["type"]) != "" && stringValue(schema["type"]) != "object" {
		expr, err := schemaExpr(schema)
		if err != nil {
			return "", err
		}
		return `{ input: ` + expr + ` }`, nil
	}
	properties, _ := schema["properties"].(map[string]any)
	required := stringSet(schema["required"])
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteByte('{')
	for i, key := range keys {
		child, _ := properties[key].(map[string]any)
		expr, err := schemaExpr(child)
		if err != nil {
			return "", err
		}
		if !required[key] {
			expr += ".optional()"
		}
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.Quote(key))
		out.WriteString(": ")
		out.WriteString(expr)
	}
	out.WriteByte('}')
	return out.String(), nil
}

func schemaExpr(schema map[string]any) (string, error) {
	if schema == nil {
		return "z.any()", nil
	}
	var expr string
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		raw, _ := json.Marshal(values)
		expr = "z.enum(" + string(raw) + ")"
	} else if value, ok := schema["const"]; ok {
		raw, _ := json.Marshal(value)
		expr = "z.literal(" + string(raw) + ")"
	} else if variants, ok := schema["anyOf"].([]any); ok && len(variants) > 0 {
		expressions, err := schemaVariants(variants)
		if err != nil {
			return "", err
		}
		expr = "z.union([" + strings.Join(expressions, ",") + "])"
	} else if variants, ok := schema["oneOf"].([]any); ok && len(variants) > 0 {
		expressions, err := schemaVariants(variants)
		if err != nil {
			return "", err
		}
		expr = "z.union([" + strings.Join(expressions, ",") + "])"
	} else {
		switch stringValue(schema["type"]) {
		case "", "object":
			args, err := schemaArgs(schema)
			if err != nil {
				return "", err
			}
			expr = "z.object(" + args + ")"
		case "string":
			expr = "z.string()"
		case "integer":
			expr = "z.number().int()"
		case "number":
			expr = "z.number()"
		case "boolean":
			expr = "z.boolean()"
		case "array":
			child, _ := schema["items"].(map[string]any)
			item, err := schemaExpr(child)
			if err != nil {
				return "", err
			}
			expr = "z.array(" + item + ")"
		case "null":
			expr = "z.null()"
		default:
			expr = "z.any()"
		}
	}
	if description := stringValue(schema["description"]); description != "" {
		expr += ".describe(" + strconv.Quote(description) + ")"
	}
	return expr, nil
}

func schemaVariants(values []any) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		schema, _ := value.(map[string]any)
		expr, err := schemaExpr(schema)
		if err != nil {
			return nil, err
		}
		result = append(result, expr)
	}
	return result, nil
}

func stringSet(value any) map[string]bool {
	result := map[string]bool{}
	values, _ := value.([]any)
	for _, item := range values {
		if text, ok := item.(string); ok {
			result[text] = true
		}
	}
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type boundedBuffer struct {
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	const max = 64 << 10
	if b.buf.Len()+len(p) > max {
		current := b.buf.Bytes()
		drop := b.buf.Len() + len(p) - max
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
	return b.buf.String()
}
