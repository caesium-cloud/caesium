package runlife

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestNaturalDrainJoinsNestedHandoffBeforeClosingAdmission(t *testing.T) {
	s := New(t.Context())
	carrier := WithSupervisor(t.Context(), s)
	root, releaseRoot, err := s.Reserve(carrier)
	require.NoError(t, err)
	drained := make(chan error, 1)
	go func() { drained <- s.Drain(t.Context()) }()
	child, releaseChild, err := s.Reserve(root)
	require.NoError(t, err, "active root may transfer ownership during natural drain")
	releaseRoot()
	select {
	case <-drained:
		t.Fatal("natural drain passed an admitted child")
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, child.Err(), "natural drain must not cancel legitimate replacement work")
	grandchild, releaseGrandchild, err := s.Reserve(child)
	require.NoError(t, err)
	releaseChild()
	require.NoError(t, grandchild.Err())
	releaseGrandchild()
	require.NoError(t, <-drained)
	_, _, err = s.Reserve(carrier)
	require.ErrorIs(t, err, ErrClosed)
	require.NoError(t, s.Wait(t.Context()))
}

func TestNaturalDrainZeroAndAdmissionAreAtomic(t *testing.T) {
	for range 50 {
		s := New(t.Context())
		_, releaseRoot, err := s.Reserve(t.Context())
		require.NoError(t, err)
		start := make(chan struct{})
		drained := make(chan error, 1)
		drainFinished := make(chan struct{})
		admitted := make(chan bool, 1)
		go func() { <-start; drained <- s.Drain(t.Context()); close(drainFinished) }()
		go func() {
			<-start
			_, releaseChild, err := s.Reserve(t.Context())
			if err != nil {
				require.ErrorIs(t, err, ErrClosed)
				admitted <- false
				return
			}
			admitted <- true
			select {
			case <-drainFinished:
				t.Error("zero-to-Add work escaped a successful drain")
			default:
			}
			releaseChild()
		}()
		close(start)
		releaseRoot()
		<-admitted
		require.NoError(t, <-drained)
		_, _, err = s.Reserve(t.Context())
		require.ErrorIs(t, err, ErrClosed)
	}
}

func TestCanceledNaturalDrainDoesNotClaimJoin(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	s := New(parent)
	child, release, err := s.Reserve(t.Context())
	require.NoError(t, err)
	cancelParent()
	require.ErrorIs(t, s.Drain(t.Context()), context.Canceled)
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not reach admitted work")
	}
	s.CloseAndCancel()
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, s.Wait(expired), context.DeadlineExceeded)
	release()
	require.NoError(t, s.Wait(t.Context()))
}
