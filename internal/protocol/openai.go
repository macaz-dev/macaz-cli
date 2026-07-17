package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var validFunctionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type ToolNames struct {
	toProvider map[string]string
	toClient   map[string]string
}

func NewToolNames(tools []Tool) *ToolNames {
	names := &ToolNames{
		toProvider: make(map[string]string, len(tools)),
		toClient:   make(map[string]string, len(tools)),
	}
	for _, tool := range tools {
		if !IsClientTool(tool) {
			continue
		}
		client := tool.Name
		provider := client
		if !validFunctionName.MatchString(provider) {
			hash := sha256.Sum256([]byte(provider))
			provider = "macaz_" + hex.EncodeToString(hash[:8])
		}
		for suffix := 2; ; suffix++ {
			if prior, exists := names.toClient[provider]; !exists || prior == client {
				break
			}
			base := provider
			if len(base) > 60 {
				base = base[:60]
			}
			provider = fmt.Sprintf("%s_%d", base, suffix)
		}
		names.toProvider[client] = provider
		names.toClient[provider] = client
	}
	return names
}

func IsClientTool(tool Tool) bool {
	toolType := strings.ToLower(strings.TrimSpace(tool.Type))
	if strings.TrimSpace(tool.Name) == "" {
		return false
	}
	return toolType == "" || toolType == "custom"
}

func ClientTools(tools []Tool) ([]Tool, error) {
	result := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		toolType := strings.ToLower(strings.TrimSpace(tool.Type))
		switch {
		case IsClientTool(tool):
			result = append(result, tool)
		case strings.HasPrefix(toolType, "tool_search_tool_"):
			// Claude can defer large client tool catalogs behind an Anthropic
			// server-side search tool. macaz expands the client-owned catalog
			// locally and omits only the search helper.
			continue
		case strings.HasPrefix(toolType, "web_search"),
			strings.HasPrefix(toolType, "web_fetch"),
			strings.HasPrefix(toolType, "code_execution"),
			strings.HasPrefix(toolType, "bash_"),
			strings.HasPrefix(toolType, "text_editor_"),
			strings.HasPrefix(toolType, "computer_"):
			return nil, fmt.Errorf("Anthropic server tool %q cannot be executed by a local client provider", tool.Type)
		default:
			return nil, fmt.Errorf("unsupported Anthropic tool type %q", tool.Type)
		}
	}
	return result, nil
}

func (n *ToolNames) Provider(client string) string {
	if mapped := n.toProvider[client]; mapped != "" {
		return mapped
	}
	return client
}

func (n *ToolNames) Client(provider string) string {
	if mapped := n.toClient[provider]; mapped != "" {
		return mapped
	}
	return provider
}

type ResponsesRequest struct {
	Body      map[string]any
	ToolNames *ToolNames
}

type ToolPolicy struct {
	Type            string
	Name            string
	DisableParallel bool
}

func ParseToolPolicy(raw json.RawMessage) (ToolPolicy, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return ToolPolicy{Type: "auto"}, nil
	}
	var choice struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return ToolPolicy{}, fmt.Errorf("decode tool_choice: %w", err)
	}
	choice.Type = strings.ToLower(strings.TrimSpace(choice.Type))
	if choice.Type == "" {
		choice.Type = "auto"
	}
	switch choice.Type {
	case "auto", "any", "none":
		return ToolPolicy{Type: choice.Type, DisableParallel: choice.DisableParallelToolUse}, nil
	case "tool":
		choice.Name = strings.TrimSpace(choice.Name)
		if choice.Name == "" {
			return ToolPolicy{}, errors.New("tool_choice.type=tool requires a tool name")
		}
		return ToolPolicy{
			Type:            choice.Type,
			Name:            choice.Name,
			DisableParallel: choice.DisableParallelToolUse,
		}, nil
	default:
		return ToolPolicy{}, fmt.Errorf("unsupported tool_choice type %q", choice.Type)
	}
}

func ApplyToolPolicy(tools []Tool, policy ToolPolicy) ([]Tool, error) {
	switch policy.Type {
	case "", "auto":
		return append([]Tool(nil), tools...), nil
	case "any":
		if len(tools) == 0 {
			return nil, errors.New("tool_choice.type=any requires at least one available tool")
		}
		return append([]Tool(nil), tools...), nil
	case "none":
		return nil, nil
	case "tool":
		for _, tool := range tools {
			if tool.Name == policy.Name {
				return []Tool{tool}, nil
			}
		}
		return nil, fmt.Errorf("tool_choice requested unavailable tool %q", policy.Name)
	default:
		return nil, fmt.Errorf("unsupported tool policy %q", policy.Type)
	}
}

