package run

import (
	"context"
	"encoding/json"
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

// --- G6 write site: SaveSchemaViolations ---------------------------------

// TestSaveSchemaViolationsAddressesOneInstance pins the re-key: the write used
// to be `WHERE job_run_id = ? AND task_id = ?`, so one bad partition's schema
// violations were broadcast onto every sibling row and all N looked invalid.
func TestSaveSchemaViolationsAddressesOneInstance(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	violations := []pkgtask.SchemaViolation{{Key: "rows", Message: "expected integer"}}
	require.NoError(t, f.store.SaveSchemaViolations(f.runID, rows[1].ID, violations))

	after := f.instances(t)
	assert.Empty(t, after[0].SchemaViolations, "sibling a must not inherit b's violations")
	assert.NotEmpty(t, after[1].SchemaViolations)
	assert.Empty(t, after[2].SchemaViolations, "sibling c must not inherit b's violations")

	// Addressing the group by its catalog task ID is genuinely ambiguous and must
	// fail loudly rather than fan the write.
	err = f.store.SaveSchemaViolations(f.runID, f.consumer.ID, violations)
	require.ErrorIs(t, err, ErrAmbiguousTaskRun)
}

func TestSaveSchemaViolationsUnfannedStillResolvesByTaskID(t *testing.T) {
	f := newFanOutFixture(t, nil)
	require.NoError(t, f.store.SaveSchemaViolations(f.runID, f.consumer.ID,
		[]pkgtask.SchemaViolation{{Key: "rows", Message: "missing"}}))

	row, err := loadUniqueTaskRun(f.db, f.runID, f.consumer.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, row.SchemaViolations)
}

// --- G6 write site: markTaskSkippedTx ------------------------------------

// TestMarkTaskSkippedGivesEachInstanceItsOwnTerminalSequence covers the
// group-level skip: a cross-step predecessor failed, so the whole fanned
// successor group is skipped. The old single task-keyed UPDATE collapsed all N
// into one statement with terminal_sequence 0 and ONE event; each instance must
// now get its own sequence and its own event.
func TestMarkTaskSkippedGivesEachInstanceItsOwnTerminalSequence(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)

	var events []event.Event
	var counts dbWriteCounts
	var marked bool
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		marked, err = f.store.markTaskSkippedTx(tx, f.runID, f.consumer.ID, "predecessor failed", &events, &counts)
		return err
	}))
	assert.True(t, marked)

	rows := f.instances(t)
	require.Len(t, rows, 3)
	seqs := map[int64]bool{}
	for _, row := range rows {
		assert.Equal(t, string(TaskStatusSkipped), row.Status)
		require.Greater(t, row.TerminalSequence, int64(0),
			"partition %s must carry a non-zero terminal_sequence or replay cannot see it", row.PartitionValue)
		assert.False(t, seqs[row.TerminalSequence], "siblings must not share a terminal_sequence")
		seqs[row.TerminalSequence] = true
	}

	require.Len(t, events, 3, "each skipped instance emits its own task_skipped event")
	partitions := map[string]bool{}
	for _, evt := range events {
		var payload TaskRun
		require.NoError(t, json.Unmarshal(evt.Payload, &payload))
		partitions[payload.PartitionValue] = true
	}
	assert.Equal(t, map[string]bool{"a": true, "b": true, "c": true}, partitions)

	tail, err := f.store.TerminalTaskRunsSince(f.runID, 0)
	require.NoError(t, err)
	assert.Len(t, tail, 3, "every skipped instance must appear in the replay tail")
}

// --- G6 write site: recordTaskEventTx ------------------------------------

// TestTaskEventsCarryTheirOwnInstance pins that a lifecycle event is built from
// the row it is about. recordTaskEventTx used to `.First()` the
// (job_run_id, task_id) predicate, so all N siblings' events described whichever
// row the database happened to return first.
func TestTaskEventsCarryTheirOwnInstance(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	require.NoError(t, f.store.StartTask(f.runID, rows[2].ID, "runtime-c"))

	var stored []models.ExecutionEvent
	require.NoError(t, f.db.Where("type = ?", string(event.TypeTaskStarted)).Find(&stored).Error)
	require.Len(t, stored, 1)

	var payload TaskRun
	require.NoError(t, json.Unmarshal(stored[0].Payload, &payload))
	assert.Equal(t, rows[2].ID, payload.ID, "the event payload's id is the instance's TaskRun ID")
	assert.Equal(t, "c", payload.PartitionValue)
	assert.Equal(t, 2, payload.PartitionIndex)
	assert.Equal(t, 3, payload.PartitionCount)
	assert.Equal(t, "runtime-c", payload.RuntimeID)
}

// --- G6 read site + C3: PredecessorOutputs -------------------------------

