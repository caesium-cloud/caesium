package replay

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type recordingDispatcher struct {
	calls []uuid.UUID
	err   error
}

func (d *recordingDispatcher) DispatchReplay(_ context.Context, runID uuid.UUID) error {
	d.calls = append(d.calls, runID)
	return d.err
}

type replayFixture struct {
	db    *gorm.DB
	store *run.Store
	jobID uuid.UUID
	runID uuid.UUID
	now   time.Time
}

func newReplayFixture(t *testing.T) replayFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	triggerID := uuid.New()
	jobID := uuid.New()
	runID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:        triggerID,
		Type:      models.TriggerTypeHTTP,
		Alias:     "manual",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "replay-job",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID:           runID,
		JobID:        jobID,
		TriggerID:    triggerID,
		TriggerType:  "http",
		TriggerAlias: "manual",
		Status:       string(run.StatusSucceeded),
		Params:       mustJSON(t, map[string]string{"mode": "baseline"}),
		StartedAt:    now,
		CompletedAt:  ptr(now.Add(time.Second)),
		CreatedAt:    now,
		UpdatedAt:    now,
	}).Error)
	return replayFixture{db: db, store: run.NewStore(db), jobID: jobID, runID: runID, now: now}
}

func TestReplayRefusesTaskNotRecordedReplaySafe(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "deploy", replaySafe: false, result: "success"})

	dispatcher := &recordingDispatcher{}
	_, err := New(f.store, dispatcher).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.ErrorIs(t, err, ErrReplayUnsafe)
	require.Contains(t, err.Error(), `step "deploy"`)
	require.Empty(t, dispatcher.calls)

	var count int64
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("job_run_id <> ? AND task_id = ?", f.runID, taskID).Count(&count).Error)
	require.Zero(t, count)
}

func TestReplayRefusesUnchangedTaskNotRecordedReplaySafe(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{name: "deploy", replaySafe: false, result: "success"})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrReplayUnsafe)
	require.Contains(t, err.Error(), `step "deploy"`)
}

func TestReplayAbortsUnchangedTaskWithoutCacheOrBaselineResult(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{name: "extract", replaySafe: true})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnavailableBaselineProof)
	require.Contains(t, err.Error(), `step "extract"`)
}

func TestReplayAbortsUnchangedFailedBaselineResult(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "extract", replaySafe: true, result: "exit_code:1"})
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).Update("status", string(run.StatusFailed)).Error)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, taskID).
		Update("status", string(run.TaskStatusFailed)).Error)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnavailableBaselineProof)
	require.Contains(t, err.Error(), `baseline status "failed" is not reusable`)

	var replayRuns int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id <> ? AND job_id = ?", f.runID, f.jobID).Count(&replayRuns).Error)
	require.Zero(t, replayRuns)
}

func TestReplayAbortsUnchangedBaselineOutputWithEmptyResult(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{
		name:       "extract",
		replaySafe: true,
		output:     map[string]string{"artifact": "one"},
	})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnavailableBaselineProof)
	require.Contains(t, err.Error(), "recorded an empty result (corruption)")
}

func TestReplayMaterializesDescriptorInsteadOfLiveRows(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{
		name:       "transform",
		replaySafe: true,
		result:     "success",
		spec: container.Spec{
			Env:     map[string]string{"MODE": "baseline", "TOKEN": "literal"},
			WorkDir: "/baseline",
			Mounts:  []container.Mount{{Type: container.MountTypeBind, Source: "/baseline/in", Target: "/in", ReadOnly: true}},
		},
		image:   "baseline/image@sha256:aaaa",
		command: []string{"sh", "-c", "echo baseline"},
	})

	require.NoError(t, f.db.Model(&models.Atom{}).Where("id IN (SELECT atom_id FROM tasks WHERE id = ?)", taskID).
		Updates(map[string]any{
			"image":   "current/image:latest",
			"command": `["sh","-c","echo current"]`,
			"spec":    mustJSON(t, container.Spec{Env: map[string]string{"MODE": "current"}, WorkDir: "/current"}),
		}).Error)

	dispatcher := &recordingDispatcher{}
	result, err := New(f.store, dispatcher).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	require.Len(t, dispatcher.calls, 1)

	var replayTask models.TaskRun
	require.NoError(t, f.db.First(&replayTask, "job_run_id = ? AND task_id = ?", result.Run.ID, taskID).Error)
	require.Equal(t, "baseline/image@sha256:aaaa", replayTask.Image)
	require.JSONEq(t, `["sh","-c","echo baseline"]`, replayTask.Command)
	require.True(t, replayTask.Quarantine)
	require.True(t, replayTask.ReplaySafe)

	var desc models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(replayTask.ExecutionDescriptor, &desc))
	require.Equal(t, "/baseline", desc.ContainerSpec.WorkDir)
	require.Equal(t, "baseline", desc.ContainerSpec.Env["MODE"])
	require.Equal(t, "/baseline/in", desc.ContainerSpec.Mounts[0].Source)
}

