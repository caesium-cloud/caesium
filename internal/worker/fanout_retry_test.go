package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	jobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// fanout_retry_test.go pins the worker's attempt loop against the two ways it
// mishandled a fan-out instance:
//
//   - it reported a non-success container result through the SUCCESS sink before
//     checking the result, so a retryable attempt durably terminalized the row
//     (and, for a producer, expanded its successors); and
//   - it then asked the store to reset that row by CATALOG task id, which names
//     N sibling rows and is refused, so the next attempt ran against a terminal
//     row and its completion was claim-rejected.
//
// Together those meant a fanned instance with retries left ended failed after
// one attempt.

// attemptResultEngine returns a configured result per CREATE, so attempt 1 can
// fail and attempt 2 succeed. Every attempt gets its own atom id, which is also
// what proves a second container was actually launched.
type attemptResultEngine struct {
	mu sync.Mutex
	// results[i] is the result of the i-th container created. Attempts past the
	// end of the slice succeed.
	results []atom.Result
	creates int
	// logs is what every container "prints".
	logs string
}

func (e *attemptResultEngine) resultFor(id string) atom.Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.results {
		if id == fmt.Sprintf("atom-%d", i+1) {
			return r
		}
	}
	return atom.Success
}

func (e *attemptResultEngine) createCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.creates
}

func (e *attemptResultEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	return &fakeMonitorAtom{id: req.ID, result: e.resultFor(req.ID)}, nil
}

func (e *attemptResultEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }

func (e *attemptResultEngine) Create(*atom.EngineCreateRequest) (atom.Atom, error) {
	e.mu.Lock()
	e.creates++
	id := fmt.Sprintf("atom-%d", e.creates)
	e.mu.Unlock()
	return &fakeMonitorAtom{id: id, result: atom.Unknown}, nil
}

func (e *attemptResultEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	return &fakeMonitorAtom{id: req.ID, result: e.resultFor(req.ID)}, nil
}

func (e *attemptResultEngine) Stop(*atom.EngineStopRequest) error { return nil }

func (e *attemptResultEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(e.logs)), nil
}

// countTerminalWrites installs an AFTER UPDATE trigger that records every
// TRANSITION of one TaskRun row into a terminal status. It counts transitions
// rather than writes so a repeated UPDATE to the same status is not mistaken for
// a second terminalization — what the assertion cares about is how many times
// the row was durably resolved.
func countTerminalWrites(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) func() []string {
	t.Helper()

	require.NoError(t, db.Exec(`CREATE TABLE terminal_writes (seq INTEGER PRIMARY KEY AUTOINCREMENT, status TEXT)`).Error)
	require.NoError(t, db.Exec(fmt.Sprintf(`
CREATE TRIGGER record_terminal_writes AFTER UPDATE ON task_runs
WHEN NEW.id = '%s'
  AND NEW.status <> OLD.status
  AND NEW.status IN ('succeeded','failed','cached','skipped','cancelled')
BEGIN INSERT INTO terminal_writes (status) VALUES (NEW.status); END;`, taskRunID)).Error)

	return func() []string {
		var rows []struct{ Status string }
		require.NoError(t, db.Raw(`SELECT status FROM terminal_writes ORDER BY seq ASC`).Scan(&rows).Error)
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Status)
		}
		return out
	}
}

func executeWithEngine(f fanOutTaskRunFixture, engine atom.Engine) {
	executor := &runtimeExecutor{
		store:     f.store,
		localSink: NewLocalSink(f.store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), f.taskRun)
}

func reloadInstanceRow(t *testing.T, db *gorm.DB, id uuid.UUID) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", id).Error)
	return row
}

