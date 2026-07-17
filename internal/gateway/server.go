package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
	"github.com/macaz-dev/macaz-cli/internal/provider"
)

type Server struct {
	cfg      config.Config
	client   string
	provider provider.Provider
	token    string
	listener net.Listener
	http     *http.Server
	wg       sync.WaitGroup
	statsMu  sync.Mutex
	stats    sessionStats
	modelMu  sync.RWMutex
	models   map[string]provider.Model
}

type ModelCatalog struct {
	IDs             []string
	Models          []provider.Model
	Default         string
	DefaultUpstream string
	UpstreamByID    map[string]string
}

type sessionStats struct {
	Requests      int64
	Failures      int64
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
	Reasoning     int64
	LastModel     string
	LastError     string
}

func New(cfg config.Config, upstream provider.Provider) (*Server, error) {
	return NewForClient(cfg, upstream, config.ClientClaude)
}

func NewForClient(cfg config.Config, upstream provider.Provider, client string) (*Server, error) {
	if err := config.ValidateClient(client); err != nil {
		return nil, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	server := &Server{
		cfg:      cfg,
		client:   client,
		provider: upstream,
		token:    token,
		models:   map[string]provider.Model{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/v1/messages", server.auth(server.handleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", server.auth(server.handleCountTokens))
	mux.HandleFunc("/v1/responses", server.auth(server.handleResponses))
	mux.HandleFunc("/v1/models", server.auth(server.handleModels))
	mux.HandleFunc("/v1/models/", server.auth(server.handleModel))
	mux.HandleFunc("/v1/status", server.auth(server.handleStatus))
	mux.HandleFunc("/v1/usage", server.auth(server.handleUsage))
	mux.HandleFunc("/", server.handleNotFound)
	server.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	return server, nil
}

func (s *Server) Start() error {
	if s.listener != nil {
		return errors.New("gateway is already started")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	s.listener = listener
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("macaz local service stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

func (s *Server) PrimeModels(ctx context.Context) (ModelCatalog, error) {
	models, err := s.provider.Models(ctx)
	if err != nil {
		return ModelCatalog{}, err
	}
	if len(models) == 0 {
		return ModelCatalog{}, errors.New("provider returned no models")
	}
	return s.installModels(models), nil
}

func (s *Server) URL() string {
	if s.listener == nil {
		return ""
	}
	return "http://" + s.listener.Addr().String()
}

func (s *Server) Token() string {
	return s.token
}

func (s *Server) Close(ctx context.Context) error {
	if s.listener == nil {
		return nil
	}
	err := s.http.Shutdown(ctx)
	s.wg.Wait()
	return err
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.Header.Get("x-api-key"))
		if token == "" {
			auth := strings.TrimSpace(r.Header.Get("Authorization"))
			token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
		if token != s.token {
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid macaz loopback token")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"provider": s.provider.Name(),
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found_error", "unsupported macaz endpoint "+r.URL.Path)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	req, err := s.decodeRequest(w, r)
	if err != nil {
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if req.MaxTokens < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens cannot be negative")
		return
	}
	requestedModel := req.Model
	upstreamModel, ok := s.resolveModel(requestedModel)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is not available in the active macaz provider")
		return
	}
	req.Model = upstreamModel
	restrictRecursiveSubagentTools(req)
	if req.Stream {
		s.streamMessages(w, r, req, requestedModel)
		return
	}
	result, err := s.provider.Generate(r.Context(), req, nil)
	if err != nil {
		s.recordFailure(err)
		writeProviderError(w, err)
		return
	}
	if requestedModel != req.Model {
		result.Model = requestedModel
	}
	s.recordResult(result)
	writeJSON(w, http.StatusOK, messageResponse(result, requestedModel))
}

func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, req *protocol.Request, requestedModel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this HTTP server")
		return
	}
	messageID := "msg_" + mustRandomToken(12)
	model := requestedModel
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	writeSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": protocol.Usage{
				InputTokens:  0,
				OutputTokens: 0,
			},
		},
	})
	flusher.Flush()

	started := map[int]protocol.Block{}
	emit := func(event protocol.Event) error {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}
		switch event.Kind {
		case protocol.EventBlockStart:
			started[event.Index] = event.Block
			writeSSE(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         event.Index,
				"content_block": streamStartBlock(event.Block),
			})
		case protocol.EventBlockDelta:
			delta := map[string]any{"type": event.DeltaType}
			switch event.DeltaType {
			case "text_delta":
				delta["text"] = event.Delta
			case "thinking_delta":
				delta["thinking"] = event.Delta
			case "signature_delta":
				delta["signature"] = event.Delta
			case "input_json_delta":
				delta["partial_json"] = event.Delta
			default:
				return fmt.Errorf("unsupported stream delta %q", event.DeltaType)
			}
			writeSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": event.Index,
				"delta": delta,
			})
		case protocol.EventBlockStop:
			if block := started[event.Index]; block.Type == "thinking" && block.Signature != "" {
				writeSSE(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": event.Index,
					"delta": map[string]any{
						"type":      "signature_delta",
						"signature": block.Signature,
					},
				})
			}
			writeSSE(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": event.Index,
			})
			delete(started, event.Index)
		}
		flusher.Flush()
		return nil
	}

	result, err := s.provider.Generate(r.Context(), req, emit)
	if err != nil {
		s.recordFailure(err)
		writeSSE(w, "error", anthropicError("api_error", err.Error()))
		flusher.Flush()
		return
	}
	if requestedModel != req.Model {
		result.Model = requestedModel
	}
	s.recordResult(result)
	for index := range started {
		_ = emit(protocol.Event{Kind: protocol.EventBlockStop, Index: index})
	}
	writeSSE(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   first(result.StopReason, "end_turn"),
			"stop_sequence": nil,
		},
		"usage": result.Usage,
	})
	writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	flusher.Flush()
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	req, err := s.decodeRequest(w, r)
	if err != nil {
		return
	}
	upstreamModel, ok := s.resolveModel(req.Model)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is not available in the active macaz provider")
		return
	}
	req.Model = upstreamModel
	restrictRecursiveSubagentTools(req)
	count, estimated, err := s.provider.CountTokens(r.Context(), req)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	if estimated {
		w.Header().Set("X-Macaz-Token-Count-Estimated", "true")
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": count})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	models, err := s.provider.Models(r.Context())
	if err != nil {
		writeProviderError(w, err)
		return
	}
	catalog := s.installModels(models)
	data := make([]map[string]any, 0, len(models))
	for index, model := range models {
		publicID := catalog.IDs[index]
		data = append(data, map[string]any{
			"id":               publicID,
			"type":             "model",
			"display_name":     first(model.DisplayName, model.ID) + " · " + s.provider.Name(),
			"created_at":       modelCreatedAt(model.Created),
			"description":      model.Description,
			"default":          model.Default,
			"efforts":          model.Efforts,
			"input_modalities": model.InputModalities,
		})
	}
	response := map[string]any{
		"data":     data,
		"has_more": false,
	}
	if len(models) > 0 {
		response["first_id"] = data[0]["id"]
		response["last_id"] = data[len(data)-1]["id"]
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) installModels(models []provider.Model) ModelCatalog {
	catalog := ModelCatalog{
		IDs:          make([]string, len(models)),
		Models:       make([]provider.Model, len(models)),
		UpstreamByID: make(map[string]string, len(models)),
	}
	selected := strings.TrimSpace(s.cfg.ResolveModel("default"))
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	s.models = make(map[string]provider.Model, len(models))
	usedPublicIDs := make(map[string]bool, len(models))
	for index, model := range models {
		baseID := publicModelID(s.client, model.ID, index)
		publicID := baseID
		for suffix := 2; usedPublicIDs[publicID]; suffix++ {
			publicID = baseID + "-" + strconv.Itoa(suffix)
		}
		usedPublicIDs[publicID] = true
		catalog.IDs[index] = publicID
		catalog.Models[index] = model
		catalog.Models[index].ID = catalog.IDs[index]
		catalog.UpstreamByID[catalog.IDs[index]] = model.ID
		s.models[catalog.IDs[index]] = model
		if catalog.Default == "" && (model.Default || strings.EqualFold(strings.TrimSpace(model.ID), selected)) {
			catalog.Default = catalog.IDs[index]
			catalog.DefaultUpstream = model.ID
		}
	}
	if catalog.Default == "" && len(catalog.IDs) > 0 {
		catalog.Default = catalog.IDs[0]
		catalog.DefaultUpstream = models[0].ID
	}
	for index := range catalog.Models {
		catalog.Models[index].Default = catalog.Models[index].ID == catalog.Default
	}
	return catalog
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found_error", "model not found")
		return
	}
	s.modelMu.RLock()
	model, ok := s.models[id]
	s.modelMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found_error", "model not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"type":             "model",
		"display_name":     first(model.DisplayName, model.ID) + " · " + s.provider.Name(),
		"created_at":       modelCreatedAt(model.Created),
		"description":      model.Description,
		"default":          model.Default,
		"efforts":          model.Efforts,
		"input_modalities": model.InputModalities,
	})
}