// TestPredecessorOutputsAggregatesFannedPredecessor is the distributed half of
// the fan-in contract. The local lane calls pkgtask.AggregateFanInOutputs once
// per group; the SQL lane used to key a map by task_id, so N siblings collapsed
// last-writer-wins and a fan-in consumer saw ONE arbitrary partition's outputs.
// Both lanes must agree.
func TestPredecessorOutputsAggregatesFannedPredecessor(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	// Add a third step downstream of the fanned consumer: discover -> process -> report.
	report := &models.Task{ID: uuid.New(), JobID: f.jobID, AtomID: f.consumer.AtomID, Name: "report", Position: 2, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	require.NoError(t, f.db.Create(report).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{ID: uuid.New(), JobID: f.jobID, FromTaskID: f.consumer.ID, ToTaskID: report.ID}).Error)

	_, err := f.expand(t, strParts("east", "west", "north"))
	require.NoError(t, err)
	rows := f.instances(t)

	setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusSucceeded, map[string]string{"rows": "10"})
	setInstanceOutcome(t, f.db, rows[1].ID, TaskStatusCached, map[string]string{"rows": "20"})
	setInstanceOutcome(t, f.db, rows[2].ID, TaskStatusFailed, nil)

	got, err := f.store.PredecessorOutputs(f.runID, report.ID)
	require.NoError(t, err)
	require.Contains(t, got, "process")

	want, aggErr := pkgtask.AggregateFanInOutputs("process", map[string]map[string]string{
		"east": {"rows": "10"},
		"west": {"rows": "20"},
	}, 2, 1)
	require.NoError(t, aggErr)
	assert.Equal(t, want, got["process"],
		"the distributed lane must produce the same aggregate the local lane does")

	// Spot-check the shape rather than only the equality, so a change in the
	// aggregator is visible here too.
	assert.Equal(t, "2", got["process"]["PARTITION_COUNT"],
		"PARTITION_COUNT is the number of partitions that contributed output, matching the local aggregator")
	assert.Equal(t, "2", got["process"]["SUCCEEDED"])
	assert.Equal(t, "1", got["process"]["FAILED"])
	var perPartition map[string]string
	require.NoError(t, json.Unmarshal([]byte(got["process"]["rows"]), &perPartition))
	assert.Equal(t, map[string]string{"east": "10", "west": "20"}, perPartition)
}

func TestPredecessorOutputsUnfannedIsUnchanged(t *testing.T) {
	f := newFanOutFixture(t, nil)

	producer, err := loadUniqueTaskRun(f.db, f.runID, f.producer.ID)
	require.NoError(t, err)
	setInstanceOutcome(t, f.db, producer.ID, TaskStatusSucceeded, map[string]string{"path": "s3://bucket"})

	got, err := f.store.PredecessorOutputs(f.runID, f.consumer.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]map[string]string{
		"discover": {"path": "s3://bucket"},
	}, got, "an unfanned predecessor must present its raw output map, exactly as before")
}

// --- G6 read site: PredecessorHashes -------------------------------------

const goldenUnfannedHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// TestPredecessorHashListUnfannedIsByteIdentical is the golden test the plan
// requires: changing PredecessorHashes must not change the cache identity of any
// existing (unfanned) job. An unfanned predecessor contributes its own effective
// hash, verbatim and alone.
func TestPredecessorHashListUnfannedIsByteIdentical(t *testing.T) {
	taskID := uuid.New()
	rows := []models.TaskRun{{
		TaskID:         taskID,
		Hash:           goldenUnfannedHash,
		Status:         string(TaskStatusSucceeded),
		PartitionIndex: 0,
	}}
	assert.Equal(t, []string{goldenUnfannedHash}, predecessorHashList(rows))

	// effective_hash still wins over hash — the value-verified short-circuit.
	rows[0].EffectiveHash = "aaaa"
	assert.Equal(t, []string{"aaaa"}, predecessorHashList(rows))
}

// TestPredecessorHashListFannedIsOneDeterministicGroupHash pins the fan-out
// contract: N instances contribute ONE aggregate identity, not N entries, or the
// downstream identity key changes shape and cache-misses forever.
func TestPredecessorHashListFannedIsOneDeterministicGroupHash(t *testing.T) {
	taskID := uuid.New()
	instances := []models.TaskRun{
		{ID: uuid.New(), TaskID: taskID, Hash: "h0", PartitionValue: "a", PartitionIndex: 0, PartitionCount: 3},
		{ID: uuid.New(), TaskID: taskID, Hash: "h1", PartitionValue: "b", PartitionIndex: 1, PartitionCount: 3},
		{ID: uuid.New(), TaskID: taskID, Hash: "h2", PartitionValue: "c", PartitionIndex: 2, PartitionCount: 3},
	}

	got := predecessorHashList(instances)
	require.Len(t, got, 1, "a fanned predecessor contributes exactly one hash")

	// Deterministic: row order must not matter, only partition index.
	shuffled := []models.TaskRun{instances[2], instances[0], instances[1]}
	assert.Equal(t, got, predecessorHashList(shuffled),
		"the group hash must not depend on which instance the database returned first")

	// Sensitive: changing one instance's hash changes the group hash.
	changed := append([]models.TaskRun(nil), instances...)
	changed[1].Hash = "h1-changed"
	assert.NotEqual(t, got, predecessorHashList(changed))

	// Order-sensitive: the same hashes at different partition indexes are a
	// different group.
	reindexed := append([]models.TaskRun(nil), instances...)
	reindexed[0].PartitionIndex, reindexed[2].PartitionIndex = 2, 0
	assert.NotEqual(t, got, predecessorHashList(reindexed))

	// And it is never one of the member hashes.
	assert.NotContains(t, []string{"h0", "h1", "h2"}, got[0])
}

