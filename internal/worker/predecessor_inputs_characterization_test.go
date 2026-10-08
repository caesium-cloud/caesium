package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func addWorkerPredecessor(t *testing.T, f fanOutTaskRunFixture, output string) *models.TaskRun {
	t.Helper()
	task := &models.Task{ID: uuid.New(), JobID: f.task.JobID, AtomID: f.task.AtomID, Name: "upstream"}
	require.NoError(t, f.db.Create(task).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{ID: uuid.New(), JobID: f.task.JobID, FromTaskID: task.ID, ToTaskID: f.task.ID}).Error)
	row := &models.TaskRun{ID: uuid.New(), TaskID: task.ID, JobRunID: f.jobRun.ID, AtomID: task.AtomID, Status: string(run.TaskStatusSucceeded), Output: datatypes.JSON(output), Hash: "upstream-hash"}
	require.NoError(t, f.db.Create(row).Error)
	return row
}

func TestRuntimePredecessorEnvPrecedenceWithoutCache(t *testing.T) {
	f := seedProducerTaskRun(t, "ordinary-input-precedence")
	f.taskRun.CacheEnabled = false
	addWorkerPredecessor(t, f, `{"value":"output"}`)
	var atomModel models.Atom
	require.NoError(t, f.db.First(&atomModel, "id = ?", f.taskRun.AtomID).Error)
	env, err := json.Marshal(container.Spec{Env: map[string]string{"CAESIUM_PARAM_BRANCH": "base", "CAESIUM_OUTPUT_UPSTREAM_VALUE": "base", "KEEP": "base"}})
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&atomModel).Update("spec", datatypes.JSON(env)).Error)
	require.NoError(t, f.db.Model(f.jobRun).Update("params", datatypes.JSON(`{"branch":"parameter"}`)).Error)
	engine := &captureCreateEngine{}
	sink := &fakeSink{}
	(&runtimeExecutor{store: f.store, localSink: sink, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }}).Execute(context.Background(), f.taskRun)
	require.NotNil(t, engine.createReq)
	require.Equal(t, "base", engine.createReq.Spec.Env["KEEP"])
	require.Equal(t, "parameter", engine.createReq.Spec.Env["CAESIUM_PARAM_BRANCH"])
	require.Equal(t, "output", engine.createReq.Spec.Env["CAESIUM_OUTPUT_UPSTREAM_VALUE"])
	require.Equal(t, 1, sink.succeeded)
}
