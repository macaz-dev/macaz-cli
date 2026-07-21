package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

func IsContextWindowOverflow(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	message = strings.NewReplacer("_", " ", "-", " ").Replace(message)
	for _, marker := range []string{
		"context window",
		"context length",
		"maximum context",
		"too many tokens",
		"input is too long",
		"prompt is too long",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func ContextWindowOverflow(message string, body []byte) error {
	return &HTTPError{
		Status:  http.StatusRequestEntityTooLarge,
		Type:    "request_too_large",
		Message: message,
		Body:    body,
	}
}

func Timeout(message string) error {
	return &HTTPError{
		Status:  http.StatusGatewayTimeout,
		Type:    "timeout_error",
		Message: message,
	}
}

func StreamFailure(kind, message string) error {
	classification := strings.ToLower(strings.TrimSpace(kind + " " + message))
	if IsContextWindowOverflow(classification) {
		return ContextWindowOverflow(message, nil)
	}
	status := http.StatusBadGateway
	errorType := "api_error"
	switch {
	case strings.Contains(classification, "rate_limit") || strings.Contains(classification, "rate limit"):
		status = http.StatusTooManyRequests
		errorType = "rate_limit_error"
	case strings.Contains(classification, "timeout") || strings.Contains(classification, "timed out"):
		status = http.StatusGatewayTimeout
		errorType = "timeout_error"
	case strings.Contains(classification, "overload") || strings.Contains(classification, "unavailable"):
		status = http.StatusServiceUnavailable
		errorType = "overloaded_error"
	case strings.Contains(classification, "authentication") || strings.Contains(classification, "unauthorized"):
		status = http.StatusUnauthorized
		errorType = "authentication_error"
	case strings.Contains(classification, "permission") || strings.Contains(classification, "forbidden"):
		status = http.StatusForbidden
		errorType = "permission_error"
	case strings.Contains(classification, "invalid_request") || strings.Contains(classification, "invalid request"):
		status = http.StatusBadRequest
		errorType = "invalid_request_error"
	}
	return &HTTPError{Status: status, Type: errorType, Message: message}
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
