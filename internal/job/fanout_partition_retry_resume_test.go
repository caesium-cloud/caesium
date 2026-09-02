package job

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// addSideTask wires an independent root task (no edges) into the job so a test
// can leave a terminal failure in the run that no partition retry resets and
// that is not a predecessor of the fanned step.
func (f *fanOutFixture) addSideTask(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	atomID := uuid.New()
	task := &models.Task{ID: id, JobID: f.jobID, AtomID: atomID, Name: name, Position: 0, TriggerRule: string(schema.TriggerRuleAllSuccess)}
	f.taskSvc.tasks = append(f.taskSvc.tasks, task)
	f.atomSvc.atoms[atomID] = fakeModelAtom(atomID)
	require.NoError(t, f.db.Create(task).Error)
	return id
}

func (f *fanOutFixture) latestJobRun(t *testing.T) models.JobRun {
	t.Helper()
	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)
	return jobRun
}

func (f *fanOutFixture) resume(t *testing.T, runID uuid.UUID) error {
	t.Helper()
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	return New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(context.Background(), runID))
}

func (f *fanOutFixture) awaitRunStatus(t *testing.T, runID uuid.UUID, want run.Status) models.JobRun {
	t.Helper()
	return f.awaitRun(t, runID, func(status string) bool { return status == string(want) }, string(want))
}

func (f *fanOutFixture) awaitRunTerminal(t *testing.T, runID uuid.UUID) models.JobRun {
	t.Helper()
	return f.awaitRun(t, runID, func(status string) bool {
		switch run.Status(status) {
		case run.StatusSucceeded, run.StatusFailed, run.StatusCancelled:
			return true
		}
		return false
	}, "terminal")
}

// awaitCallbacks waits for the completion-callback counter to reach want.
// Callbacks are dispatched after the terminal status write, so a test that
// observed the run become terminal must still wait for them.
func awaitCallbacks(t *testing.T, counter *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for counter.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("callbacks did not reach %d (got %d)", want, counter.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let a mistakenly duplicated dispatch show itself before the caller
	// asserts the exact count.
	time.Sleep(50 * time.Millisecond)
}

func (f *fanOutFixture) awaitRun(t *testing.T, runID uuid.UUID, done func(string) bool, want string) models.JobRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var jobRun models.JobRun
		require.NoError(t, f.db.First(&jobRun, "id = ?", runID).Error)
		if done(jobRun.Status) {
			return jobRun
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach %s (status=%q)", runID, want, jobRun.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFanOutPartitionRetryResumesPastPreservedSiblingFailure drives the
// preserved-failure topology through the real local Run surface. Under the
// continue failure policy an independent `side` task fails and is never reset;
// `process` runs normally and one of its partitions fails. Retrying that
// partition reopens the run, and the resumed engine must treat the preserved
// `side` failure as a settled outcome — executing the reset instance and
// finishing the run failed — instead of bailing on "previously failed",
// tripping the completion fence, and spawning replacement engines forever.
func TestFanOutPartitionRetryResumesPastPreservedSiblingFailure(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	side := f.addSideTask(t, "side")
	f.engine.resultByName[side.String()] = atom.Failure
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom")

	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	require.Equal(t, string(run.StatusFailed), jobRun.Status)
	before := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), before["flaky"])
	require.Equal(t, string(run.TaskStatusSucceeded), before["ok"])
	var flakyID uuid.UUID
	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "flaky" {
			flakyID = r.ID
		}
	}

	f.engine.mu.Lock()
	delete(f.engine.createErrByPartition, "flaky")
	f.engine.mu.Unlock()
	reset, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, flakyID)
	require.NoError(t, err)
	require.True(t, reopened)
	require.Equal(t, 0, reset.OutstandingPredecessors)

	err = f.resume(t, jobRun.ID)
	require.Error(t, err, "the preserved sibling failure still fails the resumed run")
	require.NotContains(t, err.Error(), "no runnable tasks")

	after := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusSucceeded), after["flaky"],
		"the reset instance must execute on resume, not stay pending behind the preserved failure")
	require.Equal(t, string(run.TaskStatusSucceeded), after["ok"])
	require.Equal(t, 1, f.engine.createCount("ok"), "resume must not re-run the succeeded sibling")
	require.Equal(t, 2, f.engine.createCount("flaky"))
	require.Len(t, f.engine.createRequestsForTask(side), 1, "resume must not re-run the preserved failure")

	final := f.awaitRunStatus(t, jobRun.ID, run.StatusFailed)
	require.NotNil(t, final.CompletedAt)
	var retried models.TaskRun
	require.NoError(t, f.db.First(&retried, "id = ?", flakyID).Error)
	require.False(t, retried.PartitionRetryPending)

	// A resumed engine that bailed would have tripped the fence and spawned a
	// replacement; give one a moment to show itself.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 2, f.engine.createCount("flaky"), "the reset instance must execute exactly once more")
	again := f.latestJobRun(t)
	require.Equal(t, final.CompletedAt.UnixNano(), again.CompletedAt.UnixNano(), "the run must be finalized exactly once")
}

