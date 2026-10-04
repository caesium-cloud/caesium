// Package runlife owns admitted asynchronous work for a server lifetime.
package runlife

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("run supervisor is closed")
var ErrMissing = errors.New("run supervisor is missing from context")

type supervisorKey struct{}

// Supervisor separates admission from cancellation and joining owned work.
type Supervisor struct {
	mu       sync.Mutex
	lifetime context.Context
	cancel   context.CancelFunc
	closed   bool
	work     sync.WaitGroup
	done     chan struct{}
}

func New(parent context.Context) *Supervisor {
	lifetime, cancel := context.WithCancel(parent)
	return &Supervisor{lifetime: lifetime, cancel: cancel, done: make(chan struct{})}
}

func WithSupervisor(ctx context.Context, s *Supervisor) context.Context {
	return context.WithValue(ctx, supervisorKey{}, s)
}
func FromContext(ctx context.Context) *Supervisor {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(supervisorKey{}).(*Supervisor)
	return s
}

// Reserve preserves request values but not its cancellation or deadline. The
// returned context ends with the server lifetime or release, and release is
// idempotent. Admission and Add share the shutdown lock.
func (s *Supervisor) Reserve(requestCtx context.Context) (context.Context, func(), error) {
	if s == nil {
		return nil, nil, ErrMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lifetime.Err() != nil {
		return nil, nil, ErrClosed
	}
	child, cancel := context.WithCancel(context.WithoutCancel(requestCtx))
	stop := context.AfterFunc(s.lifetime, cancel)
	s.work.Add(1)
	var once sync.Once
	release := func() { once.Do(func() { stop(); cancel(); s.work.Done() }) }
	return child, release, nil
}

// CloseAndCancel refuses new submissions and cancels all admitted contexts.
func (s *Supervisor) CloseAndCancel() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	go func() { s.work.Wait(); close(s.done) }()
}

// Wait joins admitted work after admission closes. An expired grace period does
// not imply that the work finished; callers must report its error.
func (s *Supervisor) Wait(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
