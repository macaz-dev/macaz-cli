package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
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

type fakeCodexReport struct {
	PID          int            `json:"pid"`
	Args         []string       `json:"args"`
	CWD          string         `json:"cwd"`
	PWD          string         `json:"pwd"`
	ThreadParams map[string]any `json:"thread_params"`
	TurnParams   map[string]any `json:"turn_params"`
	ThreadStarts int            `json:"thread_starts"`
	TurnStarts   int            `json:"turn_starts"`
	ToolResults  int            `json:"tool_results"`
}

func TestMain(m *testing.M) {
	if os.Getenv("MACAZ_FAKE_CODEX") == "1" {
		os.Exit(runFakeCodex())
	}
	os.Exit(m.Run())
}

func TestProviderNameMarksCLIExperimental(t *testing.T) {
	if got := New(config.Default()).Name(); got != "Codex-CLI (experimental)" {
		t.Fatalf("provider name = %q", got)
	}
}

func runFakeCodex() int {
	if len(os.Args) < 2 {
		return 2
	}
	if os.Args[1] == "--version" {
		_, _ = io.WriteString(os.Stdout, "codex-cli 9.9.9\n")
		return 0
	}
	if os.Args[1] != "app-server" {
		return 2
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	cwd, _ := os.Getwd()
	report := fakeCodexReport{
		PID:  os.Getpid(),
		Args: append([]string(nil), os.Args[1:]...),
		CWD:  cwd,
		PWD:  os.Getenv("PWD"),
	}
	writeReport := func() {
		if path := os.Getenv("MACAZ_FAKE_CODEX_REPORT"); path != "" {
			raw, _ := json.Marshal(report)
			_ = os.WriteFile(path, raw, 0o600)
		}
	}
	for scanner.Scan() {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"serverInfo": map[string]any{"name": "fake-codex"}},
			})
		case "initialized":
		case "model/list":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"data": []any{
						map[string]any{
							"model":         "fake-default",
							"displayName":   "Fake Default",
							"isDefault":     true,
							"contextWindow": 272000,
							"supportedReasoningEfforts": []any{
								map[string]any{"reasoningEffort": "low"},
								map[string]any{"reasoningEffort": "high"},
							},
							"inputModalities": []any{"text", "image"},
						},
						map[string]any{
							"model":       "fake-next",
							"displayName": "Fake Next",
							"supportedReasoningEfforts": []any{
								map[string]any{"reasoningEffort": "medium"},
							},
							"inputModalities": []any{"text"},
						},
					},
					"nextCursor": nil,
				},
			})
		case "thread/start":
			report.ThreadParams = request.Params
			report.ThreadStarts++
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"thread": map[string]any{"id": "thread-fake"}},
			})
		case "turn/start":
			report.TurnParams = request.Params
			report.TurnStarts++
			writeReport()
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"turn": map[string]any{"id": "turn-fake"}},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "thread/tokenUsage/updated",
				"params": map[string]any{
					"tokenUsage": map[string]any{
						"last": map[string]any{
							"inputTokens":           31,
							"outputTokens":          7,
							"cachedInputTokens":     4,
							"reasoningOutputTokens": 3,
						},
					},
				},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "item/agentMessage/delta",
				"params":  map[string]any{"delta": "checking "},
			})
			if os.Getenv("MACAZ_FAKE_CODEX_CLOSE_AFTER_DELTA") == "1" {
				return 0
			}
			if os.Getenv("MACAZ_FAKE_CODEX_CONTEXT_OVERFLOW") == "1" {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "turn/completed",
					"params": map[string]any{"turn": map[string]any{
						"id": "turn-fake", "status": "failed",
						"error": map[string]any{"message": "context window exceeded"},
					}},
				})
				continue
			}
			toolCall := map[string]any{
				"jsonrpc": "2.0",
				"method":  "item/tool/call",
				"params": map[string]any{
					"callId":    "call-fake",
					"threadId":  "thread-fake",
					"turnId":    "turn-fake",
					"tool":      "Read",
					"arguments": map[string]any{"path": "README.md"},
				},
			}
			if os.Getenv("MACAZ_FAKE_CODEX_STRUCTURED_HANDOFF") == "1" {
				toolCall["id"] = "tool-request-fake"
			}
			_ = encoder.Encode(toolCall)
		case "turn/interrupt":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{},
			})
			// Simulate the late completion that app-server can emit after the
			// interrupted tool turn. A reused connection must drain this event.
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "turn/completed",
				"params":  map[string]any{"turn": map[string]any{"id": "turn-fake", "status": "interrupted"}},
			})
		case "thread/unsubscribe":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{},
			})
		case "":
			if request.ID != "tool-request-fake" {
				continue
			}
			if request.Result["success"] != true || len(request.Result["contentItems"].([]any)) == 0 {
				return 3
			}
			report.ToolResults++
			writeReport()
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "item/agentMessage/delta",
				"params":  map[string]any{"delta": "finished"},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "turn/completed",
				"params": map[string]any{"turn": map[string]any{
					"id": "turn-fake", "status": "completed",
				}},
			})
		}
	}
	return 0
}

