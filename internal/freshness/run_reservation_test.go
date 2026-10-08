package freshness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/runlife"
)

func TestRunReservationPreservesAdmissionCancellationAndOwnsExecution(t *testing.T) {
	owner := runlife.New(context.Background())
	tick, cancelTick := context.WithCancel(runlife.WithSupervisor(t.Context(), owner))
	defer cancelTick()
	admission, releaseUnused, err := reserveRunLaunch(tick)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseUnused()
	cancelTick()
	if !errors.Is(admission.Err(), context.Canceled) {
		t.Fatal("admission lost tick cancellation")
	}
	work, release, err := TakeRunReservation(admission)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if work.Err() != nil {
		t.Fatal("execution inherited tick cancellation")
	}
	if _, _, err := TakeRunReservation(admission); !errors.Is(err, errRunReservationConsumed) {
		t.Fatalf("second transfer: %v", err)
	}
	releaseUnused() // A synchronous evaluator return must not release transferred work.
	owner.CloseAndCancel()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("server cancellation did not reach execution")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(owner.Wait(expired), context.DeadlineExceeded) {
		t.Fatal("owner joined before transferred execution released")
	}
	release()
	release()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := owner.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
}

func TestRunReservationReleasesDeclinedAndSynchronousLaunchesOnce(t *testing.T) {
	owner := runlife.New(context.Background())
	ctx, release, err := reserveRunLaunch(runlife.WithSupervisor(t.Context(), owner))
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if _, _, err := TakeRunReservation(ctx); !errors.Is(err, errRunReservationConsumed) {
		t.Fatalf("take after release: %v", err)
	}
	owner.CloseAndCancel()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owner.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reserveRunLaunch(runlife.WithSupervisor(t.Context(), owner)); !errors.Is(err, runlife.ErrClosed) {
		t.Fatalf("closed admission: %v", err)
	}
}

func TestRunReservationRetainsDirectEvaluatorInjection(t *testing.T) {
	ctx, release, err := reserveRunLaunch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if ctx != t.Context() {
		t.Fatal("direct admission context changed")
	}
	if _, _, err := TakeRunReservation(ctx); !errors.Is(err, ErrNoRunReservation) {
		t.Fatalf("direct launch: %v", err)
	}
}
