package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestResponsesInputPreservesMixedOrder(t *testing.T) {
	req := &Request{
		Model: "sonnet",
		Messages: []Message{
			{
				Role: "assistant",
				Content: json.RawMessage(`[
					{"type":"text","text":"before"},
					{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"a.go"}},
					{"type":"text","text":"after"}
				]`),
			},
			{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"},
					{"type":"text","text":"continue"}
				]`),
			},
		},
		Tools: []Tool{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
	}
	got, err := ToResponses(req, "gpt-5.6", "high")
	if err != nil {
		t.Fatal(err)
	}
	input := got.Body["input"].([]any)
	types := make([]string, 0, len(input))
	for _, item := range input {
		types = append(types, item.(map[string]any)["type"].(string))
	}
	want := []string{"message", "function_call", "message", "function_call_output", "message"}
	if len(types) != len(want) {
		t.Fatalf("types = %#v, want %#v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %#v, want %#v", types, want)
		}
	}
}

func TestToolResultPreservesImagesAndDocuments(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("file"))
	req := &Request{
		Messages: []Message{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"call_1","content":[
					{"type":"text","text":"screenshot"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}},
					{"type":"document","title":"report.pdf","source":{"type":"url","url":"https://example.test/report.pdf"}}
				]}
			]`),
		}},
	}
	translated, err := ToResponses(req, "test-model", "high")
	if err != nil {
		t.Fatal(err)
	}
	input := translated.Body["input"].([]any)
	output := input[0].(map[string]any)["output"].([]map[string]any)
	if len(output) != 3 {
		t.Fatalf("output = %#v", output)
	}
	if output[1]["type"] != "input_image" || output[2]["file_url"] != "https://example.test/report.pdf" {
		t.Fatalf("output = %#v", output)
	}

	transcript, attachments, err := TranscriptWithAttachments(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 2 || attachments[0].Kind != "image" || attachments[1].Kind != "document" {
		t.Fatalf("attachments = %#v", attachments)
	}
	if strings.Contains(transcript, data) {
		t.Fatal("transcript must not inline base64 tool-result data")
	}
}

func TestToolResultPreservesToolReferencesAndSearchResults(t *testing.T) {
	req := &Request{
		Messages: []Message{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"call_search","content":[
					{"type":"tool_reference","tool_name":"mcp__news__search"},
					{"type":"search_result","source":"https://example.test/news","title":"News","content":[{"type":"text","text":"result"}]}
				]}
			]`),
		}},
	}
	translated, err := ToResponses(req, "test-model", "high")
	if err != nil {
		t.Fatal(err)
	}
	input := translated.Body["input"].([]any)
	output := input[0].(map[string]any)["output"].([]map[string]any)
	if len(output) != 2 {
		t.Fatalf("output = %#v", output)
	}
	if text := output[0]["text"].(string); !strings.Contains(text, `"tool_name":"mcp__news__search"`) {
		t.Fatalf("tool reference = %q", text)
	}
	if text := output[1]["text"].(string); !strings.Contains(text, `"type":"search_result"`) ||
		!strings.Contains(text, `"title":"News"`) {
		t.Fatalf("search result = %q", text)
	}

	transcript, attachments, err := TranscriptWithAttachments(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 0 ||
		!strings.Contains(transcript, `"tool_name":"mcp__news__search"`) ||
		!strings.Contains(transcript, `"type":"search_result"`) {
		t.Fatalf("transcript = %q, attachments = %#v", transcript, attachments)
	}
}

func TestToolNameRoundTrip(t *testing.T) {
	names := NewToolNames([]Tool{{Name: "mcp/tool with spaces"}})
	provider := names.Provider("mcp/tool with spaces")
	if provider == "mcp/tool with spaces" {
		t.Fatal("invalid provider name was not mapped")
	}
	if got := names.Client(provider); got != "mcp/tool with spaces" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestEffort(t *testing.T) {
	req := &Request{
		Thinking:     json.RawMessage(`{"type":"adaptive"}`),
		OutputConfig: json.RawMessage(`{"effort":"xhigh"}`),
	}
	if got := Effort(req, "medium"); got != "xhigh" {
		t.Fatalf("effort = %q", got)
	}
}

func TestCompactionEffortIsCappedAtLow(t *testing.T) {
	req := &Request{
		System:       json.RawMessage(`"You are a helpful AI assistant tasked with summarizing conversations."`),
		OutputConfig: json.RawMessage(`{"effort":"max"}`),
	}
	if got := Effort(req, "high"); got != "low" {
		t.Fatalf("compaction effort = %q", got)
	}
	req.OutputConfig = json.RawMessage(`{"effort":"minimal"}`)
	if got := Effort(req, "high"); got != "minimal" {
		t.Fatalf("low explicit compaction effort was raised to %q", got)
	}
}

func TestToolChoiceFiltersToolsAndDisablesParallelCalls(t *testing.T) {
	req := &Request{
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"use a tool"`)}},
		Tools: []Tool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
			{Name: "Bash", InputSchema: map[string]any{"type": "object"}},
		},
		ToolChoice: json.RawMessage(`{
			"type":"tool",
			"name":"Read",
			"disable_parallel_tool_use":true
		}`),
	}
	translated, err := ToResponses(req, "gpt-test", "high")
	if err != nil {
		t.Fatal(err)
	}
	tools := translated.Body["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "Read" {
		t.Fatalf("tools = %#v", tools)
	}
	if translated.Body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", translated.Body["parallel_tool_calls"])
	}
	choice := translated.Body["tool_choice"].(map[string]any)
	if choice["name"] != "Read" {
		t.Fatalf("tool_choice = %#v", choice)
	}
}

