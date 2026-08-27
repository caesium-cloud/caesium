package run

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// instanceFixture materializes one expanded fan-out group so the instance-keyed
// write helpers can be exercised against real sibling rows.
type instanceFixture struct {
	store  *Store
	runID  uuid.UUID
	taskID uuid.UUID
	byKey  map[string]uuid.UUID
}

func newInstanceFixture(t *testing.T, partitions map[string][]string) *instanceFixture {
	t.Helper()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomModel := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hello"]`,
	}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomModel.ID,
		Name:   "process",
	}
	require.NoError(t, db.Create(task).Error)

	f := &instanceFixture{
		store:  store,
		runID:  runRecord.ID,
		taskID: task.ID,
		byKey:  map[string]uuid.UUID{},
	}

	// Deterministic order so partition_index is stable across runs.
	keys := make([]string, 0, len(partitions))
	for k := range partitions {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	for i, key := range keys {
		deps := partitions[key]
		var depsJSON datatypes.JSON
		if len(deps) > 0 {
			raw, err := json.Marshal(deps)
			require.NoError(t, err)
			depsJSON = datatypes.JSON(raw)
		}
		id := uuid.New()
		f.byKey[key] = id
		require.NoError(t, db.Create(&models.TaskRun{
			ID:                      id,
			JobRunID:                runRecord.ID,
			TaskID:                  task.ID,
			AtomID:                  atomModel.ID,
			Engine:                  models.AtomEngineDocker,
			Image:                   atomModel.Image,
			Command:                 atomModel.Command,
			Status:                  string(TaskStatusPending),
			Attempt:                 1,
			MaxAttempts:             2,
			PartitionValue:          key,
			PartitionIndex:          i,
			PartitionCount:          len(keys),
			PartitionDependsOn:      depsJSON,
			OutstandingPredecessors: len(deps),
		}).Error)
	}

	return f
}

// setFailurePolicy writes a fanOut block onto the group's catalog Task so a test
// can state which failurePolicy it means. The fixture leaves it unset by
// default, which the store reads as fail_fast (matching pkg/jobdef's
// validateSteps and the owner engine's normalizeFanOutFailurePolicy).
func (f *instanceFixture) setFailurePolicy(t *testing.T, policy string) {
	t.Helper()
	encoded, err := json.Marshal(jobdefschema.FanOut{
		From:          "discover",
		MaxPartitions: 16,
		FailurePolicy: policy,
	})
	require.NoError(t, err)
	require.NoError(t, f.store.db.Model(&models.Task{}).
		Where("id = ?", f.taskID).
		Update("fan_out_config", datatypes.JSON(encoded)).Error)
}

func (f *instanceFixture) row(t *testing.T, key string) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, f.store.db.First(&row, "id = ?", f.byKey[key]).Error)
	return row
}

func TestRetryTaskInstanceResetsOnlyThatInstance(t *testing.T) {
	f := newInstanceFixture(t, map[string][]string{"a": nil, "b": nil})
	// Stated, not implied: this test's control is that b is left alone by the
	// RETRY, which only means something while b is still running when the retry
	// happens. Under the default (fail_fast) a's failure would resolve b first.
	f.setFailurePolicy(t, jobdefschema.FanOutFailureContinue)

	require.NoError(t, f.store.StartTask(f.runID, f.byKey["a"], "runtime-a"))
	require.NoError(t, f.store.StartTask(f.runID, f.byKey["b"], "runtime-b"))
	require.NoError(t, f.store.FailTaskInstance(f.runID, f.byKey["a"], errors.New("boom")))

	require.NoError(t, f.store.RetryTaskInstance(f.runID, f.byKey["a"], 2))

	a := f.row(t, "a")
	require.Equal(t, string(TaskStatusPending), a.Status)
	require.Equal(t, 2, a.Attempt)
	require.Empty(t, a.Error, "a retry must clear the previous attempt's error")
	require.Empty(t, a.RuntimeID)
	require.Nil(t, a.CompletedAt)

	b := f.row(t, "b")
	require.Equal(t, string(TaskStatusRunning), b.Status, "sibling b must be untouched")
	require.Equal(t, 1, b.Attempt)
}

// TestRetryTaskInstanceRefusesResolvedInstance pins the resurrection guard: an
// instance a cascade or cancellation already resolved must not be reset back to
// pending, or the local loop would re-dispatch it forever.
func TestRetryTaskInstanceRefusesResolvedInstance(t *testing.T) {
	f := newInstanceFixture(t, map[string][]string{"a": nil})

	require.NoError(t, f.store.SkipTaskInstance(f.runID, f.byKey["a"], "cancelled"))

	err := f.store.RetryTaskInstance(f.runID, f.byKey["a"], 2)
	require.ErrorIs(t, err, ErrTaskInstanceNotRetryable)
	require.Equal(t, string(TaskStatusSkipped), f.row(t, "a").Status)
}

