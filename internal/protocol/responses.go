package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ResponsesInput struct {
	Model              string          `json:"model"`
	Instructions       json.RawMessage `json:"instructions,omitempty"`
	Input              json.RawMessage `json:"input"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

func FromResponses(input ResponsesInput) (*Request, error) {
	system, err := responseInstructions(input.Instructions)
	if err != nil {
		return nil, err
	}
	tools, err := responseTools(input.Tools)
	if err != nil {
		return nil, err
	}
	messages, extraSystem, err := responseMessages(input.Input, tools)
	if err != nil {
		return nil, err
	}
	if extraSystem != "" {
		if system != "" {
			system += "\n\n"
		}
		system += extraSystem
	}
	choice, err := responseToolChoice(input.ToolChoice)
	if err != nil {
		return nil, err
	}
	request := &Request{
		Model:        strings.TrimSpace(input.Model),
		MaxTokens:    input.MaxOutputTokens,
		Messages:     messages,
		Tools:        tools,
		ToolChoice:   choice,
		Stream:       input.Stream,
		Temperature:  input.Temperature,
		TopP:         input.TopP,
		OutputConfig: responseEffort(input.Reasoning),
		OutputFormat: responseFormat(input.Text),
		Metadata:     input.Metadata,
	}
	if system != "" {
		request.System, _ = json.Marshal(system)
	}
	return request, nil
}

func responseInstructions(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	return "", errors.New("Responses instructions must be a string")
}

func responseMessages(raw json.RawMessage, tools []Tool) ([]Message, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		content, _ := json.Marshal([]Block{{Type: "text", Text: text}})
		return []Message{{Role: "user", Content: content}}, "", nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, "", fmt.Errorf("decode Responses input: %w", err)
	}
	var messages []Message
	var systems []string
	for _, item := range items {
		var itemType string
		_ = json.Unmarshal(item["type"], &itemType)
		itemType = strings.ToLower(strings.TrimSpace(itemType))
		switch itemType {
		case "", "message":
			var role string
			_ = json.Unmarshal(item["role"], &role)
			role = strings.ToLower(strings.TrimSpace(role))
			blocks, err := responseContentBlocks(item["content"])
			if err != nil {
				return nil, "", err
			}
			switch role {
			case "system", "developer":
				for _, block := range blocks {
					if block.Type != "text" {
						return nil, "", fmt.Errorf("Responses %s messages only support text content", role)
					}
					if block.Text != "" {
						systems = append(systems, block.Text)
					}
				}
			case "user", "assistant":
				if err := appendResponseMessage(&messages, role, blocks); err != nil {
					return nil, "", err
				}
			default:
				return nil, "", fmt.Errorf("unsupported Responses message role %q", role)
			}
		case "function_call", "custom_tool_call":
			var callID, name, namespace, arguments string
			_ = json.Unmarshal(item["call_id"], &callID)
			if callID == "" {
				_ = json.Unmarshal(item["id"], &callID)
			}
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["namespace"], &namespace)
			if itemType == "custom_tool_call" {
				_ = json.Unmarshal(item["input"], &arguments)
			} else {
				_ = json.Unmarshal(item["arguments"], &arguments)
			}
			if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" {
				return nil, "", fmt.Errorf("Responses %s requires call_id and name", itemType)
			}
			name = responseInternalToolName(tools, namespace, name)
			if itemType == "custom_tool_call" {
				wrapped, err := json.Marshal(map[string]string{"input": arguments})
				if err != nil {
					return nil, "", err
				}
				arguments = string(wrapped)
			} else if arguments == "" || !json.Valid([]byte(arguments)) {
				arguments = "{}"
			}
			if err := appendResponseMessage(&messages, "assistant", []Block{{
				Type: "tool_use", ID: callID, Name: name, Input: json.RawMessage(arguments),
			}}); err != nil {
				return nil, "", err
			}
		case "function_call_output", "custom_tool_call_output":
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			if strings.TrimSpace(callID) == "" {
				return nil, "", fmt.Errorf("Responses %s requires call_id", itemType)
			}
			content, err := responseToolOutput(item["output"])
			if err != nil {
				return nil, "", err
			}
			if err := appendResponseMessage(&messages, "user", []Block{{
				Type: "tool_result", ToolUseID: callID, Content: content,
			}}); err != nil {
				return nil, "", err
			}
		case "reasoning", "item_reference":
			// Reasoning payloads and provider-side item references are not portable.
			// Codex also sends the corresponding conversation/tool items needed to
			// continue a local client session.
		default:
			return nil, "", fmt.Errorf("unsupported Responses input item %q", itemType)
		}
	}
	return messages, strings.Join(systems, "\n\n"), nil
}

func appendResponseMessage(messages *[]Message, role string, blocks []Block) error {
	if len(blocks) == 0 {
		return nil
	}
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == role {
		prior, err := DecodeBlocks((*messages)[len(*messages)-1].Content)
		if err != nil {
			return err
		}
		blocks = append(prior, blocks...)
		content, err := json.Marshal(blocks)
		if err != nil {
			return err
		}
		(*messages)[len(*messages)-1].Content = content
		return nil
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	*messages = append(*messages, Message{Role: role, Content: content})
	return nil
}

func responseContentBlocks(raw json.RawMessage) ([]Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []Block{{Type: "text", Text: text}}, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode Responses message content: %w", err)
	}
	blocks := make([]Block, 0, len(parts))
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.ToLower(strings.TrimSpace(partType)) {
		case "text", "input_text", "output_text":
			var value string
			_ = json.Unmarshal(part["text"], &value)
			blocks = append(blocks, Block{Type: "text", Text: value})
		case "input_image", "image_url":
			block, err := responseImageBlock(part)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case "input_file":
			block, err := responseFileBlock(part)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		default:
			return nil, fmt.Errorf("unsupported Responses content part %q", partType)
		}
	}
	return blocks, nil
}

func responseImageBlock(part map[string]json.RawMessage) (Block, error) {
	url := rawString(part["image_url"])
	if url == "" {
		url = rawString(part["url"])
	}
	if url == "" {
		if raw := part["image_url"]; len(raw) > 0 {
			var nested struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(raw, &nested)
			url = nested.URL
		}
	}
	if strings.TrimSpace(url) == "" {
		return Block{}, errors.New("Responses input_image requires image_url; file_id inputs are not portable")
	}
	source, err := responseSource(url)
	if err != nil {
		return Block{}, err
	}
	return Block{Type: "image", Source: source}, nil
}

func responseFileBlock(part map[string]json.RawMessage) (Block, error) {
	value := rawString(part["file_data"])
	if value == "" {
		value = rawString(part["file_url"])
	}
	if value == "" {
		return Block{}, errors.New("Responses input_file requires file_data or file_url; file_id inputs are not portable")
	}
	source, err := responseSource(value)
	if err != nil {
		return Block{}, err
	}
	return Block{Type: "document", Title: rawString(part["filename"]), Source: source}, nil
}

func responseSource(value string) (*Source, error) {
	if !strings.HasPrefix(value, "data:") {
		return &Source{Type: "url", URL: value}, nil
	}
	header, data, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return nil, errors.New("Responses data URL must use base64 encoding")
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil, fmt.Errorf("decode Responses data URL: %w", err)
	}
	return &Source{Type: "base64", MediaType: strings.TrimSuffix(header, ";base64"), Data: data}, nil
}

func responseToolOutput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`""`), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return json.RawMessage(append([]byte(nil), raw...)), nil
	}
	blocks, err := responseContentBlocks(raw)
	if err != nil {
		return nil, fmt.Errorf("decode Responses function output: %w", err)
	}
	return json.Marshal(blocks)
}

func responseTools(raw json.RawMessage) ([]Tool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decode Responses tools: %w", err)
	}
	tools := make([]Tool, 0, len(items))
	for _, item := range items {
		var toolType string
		_ = json.Unmarshal(item["type"], &toolType)
		toolType = strings.ToLower(strings.TrimSpace(toolType))
		if toolType == "namespace" {
			flattened, err := responseNamespaceTools(item)
			if err != nil {
				return nil, err
			}
			tools = append(tools, flattened...)
			continue
		}
		if toolType != "function" && toolType != "custom" {
			return nil, fmt.Errorf("Responses server tool %q is not portable through a local provider gateway", toolType)
		}
		var tool Tool
		_ = json.Unmarshal(item["name"], &tool.Name)
		_ = json.Unmarshal(item["description"], &tool.Description)
		if toolType == "custom" {
			tool.ClientType = "custom"
			tool.InputSchema = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Raw input for the custom tool",
					},
				},
				"required":             []string{"input"},
				"additionalProperties": false,
			}
		} else if len(item["parameters"]) > 0 {
			if err := json.Unmarshal(item["parameters"], &tool.InputSchema); err != nil {
				return nil, fmt.Errorf("decode schema for tool %q: %w", tool.Name, err)
			}
		}
		if strings.TrimSpace(tool.Name) == "" {
			return nil, fmt.Errorf("Responses %s tool requires a name", toolType)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func responseNamespaceTools(item map[string]json.RawMessage) ([]Tool, error) {
	namespace := strings.TrimSpace(rawString(item["name"]))
	if namespace == "" {
		return nil, errors.New("Responses namespace tool requires a name")
	}
	var children []map[string]json.RawMessage
	if err := json.Unmarshal(item["tools"], &children); err != nil {
		return nil, fmt.Errorf("decode Responses namespace %q tools: %w", namespace, err)
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("Responses namespace %q contains no tools", namespace)
	}
	result := make([]Tool, 0, len(children))
	for _, child := range children {
		childType := strings.ToLower(strings.TrimSpace(rawString(child["type"])))
		if childType == "" {
			childType = "function"
		}
		if childType != "function" {
			return nil, fmt.Errorf("Responses namespace %q contains unsupported tool type %q", namespace, childType)
		}
		name := strings.TrimSpace(rawString(child["name"]))
		if name == "" {
			return nil, fmt.Errorf("Responses namespace %q contains an unnamed function", namespace)
		}
		var schema map[string]any
		schemaRaw := child["parameters"]
		if len(schemaRaw) == 0 {
			schemaRaw = child["input_schema"]
		}
		if len(schemaRaw) == 0 {
			schemaRaw = child["inputSchema"]
		}
		if len(schemaRaw) > 0 {
			if err := json.Unmarshal(schemaRaw, &schema); err != nil {
				return nil, fmt.Errorf("decode schema for namespace tool %q.%q: %w", namespace, name, err)
			}
		}
		result = append(result, Tool{
			Name:            responseNamespacedToolName(namespace, name),
			Description:     rawString(child["description"]),
			InputSchema:     schema,
			ClientType:      "namespace",
			ClientName:      name,
			ClientNamespace: namespace,
		})
	}
	return result, nil
}

func responseNamespacedToolName(namespace, name string) string {
	return namespace + "__" + name
}

func responseInternalToolName(tools []Tool, namespace, name string) string {
	if namespace == "" {
		return name
	}
	for _, tool := range tools {
		if tool.ClientNamespace == namespace && tool.ClientName == name {
			return tool.Name
		}
	}
	return responseNamespacedToolName(namespace, name)
}

func responseToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		var anthropic string
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "auto":
			anthropic = `{"type":"auto"}`
		case "required":
			anthropic = `{"type":"any"}`
		case "none":
			anthropic = `{"type":"none"}`
		default:
			return nil, fmt.Errorf("unsupported Responses tool_choice %q", choice)
		}
		return json.RawMessage(anthropic), nil
	}
	var selected struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &selected); err != nil {
		return nil, fmt.Errorf("decode Responses tool_choice: %w", err)
	}
	selected.Type = strings.ToLower(strings.TrimSpace(selected.Type))
	if (selected.Type != "function" && selected.Type != "custom") || strings.TrimSpace(selected.Name) == "" {
		return nil, errors.New("Responses object tool_choice must select a function or custom tool name")
	}
	return json.Marshal(map[string]any{"type": "tool", "name": selected.Name})
}

func responseEffort(raw json.RawMessage) json.RawMessage {
	var reasoning struct {
		Effort string `json:"effort"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &reasoning) != nil {
		return nil
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	switch effort {
	case "none", "minimal":
		effort = "low"
	case "xhigh", "ultra":
		effort = "max"
	}
	if effort == "" {
		return nil
	}
	result, _ := json.Marshal(map[string]any{"effort": effort})
	return result
}

func responseFormat(raw json.RawMessage) json.RawMessage {
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil || len(text.Format) == 0 {
		return nil
	}
	return text.Format
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
