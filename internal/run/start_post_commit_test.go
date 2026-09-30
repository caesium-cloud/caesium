package run

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// cancellingBus cancels a context the first time an event is published. Bus
// publication is the first thing startRun does after the run-creation
// transaction commits and the last thing before it reads the record back, so
// this is a precise fault injection for "the caller's context dies between
// COMMIT and the post-commit read".
type cancellingBus struct {
	inner  event.Bus
	cancel context.CancelFunc
}

func (b *cancellingBus) Publish(e event.Event) {
	if b.cancel != nil {
		b.cancel()
	}
	if b.inner != nil {
		b.inner.Publish(e)
	}
}

func (b *cancellingBus) Subscribe(ctx context.Context, filter event.Filter) (<-chan event.Event, error) {
	return b.inner.Subscribe(ctx, filter)
}

// TestStartWithContextReturnsCommittedRunWhenContextCancelsAfterCommit is the
// regression for a run stranded by post-commit cancellation.
//
// By the time startRun reads the record back, the run row is committed,
// run_started has been published and the lease is taken — the run is LIVE. If
// that read ran on the caller's context, a cancellation landing in this window
// (server shutdown during a freshness evaluator tick, say) would turn a live run
// into (nil, context.Canceled): the caller neither executes nor finalizes it,
// and the row sits `running` with no tasks forever, blocking a maxRuns policy
// and producing skipped_active_run on every later evaluation.
func TestStartWithContextReturnsCommittedRunWhenContextCancelsAfterCommit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store.SetBus(&cancellingBus{inner: event.New(), cancel: cancel})

	jobID := uuid.New()
	runRecord, err := store.StartWithContext(ctx, jobID, nil)

	require.NoError(t, err, "a committed run must not be reported as a failure just because the caller's context died")
	require.NotNil(t, runRecord, "the caller must receive the identity of the run that was committed")
	require.Equal(t, jobID, runRecord.JobID)
	require.Equal(t, StatusRunning, runRecord.Status)
	require.Error(t, ctx.Err(), "the fault injection must actually have cancelled the context")

	// And the row really is there, running, for the caller to drive.
	var persisted models.JobRun
	require.NoError(t, db.First(&persisted, "id = ?", runRecord.ID).Error)
	require.Equal(t, string(StatusRunning), persisted.Status)
}

// TestStartWithContextHonoursCancellationBeforeCommit proves the detached
// post-commit read did not make admission itself uncancellable: a context that
// is already dead when Start is called creates nothing.
func TestStartWithContextHonoursCancellationBeforeCommit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jobID := uuid.New()
	runRecord, err := store.StartWithContext(ctx, jobID, nil)
	require.Error(t, err, "an already-cancelled context must not admit a run")
	require.Nil(t, runRecord)

	var count int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", jobID).Count(&count).Error)
	require.Zero(t, count, "no run row may be committed for a cancelled admission")
}
