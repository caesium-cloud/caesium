package start

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/api"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/log"
	"golang.org/x/sync/errgroup"
)

const defaultShutdownGracePeriod = 30 * time.Second

type shutdownConfig struct {
	cancel           context.CancelFunc
	gracePeriod      time.Duration
	apiShutdown      func(context.Context) error
	internalShutdown func(context.Context) error
	closeDB          func() error
}

type shutdownCoordinator struct {
	once sync.Once
	wg   sync.WaitGroup

	cancel           context.CancelFunc
	gracePeriod      time.Duration
	apiShutdown      func(context.Context) error
	internalShutdown func(context.Context) error
	closeDB          func() error

	err error
}

var (
	activeShutdownMu sync.Mutex
	activeShutdown   *shutdownCoordinator
)

func newShutdownCoordinator(config shutdownConfig) *shutdownCoordinator {
	if config.gracePeriod <= 0 {
		config.gracePeriod = defaultShutdownGracePeriod
	}
	if config.apiShutdown == nil {
		config.apiShutdown = api.Shutdown
	}
	if config.internalShutdown == nil {
		config.internalShutdown = func(context.Context) error { return nil }
	}
	if config.closeDB == nil {
		config.closeDB = func() error {
			return db.DefaultRouter().Close()
		}
	}

	return &shutdownCoordinator{
		cancel:           config.cancel,
		gracePeriod:      config.gracePeriod,
		apiShutdown:      config.apiShutdown,
		internalShutdown: config.internalShutdown,
		closeDB:          config.closeDB,
	}
}

func activateShutdownCoordinator(coordinator *shutdownCoordinator) func() {
	activeShutdownMu.Lock()
	previous := activeShutdown
	activeShutdown = coordinator
	activeShutdownMu.Unlock()

	return func() {
		activeShutdownMu.Lock()
		if activeShutdown == coordinator {
			activeShutdown = previous
		}
		activeShutdownMu.Unlock()
	}
}

func (s *shutdownCoordinator) runAsync(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

func shutdown() error {
	activeShutdownMu.Lock()
	coordinator := activeShutdown
	activeShutdownMu.Unlock()

	if coordinator == nil {
		return nil
	}
	return coordinator.Shutdown()
}

func (s *shutdownCoordinator) Shutdown() error {
	if s == nil {
		return nil
	}

	s.once.Do(func() {
		s.err = s.shutdown()
	})
	return s.err
}

func (s *shutdownCoordinator) shutdown() error {
	graceCtx, cancel := context.WithTimeout(context.Background(), s.gracePeriod)
	defer cancel()

	var shutdownErrs []error
	if err := s.drainHTTP(graceCtx); err != nil {
		shutdownErrs = append(shutdownErrs, err)
	}

	if s.cancel != nil {
		s.cancel()
	}

	if err := s.wait(graceCtx); err != nil {
		log.Error("shutdown timed out waiting for background routines", "error", err)
		shutdownErrs = append(shutdownErrs, err)
	}

	if err := s.closeDB(); err != nil {
		log.Error("database close failed during shutdown", "error", err)
		shutdownErrs = append(shutdownErrs, err)
	}

	return errors.Join(shutdownErrs...)
}

func (s *shutdownCoordinator) drainHTTP(ctx context.Context) error {
	var group errgroup.Group
	group.Go(func() error {
		return s.apiShutdown(ctx)
	})
	group.Go(func() error {
		return s.internalShutdown(ctx)
	})
	return group.Wait()
}

func (s *shutdownCoordinator) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