// TestWorkerFannedInstanceRetriesAfterFailedAttempt is the P1: an instance whose
// container exits non-zero on attempt 1 must run attempt 2 and end SUCCEEDED,
// having been terminalized exactly once.
//
// Before the fix the failing attempt was pushed through sink.Succeeded (which
// re-derives status=failed from the result), so the row was terminal by the time
// the loop tried to reset it — by catalog task id, which the store refuses for a
// group. The instance ended failed after one container.
func TestWorkerFannedInstanceRetriesAfterFailedAttempt(t *testing.T) {
	f := seedFanOutTaskRun(t, "fanout-retry-job", `{"from":"producer"}`,
		pkgtask.Partition{Key: "shard-0"}, 2, false)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ?", f.jobRun.ID).
		Update("max_attempts", 2).Error)
	f.taskRun.MaxAttempts = 2

	var sibling models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND id <> ?", f.jobRun.ID, f.taskRun.ID).
		First(&sibling).Error)

	terminalWrites := countTerminalWrites(t, f.db, f.taskRun.ID)

	engine := &attemptResultEngine{results: []atom.Result{atom.Failure}}
	executeWithEngine(f, engine)

	require.Equal(t, 2, engine.createCount(),
		"the instance must get its second attempt; a failed attempt is not a failed task")

	row := reloadInstanceRow(t, f.db, f.taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status,
		"attempt 2 succeeded, so the instance must end succeeded")
	require.Equal(t, string(atom.Success), row.Result)
	require.Equal(t, 2, row.Attempt, "the retry must have advanced the persisted attempt counter")

	require.Equal(t, []string{string(run.TaskStatusSucceeded)}, terminalWrites(),
		"a retried instance must be terminalized exactly once, by its final attempt")

	// The premature terminal write also ran the group's failure consequences.
	// With no failure recorded, the sibling is left exactly as it was.
	after := reloadInstanceRow(t, f.db, sibling.ID)
	require.Equal(t, sibling.Status, after.Status,
		"a retryable attempt must not resolve the failing instance's siblings")
}

// TestWorkerUnfannedTaskRetriesAfterFailedAttempt is the control: the same
// sequence on an UNFANNED task must still end succeeded on attempt 2. It also
// pins the improvement the restructure brings to that lane — the intermediate
// terminal write on attempt 1 is gone, so the row is resolved once.
func TestWorkerUnfannedTaskRetriesAfterFailedAttempt(t *testing.T) {
	f := seedFanOutTaskRun(t, "unfanned-retry-job", "",
		pkgtask.Partition{}, 1, false)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("id = ?", f.taskRun.ID).
		Updates(map[string]any{"max_attempts": 2, "partition_count": 0}).Error)
	f.taskRun.MaxAttempts = 2
	f.taskRun.PartitionCount = 0

	terminalWrites := countTerminalWrites(t, f.db, f.taskRun.ID)

	engine := &attemptResultEngine{results: []atom.Result{atom.Failure}}
	executeWithEngine(f, engine)

	require.Equal(t, 2, engine.createCount())

	row := reloadInstanceRow(t, f.db, f.taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
	require.Equal(t, 2, row.Attempt)
	require.Equal(t, []string{string(run.TaskStatusSucceeded)}, terminalWrites(),
		"the non-final attempt must not durably terminalize the row")
}

// TestWorkerExhaustedRetriesStillTerminalizeTheInstance pins the other half of
// the restructure: when the LAST attempt is the one that fails, the completion
// route still runs, so the failure consequences (result string, canned error,
// the group's failurePolicy cascade) are unchanged.
func TestWorkerExhaustedRetriesStillTerminalizeTheInstance(t *testing.T) {
	f := seedFanOutTaskRun(t, "fanout-exhausted-job", `{"from":"producer"}`,
		pkgtask.Partition{Key: "shard-0"}, 2, false)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ?", f.jobRun.ID).
		Update("max_attempts", 2).Error)
	f.taskRun.MaxAttempts = 2

	terminalWrites := countTerminalWrites(t, f.db, f.taskRun.ID)

	engine := &attemptResultEngine{results: []atom.Result{atom.Failure, atom.Failure}}
	executeWithEngine(f, engine)

	require.Equal(t, 2, engine.createCount(), "both attempts must run before the task fails")

	row := reloadInstanceRow(t, f.db, f.taskRun.ID)
	require.Equal(t, string(run.TaskStatusFailed), row.Status)
	require.Equal(t, string(atom.Failure), row.Result,
		"the container's own result must survive onto the failed row")
	require.Equal(t, []string{string(run.TaskStatusFailed)}, terminalWrites())
}

