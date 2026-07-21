package codexcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

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

func startAppServer(ctx context.Context, executable string) (*appServer, error) {
	baseDir, err := os.MkdirTemp("", "macaz-codex-server-*")
	if err != nil {
		return nil, err
	}
	processCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(processCtx, executable, appServerArgs()...)
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
		server, err = startAppServer(ctx, executable)
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
