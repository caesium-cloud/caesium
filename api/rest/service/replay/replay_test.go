package replay

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
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

func TestFingerprintScopesAndSortsOverrides(t *testing.T) {
	jobID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	baselineID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	otherJobID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	keyID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	otherKeyID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, KeyID: &keyID}

	first, err := Fingerprint(jobID, baselineID, principal, map[string]string{
		"z": "last",
		"a": "first",
	}, "same-key")
	require.NoError(t, err)
	second, err := Fingerprint(jobID, baselineID, principal, map[string]string{
		"a": "first",
		"z": "last",
	}, "same-key")
	require.NoError(t, err)
	require.Equal(t, first, second, "override map iteration order must not affect the fingerprint")

	otherPrincipal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, KeyID: &otherKeyID}
	differentInputs := []struct {
		name          string
		jobID         uuid.UUID
		baselineRunID uuid.UUID
		principal     *iauth.Principal
		overrides     map[string]string
		key           string
	}{
		{name: "job", jobID: otherJobID, baselineRunID: baselineID, principal: principal, overrides: map[string]string{"a": "first", "z": "last"}, key: "same-key"},
		{name: "baseline", jobID: jobID, baselineRunID: uuid.New(), principal: principal, overrides: map[string]string{"a": "first", "z": "last"}, key: "same-key"},
		{name: "principal", jobID: jobID, baselineRunID: baselineID, principal: otherPrincipal, overrides: map[string]string{"a": "first", "z": "last"}, key: "same-key"},
		{name: "overrides", jobID: jobID, baselineRunID: baselineID, principal: principal, overrides: map[string]string{"a": "changed", "z": "last"}, key: "same-key"},
		{name: "key", jobID: jobID, baselineRunID: baselineID, principal: principal, overrides: map[string]string{"a": "first", "z": "last"}, key: "different-key"},
	}
	for _, tt := range differentInputs {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Fingerprint(tt.jobID, tt.baselineRunID, tt.principal, tt.overrides, tt.key)
			require.NoError(t, err)
			require.NotEqual(t, first, got)
		})
	}
}

func TestReplayResumesPendingReservation(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := runstorage.NewStore(db)
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	jobID := uuid.New()
	baselineRunID := uuid.New()
	replayRunID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()
	triggerID := uuid.New()
	keyID := uuid.New()
	principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, KeyID: &keyID}
	overrides := map[string]string{"mode": "what-if"}
	fingerprint, err := Fingerprint(jobID, baselineRunID, principal, overrides, "retry-key")
	require.NoError(t, err)

	require.NoError(t, db.Create(&models.Trigger{
		ID:        triggerID,
		Type:      models.TriggerTypeHTTP,
		Alias:     "manual",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "resume-replay",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID:           baselineRunID,
		JobID:        jobID,
		TriggerID:    triggerID,
		TriggerType:  string(models.TriggerTypeHTTP),
		TriggerAlias: "manual",
		Status:       string(runstorage.StatusSucceeded),
		StartedAt:    now,
		CompletedAt:  &now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID:                replayRunID,
		JobID:             jobID,
		Status:            string(runstorage.StatusRunning),
		Quarantine:        true,
		ReplayFingerprint: &fingerprint,
		TriggerType:       "replay",
		TriggerAlias:      "quarantined-replay",
		StartedAt:         now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                replayRunID,
		TaskID:                  taskID,
		AtomID:                  atomID,
		Engine:                  models.AtomEngineDocker,
		Image:                   "alpine:3.23",
		Command:                 `["sh","-c","echo replay"]`,
		Status:                  string(runstorage.TaskStatusPending),
		Attempt:                 1,
		MaxAttempts:             1,
		OutstandingPredecessors: 0,
		Quarantine:              true,
		ReplaySafe:              true,
		CreatedAt:               now,
		UpdatedAt:               now,
	}).Error)

	dispatcher := &recordingDispatcher{}
	result, err := (&Service{
		ctx:        context.Background(),
		store:      store,
		dispatcher: dispatcher,
		executionMode: func() string {
			return "distributed"
		},
	}).Replay(Request{
		JobID:          jobID,
		BaselineRunID:  baselineRunID,
		Set:            overrides,
		IdempotencyKey: "retry-key",
		Principal:      principal,
	})
	require.NoError(t, err)
	require.True(t, result.Existing)
	require.Equal(t, replayRunID, result.Run.ID)
	require.Equal(t, []uuid.UUID{replayRunID}, dispatcher.calls)

	var replayRuns int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("replay_fingerprint = ?", fingerprint).Count(&replayRuns).Error)
	require.EqualValues(t, 1, replayRuns)
}

