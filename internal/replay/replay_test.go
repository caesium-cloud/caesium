package replay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	vault "github.com/hashicorp/vault/api"
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

func TestReplayCacheHitsUnchangedTaskNotRecordedReplaySafe(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "deploy", replaySafe: false, result: "success"})

	dispatcher := &recordingDispatcher{}
	result, err := New(f.store, dispatcher).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)
	require.Empty(t, dispatcher.calls)
	require.Len(t, result.Decisions, 1)
	require.True(t, result.Decisions[0].CacheHit)
	require.False(t, result.Decisions[0].Reexecute)

	var replayTask models.TaskRun
	require.NoError(t, f.db.First(&replayTask, "job_run_id = ? AND task_id = ?", result.Run.ID, taskID).Error)
	require.True(t, replayTask.CacheHit)
	require.False(t, replayTask.ReplaySafe)
}

func TestReplayAbortsUnchangedTaskWithoutCacheOrBaselineResult(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{name: "extract", replaySafe: true})

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnavailableBaselineProof)
	require.Contains(t, err.Error(), `step "extract"`)
}

func TestReplayAbortsUnchangedCacheEnabledTaskWhenCacheExpired(t *testing.T) {
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "extract", replaySafe: true, result: "success"})
	require.NoError(t, f.db.Model(&models.TaskCache{}).
		Where("run_id = ? AND task_run_id IN (SELECT id FROM task_runs WHERE job_run_id = ? AND task_id = ?)", f.runID, f.runID, taskID).
		Update("expires_at", f.now.Add(-time.Minute)).Error)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnavailableBaselineProof)
	require.Contains(t, err.Error(), `step "extract"`)
	require.Contains(t, err.Error(), "cache entry is unavailable or expired")
}

func TestReplayBaselineWithoutTaskRunsIsUnavailableProof(t *testing.T) {
	f := newReplayFixture(t)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnavailableBaselineProof)
	require.Contains(t, err.Error(), "has no task runs")
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

func TestReplayMaterializesNonObviousDescriptorEnvelopeInsteadOfLiveRows(t *testing.T) {
	f := newReplayFixture(t)
	automount := false
	taskID := f.seedTask(t, seedTaskConfig{
		name:       "transform",
		replaySafe: true,
		result:     "success",
		spec: container.Spec{
			Env: map[string]string{"MODE": "baseline"},
			Kubernetes: &container.KubernetesSpec{
				ServiceAccountName:           "baseline-sa",
				AutomountServiceAccountToken: &automount,
				QueueName:                    "baseline-queue",
			},
		},
	})

	var baselineTask models.TaskRun
	require.NoError(t, f.db.First(&baselineTask, "job_run_id = ? AND task_id = ?", f.runID, taskID).Error)
	var desc models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(baselineTask.ExecutionDescriptor, &desc))
	desc.Runtime.RetryCount = 3
	desc.Runtime.RetryDelay = 7 * time.Second
	desc.Runtime.RetryBackoff = true
	desc.Timing.TaskTimeout = 11 * time.Second
	desc.Timing.RunTimeout = time.Minute
	desc.Cache.Enabled = true
	desc.Cache.TTL = 2 * time.Hour
	desc.Cache.Version = 9
	desc.Schema.InputSchema = datatypes.JSON(`{"extract":{"type":"object"}}`)
	desc.Schema.OutputSchema = datatypes.JSON(`{"type":"object","required":["clean"]}`)
	desc.Schema.ValidationMode = "fail"
	desc.DAG.TriggerRule = "all_done"
	desc.Job.TriggerConfig = datatypes.JSONMap{"cron": "0 2 * * *", "timezone": "UTC"}
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("id = ?", baselineTask.ID).
		Update("execution_descriptor", mustJSON(t, desc)).Error)

	require.NoError(t, f.db.Model(&models.Task{}).Where("id = ?", taskID).Updates(map[string]any{
		"retries":       0,
		"retry_delay":   time.Second,
		"retry_backoff": false,
		"trigger_rule":  "live_only",
		"output_schema": datatypes.JSON(`{"type":"object","required":["live"]}`),
		"input_schema":  datatypes.JSON(`{"live":{"type":"object"}}`),
		"replay_safe":   false,
	}).Error)
	require.NoError(t, f.db.Model(&models.Atom{}).Where("id IN (SELECT atom_id FROM tasks WHERE id = ?)", taskID).
		Update("spec", mustJSON(t, container.Spec{
			Env: map[string]string{"MODE": "live"},
			Kubernetes: &container.KubernetesSpec{
				ServiceAccountName: "live-sa",
				QueueName:          "live-queue",
			},
		})).Error)

	result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)

	var replayTask models.TaskRun
	require.NoError(t, f.db.First(&replayTask, "job_run_id = ? AND task_id = ?", result.Run.ID, taskID).Error)
	require.Equal(t, 4, replayTask.MaxAttempts)
	require.True(t, replayTask.CacheEnabled)
	require.Equal(t, 2*time.Hour, replayTask.CacheTTL)
	require.Equal(t, 9, replayTask.CacheVersion)
	require.JSONEq(t, `{"type":"object","required":["clean"]}`, string(replayTask.OutputSchema))
	require.Equal(t, "fail", replayTask.SchemaValidation)
	require.True(t, replayTask.Quarantine)

	var replayDesc models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(replayTask.ExecutionDescriptor, &replayDesc))
	require.Equal(t, "all_done", replayDesc.DAG.TriggerRule)
	require.Equal(t, 11*time.Second, replayDesc.Timing.TaskTimeout)
	require.Equal(t, "UTC", replayDesc.Job.TriggerConfig["timezone"])
	require.NotNil(t, replayDesc.KubernetesSpec)
	require.Equal(t, "baseline-sa", replayDesc.KubernetesSpec.ServiceAccountName)
	require.NotNil(t, replayDesc.KubernetesSpec.AutomountServiceAccountToken)
	require.False(t, *replayDesc.KubernetesSpec.AutomountServiceAccountToken)
	require.Equal(t, "baseline-queue", replayDesc.KubernetesSpec.QueueName)
	require.Equal(t, "baseline-sa", replayDesc.ContainerSpec.Kubernetes.ServiceAccountName)
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

