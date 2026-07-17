package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

type fakeProvider struct{}

func (fakeProvider) Name() string                { return "fake" }
func (fakeProvider) Check(context.Context) error { return nil }
func (fakeProvider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "fake-model", Default: true, Efforts: []string{"high"}}}, nil
}
func (fakeProvider) CountTokens(context.Context, *protocol.Request) (int, bool, error) {
	return 42, false, nil
}
func (fakeProvider) Generate(_ context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	block := protocol.Block{Type: "text", Text: "hello"}
	if req.Stream {
		_ = emit(protocol.Event{Kind: protocol.EventBlockStart, Index: 0, Block: protocol.Block{Type: "text"}})
		_ = emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: 0, DeltaType: "text_delta", Delta: "hello"})
		_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: 0})
	}
	return protocol.Result{
		ID:         "msg_fake",
		Model:      "fake-model",
		Blocks:     []protocol.Block{block},
		StopReason: "end_turn",
		Usage:      protocol.Usage{InputTokens: 2, OutputTokens: 1},
	}, nil
}

func TestMessagesAndCountTokens(t *testing.T) {
	server, err := New(config.Default(), fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close(context.Background())
	if _, err := server.PrimeModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	request := `{"model":"fake-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp := doRequest(t, server, "/v1/messages", request)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var message map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	if message["stop_reason"] != "end_turn" {
		t.Fatalf("message = %#v", message)
	}

	resp = doRequest(t, server, "/v1/messages/count_tokens", request)
	defer resp.Body.Close()
	var count map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&count); err != nil {
		t.Fatal(err)
	}
	if count["input_tokens"] != float64(42) {
		t.Fatalf("count = %#v", count)
	}
}

func TestStreamingOrder(t *testing.T) {
	server, err := New(config.Default(), fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close(context.Background())
	if _, err := server.PrimeModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	resp := doRequest(t, server, "/v1/messages", `{"model":"fake-model","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	got := strings.Join(events, ",")
	want := "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func TestUnknownModelIsRejectedBeforeProviderCall(t *testing.T) {
	server, err := New(config.Default(), fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close(context.Background())
	if _, err := server.PrimeModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	resp := doRequest(t, server, "/v1/messages", `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestCatalogKeepsPublicDefaultSeparateFromUpstreamAlias(t *testing.T) {
	cfg := config.Default()
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = "fake-model"
	}
	server, err := New(cfg, fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := server.PrimeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Default == "" || catalog.Default == catalog.DefaultUpstream {
		t.Fatalf("catalog = %#v", catalog)
	}
	if catalog.DefaultUpstream != "fake-model" {
		t.Fatalf("upstream alias = %q", catalog.DefaultUpstream)
	}
	if catalog.UpstreamByID[catalog.Default] != "fake-model" {
		t.Fatalf("upstream mapping = %#v", catalog.UpstreamByID)
	}
	if resolved, ok := server.resolveModel(catalog.DefaultUpstream); !ok || resolved != "fake-model" {
		t.Fatalf("resolve upstream alias = %q, %v", resolved, ok)
	}
}

func TestRecursiveSubagentLosesOnlyAgentTool(t *testing.T) {
	req := &protocol.Request{
		System: json.RawMessage(`"x-anthropic-billing-header: cc_is_subagent=true;"`),
		Tools: []protocol.Tool{
			{Name: "Agent"},
			{Name: "Skill"},
			{Name: "Bash"},
		},
	}
	restrictRecursiveSubagentTools(req)
	if len(req.Tools) != 2 || req.Tools[0].Name != "Skill" || req.Tools[1].Name != "Bash" {
		t.Fatalf("subagent tools = %#v", req.Tools)
	}

	main := &protocol.Request{
		System: json.RawMessage(`"Use subagents when appropriate."`),
		Tools:  []protocol.Tool{{Name: "Agent"}, {Name: "Bash"}},
	}
	restrictRecursiveSubagentTools(main)
	if len(main.Tools) != 2 {
		t.Fatalf("main-agent tools were restricted: %#v", main.Tools)
	}
}

func doRequest(t *testing.T, server *Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL()+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", server.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
