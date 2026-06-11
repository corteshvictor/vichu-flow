package adapters

import (
	"context"
	"sync"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// bufferedSession serves a precomputed result over a channel of events that a
// producer goroutine fills and closes. Callers must drain Events() before
// calling Result(); the fake adapter completes all its side effects before
// closing the channel, so a fully drained channel implies the result is ready.
type bufferedSession struct {
	events chan AgentEvent
	result core.Result
	err    error
}

func (s *bufferedSession) Events() <-chan AgentEvent { return s.events }

func (s *bufferedSession) Result(context.Context) (core.Result, error) {
	return s.result, s.err
}

func (s *bufferedSession) Interrupt(context.Context) error { return nil }

// cmdSession wraps a running OS process. Its result becomes available when the
// process exits and all output has been streamed.
type cmdSession struct {
	events chan AgentEvent
	done   chan struct{}

	mu        sync.Mutex
	result    core.Result
	err       error
	interrupt func() error
}

func (s *cmdSession) Events() <-chan AgentEvent { return s.events }

func (s *cmdSession) Result(ctx context.Context) (core.Result, error) {
	select {
	case <-ctx.Done():
		return core.Result{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, s.err
	}
}

func (s *cmdSession) Interrupt(context.Context) error {
	if s.interrupt != nil {
		return s.interrupt()
	}
	return nil
}

func (s *cmdSession) finish(result core.Result, err error) {
	s.mu.Lock()
	s.result = result
	s.err = err
	s.mu.Unlock()
	close(s.done)
}