func TestPredecessorHashesFannedPredecessorEndToEnd(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	report := &models.Task{ID: uuid.New(), JobID: f.jobID, AtomID: f.consumer.AtomID, Name: "report", Position: 2, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	require.NoError(t, f.db.Create(report).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{ID: uuid.New(), JobID: f.jobID, FromTaskID: f.consumer.ID, ToTaskID: report.ID}).Error)

	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)
	for i, row := range rows {
		require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", row.ID).Updates(map[string]any{
			"status": string(TaskStatusSucceeded),
			"hash":   []string{"hash-a", "hash-b"}[i],
		}).Error)
	}

	hashes, err := f.store.PredecessorHashes(f.runID, report.ID)
	require.NoError(t, err)
	require.Len(t, hashes, 1, "two sibling instances must present ONE predecessor identity")
	assert.NotContains(t, []string{"hash-a", "hash-b"}, hashes[0],
		"the group identity is an aggregate, never one arbitrary instance's hash")

	// It matches the pure-function definition, so the SQL path and the kernel
	// cannot drift.
	var reloaded []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Order("partition_index ASC").Find(&reloaded).Error)
	assert.Equal(t, predecessorGroupHash(reloaded), hashes[0])
}

// --- G1 open question: RateLimitTask -------------------------------------

// TestRateLimitTaskParksOneInstance settles the G1 open question in a test: the
// `status IN (pending, running)` predicate is kept (a claim flips the row to
// running BEFORE the rate-limit rejection is discovered, so the parked row is
// legitimately running and has no container to orphan), but the write is keyed
// to one instance so a running SIBLING is never re-pended out from under its
// live container.
func TestRateLimitTaskParksOneInstance(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	// b and c are already running with live containers.
	for _, row := range rows[1:] {
		require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", row.ID).Updates(map[string]any{
			"status":     string(TaskStatusRunning),
			"runtime_id": "container-" + row.PartitionValue,
			"claimed_by": "worker-1",
		}).Error)
	}
	// a is running too and is the one whose rate-limit acquisition was rejected.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"runtime_id": "container-a",
		"claimed_by": "worker-1",
	}).Error)

	retryAfter := time.Now().UTC().Add(time.Minute)
	require.NoError(t, f.store.RateLimitTask(context.Background(), f.runID, rows[0].ID, retryAfter))

	after := f.instances(t)
	assert.Equal(t, string(TaskStatusPending), after[0].Status, "the rejected instance parks")
	assert.Empty(t, after[0].RuntimeID)
	require.NotNil(t, after[0].RateLimitRetryAfter)

	for _, row := range after[1:] {
		assert.Equal(t, string(TaskStatusRunning), row.Status,
			"partition %s was running: parking a sibling must not orphan its container", row.PartitionValue)
		assert.Equal(t, "container-"+row.PartitionValue, row.RuntimeID)
		assert.Nil(t, row.RateLimitRetryAfter)
	}

	// Naming the group by its catalog task ID is ambiguous and must not park a
	// sibling silently.
	err = f.store.RateLimitTask(context.Background(), f.runID, f.consumer.ID, retryAfter)
	require.ErrorIs(t, err, ErrAmbiguousTaskRun)
}

// --- E1: RetryPartition --------------------------------------------------

func markRunTerminalForPartitionRetry(t *testing.T, db *gorm.DB, runID uuid.UUID, status Status) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).
		Updates(map[string]any{"status": string(status), "completed_at": now}).Error)
}

func TestRetryPartitionRejectsNonTerminalRun(t *testing.T) {
	for _, status := range []Status{StatusRunning, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
			_, err := f.expand(t, strParts("a", "b"))
			require.NoError(t, err)
			rows := f.instances(t)
			setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusFailed, nil)
			if status == StatusCancelled {
				markRunTerminalForPartitionRetry(t, f.db, f.runID, status)
			}

			_, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
			require.ErrorIs(t, err, ErrRunNotTerminal)

			var row models.TaskRun
			require.NoError(t, f.db.First(&row, "id = ?", rows[0].ID).Error)
			assert.Equal(t, string(TaskStatusFailed), row.Status,
				"a rejected retry must not reset the failed instance")
		})
	}
}

func TestRetryPartitionRejectsNonTerminalInstance(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)
	markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)

	// pending
	_, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.ErrorIs(t, err, ErrTaskRunNotTerminal)

	// running — the dangerous case: resetting here orphans a live container.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).
		Update("status", string(TaskStatusRunning)).Error)
	_, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.ErrorIs(t, err, ErrTaskRunNotTerminal)

	after := f.instances(t)
	assert.Equal(t, string(TaskStatusRunning), after[0].Status, "a refused retry must not mutate the row")
}

