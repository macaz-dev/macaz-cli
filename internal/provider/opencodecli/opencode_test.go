package opencodecli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

type fakeOpenCodeReport struct {
	Args         []string          `json:"args"`
	Environment  map[string]string `json:"environment"`
	Stdin        string            `json:"stdin"`
	InlineConfig string            `json:"inline_config"`
	Plugin       string            `json:"plugin"`
	ToolFiles    []string          `json:"tool_files"`
	Attachments  map[string]string `json:"attachments"`
}

func TestMain(m *testing.M) {
	if os.Getenv("MACAZ_FAKE_OPENCODE") == "1" {
		os.Exit(runFakeOpenCode())
	}
	os.Exit(m.Run())
}

func TestProviderNameMarksCLIExperimental(t *testing.T) {
	if got := New(config.Default()).Name(); got != "OpenCode-CLI (experimental)" {
		t.Fatalf("provider name = %q", got)
	}
}

func runFakeOpenCode() int {
	if len(os.Args) < 2 {
		return 2
	}
	switch os.Args[1] {
	case "--version":
		_, _ = io.WriteString(os.Stdout, "opencode 1.2.3\n")
		return 0
	case "models":
		_, _ = io.WriteString(os.Stdout, `fake/default
{
  "name": "Fake Default",
  "variants": {"low": {}, "high": {}},
  "capabilities": {"input": {"image": true, "pdf": true}}
}
fake/next
{
  "name": "Fake Next",
  "variants": {"medium": {}},
  "capabilities": {"input": {}}
}
`)
		return 0
	case "run":
		configDir := os.Getenv("OPENCODE_CONFIG_DIR")
		stdin, _ := io.ReadAll(os.Stdin)
		plugin, _ := os.ReadFile(filepath.Join(configDir, "plugins", "macaz-context.js"))
		entries, _ := os.ReadDir(filepath.Join(configDir, "tools"))
		toolFiles := make([]string, 0, len(entries))
		for _, entry := range entries {
			toolFiles = append(toolFiles, entry.Name())
		}
		report := fakeOpenCodeReport{
			Args:         append([]string(nil), os.Args[1:]...),
			Environment:  map[string]string{},
			Stdin:        string(stdin),
			InlineConfig: os.Getenv("OPENCODE_CONFIG_CONTENT"),
			Plugin:       string(plugin),
			ToolFiles:    toolFiles,
			Attachments:  map[string]string{},
		}
		for index := 2; index < len(os.Args); index++ {
			if os.Args[index] != "--file" || index+1 >= len(os.Args) {
				continue
			}
			path := os.Args[index+1]
			raw, err := os.ReadFile(path)
			if err == nil {
				report.Attachments[filepath.Base(path)] = base64.StdEncoding.EncodeToString(raw)
			}
			index++
		}
		for _, key := range []string{
			"OPENCODE_CONFIG_DIR",
			"OPENCODE_DB",
			"OPENCODE_DISABLE_PROJECT_CONFIG",
			"OPENCODE_DISABLE_CLAUDE_CODE",
			"OPENCODE_DISABLE_EXTERNAL_SKILLS",
			"XDG_CONFIG_HOME",
			"XDG_CACHE_HOME",
			"PWD",
		} {
			report.Environment[key] = os.Getenv(key)
		}
		if path := os.Getenv("MACAZ_FAKE_OPENCODE_REPORT"); path != "" {
			raw, _ := json.Marshal(report)
			_ = os.WriteFile(path, raw, 0o600)
		}
		_, _ = io.WriteString(os.Stdout, `{"type":"step_finish","part":{"tokens":{"input":21,"output":4,"reasoning":2,"cache":{"read":3,"write":1}},"reason":"tool-calls"}}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"type":"tool_use","part":{"callID":"call_fake","tool":"Read","state":{"status":"running","input":{"path":"README.md"}}}}`+"\n")
		return 0
	default:
		return 2
	}
}

func TestSchemaArgs(t *testing.T) {
	args, err := schemaArgs(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path"},
			"line": map[string]any{"type": "integer"},
		},
		"required": []any{"path"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, `"path": z.string().describe("File path")`) {
		t.Fatalf("args = %s", args)
	}
	if !strings.Contains(args, `"line": z.number().int().optional()`) {
		t.Fatalf("args = %s", args)
	}
}

