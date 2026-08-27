package worker

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// log_snapshot_test.go pins that the distributed executor persists the captured
// container log on EVERY terminal path.
//
// The snapshot is not a convenience copy: engine.Stop is stop-AND-remove on
// every runtime, so the moment executeTask returns, the container the logs
// endpoint would stream is gone. Whatever was not written to task_runs.log_text
// is lost, and `GET …/runs/:run_id/logs` can only answer 204 unavailable.

func newLogSnapshotExecutor(store *run.Store, engine atom.Engine) *runtimeExecutor {
	return &runtimeExecutor{
		store:     store,
		localSink: NewLocalSink(store),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
}

func reloadTaskRun(t *testing.T, executor *runtimeExecutor, id any) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, executor.store.DB().First(&row, "id = ?", id).Error)
	return row
}

// TestRuntimeExecutorPersistsLogSnapshotOnSuccess pins the working path.
func TestRuntimeExecutorPersistsLogSnapshotOnSuccess(t *testing.T) {
	taskRun, db := seedSchemaValidationTaskRun(t, jobdef.SchemaValidationFail)
	engine := &captureCreateEngine{
		logs: "connecting to warehouse\n##caesium::output {\"rows_written\":\"42\"}\ndone\n",
	}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)

	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
	require.Contains(t, row.LogText, "connecting to warehouse")
	require.Contains(t, row.LogText, "done")
}

// TestRuntimeExecutorPersistsLogSnapshotWhenSchemaValidationFails: a task that
// fails its declared outputSchema is exactly the task whose log someone will
// open. The snapshot used to be written only after the completion sink, so this
// early return dropped it and the log endpoint answered 204 unavailable for a
// container that had, in fact, printed the explanation.
func TestRuntimeExecutorPersistsLogSnapshotWhenSchemaValidationFails(t *testing.T) {
	taskRun, db := seedSchemaValidationTaskRun(t, jobdef.SchemaValidationFail)
	engine := &captureCreateEngine{
		logs: "connecting to warehouse\nWARN: row count unavailable\n##caesium::output {\"rows_written\":\"unknown\"}\n",
	}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)

	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusFailed), row.Status)
	require.Contains(t, row.LogText, "WARN: row count unavailable",
		"a schema-validation failure must not cost the log that explains it")
}