func TestRetryPartitionResetsEveryExecutionColumn(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	// Satisfy the cross-step predecessor so the retried instance comes back ready.
	producer, err := loadUniqueTaskRun(f.db, f.runID, f.producer.ID)
	require.NoError(t, err)
	setInstanceOutcome(t, f.db, producer.ID, TaskStatusSucceeded, nil)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Update("outstanding_predecessors", 0).Error)

	now := time.Now().UTC()
	originRun := uuid.New()
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).Updates(map[string]any{
		"status":              string(TaskStatusFailed),
		"completed_at":        now,
		"started_at":          now.Add(-time.Minute),
		"result":              "failure",
		"error":               "boom",
		"claimed_by":          "worker-7",
		"claim_expires_at":    now.Add(time.Minute),
		"runtime_id":          "container-abc",
		"attempt":             3,
		"cache_hit":           true,
		"cache_origin_run_id": originRun,
		"cache_created_at":    now,
		"cache_expires_at":    now.Add(time.Hour),
	}).Error)
	markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)

	updated, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusPending, updated.Status)

	var row models.TaskRun
	require.NoError(t, f.db.Where("id = ?", rows[0].ID).First(&row).Error)
	assert.Equal(t, string(TaskStatusPending), row.Status)
	assert.Nil(t, row.CompletedAt)
	assert.Nil(t, row.StartedAt)
	assert.Empty(t, row.Result)
	assert.Empty(t, row.Error)
	assert.Empty(t, row.ClaimedBy, "a retried instance must not stay claimed by a worker that will never run it")
	assert.Nil(t, row.ClaimExpiresAt)
	assert.Empty(t, row.RuntimeID)
	assert.Equal(t, 1, row.Attempt)
	assert.False(t, row.CacheHit)
	assert.Nil(t, row.CacheOriginRunID)
	assert.Nil(t, row.CacheCreatedAt)
	assert.Nil(t, row.CacheExpiresAt)
	assert.Equal(t, 0, row.OutstandingPredecessors, "its predecessors already succeeded, so it is immediately ready")

	// The sibling is untouched.
	var sibling models.TaskRun
	require.NoError(t, f.db.Where("id = ?", rows[1].ID).First(&sibling).Error)
	assert.Equal(t, string(TaskStatusPending), sibling.Status)
	assert.Equal(t, 0, sibling.OutstandingPredecessors)
}

func TestRetryPartitionReseedsInGroupIndegreeOverNonTerminalDeps(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
	})
	require.NoError(t, err)

	rows := f.instances(t)
	byKey := map[string]models.TaskRun{}
	for _, r := range rows {
		byKey[r.PartitionValue] = r
	}
	producer, err := loadUniqueTaskRun(f.db, f.runID, f.producer.ID)
	require.NoError(t, err)
	setInstanceOutcome(t, f.db, producer.ID, TaskStatusSucceeded, nil)

	// a succeeded, b failed. Retrying b must come back ready (indegree 0) because
	// its only in-group dependency already succeeded.
	setInstanceOutcome(t, f.db, byKey["a"].ID, TaskStatusSucceeded, nil)
	setInstanceOutcome(t, f.db, byKey["b"].ID, TaskStatusFailed, nil)
	markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)

	_, err = f.store.RetryPartition(context.Background(), f.runID, byKey["b"].ID)
	require.NoError(t, err)
	var b models.TaskRun
	require.NoError(t, f.db.Where("id = ?", byKey["b"].ID).First(&b).Error)
	assert.Equal(t, 0, b.OutstandingPredecessors)

	// Now fail a too and retry b again; b must NOT be dragged along, and retrying
	// b while a is non-terminal must leave b waiting on a. b is put back into the
	// FAILED state for the second retry because failed is the retryable set
	// (RetryPartition rejects skipped with ErrPartitionNotRetryable); the
	// indegree re-seed under test is unaffected by which terminal state b came
	// from.
	setInstanceOutcome(t, f.db, byKey["a"].ID, TaskStatusFailed, nil)
	setInstanceOutcome(t, f.db, byKey["b"].ID, TaskStatusFailed, nil)
	markRunTerminalForPartitionRetry(t, f.db, f.runID, StatusFailed)
	_, err = f.store.RetryPartition(context.Background(), f.runID, byKey["b"].ID)
	require.NoError(t, err)
	require.NoError(t, f.db.Where("id = ?", byKey["b"].ID).First(&b).Error)
	assert.Equal(t, 1, b.OutstandingPredecessors,
		"b waits on a, which is not a terminal success")
}

func TestRetryPartitionReopensTerminalRunAndInvalidatesCheckpoints(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusFailed, nil)
	setInstanceOutcome(t, f.db, rows[1].ID, TaskStatusSucceeded, nil)
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).
		Updates(map[string]any{"status": string(StatusFailed), "completed_at": time.Now().UTC()}).Error)
	require.NoError(t, f.store.WriteCheckpoint(f.runID, 5, 1, 0, []byte(`{"v":1}`), false))

	_, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)

	var jobRun models.JobRun
	require.NoError(t, f.db.Where("id = ?", f.runID).First(&jobRun).Error)
	assert.Equal(t, string(StatusRunning), jobRun.Status,
		"a per-partition retry of a finished run must re-open the run or nothing ever dispatches it")
	assert.Nil(t, jobRun.CompletedAt)

	cp, err := f.store.LatestFullCheckpoint(f.runID)
	require.NoError(t, err)
	assert.Nil(t, cp, "the pre-retry checkpoint must be invalidated or a recovering owner re-adopts the stale state")
}

// --- G1/item 4: CompleteTaskOwner skip loop ------------------------------

