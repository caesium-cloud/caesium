package worker

import (
	"context"
	"sync"
)

// Pool bounds concurrent goroutines using a semaphore.
type Pool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func NewPool(size int) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{sem: make(chan struct{}, size)}
}

func (p *Pool) Submit(ctx context.Context, fn func()) error {
	select {
	case p.sem <- struct{}{}:
		p.wg.Add(1)
		go func() {
			defer func() {
				<-p.sem
				p.wg.Done()
			}()
			fn()
		}()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) Wait() {
	p.wg.Wait()
}
