package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	input, err := s.decodeResponsesInput(w, r)
	if err != nil {
		return
	}
	if strings.TrimSpace(input.PreviousResponseID) != "" {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "previous_response_id is not supported; Codex must send conversation input explicitly")
		return
	}
	if strings.TrimSpace(input.Model) == "" {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	requestedModel := input.Model
	upstreamModel, ok := s.resolveModel(requestedModel)
	if !ok {
		writeResponsesError(w, http.StatusBadRequest, "model_not_found", "model is not available in the active macaz provider")
		return
	}
	request, err := protocol.FromResponses(input)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.Model = upstreamModel
	if input.Stream {
		s.streamResponses(w, r, request, requestedModel)
		return
	}
	result, err := s.provider.Generate(r.Context(), request, nil)
	if err != nil {
		s.recordFailure(err)
		writeResponsesProviderError(w, err)
		return
	}
	result.Model = requestedModel
	s.recordResult(result)
	writeJSON(w, http.StatusOK, responsesPayload(result, requestedModel, request.Tools))
}

func (s *Server) decodeResponsesInput(w http.ResponseWriter, r *http.Request) (protocol.ResponsesInput, error) {
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.UseNumber()
	var input protocol.ResponsesInput
	if err := decoder.Decode(&input); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return protocol.ResponsesInput{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "request body must contain one JSON object")
		return protocol.ResponsesInput{}, errors.New("multiple JSON values")
	}
	return input, nil
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, request *protocol.Request, requestedModel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this HTTP server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	stream := newResponsesStream(w, flusher, requestedModel, request.Tools)
	stream.created()
	result, err := s.provider.Generate(r.Context(), request, stream.emit)
	if err != nil {
		s.recordFailure(err)
		stream.failed(err)
		return
	}
	result.Model = requestedModel
	s.recordResult(result)
	if len(stream.output) == 0 {
		stream.replay(result.Blocks)
	}
	stream.complete(result)
}

type responsesStreamBlock struct {
	index       int
	outputIndex int
	itemID      string
	itemType    string
	callID      string
	name        string
	namespace   string
	text        strings.Builder
	arguments   strings.Builder
	custom      bool
	done        bool
}

type responsesStream struct {
	w            io.Writer
	flusher      http.Flusher
	responseID   string
	model        string
	sequence     int
	blocks       map[int]*responsesStreamBlock
	output       []map[string]any
	toolMetadata map[string]protocol.Tool
}

func newResponsesStream(w io.Writer, flusher http.Flusher, model string, tools []protocol.Tool) *responsesStream {
	return &responsesStream{
		w:            w,
		flusher:      flusher,
		responseID:   "resp_" + mustRandomToken(12),
		model:        model,
		sequence:     1,
		blocks:       map[int]*responsesStreamBlock{},
		toolMetadata: responseToolMetadata(tools),
	}
}

func (s *responsesStream) created() {
	s.event("response.created", map[string]any{
		"type":     "response.created",
		"response": responseBase(s.responseID, s.model, "in_progress"),
	})
}

func (s *responsesStream) emit(event protocol.Event) error {
	switch event.Kind {
	case protocol.EventBlockStart:
		s.start(event.Index, event.Block)
	case protocol.EventBlockDelta:
		s.delta(event.Index, event.DeltaType, event.Delta)
	case protocol.EventBlockStop:
		s.stop(event.Index)
	}
	return nil
}

func (s *responsesStream) start(index int, block protocol.Block) {
	if prior := s.blocks[index]; prior != nil {
		return
	}
	state := &responsesStreamBlock{index: index, outputIndex: len(s.output)}
	switch block.Type {
	case "tool_use":
		metadata := s.toolMetadata[block.Name]
		state.custom = strings.EqualFold(strings.TrimSpace(metadata.ClientType), "custom")
		state.itemType = "function_call"
		state.itemID = "fc_" + mustRandomToken(10)
		inputKey := "arguments"
		if state.custom {
			state.itemType = "custom_tool_call"
			state.itemID = "ctc_" + mustRandomToken(10)
			inputKey = "input"
		}
		state.callID = first(block.ID, "call_"+mustRandomToken(10))
		state.name = block.Name
		if metadata.ClientName != "" {
			state.name = metadata.ClientName
		}
		state.namespace = metadata.ClientNamespace
		item := map[string]any{
			"id": state.itemID, "type": state.itemType, "status": "in_progress",
			"call_id": state.callID, "name": state.name, inputKey: "",
		}
		if state.namespace != "" {
			item["namespace"] = state.namespace
		}
		s.output = append(s.output, item)
		if state.custom {
			s.event("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": state.outputIndex, "item": cloneResponseMap(item),
			})
		}
	case "thinking", "redacted_thinking":
		state.itemType = "reasoning"
		state.itemID = "rs_" + mustRandomToken(10)
		item := map[string]any{
			"id": state.itemID, "type": "reasoning", "status": "in_progress", "summary": []any{},
		}
		s.output = append(s.output, item)
		s.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": state.outputIndex, "item": cloneResponseMap(item),
		})
		s.event("response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "item_id": state.itemID,
			"output_index": state.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""},
		})
	default:
		state.itemType = "message"
		state.itemID = "msg_" + mustRandomToken(10)
		item := map[string]any{
			"id": state.itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
		}
		s.output = append(s.output, item)
		s.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": state.outputIndex, "item": cloneResponseMap(item),
		})
		s.event("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": state.itemID, "output_index": state.outputIndex,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	s.blocks[index] = state
}