func TestToolSourceUsesRequestLocalOpenCodeDependency(t *testing.T) {
	source, err := toolSource(protocol.Tool{
		Name:        "Read",
		Description: "Read a file",
		InputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, `from "../node_modules/zod/index.js"`) {
		t.Fatalf("tool source does not use request-local dependency: %s", source)
	}
	if strings.Contains(source, `from "@opencode-ai/plugin"`) ||
		strings.Contains(source, `from "zod"`) {
		t.Fatalf("tool source retained an unsupported bare import: %s", source)
	}
}

func TestOrderedPermissions(t *testing.T) {
	tools := []protocol.Tool{{Name: "Read"}, {Name: "Bash"}}
	names := protocol.NewToolNames(tools)
	got := orderedPermissions(tools, names)
	if !strings.HasPrefix(got, `{"*":"deny"`) {
		t.Fatalf("permissions = %s", got)
	}
}

func TestIsolatedEnvironmentReplacesGlobalContextSources(t *testing.T) {
	dir := t.TempDir()
	env := mergeEnvironment([]string{
		"PATH=/bin",
		"XDG_CONFIG_HOME=/real-config",
		"OPENCODE_CONFIG_DIR=/real-opencode",
	}, isolatedEnvironment(dir, `{"share":"disabled"}`))
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"XDG_CONFIG_HOME=/real-config",
		"OPENCODE_CONFIG_DIR=/real-opencode",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment retained %s:\n%s", forbidden, joined)
		}
	}
	for _, required := range []string{
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_DB=",
		"XDG_CONFIG_HOME=",
		"PWD=" + dir,
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("environment missing %s:\n%s", required, joined)
		}
	}
}

func TestContextPluginReplacesProviderSystem(t *testing.T) {
	source := contextPluginSource("CLIENT SYSTEM")
	if !strings.Contains(source, "output.system.splice(0, output.system.length)") {
		t.Fatalf("plugin does not clear provider system: %s", source)
	}
	if !strings.Contains(source, `"CLIENT SYSTEM"`) {
		t.Fatalf("plugin does not install client system: %s", source)
	}
}

func TestProviderUsesOnlyClientContextAndSelectedTools(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_OPENCODE", "1")
	t.Setenv("MACAZ_FAKE_OPENCODE_REPORT", reportPath)

	cfg := config.Default()
	cfg.Provider = config.ProviderOpenCodeCLI
	cfg.OpenCodeExecutable = os.Args[0]
	cfg.OpenCodeModel = "fake/default"
	upstream := New(cfg)

	req := &protocol.Request{
		Model:  "fake/next",
		System: json.RawMessage(`[{"type":"text","text":"CLIENT SYSTEM ONLY"}]`),
		Messages: []protocol.Message{{
			Role: "user",
			Content: mustJSON(t, []any{
				map[string]any{"type": "text", "text": "inspect the repository"},
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       base64.StdEncoding.EncodeToString(openCodeLiveTestImage(t)),
					},
				},
				map[string]any{
					"type":  "document",
					"title": "notes.txt",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "text/plain",
						"data":       base64.StdEncoding.EncodeToString([]byte("MACAZ_DOCUMENT_OK\n")),
					},
				},
			}),
		}},
		Tools: []protocol.Tool{
			{Name: "Read", Description: "Read a file", InputSchema: map[string]any{"type": "object"}},
			{Name: "Bash", Description: "Run a command", InputSchema: map[string]any{"type": "object"}},
		},
		ToolChoice: json.RawMessage(`{
			"type":"tool",
			"name":"Read",
			"disable_parallel_tool_use":true
		}`),
		OutputConfig: json.RawMessage(`{"effort":"high"}`),
	}
	result, err := upstream.Generate(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "fake/next" || result.StopReason != "tool_use" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Blocks) != 1 || result.Blocks[0].Name != "Read" {
		t.Fatalf("blocks = %#v", result.Blocks)
	}
	if result.Usage.InputTokens != 21 || result.Usage.OutputTokens != 4 ||
		result.Usage.ReasoningOutputTokens != 2 || result.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %#v", result.Usage)
	}

	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeOpenCodeReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	joinedArgs := strings.Join(report.Args, "\n")
	for _, expected := range []string{
		"--model\nfake/next",
		"--variant\nhigh",
		"--agent\nmacaz-provider",
		"--title\nmacaz provider request",
	} {
		if !strings.Contains(joinedArgs, expected) {
			t.Fatalf("args missing %q: %#v", expected, report.Args)
		}
	}
	if !strings.Contains(report.Stdin, "inspect the repository") {
		t.Fatalf("stdin = %q", report.Stdin)
	}
	if !strings.Contains(report.Plugin, "output.system.splice(0, output.system.length)") ||
		!strings.Contains(report.Plugin, "CLIENT SYSTEM ONLY") {
		t.Fatalf("plugin = %s", report.Plugin)
	}
	if !slices.Equal(report.ToolFiles, []string{"Read.js"}) {
		t.Fatalf("tool files = %#v", report.ToolFiles)
	}
	if len(report.Attachments) != 2 {
		t.Fatalf("attachments = %#v, args = %#v", report.Attachments, report.Args)
	}
	var imageFound bool
	var documentFound bool
	for name, encoded := range report.Attachments {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		switch filepath.Ext(name) {
		case ".png":
			imageFound = bytes.Equal(raw, openCodeLiveTestImage(t))
		case ".txt":
			documentFound = string(raw) == "MACAZ_DOCUMENT_OK\n"
		}
	}
	if !imageFound || !documentFound {
		t.Fatalf("materialized attachments: image=%t document=%t report=%#v", imageFound, documentFound, report.Attachments)
	}
	if !strings.Contains(report.InlineConfig, `"*":"deny"`) ||
		!strings.Contains(report.InlineConfig, `"Read":"allow"`) ||
		strings.Contains(report.InlineConfig, `"Bash":"allow"`) {
		t.Fatalf("inline config = %s", report.InlineConfig)
	}
	for _, key := range []string{
		"OPENCODE_DISABLE_PROJECT_CONFIG",
		"OPENCODE_DISABLE_CLAUDE_CODE",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS",
	} {
		if report.Environment[key] != "1" {
			t.Fatalf("%s = %q", key, report.Environment[key])
		}
	}
	if report.Environment["OPENCODE_CONFIG_DIR"] == "" ||
		report.Environment["OPENCODE_DB"] == "" ||
		report.Environment["XDG_CONFIG_HOME"] == "" ||
		report.Environment["PWD"] != report.Environment["OPENCODE_CONFIG_DIR"] {
		t.Fatalf("isolated environment = %#v", report.Environment)
	}
}

