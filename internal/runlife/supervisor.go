// Package runlife owns admitted asynchronous work for a server lifetime.
package runlife

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("run supervisor is closed")
var ErrMissing = errors.New("run supervisor is missing from context")

// ErrServerShutdown releases in-process execution without terminating durable work.
var ErrServerShutdown = errors.New("server shutting down; execution left for takeover")

type supervisorKey struct{}

// Supervisor separates admission from cancellation and joining owned work.
type Supervisor struct {
	mu       sync.Mutex
	lifetime context.Context
	cancel   context.CancelCauseFunc
	closed   bool
	work     sync.WaitGroup
	done     chan struct{}
	active   int
	changed  chan struct{}
}

func New(parent context.Context) *Supervisor {
	lifetime, cancel := context.WithCancelCause(parent)
	return &Supervisor{lifetime: lifetime, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
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

// Cause identifies lifetime cancellation even when no child was admitted.
func (s *Supervisor) Cause() error {
	if s == nil {
		return nil
	}
	return context.Cause(s.lifetime)
}

// CancellationCause also observes lifetime cancellation before Reserve's
// AfterFunc reaches the child. A cause already recorded by the child wins.
func CancellationCause(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if s := FromContext(ctx); s != nil {
		return context.Cause(s.lifetime)
	}
	return nil
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
	child, cancel := context.WithCancelCause(context.WithoutCancel(requestCtx))
	stop := context.AfterFunc(s.lifetime, func() { cancel(context.Cause(s.lifetime)) })
	s.work.Add(1)
	s.active++
	var once sync.Once
	release := func() {
		once.Do(func() {
			stop()
			cancel(nil)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.work.Done()
			s.active--
			close(s.changed)
			s.changed = make(chan struct{})
		})
	}
	return child, release, nil
}

// CloseAndCancel refuses new submissions and cancels all admitted contexts.
func (s *Supervisor) CloseAndCancel() {
	s.CloseAndCancelCause(context.Canceled)
}

// CloseAndCancelCause closes admission and preserves why execution was stopped.
func (s *Supervisor) CloseAndCancelCause(cause error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closeLocked(cause)
}

func (s *Supervisor) closeLocked(cause error) {
	s.closed = true
	s.cancel(cause)
	go func() { s.work.Wait(); close(s.done) }()
}

// Drain allows admitted work to transfer ownership to children until all work
// finishes naturally. Observing zero and closing admission share Reserve's lock,
// so no zero-to-Add interval can let work escape the join. Cancellation returns
// an error without claiming a join; callers must then cancel and bound Wait.
func (s *Supervisor) Drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return s.Wait(ctx)
		}
		if s.active == 0 {
			s.closeLocked(nil)
			s.mu.Unlock()
			return s.Wait(ctx)
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-s.lifetime.Done():
			return context.Cause(s.lifetime)
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
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