func modelCreatedAt(created int64) string {
	if created <= 0 {
		return "1970-01-01T00:00:00Z"
	}
	return time.Unix(created, 0).UTC().Format(time.RFC3339)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	checkCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.provider.Check(checkCtx); err != nil {
		writeProviderError(w, err)
		return
	}
	s.statsMu.Lock()
	stats := s.stats
	s.statsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"provider":   s.provider.Name(),
		"last_model": stats.LastModel,
	})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	s.statsMu.Lock()
	stats := s.stats
	s.statsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":                    stats.Requests,
		"failures":                    stats.Failures,
		"input_tokens":                stats.InputTokens,
		"output_tokens":               stats.OutputTokens,
		"cache_read_input_tokens":     stats.CacheRead,
		"cache_creation_input_tokens": stats.CacheCreation,
		"reasoning_output_tokens":     stats.Reasoning,
		"last_model":                  stats.LastModel,
		"last_error":                  stats.LastError,
	})
}

func (s *Server) recordResult(result protocol.Result) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.Requests++
	s.stats.InputTokens += result.Usage.InputTokens
	s.stats.OutputTokens += result.Usage.OutputTokens
	s.stats.CacheRead += result.Usage.CacheReadInputTokens
	s.stats.CacheCreation += result.Usage.CacheCreationInputTokens
	s.stats.Reasoning += result.Usage.ReasoningOutputTokens
	s.stats.LastModel = result.Model
	s.stats.LastError = ""
}