func TestReplayPlansDescriptorDAGTopologicallyWhenYAMLOrderReversed(t *testing.T) {
	f := newReplayFixture(t)
	successorID := f.seedTask(t, seedTaskConfig{
		name:       "deploy",
		replaySafe: false,
		result:     "success",
		position:   0,
	})
	predecessorID := f.seedTask(t, seedTaskConfig{
		name:       "build",
		replaySafe: false,
		result:     "success",
		output:     map[string]string{"artifact": "one"},
		position:   1,
	})
	f.linkDescriptors(t, predecessorID, successorID)

	dispatcher := &recordingDispatcher{}
	result, err := New(f.store, dispatcher).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)
	require.Empty(t, dispatcher.calls)
	require.Len(t, result.Decisions, 2)
	require.Equal(t, "build", result.Decisions[0].TaskName)
	require.Equal(t, "deploy", result.Decisions[1].TaskName)
	for _, decision := range result.Decisions {
		require.True(t, decision.CacheHit, decision.TaskName)
		require.False(t, decision.Reexecute, decision.TaskName)
	}

	var replayTasks []models.TaskRun
	require.NoError(t, f.db.Order("created_at ASC").Find(&replayTasks, "job_run_id = ?", result.Run.ID).Error)
	require.Len(t, replayTasks, 2)
	for _, task := range replayTasks {
		require.Equal(t, string(run.TaskStatusCached), task.Status)
		require.True(t, task.CacheHit)
		require.False(t, task.ReplaySafe)
	}
}

func TestReplayFailsClosedOnDescriptorDAGCycle(t *testing.T) {
	f := newReplayFixture(t)
	firstID := f.seedTask(t, seedTaskConfig{
		name:       "first",
		replaySafe: true,
		result:     "success",
		output:     map[string]string{"first": "ok"},
		position:   0,
	})
	secondID := f.seedTask(t, seedTaskConfig{
		name:       "second",
		replaySafe: true,
		result:     "success",
		output:     map[string]string{"second": "ok"},
		position:   1,
	})
	f.linkDescriptors(t, firstID, secondID)
	f.linkDescriptors(t, secondID, firstID)

	_, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, ErrUnsupportedDescriptor)
	require.Contains(t, err.Error(), "descriptor DAG cycle")
}

func TestReplayPinnedVaultSecretUsesResolvedValueWithoutSecondRead(t *testing.T) {
	f := newReplayFixture(t)
	ref := "secret://vault/secret/data/path?field=token"
	keyring, err := secret.NewIdentityKeyring("k2", map[string][]byte{
		"k1": []byte("baseline-key"),
		"k2": []byte("current-key"),
	})
	require.NoError(t, err)
	baselineDigest, ok := keyring.HMACWithKeyID("k1", []byte("abc"))
	require.True(t, ok)

	f.seedTask(t, seedTaskConfig{
		name:       "deploy",
		replaySafe: true,
		result:     "success",
		spec:       container.Spec{Env: map[string]string{"TOKEN": ref}},
		secretRefs: []models.TaskExecutionSecretRef{
			run.SecretIdentityDescriptorRef("TOKEN", ref, secret.Identity{
				Provider:   "vault",
				Ref:        ref,
				Version:    "7",
				KeyID:      "k1",
				HMACSHA256: baselineDigest,
				Verifiable: true,
			}),
		},
	})
	logical := &replayVaultLogical{response: &vault.Secret{Data: map[string]any{
		"data":     map[string]any{"token": "abc"},
		"metadata": map[string]any{"version": 7},
	}}}
	resolver := secret.NewVaultResolverWithLogicalAndKeyring(logical, keyring)

	dispatcher := &recordingDispatcher{}
	_, err = New(f.store, dispatcher, WithSecretResolver(resolver)).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	require.Len(t, dispatcher.calls, 1)
	require.Equal(t, []string{"secret/data/path"}, logical.paths)
	require.Empty(t, logical.dataRequests)
}

