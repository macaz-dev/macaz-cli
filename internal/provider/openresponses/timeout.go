package openresponses

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var ErrResponseIdleTimeout = errors.New("provider response stream exceeded the idle timeout")

// NewIdleReadCloser cancels an in-flight streaming request when the upstream
// stops producing bytes for longer than timeout. It is shared by Responses
// providers so a half-open connection cannot occupy a Claude turn forever.
func NewIdleReadCloser(body io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) io.ReadCloser {
	reader := &idleReadCloser{body: body, cancel: cancel, timeout: timeout, generation: 1}
	reader.timer = time.AfterFunc(timeout, func() { reader.expire(1) })
	return reader
}

type idleReadCloser struct {
	body       io.ReadCloser
	cancel     context.CancelFunc
	timer      *time.Timer
	timeout    time.Duration
	mu         sync.Mutex
	closed     bool
	timedOut   bool
	generation uint64
}

func (r *idleReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.body.Read(buffer)
	if n > 0 {
		r.reset()
	}
	r.mu.Lock()
	timedOut := r.timedOut
	r.mu.Unlock()
	if err != nil && timedOut {
		return n, ErrResponseIdleTimeout
	}
	return n, err
}

func (r *idleReadCloser) Close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		r.generation++
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.body.Close()
}

func (r *idleReadCloser) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.timedOut {
		return
	}
	r.generation++
	generation := r.generation
	r.timer.Stop()
	r.timer = time.AfterFunc(r.timeout, func() { r.expire(generation) })
}

func (r *idleReadCloser) expire(generation uint64) {
	r.mu.Lock()
	if r.closed || r.timedOut || generation != r.generation {
		r.mu.Unlock()
		return
	}
	r.timedOut = true
	r.mu.Unlock()
	r.cancel()
}