func TestReplayConcurrentIdenticalRequestsReturnSingleReservation(t *testing.T) {
	f := newServiceReplayFixture(t)
	f.seedTask(t, true, "success")
	key := "parallel-retry-key"
	svc := (&Service{
		ctx:        context.Background(),
		store:      f.store,
		dispatcher: &recordingDispatcher{},
	}).WithExecutionMode("local")

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*Result, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = svc.Replay(Request{
				JobID:          f.jobID,
				BaselineRunID:  f.runID,
				IdempotencyKey: key,
				Principal:      f.principal,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var replayID uuid.UUID
	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		require.NotNil(t, results[i])
		require.True(t, results[i].Run.Quarantine)
		if replayID == uuid.Nil {
			replayID = results[i].Run.ID
			continue
		}
		require.Equal(t, replayID, results[i].Run.ID)
	}

	fingerprint, err := Fingerprint(f.jobID, f.runID, f.principal, nil, key)
	require.NoError(t, err)
	var replayRuns int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("replay_fingerprint = ?", fingerprint).Count(&replayRuns).Error)
	require.EqualValues(t, 1, replayRuns)
}

func TestReplayRetryResumesAfterDispatchFailure(t *testing.T) {
	f := newServiceReplayFixture(t)
	f.seedTask(t, true, "success")
	dispatchErr := errors.New("dispatch unavailable")
	req := Request{
		JobID:          f.jobID,
		BaselineRunID:  f.runID,
		Set:            map[string]string{"mode": "what-if"},
		IdempotencyKey: "resume-after-dispatch-error",
		Principal:      f.principal,
	}

	firstDispatcher := &recordingDispatcher{err: dispatchErr}
	_, err := (&Service{
		ctx:        context.Background(),
		store:      f.store,
		dispatcher: firstDispatcher,
	}).WithExecutionMode("distributed").Replay(req)
	require.ErrorIs(t, err, dispatchErr)
	require.Len(t, firstDispatcher.calls, 1)

	fingerprint, err := Fingerprint(f.jobID, f.runID, f.principal, req.Set, req.IdempotencyKey)
	require.NoError(t, err)
	var reserved models.JobRun
	require.NoError(t, f.db.First(&reserved, "replay_fingerprint = ?", fingerprint).Error)
	require.Equal(t, string(runstorage.StatusRunning), reserved.Status)

	var pendingTasks int64
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", reserved.ID, string(runstorage.TaskStatusPending)).
		Count(&pendingTasks).Error)
	require.EqualValues(t, 1, pendingTasks)

	secondDispatcher := &completingDispatcher{store: f.store}
	result, err := (&Service{
		ctx:        context.Background(),
		store:      f.store,
		dispatcher: secondDispatcher,
	}).WithExecutionMode("distributed").Replay(req)
	require.NoError(t, err)
	require.True(t, result.Existing)
	require.Equal(t, reserved.ID, result.Run.ID)
	require.Equal(t, runstorage.StatusSucceeded, result.Run.Status)
	require.Equal(t, []uuid.UUID{reserved.ID}, secondDispatcher.calls)

	var replayRuns int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("replay_fingerprint = ?", fingerprint).Count(&replayRuns).Error)
	require.EqualValues(t, 1, replayRuns)
}

func TestReplayRefusesLocalModeWhenReplayWouldReexecute(t *testing.T) {
	f := newServiceReplayFixture(t)
	f.seedTask(t, true, "success")
	overrides := map[string]string{"mode": "what-if"}
	key := "local-reexecute"
	dispatcher := &recordingDispatcher{}

	_, err := (&Service{
		ctx:        context.Background(),
		store:      f.store,
		dispatcher: dispatcher,
	}).WithExecutionMode("local").Replay(Request{
		JobID:          f.jobID,
		BaselineRunID:  f.runID,
		Set:            overrides,
		IdempotencyKey: key,
		Principal:      f.principal,
	})
	require.ErrorIs(t, err, ErrReplayRequiresDistributedMode)
	require.Empty(t, dispatcher.calls)

	fingerprint, err := Fingerprint(f.jobID, f.runID, f.principal, overrides, key)
	require.NoError(t, err)
	var replayRuns int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("replay_fingerprint = ?", fingerprint).Count(&replayRuns).Error)
	require.Zero(t, replayRuns)
}

