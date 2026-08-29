package run

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// store_fanout_review_test.go covers the SQL-lane fan-out defects raised in
// adversarial review of #349. Each test names the invariant it pins, not the
// function it calls, because several of these bugs were invisible to the
// existing tests precisely because those tests called the internal function
// directly instead of driving the state machine.

// --- Review finding 1: the group's terminal decision must be serialized ----

// TestGroupTerminalLockSQLIsDialectAware pins the Postgres guard for the
// concurrent terminal-group decision.
//
// On PostgreSQL two sibling completion transactions run concurrently under READ
// COMMITTED: each writes its own row terminal, then reads the group. Neither
// sees the other's uncommitted write, so BOTH conclude "a sibling is still
// running", both return false, and the group's cross-step successors are never
// advanced — the run hangs with every instance terminal. Serializing the
// decision on a single stable row is what makes exactly one transaction observe
// the complete set.
//
// dqlite/SQLite need no lock (every writer serializes through the Raft log /
// the single write connection), so the statement must be EMPTY there rather
// than emitting `FOR UPDATE`, which SQLite cannot parse.
func TestGroupTerminalLockSQLIsDialectAware(t *testing.T) {
	pg, err := groupTerminalLockSQL("postgres")
	require.NoError(t, err)
	assert.Contains(t, pg, "FOR UPDATE",
		"Postgres must take a row lock or two concurrent siblings both decide 'not yet'")
	assert.Contains(t, pg, "ORDER BY",
		"the locked row must be deterministic across transactions or they deadlock")

	for _, dialect := range []string{"sqlite", "dqlite"} {
		stmt, err := groupTerminalLockSQL(dialect)
		require.NoError(t, err, dialect)
		assert.Empty(t, stmt, "%s serializes writers already; FOR UPDATE is unparseable there", dialect)
	}

	_, err = groupTerminalLockSQL("mysql")
	require.Error(t, err, "an unknown dialect must fail loudly rather than silently skip the guard")
}

// TestGroupAdvancesExactlyOnceWhenFinalSiblingsRace pins the invariant the
// Postgres lock protects, on the dialect the unit suite can actually run: when
// the last siblings of a group complete, the group's cross-step successor is
// released exactly once — never zero times (the hang) and never twice.
func TestGroupAdvancesExactlyOnceWhenFinalSiblingsRace(t *testing.T) {
	f := newFanOutFixtureWithSuccessor(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)
	require.Len(t, rows, 2)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range rows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.store.CompleteTaskInstance(rows[i].ID, "success", nil, nil, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "sibling %d", i)
	}

	var successor models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.successor.ID).First(&successor).Error)
	assert.Equal(t, 0, successor.OutstandingPredecessors,
		"the group is fully terminal, so its cross-step successor must be released exactly once")

	// Scoped to the successor: the run's own start already announced the
	// producer ready, which is unrelated to the group's resolution.
	var ready []models.ExecutionEvent
	require.NoError(t, f.db.Where("type = ? AND run_id = ? AND task_id = ?",
		string(event.TypeTaskReady), f.runID, f.successor.ID).Find(&ready).Error)
	assert.Len(t, ready, 1, "the successor must be announced ready exactly once, not once per sibling")
}

// --- Review finding 2: busy-retry must not re-resolve a group by task id ---

// TestCacheHitInstanceSurvivesBusyRetry pins the retry-safety of the closure
// that runs under withStoreBusyRetry.
//
// cacheHitTask overwrote its captured `taskID` parameter with the row's CATALOG
// task id on the first attempt. A transient contention error rolls the
// transaction back and re-runs the same closure — which now resolves the
// catalog id, matching all N sibling rows, and fails with ErrAmbiguousTaskRun
// (reported to the worker as a claim mismatch). The retry must address exactly
// the instance the caller named.
func TestCacheHitInstanceSurvivesBusyRetry(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	failNextTaskRunUpdate(t, f.db)

	_, err = f.store.CacheHitTaskWithPartitions(f.runID, rows[1].ID, CacheHitSource{
		RunID: uuid.New(), CreatedAt: time.Now().UTC(),
	}, "success", nil, nil, nil)
	require.NoError(t, err, "a busy retry must re-address the same instance, not the ambiguous group")

	after := f.instances(t)
	assert.Equal(t, string(TaskStatusCached), after[1].Status)
	assert.Equal(t, string(TaskStatusPending), after[0].Status, "siblings are untouched")
	assert.Equal(t, string(TaskStatusPending), after[2].Status)
}