// TestFanOutPartitionRetryResumesRootBehindSkippedDependentInEmissionOrder pins
// the group-level indegree the resumed engine seeds from. The run view
// collapses a fanned group onto its first instance in emission order; when
// that instance is a dependent the sweep skipped with its in-group indegree
// still outstanding, seeding from it parks the whole group and the retried
// root never runs.
func TestFanOutPartitionRetryResumesRootBehindSkippedDependentInEmissionOrder(t *testing.T) {
	f := newFanOutFixture(t, `[{"key":"b","dependsOn":["a"]},{"key":"a"}]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["a"] = fmt.Errorf("boom")

	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	before := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusFailed), before["a"])
	require.Equal(t, string(run.TaskStatusSkipped), before["b"],
		"precondition: the dependent must have been resolved by the straggler sweep")
	var aID uuid.UUID
	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "a" {
			aID = r.ID
		}
	}

	f.engine.mu.Lock()
	delete(f.engine.createErrByPartition, "a")
	f.engine.mu.Unlock()
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, aID)
	require.NoError(t, err)
	require.True(t, reopened)

	_ = f.resume(t, jobRun.ID)

	after := statusByPartition(f.instanceRows(t))
	require.Equal(t, string(run.TaskStatusSucceeded), after["a"],
		"the retried root must execute even though the group's first emitted instance is a skipped dependent")
	require.Equal(t, string(run.TaskStatusSkipped), after["b"], "a partition retry must not cascade to a resolved dependent")
	require.Equal(t, 2, f.engine.createCount("a"))
	// Which terminal status the run reaches is the DAG's own accounting (a
	// skipped dependent is not a failure to the engine); what matters here is
	// that the run finalizes instead of looping on a parked group.
	final := f.awaitRunTerminal(t, jobRun.ID)
	require.NotNil(t, final.CompletedAt)
}

// TestFanOutPartitionRetryReplacementAbandonsUndispatchableRetry bounds the
// completion fence. When a resumed engine cannot drive the reset instance at
// all, Complete refuses, a replacement is started, and if the replacement
// cannot drive it either the retry must be abandoned explicitly (skipped with
// a reason, provenance cleared) so the run finalizes — never a third engine.
func TestFanOutPartitionRetryReplacementAbandonsUndispatchableRetry(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom")
	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	var flakyID uuid.UUID
	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "flaky" {
			flakyID = r.ID
		}
	}
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, flakyID)
	require.NoError(t, err)
	require.True(t, reopened)
	// Wedge the group: no instance can ever become ready, so no engine can
	// make progress on the accepted retry.
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", jobRun.ID, f.fanned).
		Update("outstanding_predecessors", 99).Error)
	createsBefore := f.engine.createCount("flaky")

	require.Error(t, f.resume(t, jobRun.ID))

	final := f.awaitRunStatus(t, jobRun.ID, run.StatusFailed)
	require.NotNil(t, final.CompletedAt)
	var retried models.TaskRun
	require.NoError(t, f.db.First(&retried, "id = ?", flakyID).Error)
	require.Equal(t, string(run.TaskStatusSkipped), retried.Status,
		"an undispatchable retry must be resolved explicitly, not left pending on a terminal run")
	require.False(t, retried.PartitionRetryPending)
	require.True(t, strings.Contains(retried.Error, "partition retry"), "the skip reason must say why: %q", retried.Error)
	require.Equal(t, createsBefore, f.engine.createCount("flaky"))

	// A third engine would reopen nothing (the marker is gone) but would still
	// be a bug; give one a moment to show itself.
	time.Sleep(50 * time.Millisecond)
	again := f.latestJobRun(t)
	require.Equal(t, string(run.StatusFailed), again.Status)
	require.Equal(t, final.CompletedAt.UnixNano(), again.CompletedAt.UnixNano(), "the run must be finalized exactly once")
}

// TestFanOutPartitionRetryResumeFailingBeforeCompletionFinalizesRun pins the
// early-initialization failure of a resumed engine. Partition-retry kickoff
// and the shutdown-window replacement both reopen the run and then hand it to
// a fresh Run; if that Run fails before its completion defer is armed (here:
// a secret resolver that cannot be built from the environment), nothing else
// will ever finalize the run. It must be marked failed with the retry-reset
// instance resolved explicitly, and callbacks must fire once as for any
// failed run — not left running forever with a pending retry.
func TestFanOutPartitionRetryResumeFailingBeforeCompletionFinalizesRun(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom")
	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	var flakyID uuid.UUID
	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "flaky" {
			flakyID = r.ID
		}
	}
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, flakyID)
	require.NoError(t, err)
	require.True(t, reopened)
	createsBefore := f.engine.createCount("flaky")

	var callbacks atomic.Int32
	vars := defaultFanOutVars()
	// A malformed identity keyring cannot be built into a resolver.
	vars.JobdefSecretsIdentityHMACKeys = "not-an-id-value-pair"
	opts := withTestDeps(f.store, vars, f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
		callbacks.Add(1)
		return nil
	}))
	err = New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(context.Background(), jobRun.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "secret resolver configuration failure",
		"precondition: the engine must fail before its completion defer is armed")

	final := f.awaitRunStatus(t, jobRun.ID, run.StatusFailed)
	require.NotNil(t, final.CompletedAt)
	require.Contains(t, final.Error, "secret resolver configuration failure")
	var retried models.TaskRun
	require.NoError(t, f.db.First(&retried, "id = ?", flakyID).Error)
	require.Equal(t, string(run.TaskStatusSkipped), retried.Status,
		"the accepted retry must be resolved explicitly, not left pending on a terminal run")
	require.False(t, retried.PartitionRetryPending)
	require.True(t, strings.Contains(retried.Error, "partition retry abandoned"), "the skip reason must say why: %q", retried.Error)
	require.Equal(t, createsBefore, f.engine.createCount("flaky"))
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load(), "a run finalized as failed dispatches callbacks exactly once")
}

// TestFanOutPartitionRetryReplacementHandsFreshRetryToAnotherEngine pins the
// boundary of the replacement loop-breaker. A replacement may only abandon the
// retries it was started for; a retry that lands in the REPLACEMENT's own
// shutdown window is a fresh, dispatchable request and must be handed to yet
// another engine — abandoning it would leave the partition skipped and, since
// only failed instances can be retried, permanently un-retryable after a 200.
func TestFanOutPartitionRetryReplacementHandsFreshRetryToAnotherEngine(t *testing.T) {
	f := newFanOutFixture(t, `["ok","r1","r2"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["r1"] = fmt.Errorf("boom r1")
	f.engine.createErrByPartition["r2"] = fmt.Errorf("boom r2")

	var callbacks atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
		callbacks.Add(1)
		return nil
	}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)

	var windows atomic.Int32
	retryInWindow := func(runID uuid.UUID, partition string) {
		f.engine.mu.Lock()
		delete(f.engine.createErrByPartition, partition)
		f.engine.mu.Unlock()
		for _, r := range f.instanceRowsFor(t, runID) {
			if r.PartitionValue == partition {
				_, reopened, err := f.store.RetryPartition(context.Background(), runID, r.ID)
				require.NoError(t, err)
				require.False(t, reopened, "the run is still running inside the engine's shutdown window")
			}
		}
	}
	// Each engine's shutdown window gets one retry: the first engine's window
	// retries r1 (handled by the replacement), the replacement's window
	// retries r2 (must be handed to a further engine, not abandoned).
	runner.beforeComplete = func(runID uuid.UUID) {
		switch windows.Add(1) {
		case 1:
			retryInWindow(runID, "r1")
		case 2:
			retryInWindow(runID, "r2")
		}
	}

	require.Error(t, runner.Run(context.Background()), "the first engine still reports its own failures")
	jobRun := f.latestJobRun(t)
	final := f.awaitRunTerminal(t, jobRun.ID)

	status := statusByPartition(f.instanceRowsFor(t, jobRun.ID))
	require.Equal(t, string(run.TaskStatusSucceeded), status["r1"])
	require.Equal(t, string(run.TaskStatusSucceeded), status["r2"],
		"a retry that lands in the replacement's shutdown window must be executed, not abandoned")
	require.Equal(t, string(run.StatusSucceeded), final.Status)
	require.Equal(t, int32(3), windows.Load(), "three engines: original, replacement for r1, replacement for r2")
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load(), "only the engine that finalizes the run dispatches callbacks")
}

