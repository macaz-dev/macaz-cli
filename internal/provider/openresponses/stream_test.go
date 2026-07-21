package openresponses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

func TestCollectorRejectsStreamWithoutTerminalEvent(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), false, nil)
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"partial\"}\n\n"
	if err := ReadSSE(bytes.NewBufferString(stream), collector.Handle); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Finalize(); err == nil || !strings.Contains(err.Error(), "without a terminal") {
		t.Fatalf("finalize error = %v", err)
	}
}

func TestCollectorRejectsInvalidToolArguments(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames([]protocol.Tool{{Name: "Read"}}), false, nil)
	if err := collector.Handle("response.output_item.added", []byte(`{
		"type":"response.output_item.added",
		"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}
	}`)); err != nil {
		t.Fatal(err)
	}
	err := collector.Handle("response.output_item.done", []byte(`{
		"type":"response.output_item.done",
		"item":{"id":"fc_1","type":"function_call","arguments":"{invalid"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "invalid or empty JSON") {
		t.Fatalf("tool argument error = %v", err)
	}
}

func TestCollectorFinalizesOnlyAfterTerminalEvent(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), false, nil)
	if err := collector.Handle("response.completed", []byte(`{
		"type":"response.completed",
		"response":{"id":"resp_ok","model":"gpt-test","status":"completed"}
	}`)); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "resp_ok" {
		t.Fatalf("result = %#v", result)
	}
}

func TestCollectorRepairsWhitespaceStalledReadCall(t *testing.T) {
	var events []protocol.Event
	collector := NewCollector("gpt-test", protocol.NewToolNames([]protocol.Tool{{Name: "Read"}}), true, func(event protocol.Event) error {
		events = append(events, event)
		return nil
	})
	if err := collector.Handle("response.output_item.added", []byte(`{
		"type":"response.output_item.added",
		"item":{"id":"fc_read","type":"function_call","call_id":"call_read","name":"Read"}
	}`)); err != nil {
		t.Fatal(err)
	}
	delta, err := json.Marshal(map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "fc_read",
		"delta":   `{"file_path":"README.md","pages":"","offset":1300000` + strings.Repeat(" ", readRepairTrailingWhitespace),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = collector.Handle("response.function_call_arguments.delta", delta)
	if !errors.Is(err, ErrToolCallReady) {
		t.Fatalf("repair signal = %v", err)
	}
	result, err := collector.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blocks) != 1 || string(result.Blocks[0].Input) != `{"file_path":"README.md"}` {
		t.Fatalf("repaired blocks = %#v", result.Blocks)
	}
	if !result.Usage.Estimated || result.Usage.OutputTokens == 0 {
		t.Fatalf("repaired usage = %#v", result.Usage)
	}
	if len(events) != 3 || events[1].Delta != `{"file_path":"README.md"}` || events[2].Kind != protocol.EventBlockStop {
		t.Fatalf("events = %#v", events)
	}
}

func TestCollectorDoesNotCloseParallelCallsToRepairRead(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames([]protocol.Tool{{Name: "Read"}, {Name: "Bash"}}), false, nil)
	for _, item := range []string{
		`{"type":"response.output_item.added","item":{"id":"fc_read","type":"function_call","call_id":"call_read","name":"Read"}}`,
		`{"type":"response.output_item.added","item":{"id":"fc_bash","type":"function_call","call_id":"call_bash","name":"Bash"}}`,
	} {
		if err := collector.Handle("response.output_item.added", []byte(item)); err != nil {
			t.Fatal(err)
		}
	}
	delta, err := json.Marshal(map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "fc_read",
		"delta":   `{"file_path":"README.md"` + strings.Repeat(" ", readRepairTrailingWhitespace),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.Handle("response.function_call_arguments.delta", delta); err != nil {
		t.Fatalf("parallel repair unexpectedly terminated the stream: %v", err)
	}
	if len(collector.open) != 2 || collector.terminal {
		t.Fatalf("collector state: open=%#v terminal=%t", collector.open, collector.terminal)
	}
}

func TestCollectorFailsClosedOnFailedResponse(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), false, nil)
	err := collector.Handle("response.failed", []byte(`{
		"type":"response.failed",
		"response":{
			"id":"resp_failed",
			"model":"gpt-test",
			"status":"failed",
			"error":{"code":"server_error","message":"upstream generation failed"}
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "upstream generation failed") {
		t.Fatalf("error = %v", err)
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || streamErr.Code != "server_error" {
		t.Fatalf("typed stream error = %#v", err)
	}
}

func TestCollectorTypesOpenRouterMidStreamErrorAndClosesBlocks(t *testing.T) {
	var events []protocol.Event
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), true, func(event protocol.Event) error {
		events = append(events, event)
		return nil
	})
	if err := collector.Handle("response.output_text.delta", []byte(`{
		"type":"response.output_text.delta","item_id":"m1","delta":"partial"
	}`)); err != nil {
		t.Fatal(err)
	}
	err := collector.Handle("error", []byte(`{
		"type":"error","error_type":"provider_overloaded",
		"error":{"code":503,"message":"provider is overloaded"}
	}`))
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || streamErr.Type != "provider_overloaded" || streamErr.Code != "503" {
		t.Fatalf("stream error = %#v", err)
	}
	if len(events) != 3 || events[2].Kind != protocol.EventBlockStop {
		t.Fatalf("events = %#v", events)
	}
}

func TestCollectorPreservesEncryptedReasoningForStatelessReplay(t *testing.T) {
	rawSignature := make([]byte, 73)
	rawSignature[0] = 0x80
	signature := base64.RawURLEncoding.EncodeToString(rawSignature)
	var events []protocol.Event
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), true, func(event protocol.Event) error {
		events = append(events, event)
		return nil
	})
	if err := collector.Handle("response.output_item.added", []byte(`{
		"type":"response.output_item.added",
		"item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+signature+`"}
	}`)); err != nil {
		t.Fatal(err)
	}
	if err := collector.Handle("response.output_item.done", []byte(`{
		"type":"response.output_item.done",
		"item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+signature+`","summary":[{"type":"summary_text","text":"summary"}]}
	}`)); err != nil {
		t.Fatal(err)
	}
	result := collector.Result()
	if len(result.Blocks) != 1 || result.Blocks[0].Signature != signature || result.Blocks[0].Thinking != "summary" {
		t.Fatalf("blocks = %#v", result.Blocks)
	}
	if len(events) != 3 || events[2].Block.Signature != signature {
		t.Fatalf("events = %#v", events)
	}
}

func TestCollectorIgnoresEmptyReasoningItem(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), false, nil)
	if err := collector.Handle("response.output_item.done", []byte(`{
		"type":"response.output_item.done",
		"item":{"id":"rs_empty","type":"reasoning","summary":[]}
	}`)); err != nil {
		t.Fatal(err)
	}
	if blocks := collector.Result().Blocks; len(blocks) != 0 {
		t.Fatalf("blocks = %#v", blocks)
	}
}

func TestCollectorMapsMaxOutputTokensToMaxTokens(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), false, nil)
	err := collector.Handle("response.incomplete", []byte(`{
		"type":"response.incomplete",
		"response":{
			"id":"resp_incomplete",
			"model":"gpt-test",
			"status":"incomplete",
			"incomplete_details":{"reason":"max_output_tokens"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := collector.Result().StopReason; got != "max_tokens" {
		t.Fatalf("stop reason = %q", got)
	}
}

func TestCollectorRejectsContentFilterAsSuccessfulTruncation(t *testing.T) {
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), false, nil)
	err := collector.Handle("response.incomplete", []byte(`{
		"type":"response.incomplete",
		"response":{
			"id":"resp_filtered",
			"model":"gpt-test",
			"status":"incomplete",
			"incomplete_details":{"reason":"content_filter"}
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("error = %v", err)
	}
}

func TestCollectorClosesOpenBlocksBeforeTerminalError(t *testing.T) {
	var events []protocol.Event
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), true, func(event protocol.Event) error {
		events = append(events, event)
		return nil
	})
	if err := collector.Handle("response.output_text.delta", []byte(`{
		"type":"response.output_text.delta",
		"item_id":"message_1",
		"delta":"partial"
	}`)); err != nil {
		t.Fatal(err)
	}
	err := collector.Handle("response.failed", []byte(`{
		"type":"response.failed",
		"response":{
			"status":"failed",
			"error":{"message":"terminal failure"}
		}
	}`))
	if err == nil {
		t.Fatal("expected terminal error")
	}
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Kind != protocol.EventBlockStart ||
		events[1].Kind != protocol.EventBlockDelta ||
		events[2].Kind != protocol.EventBlockStop {
		t.Fatalf("events = %#v", events)
	}
}

func TestCollectorPropagatesBlockCloseFailure(t *testing.T) {
	want := errors.New("emit failed")
	collector := NewCollector("gpt-test", protocol.NewToolNames(nil), true, func(event protocol.Event) error {
		if event.Kind == protocol.EventBlockStop {
			return want
		}
		return nil
	})
	if err := collector.Handle("response.output_text.delta", []byte(`{
		"type":"response.output_text.delta",
		"item_id":"message_1",
		"delta":"partial"
	}`)); err != nil {
		t.Fatal(err)
	}
	err := collector.Handle("response.completed", []byte(`{
		"type":"response.completed",
		"response":{"status":"completed"}
	}`))
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