func (s *responsesStream) delta(index int, deltaType, value string) {
	state := s.blocks[index]
	if state == nil || state.done || value == "" {
		return
	}
	switch deltaType {
	case "input_json_delta":
		state.arguments.WriteString(value)
		return
	case "thinking_delta":
		state.text.WriteString(value)
		s.event("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": state.itemID,
			"output_index": state.outputIndex, "summary_index": 0, "delta": value,
		})
	case "text_delta":
		state.text.WriteString(value)
		s.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": state.itemID,
			"output_index": state.outputIndex, "content_index": 0, "delta": value,
		})
	}
}

func (s *responsesStream) stop(index int) {
	state := s.blocks[index]
	if state == nil || state.done {
		return
	}
	state.done = true
	item := s.output[state.outputIndex]
	switch state.itemType {
	case "custom_tool_call":
		input := responseCustomToolInput(state.arguments.String())
		item["input"] = input
		item["status"] = "completed"
		if input != "" {
			s.event("response.custom_tool_call_input.delta", map[string]any{
				"type": "response.custom_tool_call_input.delta", "item_id": state.itemID,
				"output_index": state.outputIndex, "delta": input,
			})
		}
		s.event("response.custom_tool_call_input.done", map[string]any{
			"type": "response.custom_tool_call_input.done", "item_id": state.itemID,
			"output_index": state.outputIndex, "input": input,
		})
	case "function_call":
		arguments := state.arguments.String()
		if arguments == "" || !json.Valid([]byte(arguments)) {
			arguments = "{}"
		}
		item["arguments"] = arguments
		item["status"] = "completed"
	case "reasoning":
		text := state.text.String()
		item["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
		item["status"] = "completed"
		s.event("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": state.itemID,
			"output_index": state.outputIndex, "summary_index": 0, "text": text,
		})
		s.event("response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": state.itemID,
			"output_index": state.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
	case "message":
		text := state.text.String()
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		item["content"] = []any{part}
		item["status"] = "completed"
		s.event("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": state.itemID,
			"output_index": state.outputIndex, "content_index": 0, "text": text,
		})
		s.event("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": state.itemID,
			"output_index": state.outputIndex, "content_index": 0, "part": part,
		})
	}
	s.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": state.outputIndex, "item": cloneResponseMap(item),
	})
}

func (s *responsesStream) replay(blocks []protocol.Block) {
	for index, block := range blocks {
		s.start(index, block)
		switch block.Type {
		case "tool_use":
			s.delta(index, "input_json_delta", string(firstRaw(block.Input, json.RawMessage(`{}`))))
		case "thinking", "redacted_thinking":
			s.delta(index, "thinking_delta", block.Thinking)
		default:
			s.delta(index, "text_delta", block.Text)
		}
		s.stop(index)
	}
}