// TestCompleteTaskOwnerInstanceSurvivesBusyRetry is the same defect on the
// run-owner durable-write path, which resolves its row the same way and is the
// only completion route under CAESIUM_RUN_OWNER_IN_MEMORY.
func TestCompleteTaskOwnerInstanceSurvivesBusyRetry(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[1].ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"claimed_by": "node-a",
	}).Error)

	failNextTaskRunUpdate(t, f.db)

	require.NoError(t, f.store.CompleteTaskOwner(
		f.runID, rows[1].ID, TaskStatusSucceeded, "success", "", "node-a",
		nil, nil, 7, 1, 0, nil, nil,
	), "a busy retry must re-address the same instance, not the ambiguous group")

	after := f.instances(t)
	assert.Equal(t, string(TaskStatusSucceeded), after[1].Status)
	assert.Equal(t, string(TaskStatusPending), after[0].Status)
	assert.Equal(t, string(TaskStatusPending), after[2].Status)
}

// --- Review finding 3: expansion must not resurrect a resolved successor ---

// TestExpandSkipsAlreadyResolvedSuccessor pins that expansion runs only from
// the successor's unexpanded, unresolved state.
//
// The expansion loaded the successor template and rewrote it into N pending
// instances without checking that it was still an UNRESOLVED PENDING template.
// A successor already skipped by a branch selection or an unsatisfied trigger
// rule was therefore resurrected: instance 0 kept its skipped status while
// N-1 brand-new PENDING sibling rows were inserted beside it, so a step the DAG
// had decided not to run got dispatched anyway.
func TestExpandSkipsAlreadyResolvedSuccessor(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	// The successor was resolved before the producer's partitions landed —
	// exactly what a branch selection or a failed trigger rule does.
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Updates(map[string]any{
			"status":       string(TaskStatusSkipped),
			"error":        "not selected by branch",
			"completed_at": time.Now().UTC(),
		}).Error)

	expansion, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err, "a resolved successor is not an error, it is simply not expanded")
	if expansion != nil {
		assert.Empty(t, expansion.Groups,
			"a skipped successor must not be reported as an expanded group")
	}

	rows := f.instances(t)
	require.Len(t, rows, 1, "a skipped successor must not gain sibling instances")
	assert.Equal(t, string(TaskStatusSkipped), rows[0].Status,
		"the skipped template must stay skipped, not be rewritten to pending")
	assert.Equal(t, 0, rows[0].PartitionCount)
}

// TestExpandStillRunsFromPendingTemplate is the control: the ordinary path must
// keep working after the guard is added.
func TestExpandStillRunsFromPendingTemplate(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	expansion, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	require.NotNil(t, expansion)
	require.Len(t, expansion.Groups, 1)
	assert.Len(t, f.instances(t), 3)
}

// --- Review finding 4: ONE comprehensive reset contract -------------------