func TestNativeToolDetection(t *testing.T) {
	for _, item := range []string{
		"commandExecution",
		"fileChange",
		"mcpToolCall",
		"webSearch",
		"imageView",
		"subAgentActivity",
		"futureNativeTool",
	} {
		if !isNativeTool(item) {
			t.Fatalf("%q should be forbidden", item)
		}
	}
	for _, item := range []string{"", "userMessage", "agentMessage", "reasoning", "plan", "dynamicToolCall"} {
		if isNativeTool(item) {
			t.Fatalf("%q must be allowed", item)
		}
	}
}

func TestCodexImageViewIsLimitedToMaterializedRequestImages(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "001-diagram.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := codexImagePaths([]map[string]any{{
		"type": "localImage",
		"path": imagePath,
	}}, dir)
	if !allowedCodexImageView(map[string]any{
		"type": "imageView",
		"path": imagePath,
	}, allowed, dir) {
		t.Fatal("materialized request image was rejected")
	}
	if !allowedCodexImageView(map[string]any{
		"type": "imageView",
		"path": filepath.Base(imagePath),
	}, allowed, dir) {
		t.Fatal("relative request image path was rejected")
	}
	if allowedCodexImageView(map[string]any{
		"type": "imageView",
		"path": filepath.Join(dir, "..", "outside.png"),
	}, allowed, dir) {
		t.Fatal("image path outside the attachment allowlist was accepted")
	}
}

func TestRPCIgnoresWrapperBannerBeforeJSONL(t *testing.T) {
	var input bytes.Buffer
	input.WriteString("Using Codex WORK profile\n")
	input.WriteString("CODEX_HOME=/tmp/codex-work\n")
	response := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{"ok": true},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	input.Write(raw)
	input.WriteByte('\n')

	var output bytes.Buffer
	rpc := newRPC(&output, &input)
	select {
	case event := <-rpc.events:
		if event.Method != "initialized" || event.Params["ok"] != true {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for JSONL event")
	}
	if diagnostics := rpc.Diagnostics(); !strings.Contains(diagnostics, "Using Codex WORK profile") {
		t.Fatalf("diagnostics = %q", diagnostics)
	}
}

func TestAppServerArgsDisableProviderContextAndNativeTools(t *testing.T) {
	joined := strings.Join(appServerArgs(), "\n")
	for _, expected := range []string{
		`project_doc_max_bytes=0`,
		`include_permissions_instructions=false`,
		`include_apps_instructions=false`,
		`include_collaboration_mode_instructions=false`,
		`skills.include_instructions=false`,
		`orchestrator_skills_enabled=false`,
		`orchestrator_mcp_enabled=false`,
		`include_environment_context=false`,
		`mcp_servers={}`,
		`web_search="disabled"`,
		"shell_tool",
		"unified_exec",
		"browser_use",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("app-server args missing %q:\n%s", expected, joined)
		}
	}
}

