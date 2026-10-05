package job

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type replacementObservedEngine struct {
	*fakeEngine
	ctx     context.Context
	started chan<- context.Context
	// Optional per-attempt evidence; ordinary terminal cleanup also uses Force.
	stopped   chan<- replacementStopObservation
	runtimeID string
}

type replacementStopObservation struct {
	runtimeID string
	force     bool
	cause     error
}

func (e *replacementObservedEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	a, err := e.fakeEngine.Create(req)
	if err == nil && req.Spec.Env["CAESIUM_PARTITION"] == "retry" {
		e.runtimeID = a.ID()
		e.started <- e.ctx
	}
	return a, err
}

func (e *replacementObservedEngine) Stop(req *atom.EngineStopRequest) error {
	if e.stopped != nil && req.ID == e.runtimeID {
		e.stopped <- replacementStopObservation{runtimeID: req.ID, force: req.Force, cause: context.Cause(e.ctx)}
	}
	return e.fakeEngine.Stop(req)
}

func TestPartitionRetryReplacementInheritsServerOwnership(t *testing.T) {
	f := newFanOutFixture(t, `["retry"]`, &schema.FanOut{From: "list", MaxPartitions: 16}, 0)
	f.engine.createErrByPartition["retry"] = errors.New("first attempt failed")
	started := make(chan context.Context, 1)
	owner := runlife.New(context.Background())
	carrier := runlife.WithSupervisor(t.Context(), owner)
	workCtx, releaseWork, err := owner.Reserve(carrier)
	require.NoError(t, err)
	finish := make(chan struct{})
	var finished atomic.Bool
	unblock := func() {
		if finished.CompareAndSwap(false, true) {
			close(finish)
		}
	}
	t.Cleanup(func() {
		unblock()
		owner.CloseAndCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, owner.Wait(ctx))
	})
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDockerEngineFactory(func(ctx context.Context) atom.Engine {
		return &replacementObservedEngine{fakeEngine: f.engine, ctx: ctx, started: started}
	}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)
	var windows atomic.Int32
	replacementCompleting := make(chan struct{})
	runner.beforeComplete = func(runID uuid.UUID) {
		if windows.Add(1) == 1 {
			rows := f.instanceRowsFor(t, runID)
			require.Len(t, rows, 1)
			_, reopened, err := f.store.RetryPartition(t.Context(), runID, rows[0].ID)
			require.NoError(t, err)
			require.False(t, reopened)
			f.engine.mu.Lock()
			delete(f.engine.createErrByPartition, "retry")
			f.engine.runDurationByPartition["retry"] = time.Hour
			f.engine.mu.Unlock()
			return
		}
		close(replacementCompleting)
		<-finish
	}
	firstDone := make(chan error, 1)
	go func() { defer releaseWork(); firstDone <- runner.Run(workCtx) }()
	require.Error(t, <-firstDone)
	var replacementCtx context.Context
	select {
	case replacementCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not create its atom")
	}
	require.NotNil(t, runlife.FromContext(replacementCtx))
	owner.CloseAndCancel()
	select {
	case <-replacementCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("server cancellation did not reach replacement")
	}
	select {
	case <-replacementCompleting:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not stop and reach completion")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded, "replacement persistence remains owned after parent returned")
	unblock()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	require.NoError(t, owner.Wait(waitCtx))
	f.engine.mu.Lock()
	forced := false
	for _, force := range f.engine.stopForceByID {
		forced = forced || force
	}
	f.engine.mu.Unlock()
	require.True(t, forced, "server cancellation stopped replacement atom")
	rows := f.instanceRows(t)
	require.Len(t, rows, 1)
	require.False(t, rows[0].PartitionRetryPending)
	require.Equal(t, string(run.StatusFailed), f.latestJobRun(t).Status)
}