func TestOpenCodeEventErrorPreservesProviderFailure(t *testing.T) {
	err := openCodeEventError(
		"APIError",
		"No payment method",
		401,
		false,
		json.RawMessage(`{"type":"error"}`),
	)
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error type = %T", err)
	}
	if httpErr.Status != 401 || httpErr.Message != "No payment method" {
		t.Fatalf("error = %#v", httpErr)
	}
	if httpErr.RetryAfter != 0 {
		t.Fatalf("retry after = %s", httpErr.RetryAfter)
	}
}

func TestProviderDiscoversModelsFromLocalCLI(t *testing.T) {
	t.Setenv("MACAZ_FAKE_OPENCODE", "1")
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenCodeCLI
	cfg.OpenCodeExecutable = os.Args[0]
	cfg.OpenCodeModel = "fake/next"
	upstream := New(cfg)
	if err := upstream.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, err := upstream.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[1].ID != "fake/next" || !models[1].Default {
		t.Fatalf("models = %#v", models)
	}
	if models[0].DisplayName != "fake / Fake Default" {
		t.Fatalf("display name = %q", models[0].DisplayName)
	}
	if got := strings.Join(models[0].Efforts, ","); got != "high,low" {
		t.Fatalf("efforts = %q", got)
	}
	if got := strings.Join(models[0].InputModalities, ","); got != "text,image,pdf" {
		t.Fatalf("modalities = %q", got)
	}
}

func TestOpenCodeModelDisplayNameDistinguishesUpstreamProviders(t *testing.T) {
	tests := map[string]struct {
		name string
		want string
	}{
		"openai/gpt-5.4":          {name: "GPT-5.4", want: "OpenAI / GPT-5.4"},
		"opencode/gpt-5.4":        {name: "GPT-5.4", want: "OpenCode Zen / GPT-5.4"},
		"github-copilot/gpt-5.4":  {name: "GPT-5.4", want: "GitHub Copilot / GPT-5.4"},
		"custom-provider/model-x": {name: "Model X", want: "custom-provider / Model X"},
	}
	for id, test := range tests {
		if got := openCodeModelDisplayName(id, test.name); got != test.want {
			t.Fatalf("openCodeModelDisplayName(%q) = %q, want %q", id, got, test.want)
		}
	}
}

