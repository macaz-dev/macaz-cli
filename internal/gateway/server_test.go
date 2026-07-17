package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
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

type capturingProvider struct {
	fakeProvider
	request *protocol.Request
}

type toolCallProvider struct{ fakeProvider }

type collidingModelProvider struct{ fakeProvider }

func (collidingModelProvider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "vendor/model", Default: true}, {ID: "vendor-model"}}, nil
}

func (toolCallProvider) Generate(_ context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	if len(req.Tools) == 0 {
		return protocol.Result{}, errors.New("test request contained no tools")
	}
	tool := req.Tools[0]
	block := protocol.Block{Type: "tool_use", ID: "call_namespaced", Name: tool.Name, Input: json.RawMessage(`{}`)}
	if req.Stream {
		_ = emit(protocol.Event{Kind: protocol.EventBlockStart, Index: 0, Block: block})
		_ = emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: 0, DeltaType: "input_json_delta", Delta: `{}`})
		_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: 0})
	}
	return protocol.Result{ID: "resp_tool", Model: req.Model, Blocks: []protocol.Block{block}, StopReason: "tool_use"}, nil
}

func (p *capturingProvider) Generate(ctx context.Context, req *protocol.Request, emit protocol.EmitFunc) (protocol.Result, error) {
	copy := *req
	p.request = &copy
	return p.fakeProvider.Generate(ctx, req, emit)
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

func TestCodexResponsesMapsRequestsAndStreamsResponsesEvents(t *testing.T) {
	upstream := &capturingProvider{}
	server, err := NewForClient(config.Default(), upstream, config.ClientCodex)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close(context.Background())
	catalog, err := server.PrimeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.IDs) != 1 || catalog.Default != "macaz-fake-model" || strings.HasPrefix(catalog.Default, "claude-") {
		t.Fatalf("Codex catalog = %#v", catalog)
	}

	body := `{
		"model":` + strconv.Quote(catalog.Default) + `,
		"instructions":"act as a coding agent",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}],
		"stream":false
	}`
	resp := doRequest(t, server, "/v1/responses", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["object"] != "response" || payload["model"] != catalog.Default || payload["output_text"] != "hello" {
		t.Fatalf("response = %#v", payload)
	}
	if upstream.request == nil || upstream.request.Model != "fake-model" || len(upstream.request.Tools) != 1 || upstream.request.Tools[0].Name != "Read" {
		t.Fatalf("mapped request = %#v", upstream.request)
	}

	body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
	resp = doRequest(t, server, "/v1/responses", body)
	defer resp.Body.Close()
	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if line := scanner.Text(); strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	got := strings.Join(events, ",")
	want := "response.created,response.output_item.added,response.content_part.added,response.output_text.delta,response.output_text.done,response.content_part.done,response.output_item.done,response.completed"
	if got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func TestPublicModelIDsStayReadableAndResolveRareCollisions(t *testing.T) {
	server, err := NewForClient(config.Default(), collidingModelProvider{}, config.ClientCodex)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := server.PrimeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"macaz-vendor-model", "macaz-vendor-model-2"}
	if len(catalog.IDs) != len(want) || catalog.IDs[0] != want[0] || catalog.IDs[1] != want[1] {
		t.Fatalf("public IDs = %#v, want %#v", catalog.IDs, want)
	}
	if catalog.UpstreamByID[want[0]] != "vendor/model" || catalog.UpstreamByID[want[1]] != "vendor-model" {
		t.Fatalf("upstream mapping = %#v", catalog.UpstreamByID)
	}
}

func TestCodexNamespaceToolRoundTripsThroughResponsesStream(t *testing.T) {
	server, err := NewForClient(config.Default(), toolCallProvider{}, config.ClientCodex)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close(context.Background())
	catalog, err := server.PrimeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := `{
		"model":` + strconv.Quote(catalog.Default) + `,
		"input":"list agents",
		"tools":[{"type":"namespace","name":"collaboration","description":"Agent tools","tools":[
			{"type":"function","name":"list_agents","description":"List agents","parameters":{"type":"object"}}
		]}],
		"stream":true
	}`
	resp := doRequest(t, server, "/v1/responses", body)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	stream := string(raw)
	for _, required := range []string{
		`"type":"function_call"`, `"call_id":"call_namespaced"`,
		`"namespace":"collaboration"`, `"name":"list_agents"`,
	} {
		if !strings.Contains(stream, required) {
			t.Fatalf("namespace stream is missing %s:\n%s", required, stream)
		}
	}
	if strings.Contains(stream, `"name":"collaboration__list_agents"`) {
		t.Fatalf("internal flattened tool name leaked to Codex:\n%s", stream)
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