func TestClosedOwnerReplacementResolvesRetryAndCompletesFailed(t *testing.T) {
	f := newFanOutFixture(t, `["retry"]`, &schema.FanOut{From: "list", MaxPartitions: 16}, 0)
	f.engine.createErrByPartition["retry"] = errors.New("first attempt failed")
	owner := runlife.New(context.Background())
	carrier := runlife.WithSupervisor(t.Context(), owner)
	workCtx, release, err := owner.Reserve(carrier)
	require.NoError(t, err)
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)
	runner.beforeComplete = func(runID uuid.UUID) {
		rows := f.instanceRowsFor(t, runID)
		require.Len(t, rows, 1)
		_, reopened, err := f.store.RetryPartition(t.Context(), runID, rows[0].ID)
		require.NoError(t, err)
		require.False(t, reopened)
		owner.CloseAndCancel()
		// Reserve propagates owner cancellation through an AfterFunc. This
		// fixture exercises an already-canceled owner at the completion boundary.
		select {
		case <-workCtx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("server cancellation did not reach reserved work before completion")
		}
	}
	returned := runner.Run(workCtx)
	require.ErrorIs(t, returned, context.Canceled)
	require.True(t, run.IsRunCancellationError(returned), "the closed owner, rather than a backend failure, settles the retry")
	release()
	require.NoError(t, owner.Wait(t.Context()))
	final := f.latestJobRun(t)
	require.Equal(t, string(run.StatusFailed), final.Status)
	require.Equal(t, context.Canceled.Error(), final.Error)
	require.NotNil(t, final.CompletedAt)
	rows := f.instanceRows(t)
	require.Len(t, rows, 1)
	require.False(t, rows[0].PartitionRetryPending)
	require.Equal(t, string(run.TaskStatusFailed), rows[0].Status)
	require.Equal(t, context.Canceled.Error(), rows[0].Error)
	require.NotNil(t, rows[0].CompletedAt)
	require.Empty(t, rows[0].ClaimedBy)
	require.Nil(t, rows[0].ClaimExpiresAt)
	f.engine.mu.Lock()
	creates := f.engine.createCallsByPartition["retry"]
	f.engine.mu.Unlock()
	require.Equal(t, 1, creates, "refused replacement starts no container")
}

func TestRejectedReplacementAbandonsOnlyItsScannedRetrySet(t *testing.T) {
	f := newFanOutFixture(t, `["a","b"]`, &schema.FanOut{From: "list", MaxPartitions: 16, FailurePolicy: schema.FanOutFailureContinue}, 0)
	f.engine.createErrByPartition["a"] = errors.New("failed a")
	f.engine.createErrByPartition["b"] = errors.New("failed b")
	require.Error(t, f.run(t, defaultFanOutVars()))
	final := f.latestJobRun(t)
	rows := f.instanceRows(t)
	require.Len(t, rows, 2)
	for _, row := range rows {
		_, _, err := f.store.RetryPartition(t.Context(), final.ID, row.ID)
		require.NoError(t, err)
	}
	owner := runlife.New(context.Background())
	owner.CloseAndCancel()
	runner := New(&models.Job{ID: f.jobID}).(*job)
	admissionErr := runner.startReplacementRun(runlife.WithSupervisor(t.Context(), owner), final.ID, nil, []uuid.UUID{rows[0].ID})
	require.ErrorIs(t, admissionErr, runlife.ErrClosed)
	require.ErrorIs(t, runner.abandonRejectedReplacement(f.store, final.ID, []uuid.UUID{rows[0].ID}, admissionErr), runlife.ErrClosed)
	var selected, fresh models.TaskRun
	require.NoError(t, f.db.First(&selected, "id = ?", rows[0].ID).Error)
	require.NoError(t, f.db.First(&fresh, "id = ?", rows[1].ID).Error)
	require.False(t, selected.PartitionRetryPending)
	require.True(t, fresh.PartitionRetryPending, "unscanned acceptance is not abandoned")
	require.Equal(t, string(run.TaskStatusPending), fresh.Status)
}

func TestDirectLocalChildReservationKeepsStandaloneDrainSemantics(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	child, release, err := reserveLocalChild(parent)
	require.NoError(t, err)
	defer release()
	cancel()
	require.NoError(t, child.Err(), "direct local child retains independent lifetime without server owner")
	require.Nil(t, runlife.FromContext(child))
}

