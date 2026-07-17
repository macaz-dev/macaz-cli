package openresponses

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

type Collector struct {
	model      string
	names      *protocol.ToolNames
	stream     bool
	emit       protocol.EmitFunc
	id         string
	blocks     []protocol.Block
	itemIndex  map[string]int
	open       map[int]bool
	arguments  map[int]string
	stopReason string
	usage      protocol.Usage
}

func NewCollector(model string, names *protocol.ToolNames, stream bool, emit protocol.EmitFunc) *Collector {
	return &Collector{
		model:      model,
		names:      names,
		stream:     stream,
		emit:       emit,
		itemIndex:  map[string]int{},
		open:       map[int]bool{},
		arguments:  map[int]string{},
		stopReason: "end_turn",
	}
}

func (c *Collector) Handle(event string, data []byte) error {
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode OpenResponses SSE %s: %w", event, err)
	}
	kind := stringValue(payload["type"])
	if kind == "" {
		kind = event
	}
	switch kind {
	case "response.created":
		if response := mapValue(payload["response"]); response != nil {
			c.id = stringValue(response["id"])
			if model := stringValue(response["model"]); model != "" {
				c.model = model
			}
		}
	case "response.output_item.added":
		item := mapValue(payload["item"])
		if item == nil {
			return nil
		}
		itemID := stringValue(item["id"])
		switch stringValue(item["type"]) {
		case "function_call":
			block := protocol.Block{
				Type:  "tool_use",
				ID:    first(stringValue(item["call_id"]), itemID, randomID("toolu")),
				Name:  c.names.Client(stringValue(item["name"])),
				Input: json.RawMessage(`{}`),
			}
			index, err := c.start(itemID, block)
			if err != nil {
				return err
			}
			c.arguments[index] = ""
			c.stopReason = "tool_use"
		case "reasoning":
			signature := stringValue(item["encrypted_content"])
			if signature != "" {
				_, err := c.start(itemID, protocol.Block{Type: "thinking", Signature: signature})
				return err
			}
			if itemID != "" {
				c.itemIndex[itemID] = -1
			}
		case "message":
			if itemID != "" {
				c.itemIndex[itemID] = -1
			}
		}
	case "response.output_text.delta":
		index, err := c.ensure(stringValue(payload["item_id"]), protocol.Block{Type: "text"})
		if err != nil {
			return err
		}
		delta := stringValue(payload["delta"])
		c.blocks[index].Text += delta
		return c.delta(index, "text_delta", delta)
	case "response.reasoning_summary_text.delta":
		index, err := c.ensure(stringValue(payload["item_id"]), protocol.Block{
			Type:      "thinking",
			Signature: "macaz-openresponses-reasoning-summary",
		})
		if err != nil {
			return err
		}
		delta := stringValue(payload["delta"])
		c.blocks[index].Thinking += delta
		return c.delta(index, "thinking_delta", delta)
	case "response.function_call_arguments.delta":
		index, ok := c.index(stringValue(payload["item_id"]))
		if !ok {
			return nil
		}
		delta := stringValue(payload["delta"])
		c.arguments[index] += delta
		return c.delta(index, "input_json_delta", delta)
	case "response.output_item.done":
		item := mapValue(payload["item"])
		itemID := ""
		if item != nil {
			itemID = stringValue(item["id"])
		}
		index, ok := c.index(itemID)
		if !ok && item != nil && stringValue(item["type"]) == "reasoning" {
			signature := stringValue(item["encrypted_content"])
			summary := responseSummaryText(item["summary"])
			if signature == "" && summary == "" {
				return nil
			}
			var err error
			index, err = c.start(itemID, protocol.Block{
				Type:      "thinking",
				Thinking:  summary,
				Signature: first(signature, "macaz-openresponses-reasoning-summary"),
			})
			if err != nil {
				return err
			}
			if summary != "" {
				if err := c.delta(index, "thinking_delta", summary); err != nil {
					return err
				}
			}
			ok = true
		}
		if !ok {
			return nil
		}
		if c.blocks[index].Type == "tool_use" {
			full := c.arguments[index]
			if full == "" && item != nil {
				full = stringValue(item["arguments"])
				if full != "" && c.stream {
					if err := c.delta(index, "input_json_delta", full); err != nil {
						return err
					}
				}
			}
			if full == "" || !json.Valid([]byte(full)) {
				full = "{}"
			}
			c.blocks[index].Input = json.RawMessage(full)
		} else if c.blocks[index].Type == "thinking" && item != nil {
			if signature := stringValue(item["encrypted_content"]); signature != "" {
				c.blocks[index].Signature = signature
			}
			if c.blocks[index].Thinking == "" {
				if summary := responseSummaryText(item["summary"]); summary != "" {
					c.blocks[index].Thinking = summary
					if err := c.delta(index, "thinking_delta", summary); err != nil {
						return err
					}
				}
			}
		}
		return c.stop(index)
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
		response := mapValue(payload["response"])
		status := strings.TrimPrefix(kind, "response.")
		if response != nil {
			if id := stringValue(response["id"]); id != "" {
				c.id = id
			}
			if model := stringValue(response["model"]); model != "" {
				c.model = model
			}
			c.readUsage(mapValue(response["usage"]))
			if value := stringValue(response["status"]); value != "" {
				status = value
			}
		}
		if err := c.closeOpen(); err != nil {
			return err
		}
		switch status {
		case "completed":
			return nil
		case "incomplete":
			reason := ""
			if response != nil {
				reason = stringValue(mapValue(response["incomplete_details"])["reason"])
			}
			switch reason {
			case "", "max_output_tokens":
				c.stopReason = "max_tokens"
				return nil
			case "content_filter":
				return errors.New("OpenResponses response incomplete: content_filter")
			default:
				return fmt.Errorf("OpenResponses response incomplete: %s", reason)
			}
		case "failed":
			return fmt.Errorf("OpenResponses response failed: %s", responseError(response))
		case "cancelled", "canceled":
			return errors.New("OpenResponses response cancelled")
		default:
			return fmt.Errorf("OpenResponses response ended with status %q", status)
		}
	case "error":
		message := stringValue(payload["message"])
		if nested := mapValue(payload["error"]); message == "" && nested != nil {
			message = first(stringValue(nested["message"]), stringValue(nested["code"]))
		}
		if message == "" {
			message = string(data)
		}
		return errors.New(message)
	}
	return nil
}