// TestFanOutPartitionRetryResumeLeavesHaltedRootsAlone pins the fail-fast
// half of re-entry. Under the default halt policy the first failure clears the
// queue, leaving never-dispatched roots pending with indegree 0. A partition
// retry resumed into that run must execute the reset instance (and whatever
// its success releases) and nothing else — not resurrect a root the halt
// deliberately suppressed.
func TestFanOutPartitionRetryResumeLeavesHaltedRootsAlone(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	later := f.addSideTask(t, "later")
	// Position it after the fanned step so, with one task at a time, the
	// group's failure halts the run before it is ever dispatched.
	for _, taskModel := range f.taskSvc.tasks {
		if taskModel.ID == later {
			taskModel.Position = 2
		}
	}
	require.NoError(t, f.db.Model(&models.Task{}).Where("id = ?", later).Update("position", 2).Error)
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom")

	vars := defaultFanOutVars()
	vars.MaxParallelTasks = 1
	vars.TaskFailurePolicy = taskFailurePolicyHalt
	require.Error(t, f.run(t, vars))
	jobRun := f.latestJobRun(t)
	require.Len(t, f.engine.createRequestsForTask(later), 0, "precondition: the halt must suppress the later root")
	var laterRow models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", jobRun.ID, later).First(&laterRow).Error)
	require.Equal(t, string(run.TaskStatusPending), laterRow.Status)
	var flakyID uuid.UUID
	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "flaky" {
			flakyID = r.ID
		}
	}

	f.engine.mu.Lock()
	delete(f.engine.createErrByPartition, "flaky")
	f.engine.mu.Unlock()
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, flakyID)
	require.NoError(t, err)
	require.True(t, reopened)

	opts := withTestDeps(f.store, vars, f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	_ = New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(context.Background(), jobRun.ID))

	require.Equal(t, string(run.TaskStatusSucceeded), statusByPartition(f.instanceRows(t))["flaky"])
	require.Len(t, f.engine.createRequestsForTask(later), 0,
		"a partition retry must not resurrect a root the fail-fast halt suppressed")
	require.NoError(t, f.db.First(&laterRow, "id = ?", laterRow.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), laterRow.Status)
	f.awaitRunTerminal(t, jobRun.ID)
}

