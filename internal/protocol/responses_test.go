package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFromResponsesMapsCodexConversationToolsAndAttachments(t *testing.T) {
	input := ResponsesInput{
		Model:        "macaz-model",
		Instructions: json.RawMessage(`"base instructions"`),
		Stream:       true,
		Reasoning:    json.RawMessage(`{"effort":"xhigh"}`),
		Text:         json.RawMessage(`{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}`),
		Tools: json.RawMessage(`[{
			"type":"function","name":"Read","description":"Read a file",
			"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
		}]`),
		ToolChoice: json.RawMessage(`"required"`),
		Input: json.RawMessage(`[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"developer policy"}]},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"inspect this"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
				{"type":"input_file","filename":"note.txt","file_data":"data:text/plain;base64,aGVsbG8="}
			]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will read it."}]},
			{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"README.md\"}"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"contents"}]},
			{"type":"reasoning","id":"reasoning_1","summary":[]}
		]`),
	}
	req, err := FromResponses(input)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != input.Model || !req.Stream || len(req.Tools) != 1 || req.Tools[0].Name != "Read" {
		t.Fatalf("request = %#v", req)
	}
	system, err := SystemText(req.System)
	if err != nil {
		t.Fatal(err)
	}
	if system != "base instructions\n\ndeveloper policy" {
		t.Fatalf("system = %q", system)
	}
	if string(req.ToolChoice) != `{"type":"any"}` {
		t.Fatalf("tool choice = %s", req.ToolChoice)
	}
	if !strings.Contains(string(req.OutputConfig), `"effort":"max"`) {
		t.Fatalf("output config = %s", req.OutputConfig)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %#v", req.Messages)
	}
	user, err := DecodeBlocks(req.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(user) != 3 || user[1].Type != "image" || user[2].Type != "document" || user[2].Title != "note.txt" {
		t.Fatalf("user blocks = %#v", user)
	}
	assistant, err := DecodeBlocks(req.Messages[1].Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(assistant) != 2 || assistant[1].Type != "tool_use" || assistant[1].ID != "call_1" || assistant[1].Name != "Read" {
		t.Fatalf("assistant blocks = %#v", assistant)
	}
	toolResult, err := DecodeBlocks(req.Messages[2].Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolResult) != 1 || toolResult[0].Type != "tool_result" || toolResult[0].ToolUseID != "call_1" {
		t.Fatalf("tool result = %#v", toolResult)
	}
}

func TestResponsesRoundTripPreservesFunctionCalls(t *testing.T) {
	input := ResponsesInput{
		Model: "public-model",
		Tools: json.RawMessage(`[{"type":"function","name":"shell","parameters":{"type":"object"}}]`),
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run it"}]},
			{"type":"function_call","call_id":"call_shell","name":"shell","arguments":"{\"command\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_shell","output":"/tmp"}
		]`),
	}
	req, err := FromResponses(input)
	if err != nil {
		t.Fatal(err)
	}
	translated, err := ToResponses(req, "upstream-model", "high")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(translated.Body["input"])
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{`"type":"function_call"`, `"call_id":"call_shell"`, `"name":"shell"`, `"type":"function_call_output"`, `"output":"/tmp"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("round trip is missing %s: %s", required, text)
		}
	}
}

func TestFromResponsesRejectsProviderSideToolsAndFileIDs(t *testing.T) {
	if _, err := FromResponses(ResponsesInput{
		Tools: json.RawMessage(`[{"type":"web_search_preview"}]`),
		Input: json.RawMessage(`"hi"`),
	}); err == nil {
		t.Fatal("provider-side web search tool was silently accepted")
	}
	if _, err := FromResponses(ResponsesInput{
		Input: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]`),
	}); err == nil {
		t.Fatal("unresolvable provider file id was silently accepted")
	}
}

func TestFromResponsesMapsCustomAndNamespaceToolHistory(t *testing.T) {
	input := ResponsesInput{
		Tools: json.RawMessage(`[
			{"type":"custom","name":"apply_patch","description":"Apply a patch"},
			{"type":"namespace","name":"mcp__calendar__","description":"Calendar tools","tools":[
				{"type":"function","name":"list_events","description":"List events","parameters":{"type":"object"}}
			]}
		]`),
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"update and list"}]},
			{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call_patch","output":"done"},
			{"type":"function_call","call_id":"call_list","namespace":"mcp__calendar__","name":"list_events","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_list","output":"[]"}
		]`),
	}
	req, err := FromResponses(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 || req.Tools[0].ClientType != "custom" || req.Tools[1].ClientType != "namespace" ||
		req.Tools[1].ClientNamespace != "mcp__calendar__" || req.Tools[1].ClientName != "list_events" {
		t.Fatalf("tools = %#v", req.Tools)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages = %#v", req.Messages)
	}
	patch, err := DecodeBlocks(req.Messages[1].Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch) != 1 || patch[0].Name != "apply_patch" || string(patch[0].Input) != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom call = %#v", patch)
	}
	namespaced, err := DecodeBlocks(req.Messages[3].Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(namespaced) != 1 || namespaced[0].Name != "mcp__calendar____list_events" {
		t.Fatalf("namespace call = %#v", namespaced)
	}
}

func TestPrepareNativeToolRequestSanitizesDefinitionsAndHistory(t *testing.T) {
	longName := "mcp/tool name with spaces and a deliberately very long suffix that exceeds provider limits"
	content, _ := json.Marshal([]Block{{Type: "tool_use", ID: "call_1", Name: longName, Input: json.RawMessage(`{}`)}})
	request := &Request{
		Messages:   []Message{{Role: "assistant", Content: content}},
		Tools:      []Tool{{Name: longName, InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"` + longName + `"}`),
	}
	prepared, names, err := PrepareNativeToolRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	providerName := names.Provider(longName)
	if providerName == longName || len(providerName) > 64 || prepared.Tools[0].Name != providerName || !validFunctionName.MatchString(providerName) {
		t.Fatalf("provider tool name = %q, request = %#v", providerName, prepared)
	}
	history, err := DecodeBlocks(prepared.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Name != providerName || !strings.Contains(string(prepared.ToolChoice), providerName) {
		t.Fatalf("prepared request = %#v, history = %#v", prepared, history)
	}
}
