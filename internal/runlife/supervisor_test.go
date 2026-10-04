package runlife

import (
	"context"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

func TestReservationLifetimeAndJoin(t *testing.T) {
	type key struct{}
	request, cancelRequest := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
	s := New(context.Background())
	carrier := WithSupervisor(request, s)
	require.Same(t, s, FromContext(carrier))
	require.Nil(t, FromContext(context.Background()))
	child, release, err := s.Reserve(carrier)
	require.NoError(t, err)
	require.Equal(t, "value", child.Value(key{}))
	cancelRequest()
	require.NoError(t, child.Err())
	s.CloseAndCancel()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("server cancellation did not reach work")
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.Wait(waitCtx), context.Canceled)
	_, _, err = s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrClosed)
	release()
	release()
	require.NoError(t, s.Wait(context.Background()))
}

func TestParentCancellationRejectsAdmission(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	s := New(parent)
	child, release, err := s.Reserve(context.Background())
	require.NoError(t, err)
	defer release()
	cancel()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not reach work")
	}
	_, _, err = s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrClosed)
	s.CloseAndCancel()
}

func TestConcurrentCloseAndReserve(t *testing.T) {
	for range 50 {
		s := New(context.Background())
		var workers sync.WaitGroup
		for range 10 {
			workers.Go(func() {
				_, release, err := s.Reserve(context.Background())
				if err == nil {
					release()
					release()
				} else {
					require.ErrorIs(t, err, ErrClosed)
				}
			})
		}
		s.CloseAndCancel()
		workers.Wait()
		require.NoError(t, s.Wait(context.Background()))
	}
}

func TestMissingOwnerFailsClosed(t *testing.T) {
	var s *Supervisor
	_, release, err := s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrMissing)
	require.Nil(t, release)
}