// TestCompleteTaskOwnerSkipDoesNotFanAcrossSiblings pins the removal of the
// task-keyed fallback UPDATE. The owner names a skip by instance identity; when
// a skip legitimately names a whole group, each instance must get its own
// terminal_sequence rather than all N sharing one.
func TestCompleteTaskOwnerSkipTargetsResolveToInstances(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	// A TaskRun primary key resolves to exactly that instance.
	var targets []models.TaskRun
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		targets, err = resolveSkipTargetsTx(tx, f.runID, rows[1].ID)
		return err
	}))
	require.Len(t, targets, 1)
	assert.Equal(t, rows[1].ID, targets[0].ID)

	// A catalog task ID naming an expanded group resolves to every instance, so
	// the caller can stamp each with its own sequence instead of broadcasting one
	// UPDATE across the group.
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		targets, err = resolveSkipTargetsTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	require.Len(t, targets, 3)
	assert.Equal(t, []string{"a", "b", "c"},
		[]string{targets[0].PartitionValue, targets[1].PartitionValue, targets[2].PartitionValue})
}

func TestCompleteTaskOwnerSkipsGroupWithPerInstanceSequences(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)

	// The producer completes through the owner path and decides the whole fanned
	// group is skipped.
	producer, err := loadUniqueTaskRun(f.db, f.runID, f.producer.ID)
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", producer.ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"claimed_by": "worker-1",
	}).Error)

	require.NoError(t, f.store.CompleteTaskOwner(
		f.runID, f.producer.ID, TaskStatusSucceeded, "success", "", "worker-1",
		nil, nil, 1, 1, 0,
		[]SkippedTask{{TaskID: f.consumer.ID, TerminalSequence: 2, Reason: "trigger rule not satisfied"}},
		nil,
	))

	rows := f.instances(t)
	require.Len(t, rows, 3)
	seqs := map[int64]bool{}
	for _, row := range rows {
		assert.Equal(t, string(TaskStatusSkipped), row.Status)
		require.Greater(t, row.TerminalSequence, int64(0))
		assert.False(t, seqs[row.TerminalSequence],
			"partition %s reused a sibling's terminal_sequence", row.PartitionValue)
		seqs[row.TerminalSequence] = true
	}
}

func setInstanceOutcome(t *testing.T, db *gorm.DB, id uuid.UUID, status TaskStatus, output map[string]string) {
	t.Helper()
	now := time.Now().UTC()
	updates := map[string]any{
		"status":       string(status),
		"started_at":   now.Add(-time.Minute),
		"completed_at": now,
	}
	if output != nil {
		encoded, err := json.Marshal(output)
		require.NoError(t, err)
		updates["output"] = encoded
	}
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", id).Updates(updates).Error)
}

// --- G1: identity setters and per-instance retry/skip ---------------------

// TestHashSettersAddressOneInstance pins the identity-write re-key. These
// setters still resolved through loadUniqueTaskRun, so for a fanned step every
// per-instance identity write returned ErrAmbiguousTaskRun and the hash was
// never persisted at all: `caesium why --partition`, `receipt get` and
// `run retry --partition` had nothing to match, and the local lane published no
// per-partition cache entry (caught by internal/job's fan-out local suite).
func TestHashSettersAddressOneInstance(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	rows := f.instances(t)

	for i, row := range rows {
		hash := "hash-" + row.PartitionValue
		require.NoError(t, f.store.SetTaskHashWithBlob(f.runID, row.ID, hash, "sha256:digest", []byte(`{"k":"v"}`)),
			"instance %d must persist its own identity", i)
	}
	require.NoError(t, f.store.SetTaskEffectiveHash(f.runID, rows[1].ID, "effective-b"))

	after := f.instances(t)
	for _, row := range after {
		assert.Equal(t, "hash-"+row.PartitionValue, row.Hash,
			"partition %s must carry its own hash, not a sibling's", row.PartitionValue)
		assert.NotEmpty(t, row.HashInputBlob)
		assert.Equal(t, "sha256:digest", row.ResolvedImageDigest)
	}
	assert.Empty(t, after[0].EffectiveHash)
	assert.Equal(t, "effective-b", after[1].EffectiveHash,
		"a value-verified short-circuit is proven per instance and recorded on that instance")
	assert.Empty(t, after[2].EffectiveHash)
}

// TestRetryTaskResetsOneInstanceOnly pins that a per-instance attempt retry does
// not discard its siblings' results. retryTask clears output/result, so
// resolving it by catalog task ID would wipe the whole group.
func TestRetryTaskResetsOneInstanceOnly(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)

	setInstanceOutcome(t, f.db, rows[0].ID, TaskStatusSucceeded, map[string]string{"rows": "10"})
	setInstanceOutcome(t, f.db, rows[1].ID, TaskStatusFailed, map[string]string{"rows": "0"})

	require.NoError(t, f.store.RetryTask(f.runID, rows[1].ID, 2))

	after := f.instances(t)
	assert.Equal(t, string(TaskStatusSucceeded), after[0].Status, "sibling a must keep its outcome")
	assert.NotEmpty(t, after[0].Output, "sibling a must keep its output")
	assert.Equal(t, string(TaskStatusPending), after[1].Status)
	assert.Equal(t, 2, after[1].Attempt)
	assert.Empty(t, after[1].Output)
}