// TestWorkerContinuePolicyFinalNonzeroSkipsUnfannedDescendants pins the
// result-bearing failure route. A final non-zero container result is reported
// through sink.Succeeded (with result=failure) before Execute observes the
// returned error. The source completion must therefore resolve its global
// taskFailurePolicy=continue successors in that FIRST transaction; a later
// sink.Failed call sees an already-terminal source and cannot safely repair the
// gap.
func TestWorkerContinuePolicyFinalNonzeroSkipsUnfannedDescendants(t *testing.T) {
	f := seedFanOutTaskRun(t, "unfanned-continue-result-job", "", pkgtask.Partition{}, 1, false)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", f.taskRun.ID).
		Update("partition_count", 0).Error)
	f.taskRun.PartitionCount = 0

	now := time.Now().UTC()
	mkSuccessor := func(name string) (*models.Task, *models.TaskRun) {
		task := &models.Task{
			ID: uuid.New(), JobID: f.jobRun.JobID, AtomID: f.task.AtomID,
			Name: name, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, f.db.Create(task).Error)
		row := &models.TaskRun{
			ID: uuid.New(), JobRunID: f.jobRun.ID, TaskID: task.ID, AtomID: f.task.AtomID,
			Status: string(run.TaskStatusPending), OutstandingPredecessors: 1,
			Attempt: 1, MaxAttempts: 1, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, f.db.Create(row).Error)
		return task, row
	}

	direct, directRow := mkSuccessor("direct")
	grandchild, grandchildRow := mkSuccessor("grandchild")
	require.NoError(t, f.db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: f.jobRun.JobID,
		FromTaskID: f.task.ID, ToTaskID: direct.ID,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: f.jobRun.JobID,
		FromTaskID: direct.ID, ToTaskID: grandchild.ID,
		CreatedAt: now, UpdatedAt: now,
	}).Error)

	engine := &attemptResultEngine{results: []atom.Result{atom.Failure}}
	executor := &runtimeExecutor{
		store:             f.store,
		localSink:         NewLocalSink(f.store),
		continueOnFailure: true,
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), f.taskRun)

	source := reloadInstanceRow(t, f.db, f.taskRun.ID)
	require.Equal(t, string(run.TaskStatusFailed), source.Status)
	require.Equal(t, string(atom.Failure), source.Result,
		"the real final container result must be the write that owns the cascade")
	require.Equal(t, string(run.TaskStatusSkipped), reloadInstanceRow(t, f.db, directRow.ID).Status)
	require.Equal(t, string(run.TaskStatusSkipped), reloadInstanceRow(t, f.db, grandchildRow.ID).Status,
		"the direct skip must propagate before the source transaction commits")
}

// TestWorkerSchemaViolationsRecordOnTheOffendingInstance pins finding 4 for the
// distributed lane: violations belong to the sibling that emitted them.
//
// SaveSchemaViolations refuses a catalog task id that resolves to N rows and the
// helper only LOGS the refusal, so keying on taskRun.TaskID recorded nothing at
// all: fail mode reported a violation with no evidence, warn mode opened an
// incident with no row.
func TestWorkerSchemaViolationsRecordOnTheOffendingInstance(t *testing.T) {
	f := seedFanOutTaskRun(t, "fanout-schema-job", `{"from":"producer"}`,
		pkgtask.Partition{Key: "shard-0"}, 2, false)

	schemaBytes, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"rows_written": map[string]any{"type": "integer"}},
		"required":   []string{"rows_written"},
	})
	require.NoError(t, err)

	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ?", f.jobRun.ID).
		Updates(map[string]any{
			"output_schema":     datatypes.JSON(schemaBytes),
			"schema_validation": jobdef.SchemaValidationWarn,
		}).Error)
	f.taskRun.OutputSchema = datatypes.JSON(schemaBytes)
	f.taskRun.SchemaValidation = jobdef.SchemaValidationWarn

	var sibling models.TaskRun
	require.NoError(t, f.db.
		Where("job_run_id = ? AND id <> ?", f.jobRun.ID, f.taskRun.ID).
		First(&sibling).Error)

	executor := &runtimeExecutor{store: f.store}
	require.NoError(t, executor.runSchemaValidation(f.taskRun, map[string]string{"rows_written": "unknown"}))

	offending := reloadInstanceRow(t, f.db, f.taskRun.ID)
	var violations []pkgtask.SchemaViolation
	require.NotEmpty(t, offending.SchemaViolations,
		"the offending instance must record its own violations")
	require.NoError(t, json.Unmarshal(offending.SchemaViolations, &violations))
	require.NotEmpty(t, violations)
	require.Contains(t, violations[0].Message, "integer")

	clean := reloadInstanceRow(t, f.db, sibling.ID)
	require.Empty(t, clean.SchemaViolations,
		"a sibling that emitted valid output must not inherit the violation")
}