func TestThreadParamsUseFullPermissions(t *testing.T) {
	params := threadStartParams("gpt-test", "/tmp/request", "client system", nil)
	if params["approvalPolicy"] != "never" {
		t.Fatalf("approvalPolicy = %#v", params["approvalPolicy"])
	}
	if params["sandbox"] != "danger-full-access" {
		t.Fatalf("sandbox = %#v, want danger-full-access", params["sandbox"])
	}
}

func TestCodexInputsPreserveImagesAndDocuments(t *testing.T) {
	imageData := base64.StdEncoding.EncodeToString(liveTestImage(t))
	documentData := base64.StdEncoding.EncodeToString([]byte("MACAZ_DOCUMENT_OK\n"))
	input, err := codexInputs(context.Background(), t.TempDir(), "client transcript", []protocol.Attachment{
		{
			Kind:      "image",
			MediaType: "image/png",
			Data:      imageData,
			Filename:  "diagram.png",
		},
		{
			Kind:      "document",
			MediaType: "text/plain",
			Data:      documentData,
			Filename:  "notes.txt",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(input) != 2 {
		t.Fatalf("input = %#v", input)
	}
	text, _ := input[0]["text"].(string)
	if input[0]["type"] != "text" ||
		!strings.Contains(text, "client transcript") ||
		!strings.Contains(text, "MACAZ_DOCUMENT_OK") ||
		!strings.Contains(text, `name="notes.txt"`) {
		t.Fatalf("text input = %#v", input[0])
	}
	if input[1]["type"] != "localImage" {
		t.Fatalf("image input = %#v", input[1])
	}
	path, _ := input[1]["path"].(string)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expectedImage, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, expectedImage) {
		t.Fatalf("image contents differ: got %d bytes, want %d", len(raw), len(expectedImage))
	}
}

func TestProviderUsesClientContextDynamicToolsAndFullPermissions(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)

	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = "fake-default"
	}
	upstream := New(cfg)
	req := &protocol.Request{
		Model:  "fake-next",
		System: json.RawMessage(`[{"type":"text","text":"CLIENT SYSTEM ONLY"}]`),
		Messages: []protocol.Message{{
			Role:    "user",
			Content: json.RawMessage(`"inspect the repository"`),
		}},
		Tools: []protocol.Tool{
			{Name: "Read", Description: "Read a file", InputSchema: map[string]any{"type": "object"}},
			{Name: "Bash", Description: "Run a command", InputSchema: map[string]any{"type": "object"}},
		},
		ToolChoice:   json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
		OutputConfig: json.RawMessage(`{"effort":"high"}`),
	}
	result, err := upstream.Generate(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "fake-next" || result.StopReason != "tool_use" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Blocks) != 2 || result.Blocks[0].Text != "checking " ||
		result.Blocks[1].Type != "tool_use" || result.Blocks[1].Name != "Read" {
		t.Fatalf("blocks = %#v", result.Blocks)
	}
	if result.Usage.InputTokens != 31 || result.Usage.OutputTokens != 7 ||
		result.Usage.CacheReadInputTokens != 4 || result.Usage.ReasoningOutputTokens != 3 {
		t.Fatalf("usage = %#v", result.Usage)
	}

	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeCodexReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.ThreadParams["model"] != "fake-next" ||
		report.ThreadParams["baseInstructions"] != "CLIENT SYSTEM ONLY" ||
		report.ThreadParams["developerInstructions"] != "" ||
		report.ThreadParams["approvalPolicy"] != "never" ||
		report.ThreadParams["sandbox"] != "danger-full-access" ||
		report.ThreadParams["ephemeral"] != true ||
		report.ThreadParams["allowProviderModelFallback"] != false {
		t.Fatalf("thread params = %#v", report.ThreadParams)
	}
	if report.CWD == "" || !strings.Contains(filepath.Base(report.CWD), "macaz-codex-") ||
		report.PWD != report.CWD {
		t.Fatalf("Codex request isolation = cwd %q, PWD %q", report.CWD, report.PWD)
	}
	dynamicTools, _ := report.ThreadParams["dynamicTools"].([]any)
	if len(dynamicTools) != 1 {
		t.Fatalf("dynamic tools = %#v", report.ThreadParams["dynamicTools"])
	}
	selected, _ := dynamicTools[0].(map[string]any)
	if selected["name"] != "Read" {
		t.Fatalf("selected dynamic tool = %#v", selected)
	}
	if report.TurnParams["effort"] != "high" {
		t.Fatalf("turn params = %#v", report.TurnParams)
	}
	input, _ := report.TurnParams["input"].([]any)
	if len(input) == 0 {
		t.Fatalf("turn input = %#v", input)
	}
	firstInput, _ := input[0].(map[string]any)
	if !strings.Contains(stringValue(firstInput["text"]), "inspect the repository") {
		t.Fatalf("turn input = %#v", input)
	}
	joinedArgs := strings.Join(report.Args, "\n")
	for _, expected := range []string{
		"project_doc_max_bytes=0",
		"include_permissions_instructions=false",
		"include_environment_context=false",
		"mcp_servers={}",
		"shell_tool",
		"unified_exec",
	} {
		if !strings.Contains(joinedArgs, expected) {
			t.Fatalf("app-server args missing %q: %#v", expected, report.Args)
		}
	}
}

