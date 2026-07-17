package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

func TestProviderUsesNativeMessagesModelsAndTokenCount(t *testing.T) {
	var messagesCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "anthropic-test-key" || r.Header.Get("anthropic-version") != anthropicVersion {
			http.Error(w, "missing Anthropic authentication", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-test","display_name":"Claude Test","created_at":"2026-01-02T03:04:05Z"}],"has_more":false}`)
		case "/v1/messages/count_tokens":
			_, _ = io.WriteString(w, `{"input_tokens":17}`)
		case "/v1/messages":
			messagesCalls++
			var request protocol.Request
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if request.Model != "claude-test" || !request.Stream || len(request.Tools) != 1 {
				http.Error(w, fmt.Sprintf("unexpected request: %#v", request), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n"+
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"Read\",\"input\":{}}}\n\n"+
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}\n\n"+
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n"+
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-test-key")
	cfg := config.Default()
	cfg.Provider = config.ProviderAnthropicAPI
	cfg.AnthropicBaseURL = server.URL + "/v1"
	cfg.AnthropicModel = "claude-test"
	cfg.ModelMap = map[string]string{"default": "claude-test"}
	provider := New(cfg)
	if err := provider.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, err := provider.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "claude-test" || models[0].DisplayName != "Claude Test" {
		t.Fatalf("models = %#v, error = %v", models, err)
	}
	request := &protocol.Request{
		Model: "claude-test", MaxTokens: 100, Stream: true,
		Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"read"`)}},
		Tools:    []protocol.Tool{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
	}
	var events []protocol.Event
	result, err := provider.Generate(context.Background(), request, func(event protocol.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if messagesCalls != 1 || result.ID != "msg_test" || result.StopReason != "tool_use" || len(result.Blocks) != 1 {
		t.Fatalf("result = %#v, calls = %d", result, messagesCalls)
	}
	if string(result.Blocks[0].Input) != `{"path":"README.md"}` || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 5 {
		t.Fatalf("translated result = %#v", result)
	}
	if len(events) != 3 || events[1].DeltaType != "input_json_delta" {
		t.Fatalf("events = %#v", events)
	}
	count, estimated, err := provider.CountTokens(context.Background(), request)
	if err != nil || count != 17 || estimated {
		t.Fatalf("count = %d, estimated = %t, error = %v", count, estimated, err)
	}
}