// TestSkipTaskSkipsTheWholeGroup pins the other half: every caller of SkipTask
// uses it to skip a STEP whose trigger rule was not satisfied, so under fan-out
// it must resolve the whole group — one terminal_sequence and one event per
// instance.
func TestSkipTaskSkipsTheWholeGroup(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)

	require.NoError(t, f.store.SkipTask(f.runID, f.consumer.ID, "trigger rule not satisfied"))

	rows := f.instances(t)
	seqs := map[int64]bool{}
	for _, row := range rows {
		assert.Equal(t, string(TaskStatusSkipped), row.Status,
			"partition %s must be skipped or its counter never reaches zero", row.PartitionValue)
		require.Greater(t, row.TerminalSequence, int64(0))
		assert.False(t, seqs[row.TerminalSequence])
		seqs[row.TerminalSequence] = true
	}

	var skipped []models.ExecutionEvent
	require.NoError(t, f.db.Where("type = ?", string(event.TypeTaskSkipped)).Find(&skipped).Error)
	assert.Len(t, skipped, 3, "one task_skipped event per instance")
}

// TestGroupIdentityHashIsTheOneDefinition pins that the SQL read path and the
// exported helper the LOCAL executor calls produce the identical value. The two
// lanes must not disagree about a fanned predecessor's identity, or a downstream
// step cache-hits in one lane and misses in the other.
func TestGroupIdentityHashIsTheOneDefinition(t *testing.T) {
	taskID := uuid.New()
	instances := []models.TaskRun{
		{ID: uuid.New(), TaskID: taskID, Hash: "h0", PartitionValue: "a", PartitionIndex: 0, PartitionCount: 3},
		{ID: uuid.New(), TaskID: taskID, Hash: "h1", EffectiveHash: "e1", PartitionValue: "b", PartitionIndex: 1, PartitionCount: 3},
		{ID: uuid.New(), TaskID: taskID, Hash: "h2", PartitionValue: "c", PartitionIndex: 2, PartitionCount: 3},
	}

	// What the local executor computes from its in-memory per-instance hashes,
	// in partition-index order, with effective_hash winning where set.
	local := GroupIdentityHash([]string{"h0", "e1", "h2"})
	require.NotEmpty(t, local)

	assert.Equal(t, local, predecessorGroupHash(instances),
		"the SQL read path and the exported helper must be one definition")
	assert.Equal(t, []string{local}, predecessorHashList(instances))

	// Empty inputs collapse to "", never to a hash of nothing.
	assert.Empty(t, GroupIdentityHash(nil))
	assert.Empty(t, GroupIdentityHash([]string{"", ""}))
}

// --- P0a: cache-hit expansion --------------------------------------------

// TestCacheHitTaskWithPartitionsExpandsTheGroup pins the route the design calls
// out as the easy one to miss: a cache hit IS a completion, and cacheHitTask is
// a different function from completeTask. A producer whose own work cache-hits
// still emits the partition list from the cached entry, so the expansion must
// ride the cache-hit transaction exactly as it rides the completion one.
// Without this the group collapses to its single template row and every
// downstream instance is silently lost — with no error anywhere, because the
// worker's interface assertion simply misses and logs a warning.
func TestCacheHitTaskWithPartitionsExpandsTheGroup(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	res, err := f.store.CacheHitTaskWithPartitions(
		f.runID, f.producer.ID, CacheHitSource{RunID: uuid.New(), CreatedAt: time.Now().UTC()},
		"success", map[string]string{"n": "3"}, nil, strParts("a", "b", "c"),
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Expansion, "the local Kahn loop learns about instances ONLY from this payload")
	require.Len(t, res.Expansion.Groups, 1)
	assert.Len(t, res.Expansion.Groups[0].Instances, 3)

	rows := f.instances(t)
	require.Len(t, rows, 3, "a cached producer must materialize one instance row per partition")
	for i, want := range []string{"a", "b", "c"} {
		assert.Equal(t, want, rows[i].PartitionValue)
		assert.Equal(t, i, rows[i].PartitionIndex)
		assert.Equal(t, 3, rows[i].PartitionCount)
	}

	// The producer's own row carries the normalized list for observability, and
	// is itself cached rather than succeeded.
	producer := f.producerRow(t)
	assert.Equal(t, string(TaskStatusCached), producer.Status)
	assert.NotEmpty(t, producer.Partitions, "the producer list must be persisted on the cache-hit route too")
}