func TestReplayFailsClosedWhenDescriptorMissing(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "load", replaySafe: true, result: "success"})
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, taskID).
		Update("execution_descriptor", nil).Error)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.ErrorIs(t, err, ErrMissingDescriptor)
}

func TestReplayFailsClosedWhenDescriptorReplaySafeMismatchesRow(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "load", replaySafe: true, result: "success"})

	var taskRun models.TaskRun
	require.NoError(t, f.db.First(&taskRun, "job_run_id = ? AND task_id = ?", f.runID, taskID).Error)
	var desc models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(taskRun.ExecutionDescriptor, &desc))
	desc.Baseline.ReplaySafe = false
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, taskID).
		Update("execution_descriptor", mustJSON(t, desc)).Error)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnsupportedDescriptor)
	require.Contains(t, err.Error(), "descriptor replay_safe=false")
}

func TestReplayRejectsQuarantinedBaseline(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{name: "safe", replaySafe: true, result: "success"})
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).Update("quarantine", true).Error)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrQuarantinedBaseline)
}

func TestReplayQuarantinesRunAndTaskRows(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "safe", replaySafe: true, result: "success"})

	result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	require.True(t, result.Run.Quarantine)

	var replayTask models.TaskRun
	require.NoError(t, f.db.First(&replayTask, "job_run_id = ? AND task_id = ?", result.Run.ID, taskID).Error)
	require.True(t, replayTask.Quarantine)
}

func TestReplayOverrideRerunsFullDAGNoOverrideCacheHits(t *testing.T) {
	f := newReplayFixture(t)
	extractID := f.seedTask(t, seedTaskConfig{
		name:       "extract",
		replaySafe: true,
		result:     "success",
		output:     map[string]string{"artifact": "one"},
		position:   0,
	})
	loadID := f.seedTask(t, seedTaskConfig{
		name:       "load",
		replaySafe: true,
		result:     "success",
		position:   1,
		predecessors: []models.TaskExecutionEdgeRef{{
			TaskID:   extractID,
			TaskName: "extract",
		}},
	})
	f.linkDescriptors(t, extractID, loadID)

	noOverrideDispatcher := &recordingDispatcher{}
	noOverride, err := New(f.store, noOverrideDispatcher).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)
	require.Empty(t, noOverrideDispatcher.calls)
	require.Len(t, noOverride.Decisions, 2)
	for _, decision := range noOverride.Decisions {
		require.True(t, decision.CacheHit, decision.TaskName)
		require.False(t, decision.Reexecute, decision.TaskName)
	}

	withOverrideDispatcher := &recordingDispatcher{}
	withOverride, err := New(f.store, withOverrideDispatcher).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	require.Len(t, withOverrideDispatcher.calls, 1)
	require.Len(t, withOverride.Decisions, 2)
	for _, decision := range withOverride.Decisions {
		require.False(t, decision.CacheHit, decision.TaskName)
		require.True(t, decision.Reexecute, decision.TaskName)
	}

	var loadTask models.TaskRun
	require.NoError(t, f.db.First(&loadTask, "job_run_id = ? AND task_id = ?", withOverride.Run.ID, loadID).Error)
	require.Equal(t, 1, loadTask.OutstandingPredecessors)
}

func TestReplaySurfacesDispatchFailureAfterDurableMaterialization(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{name: "safe", replaySafe: true, result: "success"})
	dispatchErr := errors.New("dispatch unavailable")

	_, err := New(f.store, &recordingDispatcher{err: dispatchErr}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.ErrorIs(t, err, dispatchErr)
}

type seedTaskConfig struct {
	name         string
	replaySafe   bool
	result       string
	output       map[string]string
	position     int
	predecessors []models.TaskExecutionEdgeRef
	successors   []models.TaskExecutionEdgeRef
	spec         container.Spec
	image        string
	command      []string
}

