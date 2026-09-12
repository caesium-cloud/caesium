package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/imagecheck"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

const movedTagDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func stubImageResolver(digest string) *imagecheck.Resolver {
	fn := func(context.Context, string) (string, error) { return digest, nil }
	return imagecheck.NewResolver(
		imagecheck.WithEngineDigestFunc(models.AtomEngineDocker, fn),
		imagecheck.WithEngineDigestFunc(models.AtomEnginePodman, fn),
		imagecheck.WithEngineDigestFunc(models.AtomEngineKubernetes, fn),
	)
}

// TestRuntimeExecutorCreatesDigestPinnedImage is the warm-tag/moved-tag case
// for the distributed executor: pinDigests records registry digest B while the
// mutable tag is what the task row still names. Create must be given B, not
// the tag — Podman ImageExists and Kubernetes PullIfNotPresent would otherwise
// run whatever content is already cached under the tag.
func TestRuntimeExecutorCreatesDigestPinnedImage(t *testing.T) {
	const imageTag = "registry.example.com/app:v1"

	for _, engineKind := range []models.AtomEngine{
		models.AtomEngineDocker,
		models.AtomEnginePodman,
		models.AtomEngineKubernetes,
	} {
		t.Run(string(engineKind), func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

			store := run.NewStore(db)
			now := time.Now().UTC()
			trigger := &models.Trigger{ID: uuid.New(), Alias: "trigger", Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
			require.NoError(t, db.Create(trigger).Error)
			job := &models.Job{ID: uuid.New(), Alias: "worker-pin-digest", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
			require.NoError(t, db.Create(job).Error)

			specBytes, err := json.Marshal(map[string]any{})
			require.NoError(t, err)
			atomModel := &models.Atom{
				ID:        uuid.New(),
				Engine:    engineKind,
				Image:     imageTag,
				Command:   `["sh","-c","true"]`,
				Spec:      datatypes.JSON(specBytes),
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, db.Create(atomModel).Error)
			task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "subject", CreatedAt: now, UpdatedAt: now}
			require.NoError(t, db.Create(task).Error)
			jobRun := &models.JobRun{
				ID:          uuid.New(),
				JobID:       job.ID,
				TriggerID:   trigger.ID,
				TriggerType: string(trigger.Type),
				Status:      string(run.StatusRunning),
				StartedAt:   now,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			require.NoError(t, db.Create(jobRun).Error)
			taskRun := &models.TaskRun{
				ID:              uuid.New(),
				JobRunID:        jobRun.ID,
				TaskID:          task.ID,
				AtomID:          atomModel.ID,
				Engine:          engineKind,
				Image:           imageTag,
				Command:         atomModel.Command,
				Status:          string(run.TaskStatusRunning),
				ClaimedBy:       "node-a",
				Attempt:         1,
				MaxAttempts:     1,
				CacheEnabled:    true,
				CachePinDigests: true,
				CacheDigestTTL:  0,
				CacheVersion:    1,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			require.NoError(t, db.Create(taskRun).Error)

			engine := &captureCreateEngine{}
			executor := &runtimeExecutor{
				store:         store,
				localSink:     NewLocalSink(store),
				imageResolver: stubImageResolver(movedTagDigest),
				engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
					return engine, nil
				},
			}
			executor.Execute(context.Background(), taskRun)

			require.NotNil(t, engine.createReq)
			require.Equal(t, imageTag+"@"+movedTagDigest, engine.createReq.Image,
				"Create must execute the registry digest, not the warm mutable tag")

			var persisted models.TaskRun
			require.NoError(t, db.First(&persisted, "id = ?", taskRun.ID).Error)
			require.Equal(t, movedTagDigest, persisted.ResolvedImageDigest,
				"the digest folded into the cache key must be the one that ran")
		})
	}
}

func TestRuntimeExecutorLeavesTagWhenPinDigestsOff(t *testing.T) {
	const imageTag = "registry.example.com/app:v1"

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "trigger", Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "worker-no-pin", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	atomModel := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEnginePodman,
		Image:     imageTag,
		Command:   `["true"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atomModel).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "subject", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)
	jobRun := &models.JobRun{
		ID:          uuid.New(),
		JobID:       job.ID,
		TriggerID:   trigger.ID,
		TriggerType: string(trigger.Type),
		Status:      string(run.StatusRunning),
		StartedAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, db.Create(jobRun).Error)
	taskRun := &models.TaskRun{
		ID:           uuid.New(),
		JobRunID:     jobRun.ID,
		TaskID:       task.ID,
		AtomID:       atomModel.ID,
		Engine:       atomModel.Engine,
		Image:        imageTag,
		Command:      atomModel.Command,
		Status:       string(run.TaskStatusRunning),
		ClaimedBy:    "node-a",
		Attempt:      1,
		MaxAttempts:  1,
		CacheEnabled: true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, db.Create(taskRun).Error)

	engine := &captureCreateEngine{}
	executor := &runtimeExecutor{
		store:         store,
		localSink:     NewLocalSink(store),
		imageResolver: stubImageResolver(movedTagDigest),
		engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) {
			return engine, nil
		},
	}
	executor.Execute(context.Background(), taskRun)

	require.NotNil(t, engine.createReq)
	require.Equal(t, imageTag, engine.createReq.Image)
}
