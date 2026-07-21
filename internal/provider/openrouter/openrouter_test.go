package openrouter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/testmedia"
)

func TestCheckModelsAndGenerate(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-openrouter-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-openrouter-key" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/key":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"label": "test"}})
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{
					"id":             "test/tool-model",
					"name":           "Tool Model",
					"description":    "contract model",
					"created":        1_700_000_000,
					"context_length": 200_000,
					"architecture": map[string]any{
						"input_modalities":  []string{"text", "image", "file"},
						"output_modalities": []string{"text"},
					},
					"top_provider": map[string]any{"max_completion_tokens": 32_000},
					"supported_parameters": []string{
						"tools", "tool_choice", "reasoning_effort", "structured_outputs",
					},
					"reasoning": map[string]any{"supported_efforts": []string{"low", "high"}},
				},
				map[string]any{
					"id": "test/no-tools",
					"architecture": map[string]any{
						"input_modalities":  []string{"text"},
						"output_modalities": []string{"text"},
					},
					"supported_parameters": []string{"temperature"},
				},
			}})
		case "/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != "test/tool-model" {
				t.Fatalf("model = %#v", body["model"])
			}
			if body["session_id"] != "macaz_session_agent" {
				t.Fatalf("session_id = %#v", body["session_id"])
			}
			if tools, _ := body["tools"].([]any); len(tools) != 1 {
				t.Fatalf("tools = %#v", body["tools"])
			}
			image, document := responsesAttachments(body)
			if image != "data:image/png;base64,aW1hZ2U=" {
				t.Fatalf("image input = %q", image)
			}
			if document != "data:application/pdf;base64,UERGLWRvY3VtZW50" {
				t.Fatalf("document input = %q", document)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": OPENROUTER PROCESSING\n\n")
			fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_or\",\"model\":\"test/tool-model\"}}\n\n")
			fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n")
			fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"hello\"}\n\n")
			fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n")
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_or\",\"model\":\"test/tool-model\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Provider = config.ProviderOpenRouterAPI
	cfg.OpenRouterBaseURL = server.URL
	cfg.OpenRouterModel = "test/tool-model"
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = cfg.OpenRouterModel
	}
	upstream := New(cfg)
	if err := upstream.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, err := upstream.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || !models[0].Default || !models[0].ToolCall || !models[0].Attachment {
		t.Fatalf("models = %#v", models)
	}
	result, err := upstream.Generate(context.Background(), &protocol.Request{
		Model:          "sonnet",
		MaxTokens:      100,
		PromptCacheKey: "macaz_session_agent",
		Messages: []protocol.Message{{
			Role: "user",
			Content: mustContent(t,
				protocol.Block{Type: "text", Text: "hi"},
				protocol.Block{
					Type: "image",
					Source: &protocol.Source{
						Type:      "base64",
						MediaType: "image/png",
						Data:      "aW1hZ2U=",
					},
				},
				protocol.Block{
					Type:  "document",
					Title: "contract.pdf",
					Source: &protocol.Source{
						Type:      "base64",
						MediaType: "application/pdf",
						Data:      "UERGLWRvY3VtZW50",
					},
				},
			),
		}},
		Tools: []protocol.Tool{{
			Name:        "Read",
			Description: "Read a file",
			InputSchema: map[string]any{"type": "object"},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blocks) != 1 || result.Blocks[0].Text != "hello" {
		t.Fatalf("result = %#v", result)
	}
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}

func TestProviderErrorPreservesRetryAfter(t *testing.T) {
	err := providerError(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"7"}}, []byte(`{"error":{"message":"slow down"}}`))
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if httpErr.RetryAfter != 7*time.Second {
		t.Fatalf("retry after = %s", httpErr.RetryAfter)
	}
}

func TestProviderErrorMapsContextOverflow(t *testing.T) {
	err := providerError(http.StatusBadRequest, nil, []byte(`{"error":{"message":"maximum context length exceeded"}}`))
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if httpErr.Status != http.StatusRequestEntityTooLarge || httpErr.Type != "request_too_large" {
		t.Fatalf("HTTP error = %#v", httpErr)
	}
}

