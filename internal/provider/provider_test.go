package provider

import (
	"net/http"
	"testing"
)

func TestStreamFailureClassification(t *testing.T) {
	tests := []struct {
		kind    string
		message string
		status  int
		typeID  string
	}{
		{kind: "context_length_exceeded", message: "maximum context length exceeded", status: http.StatusRequestEntityTooLarge, typeID: "request_too_large"},
		{kind: "rate_limit_exceeded", message: "slow down", status: http.StatusTooManyRequests, typeID: "rate_limit_error"},
		{kind: "provider_overloaded", message: "try later", status: http.StatusServiceUnavailable, typeID: "overloaded_error"},
		{kind: "server_error", message: "upstream failed", status: http.StatusBadGateway, typeID: "api_error"},
	}
	for _, test := range tests {
		err := StreamFailure(test.kind, test.message)
		if Status(err) != test.status {
			t.Fatalf("%s status = %d", test.kind, Status(err))
		}
		httpErr := err.(*HTTPError)
		if httpErr.Type != test.typeID {
			t.Fatalf("%s type = %q", test.kind, httpErr.Type)
		}
	}
}
