package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/macaz-dev/macaz-cli/internal/attachments"
	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
	"github.com/macaz-dev/macaz-cli/internal/provider/openresponses"
	"github.com/macaz-dev/macaz-cli/internal/testmedia"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type contextReadCloser struct {
	ctx context.Context
}

func (r contextReadCloser) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (contextReadCloser) Close() error { return nil }

func TestIdleReadCloserCancelsStalledStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := openresponses.NewIdleReadCloser(contextReadCloser{ctx: ctx}, cancel, 5*time.Millisecond)
	defer reader.Close()
	_, err := reader.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("idle read error = %v", err)
	}
}

func TestSubscriptionRejectsTruncatedStreamAndMapsContextOverflow(t *testing.T) {
	keyring.MockInit()
	if err := saveAccountCredential(accountCredentials{
		Type: accountCredentialType, Method: accountCredentialMethod,
		Access: "access-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenAISubscription
	cfg.ModelMap["default"] = "gpt-test"
	upstream, err := New(ModeSubscription, cfg)
	if err != nil {
		t.Fatal(err)
	}
	responseBody := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"partial\"}\n\n"
	upstream.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody)), Request: req}, nil
	})}
	upstream.account.httpClient = upstream.httpClient
	request := &protocol.Request{Model: "gpt-test", Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}}
	if _, err := upstream.Generate(context.Background(), request, nil); err == nil || !strings.Contains(err.Error(), "without a terminal") {
		t.Fatalf("truncated stream error = %v", err)
	}

	upstream.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"detail":"maximum context window exceeded"}`)), Request: req,
		}, nil
	})}
	upstream.account.httpClient = upstream.httpClient
	_, err = upstream.Generate(context.Background(), request, nil)
	if provider.Status(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("context overflow status = %d, err = %v", provider.Status(err), err)
	}
}

func TestSubscriptionRefreshesOnceAfterStaleToken401(t *testing.T) {
	keyring.MockInit()
	if err := saveAccountCredential(accountCredentials{
		Type: accountCredentialType, Method: accountCredentialMethod,
		Access: "stale-access", Refresh: "refresh-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	var responseAttempts atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == accountTokenEndpoint {
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
				Body: io.NopCloser(strings.NewReader(`{"access_token":"fresh-access","refresh_token":"refresh-token","expires_in":3600}`)),
			}, nil
		}
		responseAttempts.Add(1)
		if req.Header.Get("Authorization") == "Bearer stale-access" {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"expired"}`)), Request: req}, nil
		}
		if req.Header.Get("Authorization") != "Bearer fresh-access" {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\"}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream)), Request: req}, nil
	})}
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenAISubscription
	upstream, err := New(ModeSubscription, cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream.httpClient = client
	upstream.account.httpClient = client
	_, err = upstream.Generate(context.Background(), &protocol.Request{
		Model: "gpt-test", Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if responseAttempts.Load() != 2 {
		t.Fatalf("response attempts = %d", responseAttempts.Load())
	}
}

func TestSubscriptionModelsUsesLiveCodexCatalog(t *testing.T) {
	keyring.MockInit()
	if err := saveAccountCredential(accountCredentials{
		Type:      accountCredentialType,
		Method:    accountCredentialMethod,
		Access:    "access-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		AccountID: "account-123",
	}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/codex/models" {
			t.Fatalf("path = %q", req.URL.Path)
		}
		if req.URL.Query().Get("client_version") != accountClientVersion {
			t.Fatalf("client_version = %q", req.URL.Query().Get("client_version"))
		}
		for header, want := range map[string]string{
			"Authorization":      "Bearer access-token",
			"ChatGPT-Account-Id": "account-123",
			"originator":         "macaz",
			"version":            accountClientVersion,
		} {
			if got := req.Header.Get(header); got != want {
				t.Fatalf("%s = %q, want %q", header, got, want)
			}
		}
		body := `{"models":[
			{"slug":"hidden","display_name":"Hidden","visibility":"hide","priority":0},
			{"slug":"gpt-live","display_name":"GPT Live","description":"live catalog","visibility":"list","priority":1,"context_window":300000,"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],"input_modalities":["text","image"]},
			{"slug":"gpt-later","display_name":"GPT Later","visibility":"list","priority":2,"supported_reasoning_levels":[{"effort":"medium"}],"input_modalities":["text"]}
		]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenAISubscription
	upstream, err := New(ModeSubscription, cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream.httpClient = client
	upstream.account.httpClient = client
	models, err := upstream.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ID != "gpt-live" || !models[0].Default {
		t.Fatalf("default model = %#v", models[0])
	}
	if got := strings.Join(models[0].Efforts, ","); got != "low,high" {
		t.Fatalf("efforts = %q", got)
	}
	if !models[0].Attachment || models[0].ContextWindow != 300000 {
		t.Fatalf("capabilities = %#v", models[0])
	}
	if err := upstream.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIProviderModelUsesCapabilityMetadata(t *testing.T) {
	metadata := openAIModelMetadata{
		ID:               "gpt-agent",
		Name:             "GPT Agent",
		Description:      "Agentic text model",
		Attachment:       true,
		ToolCall:         true,
		StructuredOutput: true,
	}
	metadata.Modalities.Input = []string{"text", "image", "pdf"}
	metadata.Modalities.Output = []string{"text"}
	metadata.Limit.Context = 400_000
	metadata.Limit.Output = 32_000
	metadata.ReasoningOptions = append(metadata.ReasoningOptions, struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	}{Type: "effort", Values: []string{"low", "high"}})

	model, ok := openAIProviderModel("gpt-agent", 123, "gpt-agent", metadata, true)
	if !ok {
		t.Fatal("capable model was filtered")
	}
	if model.DisplayName != "GPT Agent" || !model.Default || !model.ToolCall || !model.Attachment {
		t.Fatalf("model = %#v", model)
	}
	if got := strings.Join(model.InputModalities, ","); got != "text,image,file" {
		t.Fatalf("input modalities = %q", got)
	}
	if got := strings.Join(model.Efforts, ","); got != "low,high" {
		t.Fatalf("efforts = %q", got)
	}
	if model.ContextWindow != 400_000 || model.MaxOutputTokens != 32_000 {
		t.Fatalf("limits = %#v", model)
	}
}

func TestOpenAIProviderModelFiltersNonAgentModels(t *testing.T) {
	metadata := openAIModelMetadata{ID: "text-embedding-test"}
	metadata.Modalities.Output = []string{"text"}
	if _, ok := openAIProviderModel("text-embedding-test", 0, "", metadata, true); ok {
		t.Fatal("metadata model without tool calling was exposed")
	}
	for _, id := range []string{
		"text-embedding-3-large",
		"gpt-image-1",
		"gpt-4o-realtime-preview",
		"gpt-4o-mini-transcribe",
		"omni-moderation-latest",
	} {
		if likelyOpenAIResponsesModel(id) {
			t.Fatalf("%q was accepted by conservative fallback", id)
		}
	}
	for _, id := range []string{"gpt-5.6", "o3", "codex-mini-latest", "ft:gpt-5:org:name"} {
		if !likelyOpenAIResponsesModel(id) {
			t.Fatalf("%q was rejected by conservative fallback", id)
		}
	}
}

func TestOfficialOpenAIBaseURLRecognition(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.openai.com/v1",
		"https://eu.api.openai.com/v1",
	} {
		if !isOfficialOpenAIBaseURL(endpoint) {
			t.Fatalf("%q was not recognized", endpoint)
		}
	}
	if isOfficialOpenAIBaseURL("https://openai.example.test/v1") {
		t.Fatal("custom endpoint was recognized as official")
	}
}

func TestSanitizeSubscriptionRemovesUnsupportedPublicFields(t *testing.T) {
	body := map[string]any{
		"store":                true,
		"stream":               false,
		"user":                 "claude-user-id",
		"max_output_tokens":    4096,
		"truncation":           "auto",
		"previous_response_id": "resp_previous",
		"model":                "gpt-5.5",
		"input":                []any{},
	}
	sanitizeSubscription(body)
	for _, key := range []string{"user", "max_output_tokens", "truncation", "previous_response_id"} {
		if _, exists := body[key]; exists {
			t.Fatalf("subscription body retained unsupported field %q: %#v", key, body)
		}
	}
	if body["store"] != false || body["stream"] != true {
		t.Fatalf("subscription flags = %#v", body)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("subscription parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}
	if body["model"] != "gpt-5.5" {
		t.Fatalf("model was changed: %#v", body)
	}
}

func TestNormalizeSubscriptionDocumentsUsesBoundedTextFallback(t *testing.T) {
	content := mustContent(t,
		protocol.Block{Type: "text", Text: "read the document"},
		protocol.Block{
			Type: "image",
			Source: &protocol.Source{
				Type:      "base64",
				MediaType: "image/png",
				Data:      "aW1hZ2U=",
			},
		},
		protocol.Block{
			Type:  "document",
			Title: "contract.pdf",
			Source: &protocol.Source{
				Type:      "base64",
				MediaType: "application/pdf",
				Data: base64.StdEncoding.EncodeToString(
					testmedia.TextPDF("MACAZ_SUBSCRIPTION_DOCUMENT_OK"),
				),
			},
		},
	)
	translated, err := protocol.ToResponses(&protocol.Request{
		Model:    "gpt-test",
		Messages: []protocol.Message{{Role: "user", Content: content}},
	}, "gpt-test", "high")
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeSubscriptionDocuments(
		context.Background(),
		translated.Body,
		attachments.DefaultMaxBytes,
	); err != nil {
		t.Fatal(err)
	}
	var imageFound bool
	var documentTextFound bool
	var nativeFileFound bool
	walkJSON(translated.Body["input"], func(value map[string]any) {
		switch value["type"] {
		case "input_image":
			imageFound = value["image_url"] == "data:image/png;base64,aW1hZ2U="
		case "input_file":
			nativeFileFound = true
		case "input_text":
			text, _ := value["text"].(string)
			documentTextFound = documentTextFound ||
				strings.Contains(text, "MACAZ_SUBSCRIPTION_DOCUMENT_OK") &&
					strings.Contains(text, `filename="contract.pdf"`)
		}
	})
	if !imageFound || !documentTextFound || nativeFileFound {
		t.Fatalf(
			"normalized subscription input: image=%t document_text=%t native_file=%t body=%#v",
			imageFound,
			documentTextFound,
			nativeFileFound,
			translated.Body["input"],
		)
	}
}

func TestNormalizeSubscriptionDocumentsRejectsImageOnlyPDF(t *testing.T) {
	body := map[string]any{
		"input": []any{map[string]any{
			"type": "message",
			"content": []map[string]any{{
				"type":     "input_file",
				"filename": "scan.pdf",
				"file_data": "data:application/pdf;base64," +
					base64.StdEncoding.EncodeToString(testmedia.TextPDF("")),
			}},
		}},
	}
	err := normalizeSubscriptionDocuments(context.Background(), body, attachments.DefaultMaxBytes)
	if err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizeSubscriptionDocumentsInsideToolResults(t *testing.T) {
	documentContent := mustContent(t, protocol.Block{
		Type:  "document",
		Title: "tool-result.pdf",
		Source: &protocol.Source{
			Type:      "base64",
			MediaType: "application/pdf",
			Data: base64.StdEncoding.EncodeToString(
				testmedia.TextPDF("MACAZ_TOOL_RESULT_DOCUMENT_OK"),
			),
		},
	})
	translated, err := protocol.ToResponses(&protocol.Request{
		Model: "gpt-test",
		Messages: []protocol.Message{
			{Role: "user", Content: json.RawMessage(`"read it"`)},
			{Role: "assistant", Content: mustContent(t, protocol.Block{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "Read",
				Input: json.RawMessage(`{"path":"tool-result.pdf"}`),
			})},
			{Role: "user", Content: mustContent(t, protocol.Block{
				Type:      "tool_result",
				ToolUseID: "call_1",
				Content:   documentContent,
			})},
		},
	}, "gpt-test", "high")
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeSubscriptionDocuments(
		context.Background(),
		translated.Body,
		attachments.DefaultMaxBytes,
	); err != nil {
		t.Fatal(err)
	}
	var tokenFound bool
	var nativeFileFound bool
	walkJSON(translated.Body["input"], func(value map[string]any) {
		switch value["type"] {
		case "input_file":
			nativeFileFound = true
		case "input_text":
			text, _ := value["text"].(string)
			tokenFound = tokenFound || strings.Contains(text, "MACAZ_TOOL_RESULT_DOCUMENT_OK")
		}
	})
	if !tokenFound || nativeFileFound {
		t.Fatalf(
			"normalized tool result: token=%t native_file=%t input=%#v",
			tokenFound,
			nativeFileFound,
			translated.Body["input"],
		)
	}
}

func TestNormalizeSubscriptionDocumentURLInfersTextMediaType(t *testing.T) {
	document := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "MACAZ_URL_DOCUMENT_OK")
	}))
	defer document.Close()
	body := map[string]any{
		"input": []any{map[string]any{
			"type": "message",
			"content": []map[string]any{{
				"type":     "input_file",
				"filename": "notes.txt",
				"file_url": document.URL + "/notes.txt",
			}},
		}},
	}
	if err := normalizeSubscriptionDocuments(
		context.Background(),
		body,
		attachments.DefaultMaxBytes,
	); err != nil {
		t.Fatal(err)
	}
	var tokenFound bool
	walkJSON(body["input"], func(value map[string]any) {
		if value["type"] == "input_text" {
			text, _ := value["text"].(string)
			tokenFound = tokenFound || strings.Contains(text, "MACAZ_URL_DOCUMENT_OK")
		}
	})
	if !tokenFound {
		t.Fatalf("normalized URL document input = %#v", body["input"])
	}
}

func TestSubscriptionRetriesRateLimitsAndSerializesConcurrentRequests(t *testing.T) {
	keyring.MockInit()
	if err := saveAccountCredential(accountCredentials{
		Type:      accountCredentialType,
		Method:    accountCredentialMethod,
		Access:    "access-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	var active atomic.Int64
	var maximum atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			prior := maximum.Load()
			if current <= prior || maximum.CompareAndSwap(prior, current) {
				break
			}
		}
		defer active.Add(-1)
		time.Sleep(5 * time.Millisecond)
		number := attempts.Add(1)
		if number == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"0"}},
				Body:       io.NopCloser(strings.NewReader(`{"detail":"Rate limit exceeded"}`)),
				Request:    req,
			}, nil
		}
		stream := "" +
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"ok\"}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    req,
		}, nil
	})}
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenAISubscription
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = "gpt-test"
	}
	upstream, err := New(ModeSubscription, cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream.httpClient = client
	upstream.account.httpClient = client
	upstream.retryBase = time.Millisecond

	req := &protocol.Request{
		Model:    "gpt-test",
		Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := upstream.Generate(context.Background(), req, nil)
			if err == nil && (len(result.Blocks) != 1 || result.Blocks[0].Text != "ok") {
				err = errors.New("unexpected subscription response")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent subscription requests = %d, want 1", maximum.Load())
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want one retry plus second request", attempts.Load())
	}
}

func TestSubscriptionGateIsScopedToPromptCacheKey(t *testing.T) {
	upstream, err := New(ModeSubscription, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := upstream.acquireGenerate(context.Background(), &protocol.Request{PromptCacheKey: "session-a"})
	if err != nil {
		t.Fatal(err)
	}

	sameKey := make(chan error, 1)
	go func() {
		release, acquireErr := upstream.acquireGenerate(context.Background(), &protocol.Request{PromptCacheKey: "session-a"})
		if acquireErr == nil {
			release()
		}
		sameKey <- acquireErr
	}()
	select {
	case err := <-sameKey:
		t.Fatalf("same session was not serialized: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	differentCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseDifferent, err := upstream.acquireGenerate(differentCtx, &protocol.Request{PromptCacheKey: "session-b"})
	if err != nil {
		t.Fatalf("independent session was blocked: %v", err)
	}
	releaseDifferent()
	releaseFirst()
	select {
	case err := <-sameKey:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("same session did not resume after release")
	}
}

func TestSubscriptionGateAppliesGlobalAccountLimit(t *testing.T) {
	cfg := config.Default()
	cfg.MaxConcurrentSubscription = 1
	upstream, err := New(ModeSubscription, cfg)
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := upstream.acquireGenerate(context.Background(), &protocol.Request{PromptCacheKey: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := upstream.acquireGenerate(ctx, &protocol.Request{PromptCacheKey: "session-b"}); err == nil {
		release()
		t.Fatal("independent session bypassed the subscription account limit")
	}
	releaseFirst()
}

func TestSubscriptionRetryExhaustionIsBounded(t *testing.T) {
	keyring.MockInit()
	if err := saveAccountCredential(accountCredentials{
		Type:      accountCredentialType,
		Method:    accountCredentialMethod,
		Access:    "access-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"0"}},
			Body:       io.NopCloser(strings.NewReader(`{"detail":"Rate limit exceeded"}`)),
			Request:    req,
		}, nil
	})}
	cfg := config.Default()
	cfg.Provider = config.ProviderOpenAISubscription
	cfg.ModelMap["default"] = "gpt-test"
	upstream, err := New(ModeSubscription, cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream.httpClient = client
	upstream.account.httpClient = client
	upstream.retryBase = time.Millisecond

	_, err = upstream.Generate(context.Background(), &protocol.Request{
		Model:    "gpt-test",
		Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}, nil)
	if err == nil {
		t.Fatal("expected rate-limit failure")
	}
	if provider.Status(err) != http.StatusTooManyRequests {
		t.Fatalf("status = %d, err = %v", provider.Status(err), err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestCollectorPreservesTextToolTextOrder(t *testing.T) {
	stream := "" +
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"before\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"id\":\"m1\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"call_1\",\"name\":\"Read\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc1\",\"delta\":\"{\\\"path\\\":\\\"a\\\"}\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"m2\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m2\",\"delta\":\"after\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"id\":\"m2\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	names := protocol.NewToolNames([]protocol.Tool{{Name: "Read"}})
	collector := openresponses.NewCollector("gpt-test", names, false, nil)
	if err := openresponses.ReadSSE(bytes.NewBufferString(stream), collector.Handle); err != nil {
		t.Fatal(err)
	}
	result := collector.Result()
	if len(result.Blocks) != 3 {
		t.Fatalf("blocks = %#v", result.Blocks)
	}
	if result.Blocks[0].Text != "before" || result.Blocks[1].Type != "tool_use" || result.Blocks[2].Text != "after" {
		t.Fatalf("unexpected order: %#v", result.Blocks)
	}
	var input map[string]any
	if err := json.Unmarshal(result.Blocks[1].Input, &input); err != nil {
		t.Fatal(err)
	}
	if input["path"] != "a" {
		t.Fatalf("tool input = %#v", input)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}

func TestLiveOpenAIAPIIntegration(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("MACAZ_OPENAI_API_INTEGRATION_MODEL"))
	if model == "" {
		t.Skip("set MACAZ_OPENAI_API_INTEGRATION_MODEL to run the live OpenAI API smoke test")
	}
	testLiveProvider(t, ModeAPIKey, model)
}

func TestLiveOpenAISubscriptionIntegration(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("MACAZ_OPENAI_SUBSCRIPTION_INTEGRATION_MODEL"))
	if model == "" {
		t.Skip("set MACAZ_OPENAI_SUBSCRIPTION_INTEGRATION_MODEL to run the live subscription smoke test")
	}
	testLiveProvider(t, ModeSubscription, model)
}

func testLiveProvider(t *testing.T, mode Mode, model string) {
	t.Helper()
	cfg := config.Default()
	if mode == ModeSubscription {
		cfg.Provider = config.ProviderOpenAISubscription
	} else {
		cfg.Provider = config.ProviderOpenAIAPIKey
	}
	cfg.OpenAIModel = model
	for _, alias := range []string{"default", "opus", "sonnet", "haiku"} {
		cfg.ModelMap[alias] = model
	}
	upstream, err := New(mode, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := upstream.Check(ctx); err != nil {
		t.Fatal(err)
	}
	models, err := upstream.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := modelByID(models, model)
	if !ok {
		t.Fatalf("configured model %q is absent from live catalog", model)
	}

	t.Run("text", func(t *testing.T) {
		generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer generateCancel()
		result, err := upstream.Generate(generateCtx, &protocol.Request{
			Model:     model,
			MaxTokens: 64,
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Reply with exactly MACAZ_CONTEXT_OK."`),
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !resultContainsText(result, "MACAZ_CONTEXT_OK") {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("forced_tool", func(t *testing.T) {
		generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer generateCancel()
		result, err := upstream.Generate(generateCtx, &protocol.Request{
			Model:     model,
			MaxTokens: 128,
			Messages: []protocol.Message{{
				Role:    "user",
				Content: json.RawMessage(`"Use the Read tool for /tmp/macaz-live-provider-test and do not answer with text."`),
			}},
			Tools: []protocol.Tool{{
				Name:        "Read",
				Description: "Read a local file",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			}},
			ToolChoice: json.RawMessage(`{"type":"tool","name":"Read","disable_parallel_tool_use":true}`),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range result.Blocks {
			if block.Type == "tool_use" && block.Name == "Read" && json.Valid(block.Input) {
				return
			}
		}
		t.Fatalf("result contains no valid Read tool call: %#v", result)
	})

	if hasInputModality(selected, "image") {
		t.Run("image", func(t *testing.T) {
			imageData, err := testmedia.TwoColorPNG()
			if err != nil {
				t.Fatal(err)
			}
			content := mustContent(t,
				protocol.Block{
					Type: "text",
					Text: "Inspect the image. Reply with the two visible half colors as lowercase words.",
				},
				protocol.Block{
					Type: "image",
					Source: &protocol.Source{
						Type:      "base64",
						MediaType: "image/png",
						Data:      base64.StdEncoding.EncodeToString(imageData),
					},
				},
			)
			generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer generateCancel()
			result, err := upstream.Generate(generateCtx, &protocol.Request{
				Model:     model,
				MaxTokens: 128,
				Messages:  []protocol.Message{{Role: "user", Content: content}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.ToLower(resultText(result))
			if !strings.Contains(text, "red") || !strings.Contains(text, "blue") {
				t.Fatalf("image response = %q, result = %#v", text, result)
			}
		})
	} else {
		t.Logf("model %q does not advertise image input; image smoke test skipped", model)
	}

	t.Run("document", func(t *testing.T) {
		content := mustContent(t,
			protocol.Block{
				Type: "text",
				Text: "Read the attached PDF and reply with only its uppercase verification token.",
			},
			protocol.Block{
				Type:  "document",
				Title: "macaz-live-document.pdf",
				Source: &protocol.Source{
					Type:      "base64",
					MediaType: "application/pdf",
					Data: base64.StdEncoding.EncodeToString(
						testmedia.TextPDF("MACAZ_DOCUMENT_OK"),
					),
				},
			},
		)
		generateCtx, generateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer generateCancel()
		result, err := upstream.Generate(generateCtx, &protocol.Request{
			Model:     model,
			MaxTokens: 128,
			Messages:  []protocol.Message{{Role: "user", Content: content}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !resultContainsText(result, "MACAZ_DOCUMENT_OK") {
			t.Fatalf("document result = %#v", result)
		}
	})
}

func hasModel(models []provider.Model, id string) bool {
	_, ok := modelByID(models, id)
	return ok
}

func modelByID(models []provider.Model, id string) (provider.Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return provider.Model{}, false
}

func hasInputModality(model provider.Model, target string) bool {
	for _, modality := range model.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), target) {
			return true
		}
	}
	return false
}

func mustContent(t *testing.T, blocks ...protocol.Block) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func resultContainsText(result protocol.Result, expected string) bool {
	for _, block := range result.Blocks {
		if block.Type == "text" && strings.Contains(block.Text, expected) {
			return true
		}
	}
	return false
}

func resultText(result protocol.Result) string {
	var text strings.Builder
	for _, block := range result.Blocks {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func walkJSON(value any, visit func(map[string]any)) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			walkJSON(item, visit)
		}
	case []map[string]any:
		for _, item := range value {
			walkJSON(item, visit)
		}
	case map[string]any:
		visit(value)
		for _, item := range value {
			walkJSON(item, visit)
		}
	}
}