func ToResponses(req *Request, model, defaultEffort string) (ResponsesRequest, error) {
	instructions, err := SystemText(req.System)
	if err != nil {
		return ResponsesRequest{}, err
	}
	allClientTools, err := ClientTools(req.Tools)
	if err != nil {
		return ResponsesRequest{}, err
	}
	policy, err := ParseToolPolicy(req.ToolChoice)
	if err != nil {
		return ResponsesRequest{}, err
	}
	clientTools, err := ApplyToolPolicy(allClientTools, policy)
	if err != nil {
		return ResponsesRequest{}, err
	}
	names := NewToolNames(allClientTools)
	input, err := responsesInput(req.Messages, names)
	if err != nil {
		return ResponsesRequest{}, err
	}
	body := map[string]any{
		"model":  model,
		"input":  input,
		"store":  false,
		"stream": req.Stream,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(clientTools) > 0 {
		tools := make([]map[string]any, 0, len(clientTools))
		for _, tool := range clientTools {
			schema := tool.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        names.Provider(tool.Name),
				"description": tool.Description,
				"parameters":  schema,
				"strict":      false,
			})
		}
		body["tools"] = tools
		body["parallel_tool_calls"] = !policy.DisableParallel
	}
	choice := responsesToolChoice(policy, names)
	if choice != nil && (len(clientTools) > 0 || policy.Type == "none") {
		body["tool_choice"] = choice
	}
	effort := Effort(req, defaultEffort)
	if len(req.Thinking) > 0 || effort != "" {
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if format := responsesFormat(req); format != nil {
		body["text"] = map[string]any{"format": format}
	}
	if req.Metadata != nil {
		if user, ok := req.Metadata["user_id"]; ok {
			value := fmt.Sprint(user)
			if len(value) > 64 {
				value = value[:64]
			}
			body["user"] = value
		}
	}
	return ResponsesRequest{Body: body, ToolNames: names}, nil
}

func responsesInput(messages []Message, names *ToolNames) ([]any, error) {
	var input []any
	for _, message := range messages {
		blocks, err := DecodeBlocks(message.Content)
		if err != nil {
			return nil, err
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		var adjacent []map[string]any
		flush := func() {
			if len(adjacent) == 0 {
				return
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": adjacent,
			})
			adjacent = nil
		}
		for _, block := range blocks {
			switch role {
			case "user":
				switch block.Type {
				case "text":
					adjacent = append(adjacent, map[string]any{"type": "input_text", "text": block.Text})
				case "image":
					url, err := sourceURL(block.Source)
					if err != nil {
						return nil, err
					}
					adjacent = append(adjacent, map[string]any{"type": "input_image", "image_url": url})
				case "document":
					file, err := inputFile(block)
					if err != nil {
						return nil, err
					}
					adjacent = append(adjacent, file)
				case "tool_result":
					flush()
					output, extra, err := toolResultOutput(block)
					if err != nil {
						return nil, err
					}
					input = append(input, map[string]any{
						"type":    "function_call_output",
						"call_id": block.ToolUseID,
						"output":  output,
					})
					if len(extra) > 0 {
						input = append(input, map[string]any{
							"type":    "message",
							"role":    "user",
							"content": extra,
						})
					}
				default:
					return nil, fmt.Errorf("unsupported user content block %q", block.Type)
				}
			case "assistant":
				switch block.Type {
				case "text":
					adjacent = append(adjacent, map[string]any{"type": "output_text", "text": block.Text})
				case "tool_use":
					flush()
					arguments := block.Input
					if len(arguments) == 0 {
						arguments = json.RawMessage(`{}`)
					}
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   block.ID,
						"name":      names.Provider(block.Name),
						"arguments": string(arguments),
					})
				case "thinking", "redacted_thinking":
					// Provider-specific thinking signatures are not replayable across vendors.
				default:
					return nil, fmt.Errorf("unsupported assistant content block %q", block.Type)
				}
			default:
				return nil, fmt.Errorf("unsupported message role %q", message.Role)
			}
		}
		flush()
	}
	return input, nil
}

func sourceURL(source *Source) (string, error) {
	if source == nil {
		return "", fmt.Errorf("image source is missing")
	}
	switch source.Type {
	case "url":
		if source.URL == "" {
			return "", fmt.Errorf("image URL is empty")
		}
		return source.URL, nil
	case "base64":
		if source.Data == "" {
			return "", fmt.Errorf("image base64 data is empty")
		}
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		return "data:" + mediaType + ";base64," + source.Data, nil
	default:
		return "", fmt.Errorf("unsupported image source type %q", source.Type)
	}
}