func TestReadToolAddsOffsetGuidanceWithoutMutatingClientSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string"},
			"offset":    map[string]any{"type": "integer", "description": "old"},
			"limit":     map[string]any{"type": "integer"},
		},
	}
	req := &Request{
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"read"`)}},
		Tools:    []Tool{{Name: "Read", Description: "Read a file", InputSchema: schema}},
	}
	translated, err := ToResponses(req, "gpt-test", "medium")
	if err != nil {
		t.Fatal(err)
	}
	tool := translated.Body["tools"].([]map[string]any)[0]
	if !strings.Contains(tool["description"].(string), "Codex Read guidance") {
		t.Fatalf("description = %q", tool["description"])
	}
	parameters := tool["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	if !strings.Contains(properties["offset"].(map[string]any)["description"].(string), "continuation") {
		t.Fatalf("offset schema = %#v", properties["offset"])
	}
	original := schema["properties"].(map[string]any)["offset"].(map[string]any)["description"]
	if original != "old" {
		t.Fatalf("client schema was mutated: %#v", schema)
	}
}

func TestToolChoiceNoneHidesEveryTool(t *testing.T) {
	req := &Request{
		Messages:   []Message{{Role: "user", Content: json.RawMessage(`"no tools"`)}},
		Tools:      []Tool{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: json.RawMessage(`{"type":"none"}`),
	}
	translated, err := ToResponses(req, "gpt-test", "high")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := translated.Body["tools"]; exists {
		t.Fatalf("tools should be absent: %#v", translated.Body)
	}
	if translated.Body["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %#v", translated.Body["tool_choice"])
	}
}

func TestToolChoiceRejectsUnknownTool(t *testing.T) {
	_, err := ApplyToolPolicy(
		[]Tool{{Name: "Read"}},
		ToolPolicy{Type: "tool", Name: "Missing"},
	)
	if err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestToolChoiceAnyRequiresAnAvailableTool(t *testing.T) {
	req := &Request{
		Model:      "gpt-test",
		Messages:   []Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
	}
	if _, err := ToResponses(req, "gpt-test", "medium"); err == nil {
		t.Fatal("expected tool_choice any without tools to fail")
	}
}

func TestResponsesPreservesSessionCacheTierAndOpenAIReasoning(t *testing.T) {
	rawSignature := make([]byte, 73)
	rawSignature[0] = 0x80
	signature := base64.RawURLEncoding.EncodeToString(rawSignature)
	req := &Request{
		PromptCacheKey: "macaz_session_cache",
		Speed:          "fast",
		Thinking:       json.RawMessage(`{"type":"adaptive"}`),
		Messages: []Message{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"private","signature":` + strconv.Quote(signature) + `}]`)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	translated, err := ToResponses(req, "gpt-test", "high")
	if err != nil {
		t.Fatal(err)
	}
	if translated.Body["prompt_cache_key"] != req.PromptCacheKey || translated.Body["service_tier"] != "priority" {
		t.Fatalf("routing fields = %#v", translated.Body)
	}
	include, _ := translated.Body["include"].([]string)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", translated.Body["include"])
	}
	input := translated.Body["input"].([]any)
	reasoning := input[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["encrypted_content"] != signature {
		t.Fatalf("reasoning = %#v", reasoning)
	}
}

func TestEstimateInputTokensCountsModelVisibleContentNotMetadata(t *testing.T) {
	short := &Request{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
		Metadata: map[string]any{"diagnostic": strings.Repeat("x", 1<<20)},
	}
	withoutMetadata := *short
	withoutMetadata.Metadata = nil
	if got, want := EstimateInputTokens(short), EstimateInputTokens(&withoutMetadata); got != want {
		t.Fatalf("metadata changed estimate: got %d, want %d", got, want)
	}
	longer := withoutMetadata
	longer.Messages = []Message{{Role: "user", Content: json.RawMessage(strconv.Quote(strings.Repeat("visible prompt ", 1000)))}}
	if EstimateInputTokens(&longer) <= EstimateInputTokens(&withoutMetadata) {
		t.Fatal("visible prompt growth did not increase the estimate")
	}
}

func TestEstimateInputTokensDoesNotCountImageBase64AsText(t *testing.T) {
	content, err := json.Marshal([]Block{{
		Type: "image",
		Source: &Source{
			Type:      "base64",
			MediaType: "image/png",
			Data:      base64.StdEncoding.EncodeToString(make([]byte, 1<<20)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	count := EstimateInputTokens(&Request{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: content}},
	})
	if count < 1_500 || count > 3_000 {
		t.Fatalf("image estimate = %d, want a bounded vision allowance", count)
	}
}