// TestCacheHitTaskClaimedWithPartitionsExpandsTheGroup is the distributed twin:
// the same expansion, claim-fenced. internal/worker/completion_sink.go resolves
// this method by interface assertion, so a signature drift shows up as a
// silently-unexpanded group rather than a compile error.
func TestCacheHitTaskClaimedWithPartitionsExpandsTheGroup(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	producer := f.producerRow(t)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", producer.ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"claimed_by": "worker-1",
	}).Error)

	src := CacheHitSource{RunID: uuid.New(), CreatedAt: time.Now().UTC()}
	require.NoError(t, f.store.CacheHitTaskClaimedWithPartitions(
		f.runID, producer.ID, src, "success", "worker-1", nil, nil, strParts("a", "b", "c"),
	))
	require.Len(t, f.instances(t), 3)

	// A mismatched claim must not expand anything.
	f2 := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	p2 := f2.producerRow(t)
	require.NoError(t, f2.db.Model(&models.TaskRun{}).Where("id = ?", p2.ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"claimed_by": "worker-1",
	}).Error)
	err := f2.store.CacheHitTaskClaimedWithPartitions(
		f2.runID, p2.ID, src, "success", "worker-2", nil, nil, strParts("a", "b", "c"),
	)
	require.ErrorIs(t, err, ErrTaskClaimMismatch)
	require.Len(t, f2.instances(t), 1, "a fenced-off cache hit must not expand the group")
}

// TestCacheHitTaskWithPartitionsRunsTheSameValidation asserts the cache-hit
// route shares expansion's validation rather than getting a second, laxer copy:
// a cycle in the producer's dependsOn graph fails the producing task and rolls
// the transaction back, leaving no instance rows behind.
func TestCacheHitTaskWithPartitionsRunsTheSameValidation(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.store.CacheHitTaskWithPartitions(
		f.runID, f.producer.ID, CacheHitSource{RunID: uuid.New()}, "success", nil, nil,
		[]pkgtask.Partition{
			{Key: "a", DependsOn: []string{"b"}},
			{Key: "b", DependsOn: []string{"a"}},
		},
	)
	require.Error(t, err)
	var pErr *pkgtask.PartitionError
	require.ErrorAs(t, err, &pErr, "a bad partition graph must fail the PRODUCER, not the store")

	require.Len(t, f.instances(t), 1, "a rejected expansion must leave the template row alone")
	assert.NotEqual(t, string(TaskStatusCached), f.producerRow(t).Status,
		"the producer's terminal write must roll back with the expansion")
}

// --- P0b: SQL-lane fail_fast ---------------------------------------------

// TestFailTaskFailFastSkipsEveryPendingSibling is the SQL half of what
// owner_state.go already does in memory (TestRunState_FailFastResolvesEveryPendingSibling).
// A lane-dependent failure policy is exactly the mode-dependent defect contract
// (7) exists to prevent: fail_fast would hold under CAESIUM_RUN_OWNER_IN_MEMORY
// and silently degrade to `continue` in the default SQL configuration.
//
// PENDING, not merely non-terminal, is what the design specifies (fail_fast
// "cancels pending siblings"), so the fixture deliberately holds one sibling in
// `running` and asserts it is LEFT ALONE — see the note on failFastSkipSiblingsTx
// about the owner lane currently being a superset here.
func TestFailTaskFailFastSkipsEveryPendingSibling(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureFailFast,
	})
	// bad, gate, and three dependents of gate: only `bad` fails, but fail_fast
	// resolves the whole group, dependency edges or not.
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "gate"},
		{Key: "x", DependsOn: []string{"gate"}},
		{Key: "y", DependsOn: []string{"gate"}},
		{Key: "z", DependsOn: []string{"gate"}},
	})
	require.NoError(t, err)
	rows := f.instances(t)
	require.Len(t, rows, 5)
	byKey := map[string]models.TaskRun{}
	for _, r := range rows {
		byKey[r.PartitionValue] = r
	}

	// `gate` is mid-flight with a live container when `bad` fails.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", byKey["gate"].ID).Updates(map[string]any{
		"status":     string(TaskStatusRunning),
		"runtime_id": "container-gate",
	}).Error)

	require.NoError(t, f.store.FailTaskInstance(f.runID, byKey["bad"].ID, assert.AnError))

	after := map[string]models.TaskRun{}
	seqs := map[int64]string{}
	for _, r := range f.instances(t) {
		after[r.PartitionValue] = r
	}
	assert.Equal(t, string(TaskStatusFailed), after["bad"].Status)

	// The load-bearing assertion: a sibling that had NOT started is resolved and
	// so is never dispatched. x/y/z are blocked behind `gate`, so under
	// `continue` they would still be waiting; fail_fast resolves them now.
	for _, key := range []string{"x", "y", "z"} {
		r := after[key]
		assert.Equal(t, string(TaskStatusSkipped), r.Status, "partition %s", key)
		assert.Equal(t, "fan-out group failed fast", r.Error,
			"the reason string is the contract both lanes emit; partition %s", key)
		require.NotZero(t, r.TerminalSequence,
			"a zero terminal_sequence is invisible to TerminalTaskRunsSince replay; partition %s", key)
		prev, dup := seqs[r.TerminalSequence]
		require.False(t, dup, "partitions %s and %s share terminal_sequence %d; the replay tail is a dense, strictly-ordered space",
			prev, key, r.TerminalSequence)
		seqs[r.TerminalSequence] = key
	}

	// …and the in-flight sibling is left alone. Marking it skipped would not
	// stop its container, only strand one whose worker still holds the claim;
	// the design's contract is that fail_fast cancels PENDING siblings.
	assert.Equal(t, string(TaskStatusRunning), after["gate"].Status,
		"fail_fast must not resolve a RUNNING sibling out from under its live container")
	assert.Equal(t, "container-gate", after["gate"].RuntimeID)

	// Every resolved instance emits its own task_skipped event carrying its own
	// partition, so a consumer can tell the siblings apart.
	skipped := taskSkippedPartitions(t, f)
	assert.ElementsMatch(t, []string{"x", "y", "z"}, skipped)

	// The group is not all-terminal yet, so its cross-step successors have not
	// been resolved: `gate` finishing is what completes the group.
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		allTerm, err := f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		require.NoError(t, err)
		assert.False(t, allTerm)
		return nil
	}))
}

