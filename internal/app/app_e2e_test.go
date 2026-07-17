package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

const (
	e2eImageBase64      = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	e2eDocumentFilename = "macaz-e2e.pdf"
)

var e2eDocumentBytes = makeE2EPDF("MACAZ_E2E_DOCUMENT")

type endToEndReport struct {
	BaseURL          string   `json:"base_url"`
	Model            string   `json:"model"`
	Args             []string `json:"args"`
	Models           []string `json:"models"`
	ResponseModel    string   `json:"response_model"`
	StopReason       string   `json:"stop_reason"`
	ToolName         string   `json:"tool_name"`
	ToolPath         string   `json:"tool_path"`
	CountTokens      int      `json:"count_tokens"`
	CountWasEstimate bool     `json:"count_was_estimate"`
	Error            string   `json:"error,omitempty"`
}

type hostRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (transport hostRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	urlCopy := *request.URL
	urlCopy.Scheme = transport.target.Scheme
	urlCopy.Host = transport.target.Host
	clone.URL = &urlCopy
	clone.Host = ""
	return transport.base.RoundTrip(clone)
}

func TestMain(m *testing.M) {
	if os.Getenv("MACAZ_APP_E2E_HELPER") == "1" {
		os.Exit(runEndToEndHelper())
	}
	os.Exit(m.Run())
}

func TestRunClaudeEndToEndWithLocalCLIProviders(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
	}{
		{name: "Codex-CLI", provider: config.ProviderCodexCLI},
		{name: "OpenCode-CLI", provider: config.ProviderOpenCodeCLI},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			reportPath := filepath.Join(root, "report.json")
			daemonMarker := filepath.Join(root, "daemon-stopped")
			sourceProfile := filepath.Join(root, "normal-claude")
			if err := os.MkdirAll(sourceProfile, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
			t.Setenv("CLAUDE_CONFIG_DIR", sourceProfile)
			t.Setenv("MACAZ_APP_E2E_HELPER", "1")
			t.Setenv("MACAZ_APP_E2E_PROVIDER", test.provider)
			t.Setenv("MACAZ_APP_E2E_REPORT", reportPath)
			t.Setenv("MACAZ_APP_E2E_DAEMON_MARKER", daemonMarker)

			cfg := config.Default()
			cfg.Provider = test.provider
			cfg.ClaudeExecutable = os.Args[0]
			cfg.CodexExecutable = os.Args[0]
			cfg.OpenCodeExecutable = os.Args[0]
			cfg.OpenCodeModel = "fake/default"
			for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
				cfg.ModelMap[alias] = "fake/default"
			}
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := Run(ctx, nil, Streams{
				In:  strings.NewReader(""),
				Out: &stdout,
				Err: &stderr,
			}); err != nil {
				t.Fatalf("run macaz: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}

			raw, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			var report endToEndReport
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			if report.Error != "" {
				t.Fatalf("fake Claude client: %s", report.Error)
			}
			if report.BaseURL == "" || !strings.HasPrefix(report.BaseURL, "http://127.0.0.1:") {
				t.Fatalf("gateway URL = %q", report.BaseURL)
			}
			if !strings.HasPrefix(report.Model, "claude-macaz-fake-default-") {
				t.Fatalf("public model = %q", report.Model)
			}
			if report.ResponseModel != report.Model {
				t.Fatalf("response model = %q, launch model = %q", report.ResponseModel, report.Model)
			}
			if report.StopReason != "tool_use" || report.ToolName != "Read" || report.ToolPath != "README.md" {
				t.Fatalf("translated response = %#v", report)
			}
			if report.CountTokens < 1 || !report.CountWasEstimate {
				t.Fatalf("count_tokens = %d, estimated = %t", report.CountTokens, report.CountWasEstimate)
			}
			if !slices.Contains(report.Models, report.Model) {
				t.Fatalf("model catalog = %#v, selected = %q", report.Models, report.Model)
			}
			if slices.Contains(report.Args, "--dangerously-skip-permissions") {
				t.Fatalf("permissions were bypassed without an explicit user flag: %#v", report.Args)
			}
			if _, err := os.Stat(daemonMarker); err != nil {
				t.Fatalf("isolated Claude daemon was not stopped: %v", err)
			}

			client := &http.Client{Timeout: 300 * time.Millisecond}
			if response, err := client.Get(report.BaseURL + "/health"); err == nil {
				response.Body.Close()
				t.Fatalf("gateway still accepted requests after Claude exited: HTTP %d", response.StatusCode)
			}
		})
	}
}