// TestFanOutPartitionRetryReplacementRetriesSamePartitionAgain pins that a
// replacement recognizes its own work by what it DISPATCHED, not by row
// identity. A partition retry reuses the instance row, so after the
// replacement ran r1 and r1 failed again, a second retry of r1 landing in the
// replacement's shutdown window is a fresh request that must be handed to a
// further engine — not abandoned as "the retry I could not dispatch".
func TestFanOutPartitionRetryReplacementRetriesSamePartitionAgain(t *testing.T) {
	f := newFanOutFixture(t, `["ok","r1"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["r1"] = fmt.Errorf("boom r1")

	var callbacks atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
		callbacks.Add(1)
		return nil
	}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)

	retryR1 := func(runID uuid.UUID) {
		for _, r := range f.instanceRowsFor(t, runID) {
			if r.PartitionValue == "r1" {
				_, reopened, err := f.store.RetryPartition(context.Background(), runID, r.ID)
				require.NoError(t, err)
				require.False(t, reopened)
			}
		}
	}
	var windows atomic.Int32
	runner.beforeComplete = func(runID uuid.UUID) {
		switch windows.Add(1) {
		case 1:
			// Still failing: the replacement will run r1 and see it fail again.
			retryR1(runID)
		case 2:
			// Fixed now: this retry lands in the replacement's own window.
			f.engine.mu.Lock()
			delete(f.engine.createErrByPartition, "r1")
			f.engine.mu.Unlock()
			retryR1(runID)
		}
	}

	require.Error(t, runner.Run(context.Background()))
	jobRun := f.latestJobRun(t)
	final := f.awaitRunTerminal(t, jobRun.ID)

	status := statusByPartition(f.instanceRowsFor(t, jobRun.ID))
	require.Equal(t, string(run.TaskStatusSucceeded), status["r1"],
		"a second retry of the same partition landing in the replacement's window must execute, not be abandoned")
	require.Equal(t, string(run.StatusSucceeded), final.Status)
	require.Equal(t, 3, f.engine.createCount("r1"), "original, replacement (failed again), further replacement (succeeded)")
	require.Equal(t, int32(3), windows.Load())
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load())
}

// TestFanOutPartitionRetryAbortedResumeHandsFreshRetryToAnotherEngine pins
// the early-failure finalizer's scope. A replacement that fails before it
// could execute anything may only abandon the retries it was started for; a
// retry accepted into the run meanwhile is fresh work that gets its own
// engine rather than being skipped unexecuted (and thereby made
// un-retryable).
func TestFanOutPartitionRetryAbortedResumeHandsFreshRetryToAnotherEngine(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky","other"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom flaky")
	f.engine.createErrByPartition["other"] = fmt.Errorf("boom other")
	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	ids := map[string]uuid.UUID{}
	for _, r := range f.instanceRows(t) {
		ids[r.PartitionValue] = r.ID
	}
	f.engine.mu.Lock()
	delete(f.engine.createErrByPartition, "flaky")
	delete(f.engine.createErrByPartition, "other")
	f.engine.mu.Unlock()
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, ids["flaky"])
	require.NoError(t, err)
	require.True(t, reopened)
	// Accepted while the run is (nominally) running again: no kickoff of its
	// own, it relies on the engine that owns the run.
	_, reopened, err = f.store.RetryPartition(context.Background(), jobRun.ID, ids["other"])
	require.NoError(t, err)
	require.False(t, reopened)

	// The replacement started for `flaky` fails to initialize; the engine it
	// hands `other` to must not.
	var envCalls atomic.Int32
	envFactory := func() env.Environment {
		vars := defaultFanOutVars()
		if envCalls.Add(1) == 1 {
			vars.JobdefSecretsIdentityHMACKeys = "not-an-id-value-pair"
		}
		return vars
	}
	var callbacks atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts,
		WithEnvVariables(envFactory),
		WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
			callbacks.Add(1)
			return nil
		}),
		withPartitionRetryReplacement([]uuid.UUID{ids["flaky"]}),
	)
	err = New(&models.Job{ID: f.jobID}, opts...).Run(run.WithContext(context.Background(), jobRun.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "secret resolver configuration failure")

	final := f.awaitRunTerminal(t, jobRun.ID)
	status := statusByPartition(f.instanceRowsFor(t, jobRun.ID))
	require.Equal(t, string(run.TaskStatusSucceeded), status["other"],
		"a retry accepted while the aborted engine was initializing is fresh work and must execute")
	require.Equal(t, string(run.TaskStatusSkipped), status["flaky"],
		"the retry the aborted engine was started for is abandoned explicitly")
	require.Equal(t, 1, f.engine.createCount("flaky"))
	require.Equal(t, 2, f.engine.createCount("other"))
	require.NotNil(t, final.CompletedAt)
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load(), "only the engine that finalizes the run dispatches callbacks")
}

// TestFanOutPartitionRetryAbortedResumeHandsRetryLandingDuringFinalizeToAnotherEngine
// pins the aborted-resume finalizer against a retry that commits between its
// scan of pending retries and its completion write. The fence refuses that
// write; the finalizer must classify again and hand the fresh retry to a
// replacement, exactly as the normal completion path does — not log and
// leave the run running with a retry nothing will execute.
func TestFanOutPartitionRetryAbortedResumeHandsRetryLandingDuringFinalizeToAnotherEngine(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky","other"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom flaky")
	f.engine.createErrByPartition["other"] = fmt.Errorf("boom other")
	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	ids := map[string]uuid.UUID{}
	for _, r := range f.instanceRows(t) {
		ids[r.PartitionValue] = r.ID
	}
	f.engine.mu.Lock()
	delete(f.engine.createErrByPartition, "flaky")
	delete(f.engine.createErrByPartition, "other")
	f.engine.mu.Unlock()
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, ids["flaky"])
	require.NoError(t, err)
	require.True(t, reopened)

	var envCalls atomic.Int32
	envFactory := func() env.Environment {
		vars := defaultFanOutVars()
		if envCalls.Add(1) == 1 {
			vars.JobdefSecretsIdentityHMACKeys = "not-an-id-value-pair"
		}
		return vars
	}
	var callbacks atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts,
		WithEnvVariables(envFactory),
		WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
			callbacks.Add(1)
			return nil
		}),
		withPartitionRetryReplacement([]uuid.UUID{ids["flaky"]}),
	)
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)
	// A retry of `other` commits after the finalizer's scan (which found only
	// `flaky`) and before its completion write. The seam is carried into the
	// replacement too; only the first window is choreographed.
	var fired atomic.Bool
	runner.beforeComplete = func(runID uuid.UUID) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		_, reopened, err := f.store.RetryPartition(context.Background(), runID, ids["other"])
		require.NoError(t, err)
		require.False(t, reopened, "the run is still open for the aborted engine's finalization")
	}
	err = runner.Run(run.WithContext(context.Background(), jobRun.ID))
	require.Error(t, err)
	require.True(t, fired.Load(), "precondition: the retry must interleave with the finalizer's completion write")

	final := f.awaitRunTerminal(t, jobRun.ID)
	status := statusByPartition(f.instanceRowsFor(t, jobRun.ID))
	require.Equal(t, string(run.TaskStatusSkipped), status["flaky"], "the retry the aborted engine was started for is abandoned")
	require.Equal(t, string(run.TaskStatusSucceeded), status["other"],
		"a retry that landed during the finalizer's completion must be handed to a further engine and executed")
	require.NotNil(t, final.CompletedAt)
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load())
}

// TestFanOutPartitionRetryCompletionNeverStrandsUnderRetryStream pins the end
// of the completion loop. Retries can keep landing in an engine's shutdown
// window; the loop is bounded so it cannot spin, but the bound must end in a
// hand-off, never in a return that leaves a retry pending on a run with no
// engine. Every retry accepted here is executed by some engine and the run
// finalizes exactly once.
func TestFanOutPartitionRetryCompletionNeverStrandsUnderRetryStream(t *testing.T) {
	f := newFanOutFixture(t, `["ok","r1","r2","r3","r4","r5","r6"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	for i := 1; i <= 6; i++ {
		f.engine.createErrByPartition[fmt.Sprintf("r%d", i)] = fmt.Errorf("boom")
	}

	var callbacks atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
		callbacks.Add(1)
		return nil
	}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)

	// Every completion window — in this engine and in each replacement —
	// retries one more failed partition, until all six have been retried.
	var next atomic.Int32
	runner.beforeComplete = func(runID uuid.UUID) {
		n := int(next.Add(1))
		if n > 6 {
			return
		}
		partition := fmt.Sprintf("r%d", n)
		f.engine.mu.Lock()
		delete(f.engine.createErrByPartition, partition)
		f.engine.mu.Unlock()
		for _, r := range f.instanceRowsFor(t, runID) {
			if r.PartitionValue == partition {
				_, _, err := f.store.RetryPartition(context.Background(), runID, r.ID)
				require.NoError(t, err)
			}
		}
	}

	require.Error(t, runner.Run(context.Background()))
	jobRun := f.latestJobRun(t)
	final := f.awaitRunTerminal(t, jobRun.ID)

	status := statusByPartition(f.instanceRowsFor(t, jobRun.ID))
	for i := 1; i <= 6; i++ {
		require.Equal(t, string(run.TaskStatusSucceeded), status[fmt.Sprintf("r%d", i)],
			"every accepted retry must be executed by some engine, never stranded at the loop's bound")
	}
	require.Equal(t, string(run.StatusSucceeded), final.Status)
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load())
}

