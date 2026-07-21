package codexcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const codexModelCatalogOverride = "MACAZ_CODEX_MODEL_CATALOG"

// appServer is one long-lived Codex app-server. A server handles one request at
// a time; Provider keeps a bounded pool so JSON-RPC notifications never leak
// between concurrent Claude turns.
type appServer struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdin      io.WriteCloser
	rpc        *rpcClient
	stderr     boundedBuffer
	stderrDone chan struct{}
	baseDir    string

	idMu   sync.Mutex
	nextID int
	close  sync.Once
}

func startAppServer(ctx context.Context, executable string, directCatalog []byte) (*appServer, error) {
	baseDir, err := os.MkdirTemp("", "macaz-codex-server-*")
	if err != nil {
		return nil, err
	}
	processCtx, cancel := context.WithCancel(context.Background())
	modelCatalog, err := writeDirectModelCatalog(baseDir, directCatalog)
	if err != nil {
		cancel()
		_ = os.RemoveAll(baseDir)
		return nil, err
	}
	cmd := exec.CommandContext(processCtx, executable, appServerArgsWithCatalog(modelCatalog)...)
	cmd.Dir = baseDir
	cmd.Env = codexEnvironment(baseDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = os.RemoveAll(baseDir)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		_ = os.RemoveAll(baseDir)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		_ = os.RemoveAll(baseDir)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		_ = os.RemoveAll(baseDir)
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	server := &appServer{
		cmd:        cmd,
		cancel:     cancel,
		stdin:      stdin,
		rpc:        newRPC(stdin, stdout),
		stderrDone: make(chan struct{}),
		baseDir:    baseDir,
		nextID:     1,
	}
	go func() {
		_, _ = io.Copy(&server.stderr, stderr)
		close(server.stderrDone)
	}()
	if _, err := server.request(ctx, "initialize", initializeParams()); err != nil {
		_ = server.Close()
		return nil, withStderr(err, server.stderr.String())
	}
	if err := server.rpc.notify("initialized", map[string]any{}); err != nil {
		_ = server.Close()
		return nil, err
	}
	return server, nil
}

func prepareDirectModelCatalog(baseDir string) (string, error) {
	raw, err := loadDirectModelCatalog("")
	if err != nil {
		return "", err
	}
	return writeDirectModelCatalog(baseDir, raw)
}

func loadDirectModelCatalog(executable string) ([]byte, error) {
	paths, strict, err := codexModelCatalogCandidates(executable)
	if err != nil {
		return nil, err
	}

	models := make([]map[string]any, 0)
	indices := make(map[string]int)
	var catalogErrors []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			catalogErrors = append(catalogErrors, fmt.Sprintf("read %q: %v", path, err))
			if strict {
				break
			}
			continue
		}
		var catalog struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(raw, &catalog); err != nil {
			catalogErrors = append(catalogErrors, fmt.Sprintf("decode %q: %v", path, err))
			if strict {
				break
			}
			continue
		}
		if len(catalog.Models) == 0 {
			continue
		}
		for _, model := range catalog.Models {
			slug, _ := model["slug"].(string)
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			// Codex 0.144.x requires the legacy field, while newer model-cache
			// payloads either renamed it or omit it because newer clients default
			// reasoning-summary support to true. Backfill the legacy shape so a
			// freshly refreshed cache remains readable by the installed CLI.
			if _, ok := model["supports_reasoning_summaries"].(bool); !ok {
				if supported, ok := model["supports_reasoning_summary_parameter"].(bool); ok {
					model["supports_reasoning_summaries"] = supported
				} else {
					model["supports_reasoning_summaries"] = true
				}
			}
			model["tool_mode"] = "direct"
			model["multi_agent_version"] = nil
			if index, ok := indices[slug]; ok {
				models[index] = model
				continue
			}
			indices[slug] = len(models)
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		detail := ""
		if len(catalogErrors) > 0 {
			detail = "; " + strings.Join(catalogErrors, "; ")
		}
		return nil, fmt.Errorf(
			"Codex model cache is unavailable; run Codex once, then retry (checked %s%s)",
			strings.Join(paths, ", "),
			detail,
		)
	}

	raw, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, fmt.Errorf("encode direct Codex model catalog: %w", err)
	}
	return raw, nil
}

func writeDirectModelCatalog(baseDir string, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("direct Codex model catalog is empty")
	}
	path := filepath.Join(baseDir, "models-direct.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write direct Codex model catalog: %w", err)
	}
	return path, nil
}

func codexModelCatalogCandidates(executable string) ([]string, bool, error) {
	if override := strings.TrimSpace(os.Getenv(codexModelCatalogOverride)); override != "" {
		return []string{override}, true, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false, fmt.Errorf("resolve home directory for Codex model cache: %w", err)
	}
	paths, err := filepath.Glob(filepath.Join(home, ".codex*", "models_cache.json"))
	if err != nil {
		return nil, false, fmt.Errorf("discover local Codex profiles: %w", err)
	}
	paths = append(paths, filepath.Join(home, ".codex", "models_cache.json"))
	sort.Strings(paths)
	paths = uniquePaths(paths)

	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		paths = appendPathLast(paths, filepath.Join(codexHome, "models_cache.json"))
	}
	// A configured wrapper can choose a profile internally, so the parent
	// process may not have the same CODEX_HOME. Many wrappers print the selected
	// profile before their version; use that as a best-effort, zero-config hint.
	// Put it last because the wrapper's value wins over the parent environment.
	if executable != "" {
		if reported := codexHomeFromExecutable(executable); reported != "" {
			paths = appendPathLast(paths, filepath.Join(reported, "models_cache.json"))
		}
	}
	return paths, false, nil
}