// TestRetryPartitionLeavesNoStaleExecutionEvidence pins that a retried instance
// is externally clean BEFORE it re-executes.
//
// retryResetColumns cleared status/claim/cache columns but left the previous
// attempt's OUTPUT, BRANCH SELECTIONS, LOG SNAPSHOT, SCHEMA VIOLATIONS, EXIT
// CODE and rate-limit park in place. A downstream step reading
// CAESIUM_OUTPUT_* off a pending-but-not-yet-rerun instance therefore consumed
// the FAILED attempt's values, `caesium logs` served the old container's tail,
// and the instance stayed parked behind a stale rate_limit_retry_after.
func TestRetryPartitionLeavesNoStaleExecutionEvidence(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	now := time.Now().UTC()
	exit := 137
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).Updates(map[string]any{
		"status":                 string(TaskStatusFailed),
		"completed_at":           now,
		"started_at":             now.Add(-time.Minute),
		"result":                 "failure",
		"error":                  "boom",
		"output":                 []byte(`{"rows":"0"}`),
		"branch_selections":      []byte(`["left"]`),
		"log_text":               "panic: previous attempt",
		"log_truncated":          true,
		"schema_violations":      []byte(`[{"key":"rows","message":"expected integer"}]`),
		"exit_code":              exit,
		"effective_hash":         "previous-effective-hash",
		"execution_descriptor":   []byte(`{"schemaVersion":1,"baseline":{"effectiveHash":"previous-effective-hash"},"cache":{"effectiveHash":"previous-effective-hash"}}`),
		"rate_limit_retry_after": now.Add(time.Hour),
	}).Error)
	markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)

	_, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)

	var row models.TaskRun
	require.NoError(t, f.db.Where("id = ?", rows[0].ID).First(&row).Error)
	assert.Empty(t, row.Output, "a downstream step must not read the failed attempt's outputs")
	assert.Empty(t, row.BranchSelections)
	assert.Empty(t, row.LogText, "the previous container's log tail must not survive the retry")
	assert.False(t, row.LogTruncated)
	assert.Empty(t, row.SchemaViolations)
	assert.Nil(t, row.ExitCode, "the failed attempt's exit code must not describe the pending one")
	assert.Empty(t, row.EffectiveHash, "the previous attempt's equivalence proof must not survive retry")
	var descriptor models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(row.ExecutionDescriptor, &descriptor))
	assert.Empty(t, descriptor.Baseline.EffectiveHash)
	assert.Empty(t, descriptor.Cache.EffectiveHash)
	assert.Nil(t, row.RateLimitRetryAfter, "a retried instance must not stay parked")
}

// TestRetryResetContractIsShared pins that there is exactly ONE definition of
// "reset for re-execution", so the run-level and partition-level retries can
// never drift apart again.
func TestRetryResetContractIsShared(t *testing.T) {
	cols := retryResetColumns()
	for _, name := range []string{
		"status", "completed_at", "started_at", "result", "error",
		"claimed_by", "claim_expires_at", "runtime_id", "attempt",
		"cache_hit", "cache_origin_run_id", "cache_created_at", "cache_expires_at",
		"effective_hash",
		"output", "branch_selections", "log_text", "log_truncated",
		"schema_violations", "exit_code", "rate_limit_retry_after",
	} {
		assert.Contains(t, cols, name, "the shared retry reset must clear %q", name)
	}
}

// --- Review finding 5: only a FAILED partition is retryable ---------------

// TestRetryPartitionOnlyAcceptsFailed pins the retryable set. Terminal is not
// the right predicate: re-running a SUCCEEDED or CACHED instance silently
// discards a result downstream steps have already consumed, and resurrecting a
// SKIPPED or CANCELLED one re-runs work the DAG deliberately resolved.
func TestRetryPartitionOnlyAcceptsFailed(t *testing.T) {
	for _, status := range []TaskStatus{
		TaskStatusSucceeded, TaskStatusCached, TaskStatusSkipped, TaskStatusCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
			_, err := f.expand(t, strParts("a", "b"))
			require.NoError(t, err)
			rows := f.instances(t)
			setInstanceOutcome(t, f.db, rows[0].ID, status, nil)
			markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)

			_, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
			require.ErrorIs(t, err, ErrPartitionNotRetryable)

			var row models.TaskRun
			require.NoError(t, f.db.Where("id = ?", rows[0].ID).First(&row).Error)
			assert.Equal(t, string(status), row.Status, "a refused retry must not mutate the row")
		})
	}

	t.Run("failed", func(t *testing.T) {
		f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
		_, err := f.expand(t, strParts("a", "b"))
		require.NoError(t, err)
		rows := f.instances(t)
		setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusFailed, nil)
		markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)

		updated, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
		require.NoError(t, err)
		assert.Equal(t, TaskStatusPending, updated.Status)
	})
}

// --- Review finding 6: a partially-failed group did not succeed -----------