// TestFanOutPartitionRetryCompletionBoundEndsInHandoff pins the last attempt
// of the completion loop. The loop re-classifies pending retries a bounded
// number of times; if the fence still refuses on the final attempt, whatever
// is pending must be handed to a replacement engine — returning would leave
// the retry pending on a run with no engine. The scan is blinded for the
// first attempts so the fence keeps refusing a row the loop cannot see.
func TestFanOutPartitionRetryCompletionBoundEndsInHandoff(t *testing.T) {
	f := newFanOutFixture(t, `["ok","r1"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["r1"] = fmt.Errorf("boom")

	var callbacks atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
		callbacks.Add(1)
		return nil
	}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)
	var windows atomic.Int32
	runner.beforeComplete = func(runID uuid.UUID) {
		if windows.Add(1) != 1 {
			return
		}
		f.engine.mu.Lock()
		delete(f.engine.createErrByPartition, "r1")
		f.engine.mu.Unlock()
		for _, r := range f.instanceRowsFor(t, runID) {
			if r.PartitionValue == "r1" {
				_, _, err := f.store.RetryPartition(context.Background(), runID, r.ID)
				require.NoError(t, err)
			}
		}
	}

	// Blind the completion loop's classify scans (the id/task_id select over
	// the marked rows) for every bounded attempt: the fence's own count still
	// sees the row, so the loop runs out of attempts with the retry pending
	// and unseen, and only its final hand-off can rescue it.
	var blinded atomic.Int32
	const name = "test:blind_pending_retry_scan"
	require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "task_runs" || len(tx.Statement.Selects) != 2 || tx.Statement.Selects[0] != "id" {
			return
		}
		where, ok := tx.Statement.Clauses["WHERE"]
		if !ok {
			return
		}
		w, ok := where.Expression.(clause.Where)
		if !ok {
			return
		}
		scan := false
		for _, e := range w.Exprs {
			if ex, ok := e.(clause.Expr); ok && strings.Contains(ex.SQL, "partition_retry_pending") {
				scan = true
			}
		}
		// Only the completion loop's scans (after the retry landed in the
		// window), not the engine's start-of-run scan.
		if !scan || windows.Load() == 0 || blinded.Load() >= 2 {
			return
		}
		blinded.Add(1)
		tx.Statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1 = 0"}}})
	}))
	t.Cleanup(func() { _ = f.db.Callback().Query().Remove(name) })

	require.Error(t, runner.Run(context.Background()))
	jobRun := f.latestJobRun(t)
	final := f.awaitRunTerminal(t, jobRun.ID)
	require.GreaterOrEqual(t, blinded.Load(), int32(2), "precondition: every classify attempt's scan was blinded")
	require.Equal(t, string(run.TaskStatusSucceeded), statusByPartition(f.instanceRowsFor(t, jobRun.ID))["r1"],
		"the loop's final attempt must hand the pending retry to a replacement, not return")
	require.Equal(t, string(run.StatusSucceeded), final.Status)
	awaitCallbacks(t, &callbacks, 1)
	require.Equal(t, int32(1), callbacks.Load())
}

// TestFanOutPartitionRetryAbandonFailureDoesNotChainReplacements pins the
// abandonment failure edge. If the store cannot resolve the rows a
// replacement was started for, they stay pending and marked; treating them as
// fresh work would start replacement after replacement, each failing the same
// way. The engine must stop and leave the run for an operator instead.
func TestFanOutPartitionRetryAbandonFailureDoesNotChainReplacements(t *testing.T) {
	f := newFanOutFixture(t, `["ok","flaky"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	f.engine.createErrByPartition["flaky"] = fmt.Errorf("boom")
	require.Error(t, f.run(t, defaultFanOutVars()))
	jobRun := f.latestJobRun(t)
	var flakyID uuid.UUID
	for _, r := range f.instanceRows(t) {
		if r.PartitionValue == "flaky" {
			flakyID = r.ID
		}
	}
	_, reopened, err := f.store.RetryPartition(context.Background(), jobRun.ID, flakyID)
	require.NoError(t, err)
	require.True(t, reopened)
	// Wedge the group so the replacement cannot dispatch the retry...
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", jobRun.ID, f.fanned).
		Update("outstanding_predecessors", 99).Error)
	// ...and make every abandonment write fail.
	const name = "test:fail_abandon"
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "task_runs" {
			return
		}
		if dest, ok := tx.Statement.Dest.(map[string]interface{}); ok {
			if status, ok := dest["status"].(string); ok && status == string(run.TaskStatusSkipped) {
				_ = tx.AddError(errors.New("simulated abandonment failure"))
			}
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Update().Remove(name) })

	var engines atomic.Int32
	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	opts = append(opts, withPartitionRetryReplacement([]uuid.UUID{flakyID}))
	runner := New(&models.Job{ID: f.jobID}, opts...).(*job)
	runner.beforeComplete = func(uuid.UUID) { engines.Add(1) }
	require.Error(t, runner.Run(run.WithContext(context.Background(), jobRun.ID)))

	// Give a mistaken chain of replacements time to show itself.
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(1), engines.Load(), "an abandonment the store refuses must not start replacement engines")
	var retried models.TaskRun
	require.NoError(t, f.db.First(&retried, "id = ?", flakyID).Error)
	require.Equal(t, string(run.TaskStatusPending), retried.Status)
	require.True(t, retried.PartitionRetryPending, "the row keeps its provenance for an operator")
	require.Equal(t, string(run.StatusRunning), f.latestJobRun(t).Status, "the run is left open rather than finalized over a retry nothing resolved")
}
