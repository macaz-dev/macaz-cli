package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Request struct {
	Model             string          `json:"model"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Messages          []Message       `json:"messages"`
	System            json.RawMessage `json:"system,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	StopSequences     []string        `json:"stop_sequences,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *int            `json:"top_k,omitempty"`
	Thinking          json.RawMessage `json:"thinking,omitempty"`
	OutputConfig      json.RawMessage `json:"output_config,omitempty"`
	OutputFormat      json.RawMessage `json:"output_format,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
	ServiceTier       string          `json:"service_tier,omitempty"`
	Speed             string          `json:"speed,omitempty"`
	// PromptCacheKey is derived by the local gateway from a trusted client
	// session header. It is provider routing metadata, not part of the
	// Anthropic Messages wire payload accepted from the client.
	PromptCacheKey string `json:"-"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type Tool struct {
	Type         string         `json:"type,omitempty"`
	Name         string         `json:"name,omitempty"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	DeferLoading bool           `json:"defer_loading,omitempty"`
	// ClientType records the caller-facing Responses tool kind without leaking
	// that kind into provider-native tool payloads. In particular, Codex custom
	// tools are represented upstream as an ordinary tool whose JSON input has a
	// single string field named "input".
	ClientType      string `json:"-"`
	ClientName      string `json:"-"`
	ClientNamespace string `json:"-"`
}

type Source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *Source         `json:"source,omitempty"`
	Title     string          `json:"title,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (b *Block) UnmarshalJSON(raw []byte) error {
	type blockAlias Block
	decodeRaw := raw
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if source := fields["source"]; len(source) > 0 {
		trimmed := strings.TrimSpace(string(source))
		if !strings.HasPrefix(trimmed, "{") && trimmed != "null" {
			delete(fields, "source")
			var err error
			decodeRaw, err = json.Marshal(fields)
			if err != nil {
				return err
			}
		}
	}
	var decoded blockAlias
	if err := json.Unmarshal(decodeRaw, &decoded); err != nil {
		return err
	}
	*b = Block(decoded)
	b.Raw = append(b.Raw[:0], raw...)
	return nil
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	ReasoningOutputTokens    int64 `json:"-"`
	Estimated                bool  `json:"-"`
}

type Result struct {
	ID         string
	Model      string
	Blocks     []Block
	StopReason string
	Usage      Usage
}

type EventKind int

const (
	EventBlockStart EventKind = iota + 1
	EventBlockDelta
	EventBlockStop
)

type Event struct {
	Kind      EventKind
	Index     int
	Block     Block
	DeltaType string
	Delta     string
}

type EmitFunc func(Event) error

func DecodeBlocks(raw json.RawMessage) ([]Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []Block{{Type: "text", Text: text}}, nil
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("decode content blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Type == "tool_use" && len(blocks[i].Input) == 0 {
			blocks[i].Input = json.RawMessage(`{}`)
		}
	}
	return blocks, nil
}

func SystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	blocks, err := DecodeBlocks(raw)
	if err != nil {
		return "", err
	}
	var out []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				out = append(out, block.Text)
			}
		default:
			return "", fmt.Errorf("unsupported system content block %q", block.Type)
		}
	}
	return strings.Join(out, "\n"), nil
}

func Effort(req *Request, fallback string) string {
	var output struct {
		Effort string `json:"effort"`
	}
	if len(req.OutputConfig) > 0 {
		_ = json.Unmarshal(req.OutputConfig, &output)
	}
	effort := strings.ToLower(strings.TrimSpace(output.Effort))
	if effort == "" && len(req.Thinking) > 0 {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if json.Unmarshal(req.Thinking, &thinking) == nil {
			switch {
			case thinking.Type == "adaptive":
				effort = fallback
			case thinking.BudgetTokens >= 64000:
				effort = "max"
			case thinking.BudgetTokens >= 32000:
				effort = "xhigh"
			case thinking.BudgetTokens >= 16000:
				effort = "high"
			case thinking.BudgetTokens >= 4000:
				effort = "medium"
			case thinking.BudgetTokens > 0:
				effort = "low"
			}
		}
	}
	if effort == "" {
		effort = fallback
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return effort
	default:
		return "high"
	}
}

func ServiceTier(req *Request) string {
	if req == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(req.Speed), "fast") {
		return "priority"
	}
	switch value := strings.ToLower(strings.TrimSpace(req.ServiceTier)); value {
	case "fast":
		return "priority"
	case "auto", "default", "flex", "priority":
		return value
	default:
		return ""
	}
}