func TestGenerateRejectsTruncatedStream(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-openrouter-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.OpenRouterBaseURL = server.URL
	cfg.OpenRouterModel = "test/tool-model"
	upstream := New(cfg)
	_, err := upstream.Generate(context.Background(), &protocol.Request{
		Model:     "test/tool-model",
		MaxTokens: 32,
		Messages:  []protocol.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "without a terminal response event") {
		t.Fatalf("error = %v", err)
	}
}

func TestLiveOpenRouterIntegration(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("MACAZ_OPENROUTER_INTEGRATION_MODEL"))
	if model == "" {
		t.Skip("set MACAZ_OPENROUTER_INTEGRATION_MODEL to run the live OpenRouter smoke test")
	}
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenRouterAPI
	cfg.OpenRouterModel = model
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = model
	}
	upstream := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := upstream.Check(ctx); err != nil {
		t.Fatal(err)
	}
	models, err := upstream.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := modelByID(models, model)
	if !ok {
		t.Fatalf("configured model %q is absent from live catalog", model)
	}

	t.Run("text", func(t *testing.T) {
		generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer generateCancel()
		result, err := upstream.Generate(generateCtx, &protocol.Request{
			Model:     model,
			MaxTokens: 64,
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Reply with exactly MACAZ_CONTEXT_OK."`),
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range result.Blocks {
			if block.Type == "text" && strings.Contains(block.Text, "MACAZ_CONTEXT_OK") {
				return
			}
		}
		t.Fatalf("result = %#v", result)
	})

	t.Run("forced_tool", func(t *testing.T) {
		generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer generateCancel()
		result, err := upstream.Generate(generateCtx, &protocol.Request{
			Model:     model,
			MaxTokens: 128,
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Use the Read tool for /tmp/macaz-live-provider-test and do not answer with text."`),
			}},
			Tools: []protocol.Tool{{
				Name:        "Read",
				Description: "Read a local file",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			}},
			ToolChoice: json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range result.Blocks {
			if block.Type == "tool_use" && block.Name == "Read" && json.Valid(block.Input) {
				return
			}
		}
		t.Fatalf("result contains no valid Read tool call: %#v", result)
	})

	if hasInputModality(selected, "image") {
		t.Run("image", func(t *testing.T) {
			imageData, err := testmedia.TwoColorPNG()
			if err != nil {
				t.Fatal(err)
			}
			content := mustContent(t,
				protocol.Block{
					Type: "text",
					Text: "Inspect the image. Reply with the two visible half colors as lowercase words.",
				},
				protocol.Block{
					Type: "image",
					Source: &protocol.Source{
						Type:      "base64",
						MediaType: "image/png",
						Data:      base64.StdEncoding.EncodeToString(imageData),
					},
				},
			)
			generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer generateCancel()
			result, err := upstream.Generate(generateCtx, &protocol.Request{
				Model:     model,
				MaxTokens: 128,
				Messages:  []protocol.Message{{Role: "user", Content: content}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.ToLower(resultText(result))
			if !strings.Contains(text, "red") || !strings.Contains(text, "blue") {
				t.Fatalf("image response = %q, result = %#v", text, result)
			}
		})
	} else {
		t.Logf("model %q does not advertise image input; image smoke test skipped", model)
	}

	if hasInputModality(selected, "file") {
		t.Run("document", func(t *testing.T) {
			content := mustContent(t,
				protocol.Block{
					Type: "text",
					Text: "Read the attached PDF and reply with only its uppercase verification token.",
				},
				protocol.Block{
					Type:  "document",
					Title: "macaz-live-document.pdf",
					Source: &protocol.Source{
						Type:      "base64",
						MediaType: "application/pdf",
						Data: base64.StdEncoding.EncodeToString(
							testmedia.TextPDF("MACAZ_DOCUMENT_OK"),
						),
					},
				},
			)
			generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer generateCancel()
			result, err := upstream.Generate(generateCtx, &protocol.Request{
				Model:     model,
				MaxTokens: 128,
				Messages:  []protocol.Message{{Role: "user", Content: content}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(resultText(result), "MACAZ_DOCUMENT_OK") {
				t.Fatalf("document result = %#v", result)
			}
		})
	} else {
		t.Logf("model %q does not advertise file input; document smoke test skipped", model)
	}
}

func responsesAttachments(body map[string]any) (image, document string) {
	input, _ := body["input"].([]any)
	for _, item := range input {
		message, _ := item.(map[string]any)
		content, _ := message["content"].([]any)
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "input_image":
				image, _ = block["image_url"].(string)
			case "input_file":
				document, _ = block["file_data"].(string)
			}
		}
	}
	return image, document
}

func modelByID(models []provider.Model, id string) (provider.Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return provider.Model{}, false
}

func hasInputModality(model provider.Model, target string) bool {
	for _, modality := range model.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), target) {
			return true
		}
	}
	return false
}

func mustContent(t *testing.T, blocks ...protocol.Block) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