func TestLiveOpenCodeIntegration(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("MACAZ_OPENCODE_INTEGRATION_MODEL"))
	if model == "" {
		t.Skip("set MACAZ_OPENCODE_INTEGRATION_MODEL to run against the authenticated local OpenCode CLI")
	}

	cfg := config.Default()
	cfg.Provider = config.ProviderOpenCodeCLI
	cfg.OpenCodeExecutable = "opencode"
	cfg.OpenCodeModel = model
	cfg.ModelMap["default"] = model
	upstream := New(cfg)
	discoveryCtx, discoveryCancel := context.WithTimeout(context.Background(), 45*time.Second)
	models, err := upstream.Models(discoveryCtx)
	discoveryCancel()
	if err != nil {
		t.Fatal(err)
	}
	modelIndex := slices.IndexFunc(models, func(candidate provider.Model) bool {
		return candidate.ID == model
	})
	if modelIndex < 0 {
		t.Fatalf("requested integration model %q is not in the OpenCode catalog", model)
	}
	selectedCapabilities := models[modelIndex]

	t.Run("text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := upstream.Generate(ctx, &protocol.Request{
			Model:  "default",
			System: json.RawMessage(`"Reply with exactly MACAZ_CONTEXT_OK and nothing else."`),
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Follow the system instruction."`),
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.StopReason != "end_turn" || len(result.Blocks) == 0 {
			t.Fatalf("result = %#v", result)
		}
		var text strings.Builder
		for _, block := range result.Blocks {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		if strings.TrimSpace(text.String()) != "MACAZ_CONTEXT_OK" {
			t.Fatalf("text = %q, result = %#v", text.String(), result)
		}
	})

	t.Run("tool", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := upstream.Generate(ctx, &protocol.Request{
			Model:  "default",
			System: json.RawMessage(`"You are the inference provider for Claude Code. Follow the client request exactly."`),
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Call the Read tool for README.md. Do not answer with text."`),
			}},
			Tools: []protocol.Tool{{
				Name:        "Read",
				Description: "Read a local file",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			}},
			ToolChoice: json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.StopReason != "tool_use" || len(result.Blocks) != 1 {
			t.Fatalf("result = %#v", result)
		}
		block := result.Blocks[0]
		if block.Type != "tool_use" || block.Name != "Read" || !strings.Contains(string(block.Input), "README.md") {
			t.Fatalf("tool block = %#v", block)
		}
	})

	t.Run("document", func(t *testing.T) {
		content := mustJSON(t, []any{
			map[string]any{
				"type": "text",
				"text": "Read the attached document and return its exact verification token.",
			},
			map[string]any{
				"type":  "document",
				"title": "macaz-live-document.txt",
				"source": map[string]any{
					"type":       "base64",
					"media_type": "text/plain",
					"data": base64.StdEncoding.EncodeToString([]byte(
						"The exact verification token in this document is MACAZ_DOCUMENT_OK.\n",
					)),
				},
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := upstream.Generate(ctx, &protocol.Request{
			Model:  "default",
			System: json.RawMessage(`"Read the client attachment. Reply with only the exact verification token contained in it."`),
			Messages: []protocol.Message{{
				Role:    "user",
				Content: content,
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if text := openCodeResultText(result); strings.TrimSpace(text) != "MACAZ_DOCUMENT_OK" {
			t.Fatalf("document response = %q, result = %#v", text, result)
		}
	})

	if slices.Contains(selectedCapabilities.InputModalities, "image") {
		t.Run("image", func(t *testing.T) {
			content := mustJSON(t, []any{
				map[string]any{
					"type": "text",
					"text": "Inspect the attached image. State the color of the left half and then the color of the right half using exactly two lowercase color words separated by a comma.",
				},
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       base64.StdEncoding.EncodeToString(openCodeLiveTestImage(t)),
					},
				},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			result, err := upstream.Generate(ctx, &protocol.Request{
				Model:  "default",
				System: json.RawMessage(`"Inspect the client image precisely. Do not infer colors from the prompt; report only what is visibly present."`),
				Messages: []protocol.Message{{
					Role:    "user",
					Content: content,
				}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if text := openCodeResultText(result); !strings.Contains(strings.ToLower(text), "red") ||
				!strings.Contains(strings.ToLower(text), "blue") {
				t.Fatalf("image response = %q, result = %#v", text, result)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func openCodeLiveTestImage(t *testing.T) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 512, 256))
	for y := picture.Bounds().Min.Y; y < picture.Bounds().Max.Y; y++ {
		for x := picture.Bounds().Min.X; x < picture.Bounds().Max.X; x++ {
			pixel := color.RGBA{R: 255, A: 255}
			if x >= picture.Bounds().Dx()/2 {
				pixel = color.RGBA{B: 255, A: 255}
			}
			picture.SetRGBA(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func openCodeResultText(result protocol.Result) string {
	var text strings.Builder
	for _, block := range result.Blocks {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}