func (c *Collector) Result() protocol.Result {
	if c.id == "" {
		c.id = randomID("msg")
	}
	return protocol.Result{
		ID:         c.id,
		Model:      c.model,
		Blocks:     c.blocks,
		StopReason: c.stopReason,
		Usage:      c.usage,
	}
}

func ReadSSE(reader io.Reader, fn func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data bytes.Buffer
	dispatch := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		raw := bytes.TrimSuffix(data.Bytes(), []byte{'\n'})
		err := fn(event, raw)
		event = ""
		data.Reset()
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read provider stream: %w", err)
	}
	return dispatch()
}

func (c *Collector) start(itemID string, block protocol.Block) (int, error) {
	index := len(c.blocks)
	c.blocks = append(c.blocks, block)
	if itemID != "" {
		c.itemIndex[itemID] = index
	}
	c.open[index] = true
	if c.stream && c.emit != nil {
		if err := c.emit(protocol.Event{Kind: protocol.EventBlockStart, Index: index, Block: block}); err != nil {
			return 0, err
		}
	}
	return index, nil
}

func (c *Collector) ensure(itemID string, block protocol.Block) (int, error) {
	if index, ok := c.index(itemID); ok {
		return index, nil
	}
	return c.start(itemID, block)
}

func (c *Collector) index(itemID string) (int, bool) {
	index, ok := c.itemIndex[itemID]
	return index, ok && index >= 0
}

func (c *Collector) delta(index int, deltaType, delta string) error {
	if !c.stream || c.emit == nil || delta == "" {
		return nil
	}
	return c.emit(protocol.Event{Kind: protocol.EventBlockDelta, Index: index, DeltaType: deltaType, Delta: delta})
}

func (c *Collector) stop(index int) error {
	if !c.open[index] {
		return nil
	}
	delete(c.open, index)
	if c.stream && c.emit != nil {
		return c.emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index, Block: c.blocks[index]})
	}
	return nil
}

func responseSummaryText(value any) string {
	items, _ := value.([]any)
	var result strings.Builder
	for _, raw := range items {
		item := mapValue(raw)
		if item == nil || (stringValue(item["type"]) != "" && stringValue(item["type"]) != "summary_text") {
			continue
		}
		result.WriteString(stringValue(item["text"]))
	}
	return result.String()
}

func (c *Collector) closeOpen() error {
	for index := range c.open {
		if err := c.stop(index); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) readUsage(usage map[string]any) {
	if usage == nil {
		return
	}
	c.usage.InputTokens = int64Value(usage["input_tokens"])
	c.usage.OutputTokens = int64Value(usage["output_tokens"])
	if details := mapValue(usage["input_tokens_details"]); details != nil {
		c.usage.CacheReadInputTokens = int64Value(details["cached_tokens"])
	}
	if details := mapValue(usage["output_tokens_details"]); details != nil {
		c.usage.ReasoningOutputTokens = int64Value(details["reasoning_tokens"])
	}
}

func responseError(response map[string]any) string {
	if response == nil {
		return "provider returned no error details"
	}
	detail := mapValue(response["error"])
	if detail == nil {
		return "provider returned no error details"
	}
	return first(
		stringValue(detail["message"]),
		stringValue(detail["code"]),
		stringValue(detail["type"]),
		"provider returned no error details",
	)
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func int64Value(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case json.Number:
		n, _ := value.Int64()
		return n
	default:
		return 0
	}
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func randomID(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "_unknown"
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}