// TestPartiallyFailedGroupEmitsNoTaskSucceeded pins the event contract under
// fanOut.failurePolicy: continue.
//
// A failed sibling emits task_failed and, under `continue`, its independent
// siblings keep running. The LAST sibling to land is then a SUCCESS, and it was
// making the group terminal and emitting a catalog-level task_succeeded — for a
// group that contains a failure. The incident subscriber treats task_succeeded
// as "this job/task later ran green" and remediated the incident the failed
// sibling had just opened, closing a live failure automatically.
func TestPartiallyFailedGroupEmitsNoTaskSucceeded(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	_, err = f.store.CompleteTaskInstance(rows[0].ID, "failure", nil, nil, nil)
	require.NoError(t, err)
	_, err = f.store.CompleteTaskInstance(rows[1].ID, "success", nil, nil, nil)
	require.NoError(t, err)

	after := f.instances(t)
	require.Equal(t, string(TaskStatusFailed), after[0].Status)
	require.Equal(t, string(TaskStatusSucceeded), after[1].Status)

	var succeeded []models.ExecutionEvent
	require.NoError(t, f.db.Where("type = ? AND run_id = ?", string(event.TypeTaskSucceeded), f.runID).
		Find(&succeeded).Error)
	assert.Empty(t, succeeded,
		"a group containing a failed partition has not succeeded; emitting task_succeeded auto-remediates the incident the failure opened")

	var failed []models.ExecutionEvent
	require.NoError(t, f.db.Where("type = ? AND run_id = ?", string(event.TypeTaskFailed), f.runID).
		Find(&failed).Error)
	assert.Len(t, failed, 1, "the failed partition still reports its own failure")
}

// TestFullySucceededGroupStillEmitsTaskSucceeded is the control: suppressing
// the event for a partially-failed group must not suppress it for a green one.
func TestFullySucceededGroupStillEmitsTaskSucceeded(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	for _, row := range rows {
		_, err = f.store.CompleteTaskInstance(row.ID, "success", nil, nil, nil)
		require.NoError(t, err)
	}

	var succeeded []models.ExecutionEvent
	require.NoError(t, f.db.Where("type = ? AND run_id = ?", string(event.TypeTaskSucceeded), f.runID).
		Find(&succeeded).Error)
	assert.Len(t, succeeded, 1, "a fully green group announces itself succeeded exactly once")
}

// --- helpers --------------------------------------------------------------

// failNextTaskRunUpdate installs a one-shot gorm callback that fails the next
// task_runs UPDATE with a transient contention error, so the caller's
// withStoreBusyRetry closure rolls back and re-runs from the top. This is the
// only way to exercise retry-safety deterministically: a real SQLITE_BUSY
// cannot be provoked on a single-connection test database.
func failNextTaskRunUpdate(t *testing.T, db *gorm.DB) {
	t.Helper()
	var (
		mu    sync.Mutex
		fired bool
	)
	const name = "test:busy_once"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		if fired || tx.Statement == nil || tx.Statement.Table != "task_runs" {
			return
		}
		fired = true
		_ = tx.AddError(errors.New("database is locked"))
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(name) })
}

// newFanOutFixtureWithSuccessor extends newFanOutFixture with a cross-step
// successor hanging off the fanned group, so a test can observe whether the
// group released it.
func newFanOutFixtureWithSuccessor(t *testing.T, fo *jobdefschema.FanOut) *fanOutSuccessorFixture {
	t.Helper()
	f := newFanOutFixture(t, fo)

	var consumerRun models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		First(&consumerRun).Error)

	successor := &models.Task{
		ID: uuid.New(), JobID: f.jobID, AtomID: consumerRun.AtomID, Name: "publish",
		Position: 2, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
	}
	require.NoError(t, f.db.Create(successor).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: f.jobID, FromTaskID: f.consumer.ID, ToTaskID: successor.ID,
	}).Error)
	require.NoError(t, f.db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: f.runID, TaskID: successor.ID, AtomID: consumerRun.AtomID,
		Engine: consumerRun.Engine, Image: consumerRun.Image, Command: consumerRun.Command,
		Status: string(TaskStatusPending), Attempt: 1, MaxAttempts: 1,
		OutstandingPredecessors: 1,
		CreatedAt:               time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error)

	return &fanOutSuccessorFixture{fanOutFixture: f, successor: successor}
}

type fanOutSuccessorFixture struct {
	*fanOutFixture
	successor *models.Task
}