func TestReplayPinnedVaultSecretAbortsOnVersionDriftWithoutSecondRead(t *testing.T) {
	f := newReplayFixture(t)
	ref := "secret://vault/secret/data/path?field=token"
	keyring, err := secret.NewIdentityKeyring("k2", map[string][]byte{
		"k1": []byte("baseline-key"),
		"k2": []byte("current-key"),
	})
	require.NoError(t, err)
	baselineDigest, ok := keyring.HMACWithKeyID("k1", []byte("abc"))
	require.True(t, ok)

	f.seedTask(t, seedTaskConfig{
		name:       "deploy",
		replaySafe: true,
		result:     "success",
		spec:       container.Spec{Env: map[string]string{"TOKEN": ref}},
		secretRefs: []models.TaskExecutionSecretRef{
			run.SecretIdentityDescriptorRef("TOKEN", ref, secret.Identity{
				Provider:   "vault",
				Ref:        ref,
				Version:    "7",
				KeyID:      "k1",
				HMACSHA256: baselineDigest,
				Verifiable: true,
			}),
		},
	})
	logical := &replayVaultLogical{response: &vault.Secret{Data: map[string]any{
		"data":     map[string]any{"token": "abc"},
		"metadata": map[string]any{"version": 8},
	}}}
	resolver := secret.NewVaultResolverWithLogicalAndKeyring(logical, keyring)

	dispatcher := &recordingDispatcher{}
	_, err = New(f.store, dispatcher, WithSecretResolver(resolver)).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.ErrorIs(t, err, ErrSecretIdentity)
	require.Contains(t, err.Error(), "version changed from 7 to 8")
	require.Empty(t, dispatcher.calls)
	require.Equal(t, []string{"secret/data/path"}, logical.paths)
	require.Empty(t, logical.dataRequests)
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
	secretRefs   []models.TaskExecutionSecretRef
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
		Schema:         models.TaskExecutionSchema{ValidationMode: "warn"},
		ContainerSpec:  cfg.spec,
		KubernetesSpec: cfg.spec.Kubernetes,
		SecretRefs:     cfg.secretRefs,
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
	if strings.TrimSpace(row.Result) != "" && run.IsSuccessfulTaskResult(row.Result) {
		require.NoError(t, f.db.Create(&models.TaskCache{
			Hash:             hash,
			JobID:            f.jobID,
			TaskName:         cfg.name,
			Result:           row.Result,
			Output:           row.Output,
			BranchSelections: row.BranchSelections,
			RunID:            f.runID,
			TaskRunID:        row.ID,
			CreatedAt:        f.now,
		}).Error)
	}
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
	fromDesc.DAG.Successors = []models.TaskExecutionEdgeRef{{TaskID: to, TaskName: toDesc.Baseline.TaskName}}
	toDesc.DAG.Predecessors = []models.TaskExecutionEdgeRef{{TaskID: from, TaskName: fromDesc.Baseline.TaskName}}
	toDesc.DAG.OutstandingPredecessors = 1

	var fromOutput map[string]string
	require.NoError(t, json.Unmarshal(fromRun.Output, &fromOutput))
	toHash := computeDescriptorHash(
		toDesc,
		map[string]string{"mode": "baseline"},
		map[string]map[string]string{fromDesc.Baseline.TaskName: fromOutput},
		[]string{fromRun.Hash},
	)
	toDesc.Cache.ComputedHash = toHash
	toDesc.Baseline.ComputedHash = toHash

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", fromRun.ID).Update("execution_descriptor", mustJSON(t, fromDesc)).Error)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", toRun.ID).Updates(map[string]any{
		"hash":                 toHash,
		"execution_descriptor": mustJSON(t, toDesc),
	}).Error)
	require.NoError(t, f.db.Model(&models.TaskCache{}).Where("task_run_id = ?", toRun.ID).Update("hash", toHash).Error)
}

type replayVaultLogical struct {
	response     *vault.Secret
	paths        []string
	dataRequests []replayVaultDataRequest
}

type replayVaultDataRequest struct {
	path string
	data map[string][]string
}

func (f *replayVaultLogical) ReadWithContext(_ context.Context, path string) (*vault.Secret, error) {
	f.paths = append(f.paths, path)
	return f.response, nil
}

func (f *replayVaultLogical) ReadWithDataWithContext(_ context.Context, path string, data map[string][]string) (*vault.Secret, error) {
	copied := make(map[string][]string, len(data))
	for key, values := range data {
		copied[key] = append([]string(nil), values...)
	}
	f.dataRequests = append(f.dataRequests, replayVaultDataRequest{path: path, data: copied})
	return f.response, nil
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