func (f replayFixture) seedTask(t *testing.T, cfg seedTaskConfig) uuid.UUID {
	t.Helper()
	if cfg.image == "" {
		cfg.image = "alpine:3.23"
	}
	if len(cfg.command) == 0 {
		cfg.command = []string{"sh", "-c", "echo " + cfg.name}
	}
	if cfg.spec.Env == nil {
		cfg.spec.Env = map[string]string{"STEP": cfg.name}
	}
	atomID := uuid.New()
	taskID := uuid.New()
	command := mustJSONString(t, cfg.command)
	require.NoError(t, f.db.Create(&models.Atom{
		ID:        atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "live/" + cfg.name + ":latest",
		Command:   `["sh","-c","echo live"]`,
		Spec:      mustJSON(t, container.Spec{Env: map[string]string{"STEP": "live"}}),
		CreatedAt: f.now,
		UpdatedAt: f.now,
	}).Error)
	require.NoError(t, f.db.Create(&models.Task{
		ID:          taskID,
		JobID:       f.jobID,
		AtomID:      atomID,
		Name:        cfg.name,
		Position:    cfg.position,
		TriggerRule: "all_success",
		ReplaySafe:  cfg.replaySafe,
		CreatedAt:   f.now.Add(time.Duration(cfg.position) * time.Millisecond),
		UpdatedAt:   f.now,
	}).Error)

	desc := models.TaskExecutionDescriptor{
		SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
		CapturedAt:    f.now,
		Baseline: models.TaskExecutionBaseline{
			JobID:         f.jobID,
			JobAlias:      "replay-job",
			TaskID:        taskID,
			TaskName:      cfg.name,
			AtomID:        atomID,
			BaselineRunID: f.runID,
			ReplaySafe:    cfg.replaySafe,
		},
		DAG: models.TaskExecutionDAG{
			Predecessors:            cfg.predecessors,
			Successors:              cfg.successors,
			TriggerRule:             "all_success",
			BranchBehavior:          "task",
			TaskPosition:            cfg.position,
			OutstandingPredecessors: len(cfg.predecessors),
		},
		Run: models.TaskExecutionRun{Params: map[string]string{"mode": "baseline"}},
		Runtime: models.TaskExecutionRuntime{
			Engine:       models.AtomEngineDocker,
			Image:        cfg.image,
			Command:      cfg.command,
			CommandRaw:   command,
			WorkDir:      cfg.spec.WorkDir,
			TaskType:     "task",
			RetryCount:   0,
			RetryDelay:   time.Second,
			RetryBackoff: true,
		},
		Cache: models.TaskExecutionCache{
			Enabled: true,
			Version: 1,
		},
		Schema:        models.TaskExecutionSchema{ValidationMode: "warn"},
		ContainerSpec: cfg.spec,
	}
	hash := computeDescriptorHash(desc, map[string]string{"mode": "baseline"}, nil, nil)
	desc.Cache.ComputedHash = hash
	desc.Baseline.ComputedHash = hash

	encodedDescriptor := mustJSON(t, desc)
	row := models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                f.runID,
		TaskID:                  taskID,
		AtomID:                  atomID,
		Engine:                  models.AtomEngineDocker,
		Image:                   cfg.image,
		Command:                 command,
		Status:                  string(run.TaskStatusSucceeded),
		Attempt:                 1,
		MaxAttempts:             1,
		Hash:                    hash,
		Result:                  cfg.result,
		OutstandingPredecessors: len(cfg.predecessors),
		CacheEnabled:            true,
		CacheVersion:            1,
		ReplaySafe:              cfg.replaySafe,
		ExecutionDescriptor:     encodedDescriptor,
		StartedAt:               ptr(f.now),
		CompletedAt:             ptr(f.now.Add(time.Second)),
		CreatedAt:               f.now.Add(time.Duration(cfg.position) * time.Millisecond),
		UpdatedAt:               f.now,
	}
	if len(cfg.output) > 0 {
		row.Output = mustJSON(t, cfg.output)
	}
	require.NoError(t, f.db.Create(&row).Error)
	return taskID
}

func (f replayFixture) linkDescriptors(t *testing.T, from, to uuid.UUID) {
	t.Helper()
	require.NoError(t, f.db.Create(&models.TaskEdge{
		ID:         uuid.New(),
		JobID:      f.jobID,
		FromTaskID: from,
		ToTaskID:   to,
		CreatedAt:  f.now,
		UpdatedAt:  f.now,
	}).Error)

	var fromRun, toRun models.TaskRun
	require.NoError(t, f.db.First(&fromRun, "job_run_id = ? AND task_id = ?", f.runID, from).Error)
	require.NoError(t, f.db.First(&toRun, "job_run_id = ? AND task_id = ?", f.runID, to).Error)

	var fromDesc, toDesc models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(fromRun.ExecutionDescriptor, &fromDesc))
	require.NoError(t, json.Unmarshal(toRun.ExecutionDescriptor, &toDesc))
	fromDesc.DAG.Successors = []models.TaskExecutionEdgeRef{{TaskID: to, TaskName: "load"}}
	toDesc.DAG.Predecessors = []models.TaskExecutionEdgeRef{{TaskID: from, TaskName: "extract"}}
	toDesc.DAG.OutstandingPredecessors = 1

	var fromOutput map[string]string
	require.NoError(t, json.Unmarshal(fromRun.Output, &fromOutput))
	toHash := computeDescriptorHash(
		toDesc,
		map[string]string{"mode": "baseline"},
		map[string]map[string]string{"extract": fromOutput},
		[]string{fromRun.Hash},
	)
	toDesc.Cache.ComputedHash = toHash
	toDesc.Baseline.ComputedHash = toHash

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", fromRun.ID).Update("execution_descriptor", mustJSON(t, fromDesc)).Error)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", toRun.ID).Updates(map[string]any{
		"hash":                 toHash,
		"execution_descriptor": mustJSON(t, toDesc),
	}).Error)
}

func mustJSON(t *testing.T, v any) datatypes.JSON {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return datatypes.JSON(data)
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func ptr[T any](v T) *T {
	return &v
}
