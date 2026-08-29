package worker

import (
	"context"
	"sync"
)

// Pool bounds concurrent goroutines using a semaphore.
//
// The reservation half of the API (TryAcquire/Acquire → Go or Release) exists
// so a caller can prove it has capacity BEFORE it takes an action that is
// externally visible as "this task is running".  Claiming a task flips its row
// to `running` in the catalog, so a claim issued without a slot advertises work
// the worker cannot start until some other task finishes — see Worker.Run and
// Worker.SubmitDispatched.
type Pool struct {
	sem chan struct{}
	// free is a wake-only signal (buffered size 1) poked whenever a slot is
	// returned, so a caller parked because the pool was full can react to
	// capacity the moment it appears instead of waiting out a poll interval.
	// A dropped poke (buffer already full) is harmless: the reader re-checks
	// capacity on every pass.
	free chan struct{}
	wg   sync.WaitGroup
}

func NewPool(size int) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{
		sem:  make(chan struct{}, size),
		free: make(chan struct{}, 1),
	}
}

// Size reports the pool's maximum concurrency.
func (p *Pool) Size() int {
	return cap(p.sem)
}

// TryAcquire reserves one execution slot without blocking, reporting whether a
// slot was available.  A successful reservation MUST be handed to Go (which
// consumes it) or returned with Release.
func (p *Pool) TryAcquire() bool {
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire reserves one execution slot, blocking until one frees or ctx is done.
func (p *Pool) Acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a reserved slot that will not be used.
func (p *Pool) Release() {
	p.releaseSlot()
}

// Free is the wake-only signal poked whenever a slot is released.  Select on it
// alongside a timer to wait for capacity without polling.
func (p *Pool) Free() <-chan struct{} {
	return p.free
}

// Go runs fn on a slot the caller has ALREADY reserved via TryAcquire/Acquire.
// The slot is released when fn returns.
func (p *Pool) Go(fn func()) {
	p.wg.Add(1)
	go func() {
		defer func() {
			p.releaseSlot()
			p.wg.Done()
		}()
		fn()
	}()
}

func (p *Pool) Submit(ctx context.Context, fn func()) error {
	if err := p.Acquire(ctx); err != nil {
		return err
	}
	p.Go(fn)
	return nil
}

func (p *Pool) Wait() {
	p.wg.Wait()
}

func (p *Pool) releaseSlot() {
	<-p.sem
	select {
	case p.free <- struct{}{}:
	default:
	}
}
