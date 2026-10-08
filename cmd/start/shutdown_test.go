package start

import (
	"context"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownCoordinatorIdempotentAndWaitsForAsync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	var apiShutdowns atomic.Int32
	var internalShutdowns atomic.Int32
	var closeDBs atomic.Int32

	coordinator := newShutdownCoordinator(shutdownConfig{
		cancel:      cancel,
		gracePeriod: time.Second,
		apiShutdown: func(context.Context) error {
			apiShutdowns.Add(1)
			return nil
		},
		internalShutdown: func(context.Context) error {
			internalShutdowns.Add(1)
			return nil
		},
		closeDB: func() error {
			closeDBs.Add(1)
			return nil
		},
	})
	deactivate := activateShutdownCoordinator(coordinator)
	defer deactivate()

	coordinator.runAsync(func() {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		close(finished)
	})
	<-started

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- shutdown()
	}()
	go func() {
		secondDone <- shutdown()
	}()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel root context")
	}

	select {
	case err := <-firstDone:
		t.Fatalf("shutdown returned before tracked goroutine stopped: %v", err)
	case err := <-secondDone:
		t.Fatalf("second shutdown returned before tracked goroutine stopped: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("tracked goroutine did not finish")
	}

	for _, ch := range []chan error{firstDone, secondDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("shutdown returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not return")
		}
	}

	if got := apiShutdowns.Load(); got != 1 {
		t.Fatalf("api shutdown count = %d, want 1", got)
	}
	if got := internalShutdowns.Load(); got != 1 {
		t.Fatalf("internal shutdown count = %d, want 1", got)
	}
	if got := closeDBs.Load(); got != 1 {
		t.Fatalf("db close count = %d, want 1", got)
	}
}

func TestShutdownDrainsThenCancelsAndJoinsOwnedWork(t *testing.T) {
	owner := runlife.New(context.Background())
	child, release, err := owner.Reserve(context.Background())
	require.NoError(t, err)
	root, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{}, 2)
	allowDrain := make(chan struct{})
	dbClosed := make(chan struct{})
	coordinator := newShutdownCoordinator(shutdownConfig{supervisor: owner, cancel: cancel, gracePeriod: time.Second,
		apiShutdown:      func(context.Context) error { drained <- struct{}{}; <-allowDrain; return nil },
		internalShutdown: func(context.Context) error { drained <- struct{}{}; <-allowDrain; return nil },
		closeDB:          func() error { close(dbClosed); return nil }})
	done := make(chan error, 1)
	go func() { done <- coordinator.Shutdown() }()
	<-drained
	<-drained
	require.NoError(t, child.Err())
	require.NoError(t, root.Err())
	// Admission stays open through HTTP drain.
	_, otherRelease, err := owner.Reserve(context.Background())
	require.NoError(t, err)
	otherRelease()
	close(allowDrain)
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("work not cancelled")
	}
	select {
	case <-dbClosed:
		t.Fatal("DB closed before owned work released")
	default:
	}
	_, _, err = owner.Reserve(context.Background())
	require.ErrorIs(t, err, runlife.ErrClosed)
	release()
	require.NoError(t, <-done)
	<-dbClosed
}

func TestShutdownTimeoutDoesNotClaimJoin(t *testing.T) {
	owner := runlife.New(context.Background())
	_, release, err := owner.Reserve(context.Background())
	require.NoError(t, err)
	defer release()
	var closed atomic.Bool
	coordinator := newShutdownCoordinator(shutdownConfig{supervisor: owner, gracePeriod: 10 * time.Millisecond, apiShutdown: func(context.Context) error { return nil }, internalShutdown: func(context.Context) error { return nil }, closeDB: func() error { closed.Store(true); return nil }})
	require.ErrorIs(t, coordinator.Shutdown(), context.DeadlineExceeded)
	require.True(t, closed.Load(), "bounded existing DB-close policy retained")
}