func codexHomeFromExecutable(executable string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, _ := exec.CommandContext(ctx, executable, "--version").CombinedOutput()
	return codexHomeFromVersionOutput(output)
}

func codexHomeFromVersionOutput(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "CODEX_HOME" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
	}
	return ""
}

func uniquePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func appendPathLast(paths []string, selected string) []string {
	selected = filepath.Clean(selected)
	result := make([]string, 0, len(paths)+1)
	for _, path := range paths {
		if filepath.Clean(path) != selected {
			result = append(result, path)
		}
	}
	return append(result, selected)
}

func appServerArgsWithCatalog(modelCatalog string) []string {
	args := appServerArgs()
	if strings.TrimSpace(modelCatalog) == "" {
		return args
	}
	return append(args, "-c", fmt.Sprintf("model_catalog_json=%q", modelCatalog))
}

func (s *appServer) request(ctx context.Context, method string, params any) (map[string]any, error) {
	s.idMu.Lock()
	id := s.nextID
	s.nextID++
	s.idMu.Unlock()
	return s.rpc.request(ctx, id, method, params)
}

func (s *appServer) sendRequest(method string, params any) error {
	s.idMu.Lock()
	id := s.nextID
	s.nextID++
	s.idMu.Unlock()
	return s.rpc.sendRequest(id, method, params)
}

func (s *appServer) interruptAndQuiesce(ctx context.Context, threadID, turnID string) bool {
	if _, err := s.request(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}); err != nil {
		return false
	}
	return s.quiesceThread(ctx, threadID)
}

func (s *appServer) quiesceThread(ctx context.Context, threadID string) bool {
	if _, err := s.request(ctx, "thread/unsubscribe", map[string]any{"threadId": threadID}); err != nil {
		return false
	}
	// The unsubscribe response is ordered after notifications already written by
	// app-server. Once it arrives, drain those queued events before reuse so the
	// next Claude request starts with a clean event stream.
	for {
		select {
		case envelope, ok := <-s.rpc.events:
			if !ok || envelope.Error != nil {
				return false
			}
		default:
			return true
		}
	}
}

func (s *appServer) Close() error {
	var waitErr error
	s.close.Do(func() {
		s.cancel()
		_ = s.stdin.Close()
		waitErr = s.cmd.Wait()
		<-s.stderrDone
		_ = os.RemoveAll(s.baseDir)
	})
	return waitErr
}

func (p *Provider) acquireServer(ctx context.Context) (*appServer, func(bool), error) {
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	p.poolMu.Lock()
	if p.closed {
		p.poolMu.Unlock()
		<-p.slots
		return nil, nil, fmt.Errorf("Codex provider is closed")
	}
	p.poolMu.Unlock()

	var server *appServer
	select {
	case server = <-p.idle:
	default:
		executable, err := exec.LookPath(p.cfg.CodexExecutable)
		if err != nil {
			<-p.slots
			return nil, nil, fmt.Errorf("find Codex executable %q: %w", p.cfg.CodexExecutable, err)
		}
		server, err = p.startAppServer(ctx, executable)
		if err != nil {
			<-p.slots
			return nil, nil, err
		}
		p.poolMu.Lock()
		if p.closed {
			p.poolMu.Unlock()
			_ = server.Close()
			<-p.slots
			return nil, nil, fmt.Errorf("Codex provider is closed")
		}
		p.servers[server] = struct{}{}
		p.poolMu.Unlock()
	}

	var once sync.Once
	release := func(healthy bool) {
		once.Do(func() {
			if !healthy {
				p.poolMu.Lock()
				delete(p.servers, server)
				p.poolMu.Unlock()
				_ = server.Close()
			} else {
				p.poolMu.Lock()
				closed := p.closed
				p.poolMu.Unlock()
				if closed {
					_ = server.Close()
				} else {
					p.idle <- server
				}
			}
			<-p.slots
		})
	}
	return server, release, nil
}

func (p *Provider) startAppServer(ctx context.Context, executable string) (*appServer, error) {
	p.catalogMu.Lock()
	if len(p.directCatalog) > 0 {
		catalog := p.directCatalog
		p.catalogMu.Unlock()
		return startAppServer(ctx, executable, catalog)
	}

	// The Codex CLI refreshes models_cache.json asynchronously. Build and
	// validate one immutable snapshot before allowing the pool to start more
	// servers, so a permission-classifier request cannot observe a different
	// catalog schema from the main Claude turn.
	catalog, err := loadDirectModelCatalog(executable)
	if err != nil {
		p.catalogMu.Unlock()
		return nil, err
	}
	server, err := startAppServer(ctx, executable, catalog)
	if err == nil {
		p.directCatalog = append([]byte(nil), catalog...)
	}
	p.catalogMu.Unlock()
	return server, err
}

func (p *Provider) Close() error {
	p.closePending()
	p.poolMu.Lock()
	if p.closed {
		p.poolMu.Unlock()
		return nil
	}
	p.closed = true
	servers := make([]*appServer, 0, len(p.servers))
	for server := range p.servers {
		servers = append(servers, server)
	}
	p.servers = map[*appServer]struct{}{}
	p.poolMu.Unlock()
	var firstErr error
	for _, server := range servers {
		if err := server.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
