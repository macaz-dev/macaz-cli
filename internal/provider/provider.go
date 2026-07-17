package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

type Provider interface {
	Name() string
	Generate(context.Context, *protocol.Request, protocol.EmitFunc) (protocol.Result, error)
	CountTokens(context.Context, *protocol.Request) (count int, estimated bool, err error)
	Models(context.Context) ([]Model, error)
	Check(context.Context) error
}

type Model struct {
	ID                  string   `json:"id"`
	DisplayName         string   `json:"display_name,omitempty"`
	Description         string   `json:"description,omitempty"`
	Default             bool     `json:"default,omitempty"`
	Efforts             []string `json:"efforts,omitempty"`
	InputModalities     []string `json:"input_modalities,omitempty"`
	OutputModalities    []string `json:"output_modalities,omitempty"`
	SupportedParameters []string `json:"supported_parameters,omitempty"`
	ContextWindow       int64    `json:"context_window,omitempty"`
	MaxOutputTokens     int64    `json:"max_output_tokens,omitempty"`
	Created             int64    `json:"created,omitempty"`
	ToolCall            bool     `json:"tool_call,omitempty"`
	StructuredOutput    bool     `json:"structured_output,omitempty"`
	Attachment          bool     `json:"attachment,omitempty"`
}

type HTTPError struct {
	Status     int
	Type       string
	Message    string
	Body       []byte
	RetryAfter time.Duration
	Cause      error
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("provider returned HTTP %d", e.Status)
}

func (e *HTTPError) Unwrap() error {
	return e.Cause
}

func InvalidRequest(err error) error {
	if err == nil {
		return nil
	}
	return &HTTPError{
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Message: err.Error(),
		Cause:   err,
	}
}

func Status(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.Status > 0 {
		return httpErr.Status
	}
	return http.StatusBadGateway
}