func TestPartitionRetryReplacementNaturalDrainJoinsWithoutCancel(t *testing.T) {
	f := newFanOutFixture(t, `["retry"]`, &schema.FanOut{From: "list", MaxPartitions: 16}, 0)
	f.engine.createErrByPartition["retry"] = errors.New("first attempt failed")
	started := make(chan context.Context, 1)
	stopped := make(chan replacementStopObservation, 16)
	owner := runlife.New(t.Context())
	workCtx, releaseWork, err := owner.Reserve(runlife.WithSupervisor(t.Context(), owner))
	require.NoError(t, err)
	finish := make(chan struct{})
	var finished atomic.Bool
	unblock := func() {
		if finished.CompareAndSwap(false, true) {
			close(finish)
		}
	}
	t.Cleanup(func() {
		unblock()
		owner.CloseAndCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, owner.Wait(ctx))
	})
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDockerEngineFactory(func(ctx context.Context) atom.Engine {
		return &replacementObservedEngine{fakeEngine: f.engine, ctx: ctx, started: started, stopped: stopped}
	}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)
	var windows atomic.Int32
	replacementCompleting := make(chan struct{})
	runner.beforeComplete = func(runID uuid.UUID) {
		if windows.Add(1) == 1 {
			rows := f.instanceRowsFor(t, runID)
			require.Len(t, rows, 1)
			_, reopened, retryErr := f.store.RetryPartition(t.Context(), runID, rows[0].ID)
			require.NoError(t, retryErr)
			require.False(t, reopened)
			f.engine.mu.Lock()
			delete(f.engine.createErrByPartition, "retry")
			f.engine.runDurationByPartition["retry"] = 20 * time.Millisecond
			f.engine.mu.Unlock()
			return
		}
		close(replacementCompleting)
		<-finish
	}
	firstDone := make(chan error, 1)
	go func() { defer releaseWork(); firstDone <- runner.Run(workCtx) }()
	require.Error(t, <-firstDone, "the original engine reports its failed attempt after handing off")
	var replacementCtx context.Context
	select {
	case replacementCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not create its atom")
	}
	drained := make(chan error, 1)
	go func() { drained <- owner.Drain(t.Context()) }()
	select {
	case <-replacementCompleting:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reach terminal persistence")
	}
	select {
	case <-drained:
		t.Fatal("natural drain passed replacement terminal persistence")
	default:
	}
	require.NoError(t, replacementCtx.Err(), "ordinary parent return must not cancel the replacement")
	// executeAtom removes even successful runtimes with Stop(Force: true).
	// Inspect this replacement attempt's actual stop cause rather than a shared
	// force map that also includes producer/earlier-attempt terminal cleanup.
	var replacementStop replacementStopObservation
	select {
	case replacementStop = <-stopped:
	case <-time.After(time.Second):
		t.Fatal("replacement did not record its ordinary terminal runtime cleanup")
	}
	require.NotEmpty(t, replacementStop.runtimeID)
	require.True(t, replacementStop.force, "ordinary terminal cleanup is forceful by design")
	require.NoError(t, replacementStop.cause, "exact replacement cleanup must not be caused by owner cancellation")
	unblock()
	select {
	case err := <-drained:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("natural drain did not join the replacement")
	}
	final := f.latestJobRun(t)
	require.Equal(t, string(run.StatusSucceeded), final.Status)
	require.NotNil(t, final.CompletedAt)
	rows := f.instanceRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, string(run.TaskStatusSucceeded), rows[0].Status)
	require.Equal(t, rows[0].RuntimeID, replacementStop.runtimeID, "stop evidence must bind the exact replacement runtime")
	require.False(t, rows[0].PartitionRetryPending)
	require.NotNil(t, rows[0].CompletedAt)
	f.engine.mu.Lock()
	creates := f.engine.createCallsByPartition["retry"]
	f.engine.mu.Unlock()
	require.Equal(t, 2, creates)
	select {
	case unexpected := <-stopped:
		t.Fatalf("replacement received an extra stop after its terminal cleanup: id=%s force=%t cause=%v", unexpected.runtimeID, unexpected.force, unexpected.cause)
	default:
	}
}
