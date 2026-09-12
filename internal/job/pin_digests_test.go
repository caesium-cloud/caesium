package job

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/imagecheck"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
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

// TestPinDigestsExecutesResolvedDigestNotWarmTag is the local-lane half of
// executing the digest pinDigests recorded. The row still names the mutable
// tag (the recipe freeze); Create must be given the registry digest so a
// locally cached tag at digest A cannot run while the cache key claims B.
func TestPinDigestsExecutesResolvedDigestNotWarmTag(t *testing.T) {
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
			engine := newFakeEngine()

			jobID := uuid.New()
			taskID := uuid.New()
			atomID := uuid.New()

			jobModel := &models.Job{ID: jobID, Alias: "pin-digest-exec", TriggerID: uuid.New()}
			require.NoError(t, db.Create(jobModel).Error)

			taskModel := &models.Task{
				ID:          taskID,
				JobID:       jobID,
				AtomID:      atomID,
				Name:        "subject",
				CacheConfig: datatypes.JSON(`{"ttl":"1h","pinDigests":true,"digestTTL":0}`),
			}
			taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
			catalogAtom := fakeModelAtom(atomID)
			catalogAtom.Image = imageTag
			catalogAtom.Engine = engineKind
			atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
			persistGraph(t, db, taskSvc.tasks, nil)

			opts := withTestDeps(store, env.Environment{
				MaxParallelTasks:  1,
				TaskFailurePolicy: taskFailurePolicyHalt,
				ExecutionMode:     executionModeLocal,
			}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)
			opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver(movedTagDigest) })

			require.NoError(t, New(jobModel, opts...).Run(context.Background()))

			created := engine.createRequestsForTask(taskID)
			require.Len(t, created, 1)
			require.Equal(t, imageTag+"@"+movedTagDigest, created[0].Image,
				"Create must execute the registry digest, not the warm mutable tag")

			frozen := taskRunByID(latestRunSnapshot(t, store, jobID), taskID)
			require.NotNil(t, frozen)
			require.Equal(t, movedTagDigest, frozen.ResolvedImageDigest,
				"the digest folded into the cache key must be the one that ran")
		})
	}
}

func TestPinDigestsOffLeavesMutableTag(t *testing.T) {
	const imageTag = "registry.example.com/app:v1"

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	jobModel := &models.Job{ID: jobID, Alias: "no-pin-digest", TriggerID: uuid.New()}
	require.NoError(t, db.Create(jobModel).Error)

	taskModel := &models.Task{
		ID:          taskID,
		JobID:       jobID,
		AtomID:      atomID,
		Name:        "subject",
		CacheConfig: datatypes.JSON(`{"ttl":"1h"}`),
	}
	taskSvc := &fakeTaskService{tasks: models.Tasks{taskModel}}
	catalogAtom := fakeModelAtom(atomID)
	catalogAtom.Image = imageTag
	catalogAtom.Engine = models.AtomEnginePodman
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: catalogAtom}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)
	opts = append(opts, func(j *job) { j.imageResolver = stubImageResolver(movedTagDigest) })

	require.NoError(t, New(jobModel, opts...).Run(context.Background()))

	created := engine.createRequestsForTask(taskID)
	require.Len(t, created, 1)
	require.Equal(t, imageTag, created[0].Image)
}