func (s *Server) recordFailure(err error) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.Requests++
	s.stats.Failures++
	s.stats.LastError = err.Error()
}

func (s *Server) resolveModel(requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	if model, ok := s.models[requested]; ok {
		return model.ID, true
	}
	for _, model := range s.models {
		if strings.EqualFold(strings.TrimSpace(model.ID), requested) {
			return model.ID, true
		}
	}
	if target := strings.TrimSpace(s.cfg.ModelMap[strings.ToLower(requested)]); target != "" {
		for _, model := range s.models {
			if strings.EqualFold(strings.TrimSpace(model.ID), target) {
				return model.ID, true
			}
		}
	}
	return "", false
}

func restrictRecursiveSubagentTools(req *protocol.Request) {
	system, err := protocol.SystemText(req.System)
	if err != nil || !strings.Contains(strings.ToLower(system), "cc_is_subagent=true") {
		return
	}
	// Claude Code exposes Agent to nested subagents but does not transmit a
	// depth value: every nested request has the same cc_is_subagent marker and
	// session ID. Removing only Agent is therefore the deterministic boundary
	// that prevents exponential fan-out while preserving every local execution
	// tool, MCP tool, skill, attachment, and normal main-session Agent support.
	tools := req.Tools[:0]
	for _, tool := range req.Tools {
		switch strings.ToLower(strings.TrimSpace(tool.Name)) {
		case "agent":
			continue
		default:
			tools = append(tools, tool)
		}
	}
	req.Tools = tools
}

func publicModelID(client, providerID string, index int) string {
	lower := strings.ToLower(providerID)
	var slug strings.Builder
	for _, char := range lower {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			slug.WriteRune(char)
		case slug.Len() > 0 && slug.String()[slug.Len()-1] != '-':
			slug.WriteByte('-')
		}
		if slug.Len() >= 64 {
			break
		}
	}
	clean := strings.Trim(slug.String(), "-")
	if clean == "" {
		clean = fmt.Sprintf("model-%d", index+1)
	}
	prefix := ""
	if client == config.ClientClaude {
		prefix = "claude-macaz-"
	}
	return prefix + clean
}

func (s *Server) decodeRequest(w http.ResponseWriter, r *http.Request) (*protocol.Request, error) {
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.UseNumber()
	var req protocol.Request
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body must contain one JSON object")
		return nil, errors.New("multiple JSON values")
	}
	return &req, nil
}

func messageResponse(result protocol.Result, requestedModel string) map[string]any {
	model := first(result.Model, requestedModel)
	return map[string]any{
		"id":            first(result.ID, "msg_"+mustRandomToken(12)),
		"type":          "message",
		"role":          "assistant",
		"content":       result.Blocks,
		"model":         model,
		"stop_reason":   first(result.StopReason, "end_turn"),
		"stop_sequence": nil,
		"usage":         result.Usage,
	}
}

func streamStartBlock(block protocol.Block) map[string]any {
	switch block.Type {
	case "tool_use":
		return map[string]any{
			"type":  "tool_use",
			"id":    block.ID,
			"name":  block.Name,
			"input": map[string]any{},
		}
	case "thinking":
		return map[string]any{
			"type":      "thinking",
			"thinking":  "",
			"signature": "",
		}
	default:
		return map[string]any{
			"type": "text",
			"text": "",
		}
	}
}

func writeProviderError(w http.ResponseWriter, err error) {
	status := provider.Status(err)
	errorType := "api_error"
	if status == http.StatusBadRequest {
		errorType = "invalid_request_error"
	} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
		errorType = "authentication_error"
	} else if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) && httpErr.RetryAfter > 0 {
		seconds := int64((httpErr.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	writeError(w, status, errorType, err.Error())
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, anthropicError(errorType, message))
}

func anthropicError(errorType, message string) map[string]any {
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSSE(w io.Writer, event string, value any) {
	raw, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func mustRandomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
