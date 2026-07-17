package openresponses

import (
	"errors"
	"strings"
	"testing"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

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