// --- Cross-agent follow-up: canonical dependsOn at the persist site --------

// TestExpandPersistsCanonicalDependsOn pins that the in-group indegree and the
// persisted dependsOn list can never disagree about a key.
//
// The two are derived from DIFFERENT slices: indegree comes from
// pkgtask.ValidatePartitionGraph, which canonicalizes dependsOn internally,
// while partition_depends_on was marshalled from the RAW partition. A dependency
// written as " a" therefore produced indegree 1 (the graph saw "a") beside a
// persisted [" a"] that decrementInGroupDependentsTx — which matches
// `d == completed.PartitionValue` — can never satisfy. The dependent instance
// waits on a decrement that never comes: a permanent stall, for a whitespace
// difference.
//
// The marker parser now canonicalizes at the source, so this covers the
// non-parser producers: a cached producer replaying entry.Partitions and the
// owner's replan path.
func TestExpandPersistsCanonicalDependsOn(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{" a ", "a"}},
	})
	require.NoError(t, err)

	rows := f.instances(t)
	require.Len(t, rows, 2)
	var deps []string
	require.NoError(t, json.Unmarshal(rows[1].PartitionDependsOn, &deps))
	assert.Equal(t, []string{"a"}, deps,
		"the persisted dependsOn must be the same canonical key the indegree was computed from")
	assert.Equal(t, 2, rows[1].OutstandingPredecessors,
		"one cross-step predecessor plus one in-group dependency")

	// The end-to-end consequence: completing a must actually release b.
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.producer.ID).
		Update("status", string(TaskStatusSucceeded)).Error)
	_, err = f.store.CompleteTaskInstance(rows[0].ID, "success", nil, nil, nil)
	require.NoError(t, err)

	after := f.instances(t)
	assert.Equal(t, 1, after[1].OutstandingPredecessors,
		"b's in-group dependency on a is satisfied; only the cross-step predecessor remains")
}

// TestExpandRejectsEmptyDependsOnKey pins that a dependency that is empty after
// trimming is a producer error, not a silently dropped edge.
func TestExpandRejectsEmptyDependsOnKey(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"   "}},
	})
	require.Error(t, err)
	var partErr *pkgtask.PartitionError
	assert.ErrorAs(t, err, &partErr,
		"a malformed partition list is the PRODUCER's fault and must be reported as such")
}

// --- Cross-agent follow-up: the reset contract covers the in-run retry too --

// TestRetryTaskUsesTheSharedResetContract closes the last hole in the retry
// reset. RetryTask is the LOCAL lane's in-run retry (internal/job calls it for
// every `retries:` attempt), and it carried its own hand-maintained column list
// that predated schema_violations and exit_code — so a task retried locally
// re-executed still carrying the previous attempt's violations and exit code,
// which is what `caesium why` reports and what the incident classifier reads.
//
// Four retry paths now derive from retryResetColumns (RetryFromFailure,
// RetryPartition, RetryTaskInstance, RetryTask), so a column added to the
// contract cannot be forgotten by one of them again.
func TestRetryTaskUsesTheSharedResetContract(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	exit := 137
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[1].ID).Updates(map[string]any{
		"status":            string(TaskStatusFailed),
		"result":            "failure",
		"error":             "boom",
		"output":            []byte(`{"rows":"0"}`),
		"log_text":          "panic: previous attempt",
		"schema_violations": []byte(`[{"key":"rows","message":"expected integer"}]`),
		"exit_code":         exit,
	}).Error)

	require.NoError(t, f.store.RetryTask(f.runID, rows[1].ID, 2))

	var row models.TaskRun
	require.NoError(t, f.db.Where("id = ?", rows[1].ID).First(&row).Error)
	assert.Equal(t, string(TaskStatusPending), row.Status)
	assert.Equal(t, 2, row.Attempt, "an in-run retry is attempt N+1, not a reset to 1")
	assert.Empty(t, row.Output)
	assert.Empty(t, row.LogText)
	assert.Empty(t, row.SchemaViolations,
		"the previous attempt's violations must not describe the pending one")
	assert.Nil(t, row.ExitCode,
		"the previous attempt's exit code must not be what the incident classifier reads")
}