func TestReplayExistingUnexpectedNonTerminalStatusIsCorrupt(t *testing.T) {
	f := newServiceReplayFixture(t)
	key := "corrupt-nonterminal"
	fingerprint, err := Fingerprint(f.jobID, f.runID, f.principal, nil, key)
	require.NoError(t, err)
	replayRunID := uuid.New()
	require.NoError(t, f.db.Create(&models.JobRun{
		ID:                replayRunID,
		JobID:             f.jobID,
		Status:            "pending",
		Quarantine:        true,
		ReplayFingerprint: &fingerprint,
		TriggerType:       "replay",
		TriggerAlias:      "quarantined-replay",
		StartedAt:         f.now,
		CreatedAt:         f.now,
		UpdatedAt:         f.now,
	}).Error)
	require.NoError(t, f.db.Create(&models.TaskRun{
		ID:          uuid.New(),
		JobRunID:    replayRunID,
		TaskID:      uuid.New(),
		AtomID:      uuid.New(),
		Engine:      models.AtomEngineDocker,
		Image:       "alpine:3.23",
		Command:     `["sh","-c","echo pending"]`,
		Status:      string(runstorage.TaskStatusPending),
		Attempt:     1,
		MaxAttempts: 1,
		Quarantine:  true,
		ReplaySafe:  true,
		CreatedAt:   f.now,
		UpdatedAt:   f.now,
	}).Error)

	dispatcher := &recordingDispatcher{}
	_, err = (&Service{
		ctx:        context.Background(),
		store:      f.store,
		dispatcher: dispatcher,
	}).WithExecutionMode("distributed").Replay(Request{
		JobID:          f.jobID,
		BaselineRunID:  f.runID,
		IdempotencyKey: key,
		Principal:      f.principal,
	})
	require.ErrorIs(t, err, ErrReplayReservationCorrupt)
	require.Empty(t, dispatcher.calls)
}

func TestUniqueReplayFingerprintClassifierUsesDriverError(t *testing.T) {
	f := newServiceReplayFixture(t)
	fingerprint := "replay:v1:duplicate-driver-error"
	first := models.JobRun{
		ID:                uuid.New(),
		JobID:             f.jobID,
		Status:            string(runstorage.StatusRunning),
		Quarantine:        true,
		ReplayFingerprint: &fingerprint,
		TriggerType:       "replay",
		TriggerAlias:      "quarantined-replay",
		StartedAt:         f.now,
		CreatedAt:         f.now,
		UpdatedAt:         f.now,
	}
	second := first
	second.ID = uuid.New()

	require.NoError(t, f.db.Create(&first).Error)
	err := f.db.Create(&second).Error
	require.Error(t, err)
	require.True(t, isUniqueReplayFingerprintError(err), "actual duplicate insert error was not classified: %v", err)
}

type serviceReplayFixture struct {
	db        *gorm.DB
	store     *runstorage.Store
	jobID     uuid.UUID
	runID     uuid.UUID
	now       time.Time
	principal *iauth.Principal
}

func newServiceReplayFixture(t *testing.T) serviceReplayFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := runstorage.NewStore(db)
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	triggerID := uuid.New()
	jobID := uuid.New()
	runID := uuid.New()
	keyID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:        triggerID,
		Type:      models.TriggerTypeHTTP,
		Alias:     "manual",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "service-replay-job",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID:           runID,
		JobID:        jobID,
		TriggerID:    triggerID,
		TriggerType:  string(models.TriggerTypeHTTP),
		TriggerAlias: "manual",
		Status:       string(runstorage.StatusSucceeded),
		Params:       serviceJSON(t, map[string]string{"mode": "baseline"}),
		StartedAt:    now,
		CompletedAt:  servicePtr(now.Add(time.Second)),
		CreatedAt:    now,
		UpdatedAt:    now,
	}).Error)

	return serviceReplayFixture{
		db:        db,
		store:     store,
		jobID:     jobID,
		runID:     runID,
		now:       now,
		principal: &iauth.Principal{Kind: iauth.PrincipalAPIKey, KeyID: &keyID},
	}
}