func toolResultOutput(block Block) (any, []map[string]any, error) {
	if len(block.Content) == 0 || string(block.Content) == "null" {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(block.Content, &text) == nil {
		return text, nil, nil
	}
	blocks, err := DecodeBlocks(block.Content)
	if err != nil {
		return nil, nil, err
	}
	var output []map[string]any
	for _, child := range blocks {
		switch child.Type {
		case "text":
			output = append(output, map[string]any{"type": "input_text", "text": child.Text})
		case "image":
			url, err := sourceURL(child.Source)
			if err != nil {
				return nil, nil, err
			}
			output = append(output, map[string]any{"type": "input_image", "image_url": url})
		case "document":
			file, err := inputFile(child)
			if err != nil {
				return nil, nil, err
			}
			output = append(output, file)
		case "tool_reference", "search_result":
			text, err := portableToolResultBlock(child)
			if err != nil {
				return nil, nil, err
			}
			output = append(output, map[string]any{"type": "input_text", "text": text})
		default:
			return nil, nil, fmt.Errorf("unsupported tool_result content block %q", child.Type)
		}
	}
	if len(output) == 0 {
		return "", nil, nil
	}
	return output, nil, nil
}

func inputFile(block Block) (map[string]any, error) {
	if block.Source == nil {
		return nil, errors.New("document source is missing")
	}
	filename := firstNonEmpty(block.Title, "document.pdf")
	switch block.Source.Type {
	case "base64":
		if block.Source.Data == "" {
			return nil, errors.New("document base64 data is empty")
		}
		mediaType := firstNonEmpty(block.Source.MediaType, "application/pdf")
		return map[string]any{
			"type":      "input_file",
			"filename":  filename,
			"file_data": "data:" + mediaType + ";base64," + block.Source.Data,
		}, nil
	case "url":
		if block.Source.URL == "" {
			return nil, errors.New("document URL is empty")
		}
		return map[string]any{
			"type":     "input_file",
			"filename": filename,
			"file_url": block.Source.URL,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported document source type %q", block.Source.Type)
	}
}

func responsesToolChoice(policy ToolPolicy, names *ToolNames) any {
	switch policy.Type {
	case "", "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "name": names.Provider(policy.Name)}
	default:
		return nil
	}
}

func responsesFormat(req *Request) map[string]any {
	raw := req.OutputFormat
	if len(raw) == 0 && len(req.OutputConfig) > 0 {
		var output struct {
			Format json.RawMessage `json:"format"`
		}
		if json.Unmarshal(req.OutputConfig, &output) == nil {
			raw = output.Format
		}
	}
	if len(raw) == 0 {
		return nil
	}
	var format map[string]any
	if json.Unmarshal(raw, &format) != nil || format["type"] != "json_schema" {
		return nil
	}
	schema, ok := format["schema"].(map[string]any)
	if !ok {
		return nil
	}
	return map[string]any{
		"type":   "json_schema",
		"name":   "structured_output",
		"schema": schema,
		"strict": true,
	}
}

type Attachment struct {
	Kind      string
	MediaType string
	Data      string
	URL       string
	Filename  string
}

func Transcript(req *Request) (string, error) {
	transcript, _, err := TranscriptWithAttachments(req)
	return transcript, err
}

func TranscriptWithAttachments(req *Request) (string, []Attachment, error) {
	var out strings.Builder
	var attachments []Attachment
	for _, message := range req.Messages {
		blocks, err := DecodeBlocks(message.Content)
		if err != nil {
			return "", nil, err
		}
		out.WriteString("<")
		out.WriteString(message.Role)
		out.WriteString(">\n")
		for _, block := range blocks {
			switch block.Type {
			case "text":
				out.WriteString(block.Text)
				out.WriteByte('\n')
			case "tool_use":
				out.WriteString("[tool_use id=")
				out.WriteString(block.ID)
				out.WriteString(" name=")
				out.WriteString(block.Name)
				out.WriteString(" input=")
				out.Write(block.Input)
				out.WriteString("]\n")
			case "tool_result":
				if err := appendTranscriptToolResult(&out, &attachments, block); err != nil {
					return "", nil, err
				}
			case "image":
				attachment, err := blockAttachment(block, len(attachments))
				if err != nil {
					return "", nil, err
				}
				attachments = append(attachments, attachment)
				fmt.Fprintf(&out, "[image attachment: %s]\n", attachment.Filename)
			case "document":
				attachment, err := blockAttachment(block, len(attachments))
				if err != nil {
					return "", nil, err
				}
				attachments = append(attachments, attachment)
				fmt.Fprintf(&out, "[document attachment: %s]\n", attachment.Filename)
			case "thinking", "redacted_thinking":
			default:
				return "", nil, fmt.Errorf("unsupported transcript block %q", block.Type)
			}
		}
		out.WriteString("</")
		out.WriteString(message.Role)
		out.WriteString(">\n\n")
	}
	return out.String(), attachments, nil
}

func appendTranscriptToolResult(out *strings.Builder, attachments *[]Attachment, block Block) error {
	out.WriteString("[tool_result id=")
	out.WriteString(block.ToolUseID)
	out.WriteString("]\n")
	if len(block.Content) == 0 || string(block.Content) == "null" {
		out.WriteString("[/tool_result]\n")
		return nil
	}
	var text string
	if json.Unmarshal(block.Content, &text) == nil {
		out.WriteString(text)
		out.WriteByte('\n')
		out.WriteString("[/tool_result]\n")
		return nil
	}
	children, err := DecodeBlocks(block.Content)
	if err != nil {
		return err
	}
	for _, child := range children {
		switch child.Type {
		case "text":
			out.WriteString(child.Text)
			out.WriteByte('\n')
		case "image", "document":
			attachment, err := blockAttachment(child, len(*attachments))
			if err != nil {
				return err
			}
			*attachments = append(*attachments, attachment)
			fmt.Fprintf(out, "[%s attachment: %s]\n", child.Type, attachment.Filename)
		case "tool_reference", "search_result":
			text, err := portableToolResultBlock(child)
			if err != nil {
				return err
			}
			out.WriteString(text)
			out.WriteByte('\n')
		default:
			return fmt.Errorf("unsupported tool_result transcript block %q", child.Type)
		}
	}
	out.WriteString("[/tool_result]\n")
	return nil
}

func portableToolResultBlock(block Block) (string, error) {
	switch block.Type {
	case "tool_reference":
		name := strings.TrimSpace(block.ToolName)
		if name == "" {
			return "", errors.New("tool_reference is missing tool_name")
		}
		raw, err := json.Marshal(map[string]string{
			"type":      "tool_reference",
			"tool_name": name,
		})
		if err != nil {
			return "", err
		}
		return string(raw), nil
	case "search_result":
		if len(block.Raw) == 0 {
			return "", errors.New("search_result content is empty")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, block.Raw); err != nil {
			return "", fmt.Errorf("encode search_result content: %w", err)
		}
		return compact.String(), nil
	default:
		return "", fmt.Errorf("unsupported portable tool result block %q", block.Type)
	}
}

func blockAttachment(block Block, index int) (Attachment, error) {
	if block.Source == nil {
		return Attachment{}, fmt.Errorf("%s source is missing", block.Type)
	}
	mediaType := strings.TrimSpace(block.Source.MediaType)
	if mediaType == "" {
		if block.Type == "image" {
			mediaType = "image/png"
		} else {
			mediaType = "application/pdf"
		}
	}
	extension := extensionForMediaType(mediaType)
	fallback := fmt.Sprintf("%s-%d%s", block.Type, index+1, extension)
	attachment := Attachment{
		Kind:      block.Type,
		MediaType: mediaType,
		Filename:  firstNonEmpty(block.Title, fallback),
	}
	switch block.Source.Type {
	case "base64":
		if strings.TrimSpace(block.Source.Data) == "" {
			return Attachment{}, fmt.Errorf("%s base64 data is empty", block.Type)
		}
		attachment.Data = block.Source.Data
	case "url":
		if strings.TrimSpace(block.Source.URL) == "" {
			return Attachment{}, fmt.Errorf("%s URL is empty", block.Type)
		}
		attachment.URL = block.Source.URL
	default:
		return Attachment{}, fmt.Errorf("unsupported %s source type %q", block.Type, block.Source.Type)
	}
	return attachment, nil
}

func extensionForMediaType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "application/json":
		return ".json"
	default:
		return ".bin"
	}
}

func EstimateInputTokens(req *Request) int {
	raw, _ := json.Marshal(req)
	if len(raw) == 0 {
		return 1
	}
	// A deterministic UTF-8 byte heuristic used only when the provider has no
	// token-count API. The caller labels this value as estimated.
	count := (len(raw) + 3) / 4
	if count < 1 {
		return 1
	}
	return count
}

func DecodeBase64(data string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(data)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