func TestRunClaudeEndToEndWithHTTPProviders(t *testing.T) {
	for _, test := range []struct {
		name              string
		provider          string
		model             string
		countWasEstimate  bool
		configureProvider func(*config.Config, string)
	}{
		{
			name:             "OpenAI API",
			provider:         config.ProviderOpenAIAPIKey,
			model:            "gpt-e2e",
			countWasEstimate: false,
			configureProvider: func(cfg *config.Config, baseURL string) {
				cfg.OpenAIBaseURL = baseURL
				cfg.OpenAIModel = "gpt-e2e"
			},
		},
		{
			name:             "OpenRouter API",
			provider:         config.ProviderOpenRouterAPI,
			model:            "openai/gpt-e2e",
			countWasEstimate: true,
			configureProvider: func(cfg *config.Config, baseURL string) {
				cfg.OpenRouterBaseURL = baseURL
				cfg.OpenRouterModel = "openai/gpt-e2e"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer e2e-api-key" {
					http.Error(w, "missing API key", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/v1/key":
					_, _ = io.WriteString(w, `{"data":{"label":"e2e"}}`)
				case "/v1/models":
					if test.provider == config.ProviderOpenRouterAPI {
						_, _ = io.WriteString(w, `{"data":[{
							"id":"openai/gpt-e2e",
							"name":"GPT E2E",
							"context_length":128000,
							"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},
							"supported_parameters":["tools","response_format"],
							"reasoning":{"supported_efforts":["low","high"]}
						}]}`)
						return
					}
					_, _ = io.WriteString(w, `{"data":[{"id":"gpt-e2e"}]}`)
				case "/v1/responses":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if body["model"] != test.model {
						http.Error(w, fmt.Sprintf("unexpected model %#v", body["model"]), http.StatusBadRequest)
						return
					}
					if err := validateResponsesE2EAttachments(body); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					tools, _ := body["tools"].([]any)
					if len(tools) != 1 {
						http.Error(w, "client tool was not forwarded", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w,
						"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_e2e\",\"model\":"+strconvQuote(test.model)+"}}\n\n"+
							"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_e2e\",\"call_id\":\"call_e2e\",\"name\":\"Read\"}}\n\n"+
							"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_e2e\",\"delta\":\"{\\\"path\\\":\\\"README.md\\\"}\"}\n\n"+
							"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_e2e\"}}\n\n"+
							"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_e2e\",\"model\":"+strconvQuote(test.model)+",\"status\":\"completed\",\"usage\":{\"input_tokens\":13,\"output_tokens\":3}}}\n\n",
					)
				case "/v1/responses/input_tokens":
					_, _ = io.WriteString(w, `{"input_tokens":17}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()

			root := t.TempDir()
			reportPath := filepath.Join(root, "report.json")
			daemonMarker := filepath.Join(root, "daemon-stopped")
			sourceProfile := filepath.Join(root, "normal-claude")
			if err := os.MkdirAll(sourceProfile, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
			t.Setenv("CLAUDE_CONFIG_DIR", sourceProfile)
			t.Setenv("MACAZ_APP_E2E_HELPER", "1")
			t.Setenv("MACAZ_APP_E2E_PROVIDER", test.provider)
			t.Setenv("MACAZ_APP_E2E_REPORT", reportPath)
			t.Setenv("MACAZ_APP_E2E_DAEMON_MARKER", daemonMarker)
			t.Setenv("OPENAI_API_KEY", "e2e-api-key")
			t.Setenv("OPENROUTER_API_KEY", "e2e-api-key")

			cfg := config.Default()
			cfg.Provider = test.provider
			cfg.ClaudeExecutable = os.Args[0]
			test.configureProvider(&cfg, upstream.URL+"/v1")
			for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
				cfg.ModelMap[alias] = test.model
			}
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}

			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := Run(ctx, nil, Streams{
				In:  strings.NewReader(""),
				Out: &output,
				Err: &output,
			}); err != nil {
				t.Fatalf("run macaz: %v\n%s", err, output.String())
			}

			raw, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			var report endToEndReport
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			if report.Error != "" {
				t.Fatalf("fake Claude client: %s", report.Error)
			}
			if report.ResponseModel != report.Model ||
				report.StopReason != "tool_use" ||
				report.ToolName != "Read" ||
				report.ToolPath != "README.md" {
				t.Fatalf("translated response = %#v", report)
			}
			if report.CountTokens < 1 || report.CountWasEstimate != test.countWasEstimate {
				t.Fatalf("count_tokens = %d, estimated = %t", report.CountTokens, report.CountWasEstimate)
			}
			if !slices.Contains(report.Models, report.Model) {
				t.Fatalf("model catalog = %#v, selected = %q", report.Models, report.Model)
			}
			if _, err := os.Stat(daemonMarker); err != nil {
				t.Fatalf("isolated Claude daemon was not stopped: %v", err)
			}
			client := &http.Client{Timeout: 300 * time.Millisecond}
			if response, err := client.Get(report.BaseURL + "/health"); err == nil {
				response.Body.Close()
				t.Fatalf("gateway still accepted requests after Claude exited: HTTP %d", response.StatusCode)
			}
		})
	}
}

func TestRunClaudeEndToEndWithOpenAISubscription(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer subscription-e2e-token" ||
			r.Header.Get("ChatGPT-Account-Id") != "account-e2e" {
			http.Error(w, "missing subscription authorization", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/backend-api/codex/models":
			_, _ = io.WriteString(w, `{"models":[{
				"slug":"gpt-subscription-e2e",
				"display_name":"GPT Subscription E2E",
				"visibility":"list",
				"priority":1,
				"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],
				"input_modalities":["text","image"]
			}]}`)
		case "/backend-api/codex/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body["model"] != "gpt-subscription-e2e" ||
				body["parallel_tool_calls"] != false {
				http.Error(w, fmt.Sprintf("unexpected subscription body %#v", body), http.StatusBadRequest)
				return
			}
			if err := validateSubscriptionE2EAttachments(body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if _, exists := body["user"]; exists {
				http.Error(w, "subscription body retained user", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_sub_e2e\",\"model\":\"gpt-subscription-e2e\"}}\n\n"+
					"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_sub_e2e\",\"call_id\":\"call_sub_e2e\",\"name\":\"Read\"}}\n\n"+
					"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_sub_e2e\",\"delta\":\"{\\\"path\\\":\\\"README.md\\\"}\"}\n\n"+
					"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_sub_e2e\"}}\n\n"+
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sub_e2e\",\"model\":\"gpt-subscription-e2e\",\"status\":\"completed\",\"usage\":{\"input_tokens\":13,\"output_tokens\":3}}}\n\n",
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	priorTransport := http.DefaultTransport
	http.DefaultTransport = hostRewriteTransport{target: target, base: priorTransport}
	t.Cleanup(func() {
		http.DefaultTransport = priorTransport
	})

	if err := secrets.Set(secrets.OpenAIAccount, fmt.Sprintf(
		`{"type":"openai_account_oauth","method":"chatgpt_headless","access":"subscription-e2e-token","expires_at":%d,"account_id":"account-e2e"}`,
		time.Now().Add(time.Hour).UnixMilli(),
	)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = secrets.Delete(secrets.OpenAIAccount)
	})

	root := t.TempDir()
	reportPath := filepath.Join(root, "report.json")
	daemonMarker := filepath.Join(root, "daemon-stopped")
	sourceProfile := filepath.Join(root, "normal-claude")
	if err := os.MkdirAll(sourceProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", sourceProfile)
	t.Setenv("MACAZ_APP_E2E_HELPER", "1")
	t.Setenv("MACAZ_APP_E2E_PROVIDER", config.ProviderOpenAISubscription)
	t.Setenv("MACAZ_APP_E2E_REPORT", reportPath)
	t.Setenv("MACAZ_APP_E2E_DAEMON_MARKER", daemonMarker)

	cfg := config.Default()
	cfg.Provider = config.ProviderOpenAISubscription
	cfg.ClaudeExecutable = os.Args[0]
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = "gpt-subscription-e2e"
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := Run(ctx, nil, Streams{
		In:  strings.NewReader(""),
		Out: &output,
		Err: &output,
	}); err != nil {
		t.Fatalf("run macaz: %v\n%s", err, output.String())
	}

	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report endToEndReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Error != "" {
		t.Fatalf("fake Claude client: %s", report.Error)
	}
	if report.ResponseModel != report.Model ||
		report.StopReason != "tool_use" ||
		report.ToolName != "Read" ||
		report.ToolPath != "README.md" ||
		report.CountTokens < 1 ||
		!report.CountWasEstimate {
		t.Fatalf("subscription flow = %#v", report)
	}
	if !slices.Contains(report.Models, report.Model) {
		t.Fatalf("subscription model catalog = %#v, selected = %q", report.Models, report.Model)
	}
	if _, err := os.Stat(daemonMarker); err != nil {
		t.Fatalf("isolated Claude daemon was not stopped: %v", err)
	}
	client := &http.Client{
		Transport: priorTransport,
		Timeout:   300 * time.Millisecond,
	}
	if response, err := client.Get(report.BaseURL + "/health"); err == nil {
		response.Body.Close()
		t.Fatalf("gateway still accepted requests after Claude exited: HTTP %d", response.StatusCode)
	}
}

func runEndToEndHelper() int {
	if len(os.Args) < 2 {
		return writeEndToEndFailure("helper received no arguments")
	}
	switch os.Args[1] {
	case "--version":
		if os.Getenv("MACAZ_APP_E2E_PROVIDER") == config.ProviderCodexCLI {
			_, _ = io.WriteString(os.Stdout, "codex-cli e2e\n")
		} else {
			_, _ = io.WriteString(os.Stdout, "opencode e2e\n")
		}
		return 0
	case "app-server":
		return runEndToEndCodex()
	case "models":
		_, _ = io.WriteString(os.Stdout, `fake/default
{
  "name": "Fake Default",
  "variants": {"low": {}, "high": {}},
  "capabilities": {"input": {"image": true, "pdf": true}}
}
`)
		return 0
	case "run":
		if err := validateOpenCodeE2EAttachments(os.Args[2:]); err != nil {
			return writeEndToEndFailure(err.Error())
		}
		_, _ = io.WriteString(os.Stdout, `{"type":"step_finish","part":{"tokens":{"input":21,"output":4,"reasoning":2,"cache":{"read":3,"write":1}},"reason":"tool-calls"}}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"type":"tool_use","part":{"callID":"call_e2e","tool":"Read","state":{"status":"running","input":{"path":"README.md"}}}}`+"\n")
		return 0
	case "daemon":
		if len(os.Args) >= 4 && os.Args[2] == "stop" && os.Args[3] == "--any" {
			if path := os.Getenv("MACAZ_APP_E2E_DAEMON_MARKER"); path != "" {
				_ = os.WriteFile(path, []byte("stopped\n"), 0o600)
			}
			return 0
		}
		return writeEndToEndFailure("unexpected daemon command")
	default:
		return runEndToEndClaude()
	}
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func runEndToEndCodex() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"serverInfo": map[string]any{"name": "e2e-codex"}},
			})
		case "initialized":
		case "model/list":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"data": []any{map[string]any{
						"model":       "fake/default",
						"displayName": "Fake Default",
						"isDefault":   true,
						"supportedReasoningEfforts": []any{
							map[string]any{"reasoningEffort": "low"},
							map[string]any{"reasoningEffort": "high"},
						},
						"inputModalities": []any{"text", "image"},
					}},
					"nextCursor": nil,
				},
			})
		case "thread/start":
			tools, _ := request.Params["dynamicTools"].([]any)
			if len(tools) != 1 {
				return writeEndToEndFailure(fmt.Sprintf("Codex dynamic tools = %#v", request.Params["dynamicTools"]))
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"thread": map[string]any{"id": "thread-e2e"}},
			})
		case "turn/start":
			if err := validateCodexE2EAttachments(request.Params); err != nil {
				return writeEndToEndFailure(err.Error())
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"turn": map[string]any{"id": "turn-e2e"}},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "thread/tokenUsage/updated",
				"params": map[string]any{"tokenUsage": map[string]any{"last": map[string]any{
					"inputTokens": 11, "outputTokens": 3,
				}}},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "item/tool/call",
				"params": map[string]any{
					"callId": "call-e2e",
					"tool":   "Read",
					"arguments": map[string]any{
						"path": "README.md",
					},
				},
			})
		case "turn/interrupt":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{},
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return writeEndToEndFailure(err.Error())
	}
	return 0
}

func runEndToEndClaude() int {
	report := endToEndReport{
		BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		Model:   os.Getenv("ANTHROPIC_MODEL"),
		Args:    append([]string(nil), os.Args[1:]...),
	}
	token := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if report.BaseURL == "" || token == "" || report.Model == "" {
		report.Error = "Claude launch environment is missing gateway URL, token, or model"
		return writeEndToEndReport(report)
	}
	client := &http.Client{Timeout: 5 * time.Second}

	modelRequest, err := http.NewRequest(http.MethodGet, report.BaseURL+"/v1/models", nil)
	if err != nil {
		report.Error = err.Error()
		return writeEndToEndReport(report)
	}
	modelRequest.Header.Set("x-api-key", token)
	modelResponse, err := client.Do(modelRequest)
	if err != nil {
		report.Error = fmt.Sprintf("list gateway models: %v", err)
		return writeEndToEndReport(report)
	}
	var modelPayload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if modelResponse.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(modelResponse.Body)
		modelResponse.Body.Close()
		report.Error = fmt.Sprintf("list gateway models: HTTP %d: %s", modelResponse.StatusCode, raw)
		return writeEndToEndReport(report)
	}
	err = json.NewDecoder(modelResponse.Body).Decode(&modelPayload)
	modelResponse.Body.Close()
	if err != nil {
		report.Error = fmt.Sprintf("decode gateway models: %v", err)
		return writeEndToEndReport(report)
	}
	for _, model := range modelPayload.Data {
		report.Models = append(report.Models, model.ID)
	}

	rawBody, err := json.Marshal(map[string]any{
		"model":      report.Model,
		"max_tokens": 256,
		"system":     "CLIENT SYSTEM E2E",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "read the project README"},
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       e2eImageBase64,
					},
				},
				map[string]any{
					"type":  "document",
					"title": e2eDocumentFilename,
					"source": map[string]any{
						"type":       "base64",
						"media_type": "application/pdf",
						"data":       base64.StdEncoding.EncodeToString(e2eDocumentBytes),
					},
				},
			},
		}},
		"tools": []any{map[string]any{
			"name":        "Read",
			"description": "Read a local file",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		}},
		"tool_choice": map[string]any{
			"type":                      "tool",
			"name":                      "Read",
			"disable_parallel_tool_use": true,
		},
	})
	if err != nil {
		report.Error = fmt.Sprintf("encode gateway request: %v", err)
		return writeEndToEndReport(report)
	}
	body := string(rawBody)
	messageResponse, err := endToEndPost(client, report.BaseURL+"/v1/messages", token, body)
	if err != nil {
		report.Error = err.Error()
		return writeEndToEndReport(report)
	}
	var message struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(messageResponse, &message); err != nil {
		report.Error = fmt.Sprintf("decode gateway message: %v", err)
		return writeEndToEndReport(report)
	}
	report.ResponseModel = message.Model
	report.StopReason = message.StopReason
	for _, block := range message.Content {
		if block.Type == "tool_use" {
			report.ToolName = block.Name
			report.ToolPath, _ = block.Input["path"].(string)
		}
	}

	countResponse, estimated, err := endToEndCount(client, report.BaseURL, token, body)
	if err != nil {
		report.Error = err.Error()
		return writeEndToEndReport(report)
	}
	report.CountTokens = countResponse
	report.CountWasEstimate = estimated
	return writeEndToEndReport(report)
}

func validateResponsesE2EAttachments(body map[string]any) error {
	var imageFound bool
	var documentFound bool
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			switch value["type"] {
			case "input_image":
				if value["image_url"] == "data:image/png;base64,"+e2eImageBase64 {
					imageFound = true
				}
			case "input_file":
				if value["filename"] == e2eDocumentFilename &&
					value["file_data"] == "data:application/pdf;base64,"+
						base64.StdEncoding.EncodeToString(e2eDocumentBytes) {
					documentFound = true
				}
			}
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(body["input"])
	if !imageFound || !documentFound {
		return fmt.Errorf(
			"multimodal input missing: image=%t document=%t body=%#v",
			imageFound,
			documentFound,
			body["input"],
		)
	}
	return nil
}

func validateSubscriptionE2EAttachments(body map[string]any) error {
	var imageFound bool
	var documentTextFound bool
	var nativeFileFound bool
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			switch value["type"] {
			case "input_image":
				imageFound = imageFound ||
					value["image_url"] == "data:image/png;base64,"+e2eImageBase64
			case "input_file":
				nativeFileFound = true
			case "input_text":
				text, _ := value["text"].(string)
				documentTextFound = documentTextFound ||
					strings.Contains(text, "MACAZ_E2E_DOCUMENT") &&
						strings.Contains(text, e2eDocumentFilename)
			}
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(body["input"])
	if !imageFound || !documentTextFound || nativeFileFound {
		return fmt.Errorf(
			"subscription multimodal fallback invalid: image=%t document_text=%t native_file=%t body=%#v",
			imageFound,
			documentTextFound,
			nativeFileFound,
			body["input"],
		)
	}
	return nil
}

func validateCodexE2EAttachments(params map[string]any) error {
	input, _ := params["input"].([]any)
	var imageFound bool
	var documentFound bool
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "text":
			text, _ := item["text"].(string)
			documentFound = strings.Contains(text, "MACAZ_E2E_DOCUMENT") &&
				strings.Contains(text, e2eDocumentFilename)
		case "localImage":
			path, _ := item["path"].(string)
			raw, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read Codex local image: %w", err)
			}
			expected, err := base64.StdEncoding.DecodeString(e2eImageBase64)
			if err != nil {
				return err
			}
			imageFound = bytes.Equal(raw, expected)
		}
	}
	if !imageFound || !documentFound {
		return fmt.Errorf(
			"Codex multimodal input missing: image=%t document=%t input=%#v",
			imageFound,
			documentFound,
			input,
		)
	}
	return nil
}

func makeE2EPDF(text string) []byte {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`(`, `\(`,
		`)`, `\)`,
	).Replace(text)
	stream := "BT\n/F1 12 Tf\n72 720 Td\n(" + escaped + ") Tj\nET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n", len(objects)+1)
	output.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(
		&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1,
		xref,
	)
	return output.Bytes()
}

func validateOpenCodeE2EAttachments(args []string) error {
	var paths []string
	for index := 0; index < len(args); index++ {
		if args[index] == "--file" && index+1 < len(args) {
			paths = append(paths, args[index+1])
			index++
		}
	}
	var imageFound bool
	var documentFound bool
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read OpenCode attachment %q: %w", path, err)
		}
		switch filepath.Ext(path) {
		case ".png":
			expected, err := base64.StdEncoding.DecodeString(e2eImageBase64)
			if err != nil {
				return err
			}
			imageFound = bytes.Equal(raw, expected)
		case ".pdf":
			documentFound = bytes.Equal(raw, e2eDocumentBytes)
		}
	}
	if !imageFound || !documentFound {
		return fmt.Errorf(
			"OpenCode --file attachments missing: image=%t document=%t args=%#v",
			imageFound,
			documentFound,
			args,
		)
	}
	return nil
}

func endToEndPost(client *http.Client, endpoint, token, body string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("x-api-key", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call gateway: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("call gateway: HTTP %d: %s", response.StatusCode, raw)
	}
	return raw, nil
}

func endToEndCount(client *http.Client, baseURL, token, body string) (int, bool, error) {
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages/count_tokens", strings.NewReader(body))
	if err != nil {
		return 0, false, err
	}
	request.Header.Set("x-api-key", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, false, fmt.Errorf("count gateway tokens: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, false, err
	}
	if response.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("count gateway tokens: HTTP %d: %s", response.StatusCode, raw)
	}
	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, false, err
	}
	return payload.InputTokens, response.Header.Get("X-Macaz-Token-Count-Estimated") == "true", nil
}

func writeEndToEndFailure(message string) int {
	report := endToEndReport{Error: message}
	if code := writeEndToEndReport(report); code != 0 {
		return code
	}
	return 2
}

func writeEndToEndReport(report endToEndReport) int {
	path := os.Getenv("MACAZ_APP_E2E_REPORT")
	if path == "" {
		_, _ = fmt.Fprintln(os.Stderr, report.Error)
		if report.Error != "" {
			return 2
		}
		return 0
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return 2
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return 2
	}
	if report.Error != "" {
		return 2
	}
	return 0
}