func (f serviceReplayFixture) seedTask(t *testing.T, replaySafe bool, result string) uuid.UUID {
	t.Helper()
	atomID := uuid.New()
	taskID := uuid.New()
	command := []string{"sh", "-c", "echo deploy"}
	commandJSON := serviceJSONString(t, command)
	spec := container.Spec{Env: map[string]string{"STEP": "deploy"}}
	require.NoError(t, f.db.Create(&models.Atom{
		ID:        atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   commandJSON,
		Spec:      serviceJSON(t, spec),
		CreatedAt: f.now,
		UpdatedAt: f.now,
	}).Error)
	require.NoError(t, f.db.Create(&models.Task{
		ID:          taskID,
		JobID:       f.jobID,
		AtomID:      atomID,
		Name:        "deploy",
		Position:    0,
		TriggerRule: "all_success",
		ReplaySafe:  replaySafe,
		CreatedAt:   f.now,
		UpdatedAt:   f.now,
	}).Error)

	desc := models.TaskExecutionDescriptor{
		SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
		CapturedAt:    f.now,
		Baseline: models.TaskExecutionBaseline{
			JobID:         f.jobID,
			JobAlias:      "service-replay-job",
			TaskID:        taskID,
			TaskName:      "deploy",
			AtomID:        atomID,
			BaselineRunID: f.runID,
			ReplaySafe:    replaySafe,
		},
		DAG: models.TaskExecutionDAG{
			TriggerRule:    "all_success",
			BranchBehavior: "task",
			TaskPosition:   0,
		},
		Run: models.TaskExecutionRun{Params: map[string]string{"mode": "baseline"}},
		Runtime: models.TaskExecutionRuntime{
			Engine:     models.AtomEngineDocker,
			Image:      "alpine:3.23",
			Command:    command,
			CommandRaw: commandJSON,
			TaskType:   "task",
		},
		Cache:         models.TaskExecutionCache{Enabled: true, Version: 1},
		Schema:        models.TaskExecutionSchema{ValidationMode: "warn"},
		ContainerSpec: spec,
	}
	hash := cache.HashInput{
		JobAlias:     "service-replay-job",
		TaskName:     "deploy",
		Image:        "alpine:3.23",
		Command:      command,
		Env:          spec.Env,
		RunParams:    map[string]string{"mode": "baseline"},
		CacheVersion: 1,
	}.Compute()
	desc.Cache.ComputedHash = hash
	desc.Baseline.ComputedHash = hash

	require.NoError(t, f.db.Create(&models.TaskRun{
		ID:                  uuid.New(),
		JobRunID:            f.runID,
		TaskID:              taskID,
		AtomID:              atomID,
		Engine:              models.AtomEngineDocker,
		Image:               "alpine:3.23",
		Command:             commandJSON,
		Status:              string(runstorage.TaskStatusSucceeded),
		Attempt:             1,
		MaxAttempts:         1,
		Hash:                hash,
		Result:              result,
		CacheEnabled:        true,
		CacheVersion:        1,
		ReplaySafe:          replaySafe,
		ExecutionDescriptor: serviceJSON(t, desc),
		StartedAt:           servicePtr(f.now),
		CompletedAt:         servicePtr(f.now.Add(time.Second)),
		CreatedAt:           f.now,
		UpdatedAt:           f.now,
	}).Error)
	return taskID
}

type completingDispatcher struct {
	store *runstorage.Store
	calls []uuid.UUID
	mu    sync.Mutex
}

func (d *completingDispatcher) DispatchReplay(ctx context.Context, runID uuid.UUID) error {
	d.mu.Lock()
	d.calls = append(d.calls, runID)
	d.mu.Unlock()

	now := time.Now().UTC()
	if err := d.store.DB().WithContext(ctx).Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", runID, string(runstorage.TaskStatusPending)).
		Updates(map[string]any{
			"status":       string(runstorage.TaskStatusSucceeded),
			"result":       "success",
			"completed_at": now,
			"updated_at":   now,
		}).Error; err != nil {
		return err
	}
	return d.store.DB().WithContext(ctx).Model(&models.JobRun{}).
		Where("id = ?", runID).
		Updates(map[string]any{
			"status":       string(runstorage.StatusSucceeded),
			"completed_at": now,
			"updated_at":   now,
		}).Error
}

func serviceJSON(t *testing.T, v any) datatypes.JSON {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return datatypes.JSON(data)
}

func serviceJSONString(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func servicePtr[T any](v T) *T {
	return &v
}