func TestProviderRejectsOutputClosureAfterPartialDelta(t *testing.T) {
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_CLOSE_AFTER_DELTA", "1")
	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)
	defer upstream.Close()
	_, err := upstream.Generate(context.Background(), &protocol.Request{
		Model:    "fake-default",
		Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "closed its output") {
		t.Fatalf("partial output error = %v", err)
	}
}

func TestProviderMapsContextOverflowAndCapsCompactionEffort(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_CONTEXT_OVERFLOW", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)
	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)
	defer upstream.Close()
	_, err := upstream.Generate(context.Background(), &protocol.Request{
		Model:        "fake-default",
		System:       json.RawMessage(`"You are a helpful AI assistant tasked with summarizing conversations."`),
		OutputConfig: json.RawMessage(`{"effort":"high"}`),
		Messages:     []protocol.Message{{Role: "user", Content: json.RawMessage(`"compact"`)}},
	}, nil)
	if provider.Status(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, err = %v", provider.Status(err), err)
	}
	raw, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var report fakeCodexReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.TurnParams["effort"] != "low" {
		t.Fatalf("compaction turn params = %#v", report.TurnParams)
	}
}

func TestProviderReusesQuiescedServerAfterInterruptedToolTurn(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)

	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)
	defer upstream.Close()

	req := &protocol.Request{
		Model: "fake-default",
		Messages: []protocol.Message{{
			Role:    "user",
			Content: json.RawMessage(`"read README.md"`),
		}},
		Tools: []protocol.Tool{{
			Name:        "Read",
			Description: "Read a file",
			InputSchema: map[string]any{"type": "object"},
		}},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
	}

	firstPID := 0
	for attempt := 0; attempt < 2; attempt++ {
		result, err := upstream.Generate(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("generate %d: %v", attempt+1, err)
		}
		if result.StopReason != "tool_use" {
			t.Fatalf("generate %d result = %#v", attempt+1, result)
		}
		raw, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		var report fakeCodexReport
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			firstPID = report.PID
		} else if report.PID != firstPID {
			t.Fatalf("app-server was not reused: first pid %d, second pid %d", firstPID, report.PID)
		}
	}
}