func TestSkipTaskInstanceLeavesTerminalRows(t *testing.T) {
	f := newInstanceFixture(t, map[string][]string{"a": nil, "b": nil})

	require.NoError(t, f.store.SkipTaskInstance(f.runID, f.byKey["a"], "fan-out group failed fast"))
	a := f.row(t, "a")
	require.Equal(t, string(TaskStatusSkipped), a.Status)
	require.Equal(t, "fan-out group failed fast", a.Error)
	require.NotNil(t, a.CompletedAt)

	// A second skip must not overwrite the first, truer reason.
	require.NoError(t, f.store.SkipTaskInstance(f.runID, f.byKey["a"], "something else"))
	require.Equal(t, "fan-out group failed fast", f.row(t, "a").Error)

	require.Equal(t, string(TaskStatusPending), f.row(t, "b").Status)
}

// TestFailTaskInstanceFailsByPrimaryKeyAndCascades pins per-instance failure
// and its skip cascade.
//
// It originally asserted that FailTask was UNUSABLE for a fanned group, because
// failTask reassigned its own taskID parameter to loaded.TaskID while resolving
// the row for metrics, so the in-transaction re-resolve looked up the CATALOG
// task id and got ErrAmbiguousTaskRun. That parameter reassignment is now fixed:
// failTask never rewrites the reference it was handed, so BOTH entry points
// address the instance, and FailTaskInstance is a thin alias that adds only a
// nil-id guard. The assertion is inverted accordingly — passing an instance's
// primary key to FailTask must now work — while a genuinely ambiguous CATALOG
// task id must still be refused rather than silently failing a sibling.
//
// The in-group dependency cascade is the `continue` policy's behavior, so the
// group declares it explicitly. Under the DEFAULT policy (fail_fast) this same
// failure resolves EVERY pending sibling, `ok` included, and the cascade is a
// no-op subset — that path is pinned by TestFailTaskFailFastSkipsEveryPendingSibling
// in store_fanout_test.go. Leaving the policy unset here would silently test
// fail_fast while claiming to test the cascade.
func TestFailTaskInstanceFailsByPrimaryKeyAndCascades(t *testing.T) {
	f := newInstanceFixture(t, map[string][]string{
		"ok":   nil,
		"bad":  nil,
		"dep":  {"bad"},
		"deep": {"dep"},
	})
	f.setFailurePolicy(t, jobdefschema.FanOutFailureContinue)

	// The task-ID-keyed API cannot address an instance at all.
	// A CATALOG task id genuinely names N rows: it must be refused, never
	// resolved to an arbitrary sibling.
	require.ErrorIs(t, f.store.FailTask(f.runID, f.taskID, errors.New("boom")), ErrAmbiguousTaskRun,
		"a catalog task id names the whole group and must not fail one sibling silently")

	// The instance's primary key works through either entry point.
	require.NoError(t, f.store.FailTask(f.runID, f.byKey["bad"], errors.New("boom")),
		"FailTask must address an instance when handed its TaskRun primary key")
	require.Equal(t, string(TaskStatusFailed), f.row(t, "bad").Status)

	require.NoError(t, f.store.FailTaskInstance(f.runID, f.byKey["bad"], errors.New("boom")))

	require.Equal(t, string(TaskStatusFailed), f.row(t, "bad").Status)
	require.Equal(t, "boom", f.row(t, "bad").Error, "the real cause must be persisted")
	require.Equal(t, string(TaskStatusSkipped), f.row(t, "dep").Status)
	require.Equal(t, string(TaskStatusSkipped), f.row(t, "deep").Status,
		"the in-group skip cascade must be transitive")
	require.Equal(t, string(TaskStatusPending), f.row(t, "ok").Status,
		"an independent sibling must be untouched")
}

// TestFailTaskInstanceKeepsFirstCause pins that failing an already-terminal row
// does not overwrite the first, truer cause (a racing sibling cascade).
func TestFailTaskInstanceKeepsFirstCause(t *testing.T) {
	f := newInstanceFixture(t, map[string][]string{"a": nil})

	require.NoError(t, f.store.FailTaskInstance(f.runID, f.byKey["a"], errors.New("first")))
	require.NoError(t, f.store.FailTaskInstance(f.runID, f.byKey["a"], errors.New("second")))

	require.Equal(t, "first", f.row(t, "a").Error)
}