// TestFailTaskContinuePolicySkipsOnlyDependents is the contrast case: under
// `continue` a failure resolves only its transitive in-group dependents, and an
// independent sibling keeps running.
func TestFailTaskContinuePolicySkipsOnlyDependents(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 16, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "dep", DependsOn: []string{"bad"}},
		{Key: "free"},
	})
	require.NoError(t, err)
	byKey := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		byKey[r.PartitionValue] = r
	}

	require.NoError(t, f.store.FailTaskInstance(f.runID, byKey["bad"].ID, assert.AnError))

	after := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		after[r.PartitionValue] = r
	}
	assert.Equal(t, string(TaskStatusFailed), after["bad"].Status)
	assert.Equal(t, string(TaskStatusSkipped), after["dep"].Status)
	assert.Equal(t, "fan-out dependency bad failed", after["dep"].Error,
		"the dependency cascade keeps its own reason under `continue`")
	assert.Equal(t, string(TaskStatusPending), after["free"].Status,
		"`continue` must not resolve an independent sibling")
}

// TestFailTaskFailFastIsTheDefaultPolicy pins the normalization rule shared with
// pkg/jobdef's validateSteps and the owner engine: an omitted failurePolicy IS
// fail_fast. A store that treated "" as `continue` would ship the wrong default
// to every job that never wrote the field.
func TestFailTaskFailFastIsTheDefaultPolicy(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("bad", "free"))
	require.NoError(t, err)
	byKey := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		byKey[r.PartitionValue] = r
	}

	require.NoError(t, f.store.FailTaskInstance(f.runID, byKey["bad"].ID, assert.AnError))

	after := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		after[r.PartitionValue] = r
	}
	assert.Equal(t, string(TaskStatusSkipped), after["free"].Status,
		"an unset failurePolicy must behave as fail_fast, matching pkg/jobdef and the owner engine")
	assert.Equal(t, "fan-out group failed fast", after["free"].Error)
}

// taskSkippedPartitions returns the partition value carried by each task_skipped
// event the run emitted, which is how a consumer tells sibling skips apart.
func taskSkippedPartitions(t *testing.T, f *fanOutFixture) []string {
	t.Helper()
	evts, err := f.store.EventStore().ListSince(context.Background(), 0, 1000, event.Filter{
		RunID: f.runID,
		Types: []event.Type{event.TypeTaskSkipped},
	})
	require.NoError(t, err)
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		var payload TaskRun
		require.NoError(t, json.Unmarshal(e.Payload, &payload))
		out = append(out, payload.PartitionValue)
	}
	return out
}

// cacheHitPartitionStoreShape mirrors, character for character, the interface
// internal/worker/completion_sink.go and internal/dispatch/dispatch.go assert
// *run.Store against to reach the cache-hit expansion route. Those bindings are
// interface assertions, not calls, so a signature drift there is invisible to
// the compiler and manifests only as a warning log plus a group that never
// expands. This compile-time assertion turns that class of drift back into a
// build failure inside the package that owns the method.
type cacheHitPartitionStoreShape interface {
	CacheHitTaskClaimedWithPartitions(runID, taskRef uuid.UUID, source CacheHitSource, result, claimedBy string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) error
}

var _ cacheHitPartitionStoreShape = (*Store)(nil)

// TestFailTaskFailFastWhenFanOutConfigIsAbsent covers the branch the two
// policy tests above do not: they set a fanOut block and vary failurePolicy
// within it, so both exercise `fo != nil`. A group whose catalog Task carries NO
// fan_out_config at all takes a different path in groupFailsFastTx (`fo == nil`)
// and must reach the same answer — instance rows can be fanned while the config
// is missing or unreadable (a hand-seeded row, a catalog rewritten mid-run, a
// decode failure), and defaulting THAT to `continue` would let a group keep
// dispatching work after a failure in exactly the case where the store knows
// least about it.
func TestFailTaskFailFastWhenFanOutConfigIsAbsent(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("bad", "free"))
	require.NoError(t, err)
	byKey := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		byKey[r.PartitionValue] = r
	}

	// Strip the config the fixture wrote, leaving fanned instance rows behind.
	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.consumer.ID).
		Update("fan_out_config", nil).Error)

	require.NoError(t, f.store.FailTaskInstance(f.runID, byKey["bad"].ID, assert.AnError))

	after := map[string]models.TaskRun{}
	for _, r := range f.instances(t) {
		after[r.PartitionValue] = r
	}
	assert.Equal(t, string(TaskStatusFailed), after["bad"].Status)
	assert.Equal(t, string(TaskStatusSkipped), after["free"].Status,
		"an absent fanOut config must fail fast, not fall through to `continue`")
	assert.Equal(t, "fan-out group failed fast", after["free"].Error)
}