func TestProviderContinuesPendingDynamicToolOnSameThread(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_STRUCTURED_HANDOFF", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)

	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)
	defer upstream.Close()

	tools := []protocol.Tool{{
		Name:        "Read",
		Description: "Read a file",
		InputSchema: map[string]any{"type": "object"},
	}}
	firstRequest := &protocol.Request{
		Model:          "fake-default",
		PromptCacheKey: "claude-session/agent-main",
		Messages:       []protocol.Message{{Role: "user", Content: json.RawMessage(`"read README.md"`)}},
		Tools:          tools,
		ToolChoice:     json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
	}
	firstResult, err := upstream.Generate(context.Background(), firstRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstResult.StopReason != "tool_use" {
		t.Fatalf("first result = %#v", firstResult)
	}
	assistantContent, err := json.Marshal(firstResult.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := upstream.Generate(context.Background(), &protocol.Request{
		Model:          "fake-default",
		PromptCacheKey: "claude-session/agent-main",
		Messages: []protocol.Message{
			{Role: "user", Content: json.RawMessage(`"read README.md"`)},
			{Role: "assistant", Content: assistantContent},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call-fake","content":"README contents"}]`)},
		},
		Tools:      tools,
		ToolChoice: json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secondResult.StopReason != "end_turn" || len(secondResult.Blocks) != 1 || secondResult.Blocks[0].Text != "finished" {
		t.Fatalf("second result = %#v", secondResult)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeCodexReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.ThreadStarts != 1 || report.TurnStarts != 1 || report.ToolResults != 1 {
		t.Fatalf("structured handoff report = %#v", report)
	}
}

func TestProviderPreservesQueuedUserMessageByStartingFreshThread(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_STRUCTURED_HANDOFF", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)

	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)
	defer upstream.Close()

	tools := []protocol.Tool{{
		Name:        "Read",
		Description: "Read a file",
		InputSchema: map[string]any{"type": "object"},
	}}
	toolChoice := json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`)
	firstRequest := &protocol.Request{
		Model:          "fake-default",
		PromptCacheKey: "claude-session/agent-main",
		Messages:       []protocol.Message{{Role: "user", Content: json.RawMessage(`"read README.md"`)}},
		Tools:          tools,
		ToolChoice:     toolChoice,
	}
	firstResult, err := upstream.Generate(context.Background(), firstRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstRaw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var firstReport fakeCodexReport
	if err := json.Unmarshal(firstRaw, &firstReport); err != nil {
		t.Fatal(err)
	}
	assistantContent, err := json.Marshal(firstResult.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := upstream.Generate(context.Background(), &protocol.Request{
		Model:          "fake-default",
		PromptCacheKey: "claude-session/agent-main",
		Messages: []protocol.Message{
			{Role: "user", Content: json.RawMessage(`"read README.md"`)},
			{Role: "assistant", Content: assistantContent},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"call-fake","content":"README contents"},
				{"type":"text","text":"stop now and explain instead"}
			]`)},
		},
		Tools:      tools,
		ToolChoice: toolChoice,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secondResult.StopReason != "tool_use" {
		t.Fatalf("second result = %#v", secondResult)
	}
	secondRaw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var secondReport fakeCodexReport
	if err := json.Unmarshal(secondRaw, &secondReport); err != nil {
		t.Fatal(err)
	}
	if secondReport.PID == firstReport.PID {
		t.Fatalf("queued user input incorrectly resumed parked app-server pid %d", secondReport.PID)
	}
	turnInput, err := json.Marshal(secondReport.TurnParams["input"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(turnInput, []byte("stop now and explain instead")) {
		t.Fatalf("queued user input missing from fresh transcript: %s", turnInput)
	}
	if secondReport.ToolResults != 0 {
		t.Fatalf("queued user input was incorrectly sent as a structured resume: %#v", secondReport)
	}
}

func TestClaimPendingFallsBackAndClearsIncompleteToolResults(t *testing.T) {
	cfg := config.Default()
	upstream := New(cfg)
	released := 0
	state := &codexTurnState{
		release:        func(bool) { released++ },
		requestDir:     t.TempDir(),
		shapeSignature: "same-shape",
		pending: map[string]json.RawMessage{
			"call-one": json.RawMessage(`1`),
			"call-two": json.RawMessage(`2`),
		},
	}
	if !state.reserveParked(upstream.parkedSlots) {
		t.Fatal("failed to reserve parked slot")
	}
	upstream.sessions["session"] = state

	claimed, results, resumed, err := upstream.claimPending("session", "same-shape", &protocol.Request{
		Messages: []protocol.Message{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call-one","content":"done"}]`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil || results != nil || resumed {
		t.Fatalf("incomplete results unexpectedly resumed: state=%#v results=%#v resumed=%v", claimed, results, resumed)
	}
	if upstream.sessions["session"] != nil || len(upstream.parkedSlots) != 0 || released != 1 {
		t.Fatalf("pending state was not cleared: sessions=%#v parked=%d released=%d", upstream.sessions, len(upstream.parkedSlots), released)
	}
}

func TestParkPendingReservesOneCLISlotForNewTraffic(t *testing.T) {
	cfg := config.Default()
	cfg.MaxConcurrentCLI = 4
	upstream := New(cfg)
	defer upstream.closePending()

	for index := 0; index < cfg.MaxConcurrentCLI; index++ {
		state := &codexTurnState{
			release: func(bool) {},
			pending: map[string]json.RawMessage{"call": json.RawMessage(`1`)},
		}
		preserved, err := upstream.parkPending("session-"+string(rune('a'+index)), state)
		if err != nil {
			t.Fatal(err)
		}
		if index < cfg.MaxConcurrentCLI-1 && !preserved {
			t.Fatalf("park %d was rejected before the reserved slot", index)
		}
		if index == cfg.MaxConcurrentCLI-1 {
			if preserved {
				t.Fatal("last CLI slot was consumed by a parked turn")
			}
			state.cleanup(false)
		}
	}
	if len(upstream.parkedSlots) != cfg.MaxConcurrentCLI-1 {
		t.Fatalf("parked slots = %d", len(upstream.parkedSlots))
	}
}

func TestProviderFallsBackWhenParkingWouldSaturatePool(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	t.Setenv("MACAZ_FAKE_CODEX_STRUCTURED_HANDOFF", "1")
	t.Setenv("MACAZ_FAKE_CODEX_REPORT", reportPath)

	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	cfg.MaxConcurrentCLI = 1
	upstream := New(cfg)
	defer upstream.Close()
	req := &protocol.Request{
		Model:          "fake-default",
		PromptCacheKey: "claude-session/agent-main",
		Messages:       []protocol.Message{{Role: "user", Content: json.RawMessage(`"read README.md"`)}},
		Tools: []protocol.Tool{{
			Name:        "Read",
			Description: "Read a file",
			InputSchema: map[string]any{"type": "object"},
		}},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
	}

	for attempt := 0; attempt < 2; attempt++ {
		result, err := upstream.Generate(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("generate %d: %v", attempt+1, err)
		}
		if result.StopReason != "tool_use" {
			t.Fatalf("generate %d result = %#v", attempt+1, result)
		}
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report fakeCodexReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.ThreadStarts != 2 || report.TurnStarts != 2 {
		t.Fatalf("fallback did not keep the single app-server reusable: %#v", report)
	}
}

func TestProviderRejectsUnknownModelInsteadOfFallingBack(t *testing.T) {
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)

	_, err := upstream.Generate(context.Background(), &protocol.Request{
		Model: "missing-model",
		Messages: []protocol.Message{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
		}},
	}, nil)
	if err == nil {
		t.Fatal("expected missing model to be rejected")
	}
	if status := provider.Status(err); status != http.StatusBadRequest {
		t.Fatalf("status = %d, err = %v", status, err)
	}
	if !strings.Contains(err.Error(), `Codex model "missing-model" is not present`) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderDiscoversModelsFromLocalCodex(t *testing.T) {
	t.Setenv("MACAZ_FAKE_CODEX", "1")
	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = os.Args[0]
	upstream := New(cfg)
	if err := upstream.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, err := upstream.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "fake-default" || !models[0].Default || models[0].ContextWindow != 272000 {
		t.Fatalf("models = %#v", models)
	}
	if !slices.Equal(models[0].Efforts, []string{"low", "high"}) ||
		!slices.Equal(models[0].InputModalities, []string{"text", "image"}) {
		t.Fatalf("model capabilities = %#v", models[0])
	}
}

func TestLiveCodexIntegration(t *testing.T) {
	executable := strings.TrimSpace(os.Getenv("MACAZ_CODEX_INTEGRATION_EXECUTABLE"))
	if executable == "" {
		t.Skip("set MACAZ_CODEX_INTEGRATION_EXECUTABLE to run against an authenticated local Codex CLI")
	}

	cfg := config.Default()
	cfg.Provider = config.ProviderCodexCLI
	cfg.CodexExecutable = executable
	discovery := New(cfg)

	discoveryCtx, discoveryCancel := context.WithTimeout(context.Background(), 45*time.Second)
	models, err := discovery.Models(discoveryCtx)
	discoveryCancel()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("Codex returned no models")
	}

	selected := strings.TrimSpace(os.Getenv("MACAZ_CODEX_INTEGRATION_MODEL"))
	if selected == "" {
		for _, model := range models {
			if model.Default {
				selected = model.ID
				break
			}
		}
	}
	if selected == "" {
		selected = models[0].ID
	}
	if !slices.ContainsFunc(models, func(model provider.Model) bool {
		return model.ID == selected
	}) {
		t.Fatalf("requested integration model %q is not in the Codex catalog", selected)
	}
	selectedCapabilities := models[slices.IndexFunc(models, func(model provider.Model) bool {
		return model.ID == selected
	})]
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = selected
	}
	upstream := New(cfg)

	t.Run("text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		result, err := upstream.Generate(ctx, &protocol.Request{
			Model:  selected,
			System: json.RawMessage(`"Reply with exactly MACAZ_CONTEXT_OK and nothing else."`),
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Follow the system instruction."`),
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.StopReason != "end_turn" {
			t.Fatalf("stop reason = %q, result = %#v", result.StopReason, result)
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
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		result, err := upstream.Generate(ctx, &protocol.Request{
			Model:  selected,
			System: json.RawMessage(`"You are an inference provider. Invoke the only client tool when the user requests it. Do not use native tools."`),
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
		if result.StopReason != "tool_use" {
			t.Fatalf("stop reason = %q, result = %#v", result.StopReason, result)
		}
		for _, block := range result.Blocks {
			if block.Type == "tool_use" && block.Name == "Read" &&
				strings.Contains(string(block.Input), "README.md") {
				return
			}
		}
		t.Fatalf("expected Read tool call for README.md, result = %#v", result)
	})

	t.Run("document", func(t *testing.T) {
		document := base64.StdEncoding.EncodeToString([]byte(
			"The exact verification token in this document is MACAZ_DOCUMENT_OK.\n",
		))
		content, err := json.Marshal([]any{
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
					"data":       document,
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		result, err := upstream.Generate(ctx, &protocol.Request{
			Model:  selected,
			System: json.RawMessage(`"Read the client attachment. Reply with only the exact verification token contained in it."`),
			Messages: []protocol.Message{{
				Role:    "user",
				Content: content,
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if text := resultText(result); strings.TrimSpace(text) != "MACAZ_DOCUMENT_OK" {
			t.Fatalf("document response = %q, result = %#v", text, result)
		}
	})

	if slices.Contains(selectedCapabilities.InputModalities, "image") {
		t.Run("image", func(t *testing.T) {
			content, err := json.Marshal([]any{
				map[string]any{
					"type": "text",
					"text": "Inspect the attached image. State the color of the left half and then the color of the right half using exactly two lowercase color words separated by a comma.",
				},
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       base64.StdEncoding.EncodeToString(liveTestImage(t)),
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			result, err := upstream.Generate(ctx, &protocol.Request{
				Model:  selected,
				System: json.RawMessage(`"Inspect the client image precisely. Do not infer colors from the prompt; report only what is visibly present."`),
				Messages: []protocol.Message{{
					Role:    "user",
					Content: content,
				}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if text := resultText(result); !strings.Contains(strings.ToLower(text), "red") ||
				!strings.Contains(strings.ToLower(text), "blue") {
				t.Fatalf("image response = %q, result = %#v", text, result)
			}
		})
	}
}

func liveTestImage(t *testing.T) []byte {
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

func resultText(result protocol.Result) string {
	var text strings.Builder
	for _, block := range result.Blocks {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}