func (s *responsesStream) complete(result protocol.Result) {
	for index := range s.blocks {
		s.stop(index)
	}
	payload := responsesPayloadWithOutput(result, s.responseID, s.model, s.output)
	s.event("response.completed", map[string]any{"type": "response.completed", "response": payload})
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

func (s *responsesStream) failed(err error) {
	response := responseBase(s.responseID, s.model, "failed")
	response["error"] = map[string]any{"type": "server_error", "code": "provider_error", "message": err.Error()}
	s.event("response.failed", map[string]any{"type": "response.failed", "response": response})
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

func (s *responsesStream) event(name string, value map[string]any) {
	value["sequence_number"] = s.sequence
	s.sequence++
	writeSSE(s.w, name, value)
	s.flusher.Flush()
}

func responsesPayload(result protocol.Result, model string, tools []protocol.Tool) map[string]any {
	return responsesPayloadWithOutput(result, first(result.ID, "resp_"+mustRandomToken(12)), model, responseOutput(result.Blocks, tools))
}

func responsesPayloadWithOutput(result protocol.Result, responseID, model string, output []map[string]any) map[string]any {
	status := "completed"
	var incomplete any
	if result.StopReason == "max_tokens" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	payload := responseBase(responseID, model, status)
	payload["output"] = output
	payload["output_text"] = responseOutputText(output)
	payload["incomplete_details"] = incomplete
	payload["usage"] = map[string]any{
		"input_tokens":          result.Usage.InputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": result.Usage.CacheReadInputTokens},
		"output_tokens":         result.Usage.OutputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": result.Usage.ReasoningOutputTokens},
		"total_tokens":          result.Usage.InputTokens + result.Usage.OutputTokens,
	}
	return payload
}

func responseBase(id, model, status string) map[string]any {
	return map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"background": false, "error": nil, "incomplete_details": nil, "instructions": nil,
		"max_output_tokens": nil, "model": model, "output": []any{}, "output_text": "",
		"parallel_tool_calls": true, "previous_response_id": nil, "store": false,
		"temperature": nil, "tool_choice": "auto", "tools": []any{}, "top_p": nil,
		"truncation": "disabled", "usage": nil, "user": nil, "metadata": map[string]any{},
	}
}

func responseOutput(blocks []protocol.Block, tools []protocol.Tool) []map[string]any {
	toolMetadata := responseToolMetadata(tools)
	output := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "tool_use":
			metadata := toolMetadata[block.Name]
			clientName := block.Name
			if metadata.ClientName != "" {
				clientName = metadata.ClientName
			}
			arguments := string(firstRaw(block.Input, json.RawMessage(`{}`)))
			if !json.Valid([]byte(arguments)) {
				arguments = "{}"
			}
			if strings.EqualFold(strings.TrimSpace(metadata.ClientType), "custom") {
				output = append(output, map[string]any{
					"id": "ctc_" + mustRandomToken(10), "type": "custom_tool_call", "status": "completed",
					"call_id": first(block.ID, "call_"+mustRandomToken(10)), "name": clientName,
					"input": responseCustomToolInput(arguments),
				})
				continue
			}
			item := map[string]any{
				"id": "fc_" + mustRandomToken(10), "type": "function_call", "status": "completed",
				"call_id": first(block.ID, "call_"+mustRandomToken(10)), "name": clientName, "arguments": arguments,
			}
			if metadata.ClientNamespace != "" {
				item["namespace"] = metadata.ClientNamespace
			}
			output = append(output, item)
		case "thinking", "redacted_thinking":
			if block.Thinking != "" {
				output = append(output, map[string]any{
					"id": "rs_" + mustRandomToken(10), "type": "reasoning", "status": "completed",
					"summary": []any{map[string]any{"type": "summary_text", "text": block.Thinking}},
				})
			}
		default:
			output = append(output, map[string]any{
				"id": "msg_" + mustRandomToken(10), "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": block.Text, "annotations": []any{}}},
			})
		}
	}
	return output
}

func responseToolMetadata(tools []protocol.Tool) map[string]protocol.Tool {
	result := make(map[string]protocol.Tool)
	for _, tool := range tools {
		if tool.Name != "" {
			result[tool.Name] = tool
		}
	}
	return result
}

func responseCustomToolInput(arguments string) string {
	var wrapped struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(arguments), &wrapped) == nil {
		return wrapped.Input
	}
	var direct string
	if json.Unmarshal([]byte(arguments), &direct) == nil {
		return direct
	}
	return arguments
}

func responseOutputText(output []map[string]any) string {
	var parts []string
	for _, item := range output {
		if item["type"] != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			if part["type"] == "output_text" {
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "")
}

func cloneResponseMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func writeResponsesProviderError(w http.ResponseWriter, err error) {
	status := provider.Status(err)
	errorType := "server_error"
	if status == http.StatusBadRequest {
		errorType = "invalid_request_error"
	} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
		errorType = "authentication_error"
	} else if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	writeResponsesError(w, status, errorType, err.Error())
}

func writeResponsesError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message, "type": errorType, "param": nil, "code": errorType,
		},
	})
}
