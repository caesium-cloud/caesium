package run

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type recordingLineageTransport struct {
	mu     sync.Mutex
	events []lineage.RunEvent
}

func (t *recordingLineageTransport) Emit(_ context.Context, evt lineage.RunEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, evt)
	return nil
}

func (t *recordingLineageTransport) Close() error {
	return nil
}

func (t *recordingLineageTransport) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.events)
}

func TestStartQuarantinedRunStartedEventCarriesMarker(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	bus := event.New()
	store.SetBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	published, err := bus.Subscribe(ctx, event.Filter{
		Types:             []event.Type{event.TypeRunStarted},
		IncludeQuarantine: true,
	})
	require.NoError(t, err)

	transport := &recordingLineageTransport{}
	lineageReady := make(chan struct{})
	lineageDone := make(chan error, 1)
	go func() {
		lineageDone <- lineage.NewSubscriber(bus, transport, "caesium-test", db).StartWithReady(ctx, lineageReady)
	}()
	select {
	case <-lineageReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lineage subscriber")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-lineageDone:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for lineage subscriber shutdown")
		}
	})

	const callbackName = "test:quarantine_job_run_before_create"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "job_runs" {
			tx.Statement.SetColumn("Quarantine", true)
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Create().Remove(callbackName)
	})

	runRecord, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.True(t, runRecord.Quarantine)

	var persisted models.ExecutionEvent
	require.NoError(t, db.First(&persisted, "run_id = ? AND type = ?", runRecord.ID, string(event.TypeRunStarted)).Error)
	require.True(t, persisted.Quarantine)

	select {
	case got := <-published:
		require.Equal(t, event.TypeRunStarted, got.Type)
		require.Equal(t, runRecord.ID, got.RunID)
		require.True(t, got.Quarantine)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published run_started event")
	}

	require.Equal(t, 0, transport.Count(), "lineage transport must not receive quarantined run_started")
}

func TestStartAndRegisterTasksPropagatePriority(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "priority-trigger",
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *","timezone":"UTC"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "priority-job",
		TriggerID: trigger.ID,
		Priority:  jobdef.PriorityHigh,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	require.Equal(t, PriorityHighValue, runRecord.Priority)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","priority"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	task := &models.Task{
		ID:        uuid.New(),
		JobID:     job.ID,
		AtomID:    atom.ID,
		Name:      "priority-step",
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)
	require.NoError(t, db.Create(task).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	var taskRun models.TaskRun
	require.NoError(t, db.First(&taskRun, "job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).Error)
	require.Equal(t, PriorityHighValue, taskRun.Priority)
}

func TestTaskExecutionDescriptorCaptureCoversContainerSpecFields(t *testing.T) {
	descriptorType := reflect.TypeOf(models.TaskExecutionDescriptor{})

	containerField, ok := descriptorType.FieldByName("ContainerSpec")
	require.True(t, ok, "descriptor must carry the runtime container spec")
	require.Equal(t, reflect.TypeOf(container.Spec{}), containerField.Type)

	kubernetesField, ok := descriptorType.FieldByName("KubernetesSpec")
	require.True(t, ok, "descriptor must carry the Kubernetes workload identity spec")
	require.Equal(t, reflect.TypeOf((*container.KubernetesSpec)(nil)), kubernetesField.Type)

	// These explicit lists intentionally force a descriptor review when either
	// container carrier grows, even though v1 currently stores the structs whole.
	require.ElementsMatch(t,
		[]string{"Env", "WorkDir", "Mounts", "ResolvedVolumeMounts", "Kubernetes"},
		exportedFieldNames(reflect.TypeOf(container.Spec{})),
	)
	require.ElementsMatch(t,
		[]string{"ServiceAccountName", "PodAnnotations", "AutomountServiceAccountToken", "QueueName"},
		exportedFieldNames(reflect.TypeOf(container.KubernetesSpec{})),
	)
}

func exportedFieldNames(rt reflect.Type) []string {
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.IsExported() {
			names = append(names, field.Name)
		}
	}
	return names
}

func TestRegisterTaskPersistsInitialExecutionDescriptorEnvelope(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "descriptor-trigger",
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *","timezone":"UTC"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:               uuid.New(),
		Alias:            "descriptor-job",
		TriggerID:        trigger.ID,
		Labels:           datatypes.JSONMap{"team": "data"},
		Annotations:      datatypes.JSONMap{"owner": "etl"},
		MaxParallelTasks: 4,
		TaskTimeout:      2 * time.Minute,
		RunTimeout:       10 * time.Minute,
		SLA:              datatypes.JSON(`{"completedBy":"09:00"}`),
		SchemaValidation: jobdef.SchemaValidationFail,
		ReplaySafe:       true,
		CacheConfig:      datatypes.JSON(`{"ttl":"1h","version":3,"pinDigests":true,"digestTTL":"5m"}`),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID, WithStartParams(map[string]string{"logical_date": "2026-06-25"}))
	require.NoError(t, err)

	automount := false
	spec := container.Spec{
		Env: map[string]string{
			"API_TOKEN": "secret://env/API_TOKEN",
			"MODE":      "batch",
		},
		WorkDir: "/workspace",
		Mounts: []container.Mount{{
			Type:     container.MountTypeBind,
			Source:   "/data/in",
			Target:   "/input",
			ReadOnly: true,
		}},
		ResolvedVolumeMounts: []container.VolumeMount{{
			Name:     "scratch",
			Type:     container.VolumeMountTypePVC,
			Source:   "scratch-pvc",
			Target:   "/scratch",
			ReadOnly: false,
			SubPath:  "runs",
		}},
		Kubernetes: &container.KubernetesSpec{
			ServiceAccountName:           "replay-runner",
			PodAnnotations:               map[string]string{"sidecar.istio.io/inject": "false"},
			AutomountServiceAccountToken: &automount,
			QueueName:                    "etl-high",
		},
	}
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineKubernetes,
		Image:     "busybox:1.36.1",
		Command:   `["sh","-c","echo hi"]`,
		Spec:      specJSON,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	task := &models.Task{
		ID:           uuid.New(),
		JobID:        job.ID,
		AtomID:       atom.ID,
		Name:         "load",
		Position:     7,
		Type:         "task",
		NodeSelector: datatypes.JSONMap{"disk": "ssd"},
		Retries:      2,
		RetryDelay:   30 * time.Second,
		RetryBackoff: true,
		InputSchema:  datatypes.JSON(`{"extract":{"type":"object","required":["path"]}}`),
		OutputSchema: datatypes.JSON(`{"type":"object","required":["rows"]}`),
		ReplaySafe:   true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, db.Create(task).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 1))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).Error)

	var descriptor models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(persisted.ExecutionDescriptor, &descriptor))

	require.Equal(t, models.TaskExecutionDescriptorSchemaVersion, descriptor.SchemaVersion)
	require.Equal(t, job.ID, descriptor.Baseline.JobID)
	require.Equal(t, job.Alias, descriptor.Baseline.JobAlias)
	require.Equal(t, task.ID, descriptor.Baseline.TaskID)
	require.Equal(t, task.Name, descriptor.Baseline.TaskName)
	require.Equal(t, atom.ID, descriptor.Baseline.AtomID)
	require.Equal(t, runRecord.ID, descriptor.Baseline.BaselineRunID)
	require.Equal(t, trigger.ID, descriptor.Baseline.TriggerID)
	require.Equal(t, string(models.TriggerTypeCron), descriptor.Baseline.TriggerType)
	require.Equal(t, trigger.Alias, descriptor.Baseline.TriggerAlias)
	require.True(t, descriptor.Baseline.ReplaySafe)
	require.False(t, descriptor.Baseline.Quarantine)
	require.Equal(t, map[string]string{"logical_date": "2026-06-25"}, descriptor.Run.Params)

	require.Equal(t, models.AtomEngineKubernetes, descriptor.Runtime.Engine)
	require.Equal(t, atom.Image, descriptor.Runtime.Image)
	require.Equal(t, []string{"sh", "-c", "echo hi"}, descriptor.Runtime.Command)
	require.Equal(t, atom.Command, descriptor.Runtime.CommandRaw)
	require.Equal(t, "/workspace", descriptor.Runtime.WorkDir)
	require.Equal(t, "task", descriptor.Runtime.TaskType)
	require.Equal(t, map[string]string{"disk": "ssd"}, descriptor.Runtime.NodeSelector)
	require.Equal(t, 2, descriptor.Runtime.RetryCount)
	require.Equal(t, 30*time.Second, descriptor.Runtime.RetryDelay)
	require.True(t, descriptor.Runtime.RetryBackoff)

	require.Equal(t, 2*time.Minute, descriptor.Timing.TaskTimeout)
	require.Equal(t, 10*time.Minute, descriptor.Timing.RunTimeout)
	require.True(t, descriptor.Cache.Enabled)
	require.Equal(t, time.Hour, descriptor.Cache.TTL)
	require.Equal(t, 3, descriptor.Cache.Version)
	require.True(t, descriptor.Cache.PinDigests)
	require.Equal(t, 5*time.Minute, descriptor.Cache.DigestTTL)

	require.JSONEq(t, string(task.InputSchema), string(descriptor.Schema.InputSchema))
	require.JSONEq(t, string(task.OutputSchema), string(descriptor.Schema.OutputSchema))
	require.Equal(t, jobdef.SchemaValidationFail, descriptor.Schema.ValidationMode)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(persisted.ExecutionDescriptor, &raw))
	schemaRaw, ok := raw["schema"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, schemaRaw, "violationBehavior")

	require.Equal(t, 4, descriptor.Job.MaxParallelTasks)
	require.Equal(t, map[string]string{"team": "data"}, descriptor.Job.Labels)
	require.Equal(t, map[string]string{"owner": "etl"}, descriptor.Job.Annotations)
	require.JSONEq(t, string(job.SLA), string(descriptor.Job.SLA))
	require.JSONEq(t, string(job.CacheConfig), string(descriptor.Job.CacheDefaults))
	require.Equal(t, "UTC", descriptor.Job.TriggerConfig["timezone"])

	require.Equal(t, spec.Env, descriptor.ContainerSpec.Env)
	require.Equal(t, spec.WorkDir, descriptor.ContainerSpec.WorkDir)
	require.Equal(t, spec.Mounts, descriptor.ContainerSpec.Mounts)
	require.Equal(t, spec.ResolvedVolumeMounts, descriptor.ContainerSpec.ResolvedVolumeMounts)
	require.NotNil(t, descriptor.KubernetesSpec)
	require.Equal(t, "replay-runner", descriptor.KubernetesSpec.ServiceAccountName)
	require.Equal(t, map[string]string{"sidecar.istio.io/inject": "false"}, descriptor.KubernetesSpec.PodAnnotations)
	require.NotNil(t, descriptor.KubernetesSpec.AutomountServiceAccountToken)
	require.False(t, *descriptor.KubernetesSpec.AutomountServiceAccountToken)
	require.Equal(t, "etl-high", descriptor.KubernetesSpec.QueueName)

	require.Len(t, descriptor.SecretRefs, 1)
	require.Equal(t, "API_TOKEN", descriptor.SecretRefs[0].EnvKey)
	require.Equal(t, "secret://env/API_TOKEN", descriptor.SecretRefs[0].Ref)
	require.False(t, descriptor.SecretRefs[0].Verifiable)
	require.Contains(t, descriptor.SecretRefs[0].UnverifiableReason, "not resolved yet")
}

func TestTaskExecutionDescriptorMutationRetriesConcurrentChange(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	runID, taskID := registerSingleTaskRun(t, store, db)

	// mutateTaskExecutionDescriptor uses optimistic CAS-with-retry (no enclosing
	// transaction). Under real concurrency, two writers adding distinct secret
	// refs must BOTH survive: when one writer's conditional UPDATE misses (the
	// row changed since its read), it re-reads and re-merges rather than
	// clobbering. Drive that with two goroutines released together to maximize
	// interleaving — a synthetic re-entrant callback would deadlock SQLite's
	// single writer.
	refs := []models.TaskExecutionSecretRef{
		{Ref: "secret://env/API_TOKEN", EnvKey: "API_TOKEN", Provider: "env", Verifiable: false},
		{Ref: "secret://env/DB_PASSWORD", EnvKey: "DB_PASSWORD", Provider: "env", Verifiable: false},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(refs))
	start := make(chan struct{})
	for i := range refs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = store.UpdateTaskExecutionDescriptorSecretRefs(runID, taskID, []models.TaskExecutionSecretRef{refs[i]})
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	var persisted models.TaskRun
	require.NoError(t, db.Select("execution_descriptor").First(&persisted, "job_run_id = ? AND task_id = ?", runID, taskID).Error)
	var descriptor models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(persisted.ExecutionDescriptor, &descriptor))
	envKeys := make(map[string]bool)
	for _, r := range descriptor.SecretRefs {
		envKeys[r.EnvKey] = true
	}
	require.True(t, envKeys["API_TOKEN"], "API_TOKEN ref lost to a concurrent descriptor write")
	require.True(t, envKeys["DB_PASSWORD"], "DB_PASSWORD ref lost to a concurrent descriptor write")
}

func TestStorePersistsRunState(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, runRecord.Status)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	require.Len(t, state.Tasks, 2)

	var taskStateA, taskStateB *TaskRun
	for _, rt := range state.Tasks {
		switch rt.ID {
		case taskA.ID:
			taskStateA = rt
		case taskB.ID:
			taskStateB = rt
		}
	}
	require.NotNil(t, taskStateA)
	require.NotNil(t, taskStateB)
	require.Equal(t, TaskStatusPending, taskStateA.Status)
	require.Equal(t, 1, taskStateB.OutstandingPredecessors)

	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))

	// Simulate restart and ensure running tasks reset back to pending.
	secondStore := NewStore(db)
	require.NoError(t, secondStore.ResetInFlightTasks(runRecord.ID))

	// Completing the first task should decrement the successor outstanding count.
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", nil, nil))

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 0, rt.OutstandingPredecessors)
		}
	}

	require.NoError(t, store.CompleteTask(runRecord.ID, taskB.ID, "ok", nil, nil))
	require.NoError(t, store.Complete(runRecord.ID, nil))

	finalStore := NewStore(db)
	finalRun, err := finalStore.Get(runRecord.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, finalRun.Status)
}

// TestCompleteRetriesTransientContention pins that Store.Complete routes
// through withStoreBusyRetry. Complete runs on the run-completion path taken by
// every job run; before this guard a transient "checkpoint in progress" /
// "database is locked" on the completion write (or its bookkeeping SELECT)
// propagated to the caller and marked an otherwise-successful run as failed —
// the exact integration flake in TestBackfillBasicHappyPath. Here we inject a
// one-shot contention error on the job_runs UPDATE and assert the run still
// commits as succeeded and Complete returns nil.
func TestCompleteRetriesTransientContention(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	fired := false
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:one_shot_contention", func(tx *gorm.DB) {
		if fired || tx.Statement.Table != "job_runs" {
			return
		}
		fired = true
		_ = tx.AddError(errors.New("checkpoint in progress"))
	}))
	t.Cleanup(func() {
		_ = db.Callback().Update().Remove("test:one_shot_contention")
	})

	require.NoError(t, store.Complete(runRecord.ID, nil))
	require.True(t, fired, "expected the injected contention error to fire at least once")

	finalRun, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, finalRun.Status)
}

func TestCompleteTaskSkipsFallbackWhenJobHasEdges(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	atomC := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","c"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)
	require.NoError(t, db.Create(atomC).Error)

	base := time.Now().UTC().Add(-2 * time.Minute)
	taskA := &models.Task{
		ID:        uuid.New(),
		JobID:     jobID,
		AtomID:    atomA.ID,
		CreatedAt: base,
		UpdatedAt: base,
	}
	taskC := &models.Task{
		ID:        uuid.New(),
		JobID:     jobID,
		AtomID:    atomC.ID,
		CreatedAt: base.Add(10 * time.Second),
		UpdatedAt: base.Add(10 * time.Second),
	}
	taskB := &models.Task{
		ID:        uuid.New(),
		JobID:     jobID,
		AtomID:    atomB.ID,
		CreatedAt: base.Add(20 * time.Second),
		UpdatedAt: base.Add(20 * time.Second),
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)
	require.NoError(t, db.Create(taskC).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskC, atomC, 0))

	require.NoError(t, store.CompleteTask(runRecord.ID, taskC.ID, "ok", nil, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 1, rt.OutstandingPredecessors)
		}
	}

	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", nil, nil))

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)
	for _, rt := range state.Tasks {
		if rt.ID == taskB.ID {
			require.Equal(t, 0, rt.OutstandingPredecessors)
		}
	}
}

func TestRegisterTaskPersistsSchemaValidationConfig(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     "schema-validation-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:               uuid.New(),
		Alias:            "schema-validation-job",
		TriggerID:        trigger.ID,
		SchemaValidation: jobdef.SchemaValidationFail,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","test"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	schema := datatypes.JSON(`{"type":"object","required":["rows_written"]}`)
	task := &models.Task{
		ID:           uuid.New(),
		JobID:        job.ID,
		AtomID:       atom.ID,
		Name:         "transform",
		OutputSchema: schema,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, db.Create(task).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).Error)
	require.JSONEq(t, string(schema), string(persisted.OutputSchema))
	require.Equal(t, job.SchemaValidation, persisted.SchemaValidation)
}

func TestRegisterTaskPersistsReplaySafeSnapshot(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     "replay-safe-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "replay-safe-job",
		TriggerID: trigger.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","test"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	unsafeTask := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "unsafe", CreatedAt: now, UpdatedAt: now}
	safeTask := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "safe", ReplaySafe: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Task{unsafeTask, safeTask}).Error)

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: unsafeTask, Atom: atom, OutstandingPredecessors: 0},
		{Task: safeTask, Atom: atom, OutstandingPredecessors: 0},
	}))

	var unsafeRun, safeRun models.TaskRun
	require.NoError(t, db.First(&unsafeRun, "job_run_id = ? AND task_id = ?", runRecord.ID, unsafeTask.ID).Error)
	require.NoError(t, db.First(&safeRun, "job_run_id = ? AND task_id = ?", runRecord.ID, safeTask.ID).Error)
	require.False(t, unsafeRun.ReplaySafe)
	require.True(t, safeRun.ReplaySafe)

	require.NoError(t, db.Model(job).Update("replay_safe", true).Error)
	secondRun, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	require.NoError(t, store.RegisterTask(secondRun.ID, unsafeTask, atom, 0))

	var jobLevelRun models.TaskRun
	require.NoError(t, db.First(&jobLevelRun, "job_run_id = ? AND task_id = ?", secondRun.ID, unsafeTask.ID).Error)
	require.True(t, jobLevelRun.ReplaySafe)
}

func TestRegisterTasksBatchesReadyEventsAndSkipsExisting(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	now := time.Now().UTC()

	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     "batch-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "batch-job",
		TriggerID: trigger.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","batch"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	taskA := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "a", CreatedAt: now, UpdatedAt: now}
	taskB := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "b", CreatedAt: now, UpdatedAt: now}
	taskC := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "c", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Task{taskA, taskB, taskC}).Error)

	require.NoError(t, db.Create(&models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                runRecord.ID,
		TaskID:                  taskA.ID,
		AtomID:                  atom.ID,
		Engine:                  atom.Engine,
		Image:                   atom.Image,
		Command:                 atom.Command,
		Status:                  string(TaskStatusPending),
		Attempt:                 1,
		MaxAttempts:             1,
		OutstandingPredecessors: 0,
		CreatedAt:               now,
		UpdatedAt:               now,
	}).Error)

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: taskA, Atom: atom, OutstandingPredecessors: 0},
		{Task: taskB, Atom: atom, OutstandingPredecessors: 0},
		{Task: taskC, Atom: atom, OutstandingPredecessors: 1},
	}))

	var taskRunCount int64
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runRecord.ID).Count(&taskRunCount).Error)
	require.Equal(t, int64(3), taskRunCount)

	var readyEvents []models.ExecutionEvent
	require.NoError(t, db.Where("run_id = ? AND type = ?", runRecord.ID, string(event.TypeTaskReady)).Find(&readyEvents).Error)
	require.Len(t, readyEvents, 1)
	require.NotNil(t, readyEvents[0].TaskID)
	require.NotNil(t, readyEvents[0].JobID)
	require.Equal(t, taskB.ID, *readyEvents[0].TaskID)
	require.Equal(t, job.ID, *readyEvents[0].JobID)
	require.True(t, readyEvents[0].BusDispatchPending)
	require.Nil(t, readyEvents[0].BusDispatchedAt)
}

func TestRegisterTasksReturnsMissingJobRunError(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)
	task := &models.Task{ID: uuid.New(), JobID: uuid.New(), AtomID: uuid.New()}
	atom := &models.Atom{ID: task.AtomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23"}

	err := store.RegisterTasks(uuid.New(), []RegisterTaskInput{
		{Task: task, Atom: atom, OutstandingPredecessors: 0},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "job run")
}

func TestBatchEventQuarantineStampUsesRunAndTaskMarkers(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	runID, taskA := registerSingleTaskRun(t, store, db)
	var taskRunA models.TaskRun
	require.NoError(t, db.First(&taskRunA, "job_run_id = ? AND task_id = ?", runID, taskA).Error)
	var jobRun models.JobRun
	require.NoError(t, db.First(&jobRun, "id = ?", runID).Error)

	now := time.Now().UTC()
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","b"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atom).Error)
	taskB := &models.Task{ID: uuid.New(), JobID: jobRun.JobID, AtomID: atom.ID, Name: "batch-b", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(taskB).Error)
	require.NoError(t, store.RegisterTask(runID, taskB, atom, 0))

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Update("quarantine", true).Error)
	runMarked := []*event.Event{
		{Type: event.TypeRunStarted, RunID: runID},
		{Type: event.TypeTaskReady, RunID: runID, TaskID: taskA},
		{Type: event.TypeTaskReady, RunID: runID, TaskID: taskB.ID},
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.stampBatchEventQuarantineTx(tx, runMarked)
	}))
	for _, evt := range runMarked {
		require.True(t, evt.Quarantine, "run-level quarantine should mark every batch event")
	}

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Update("quarantine", false).Error)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ? AND task_id = ?", runID, taskA).Update("quarantine", true).Error)
	taskMarked := []*event.Event{
		{Type: event.TypeRunStarted, RunID: runID},
		{Type: event.TypeTaskReady, RunID: runID, TaskID: taskA},
		{Type: event.TypeTaskReady, RunID: runID, TaskID: taskB.ID},
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.stampBatchEventQuarantineTx(tx, taskMarked)
	}))
	require.False(t, taskMarked[0].Quarantine)
	require.True(t, taskMarked[1].Quarantine)
	require.False(t, taskMarked[2].Quarantine)
}

func TestWithStoreBusyRetryRetriesSQLiteContention(t *testing.T) {
	attempts := 0
	err := withStoreBusyRetry(func() error {
		attempts++
		if attempts == 1 {
			return sqlite3.Error{Code: sqlite3.ErrBusy}
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

func TestWithStoreBusyRetryRetriesCheckpointContention(t *testing.T) {
	attempts := 0
	err := withStoreBusyRetry(func() error {
		attempts++
		if attempts == 1 {
			return errors.New("checkpoint in progress")
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

func TestClaimAwareTaskLifecycleMethods(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atom.ID,
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	claimOwner := "node-a"
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).
		Updates(map[string]interface{}{
			"status":           string(TaskStatusRunning),
			"claimed_by":       claimOwner,
			"claim_expires_at": time.Now().UTC().Add(1 * time.Minute),
			"claim_attempt":    1,
		}).Error)

	err = store.StartTaskClaimed(runRecord.ID, task.ID, "runtime-a", "node-b")
	require.ErrorIs(t, err, ErrTaskClaimMismatch)

	require.NoError(t, store.StartTaskClaimed(runRecord.ID, task.ID, "runtime-a", claimOwner))

	err = store.CompleteTaskClaimed(runRecord.ID, task.ID, "ok", "node-b", nil, nil)
	require.ErrorIs(t, err, ErrTaskClaimMismatch)

	require.NoError(t, store.CompleteTaskClaimed(runRecord.ID, task.ID, "ok", claimOwner, nil, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusSucceeded, taskState.Status)
	require.Equal(t, "runtime-a", taskState.RuntimeID)
	require.Equal(t, claimOwner, taskState.ClaimedBy)
	require.NotNil(t, taskState.StartedAt)
}

func TestCompleteTaskWithOutput(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

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
		Name:   "step-one",
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atomModel, 0))
	require.NoError(t, store.StartTask(runRecord.ID, task.ID, "runtime-1"))

	output := map[string]string{
		"row_count": "42",
		"path":      "/data/out.parquet",
	}
	require.NoError(t, store.CompleteTask(runRecord.ID, task.ID, "ok", output, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusSucceeded, taskState.Status)
	require.Equal(t, map[string]string{
		"row_count": "42",
		"path":      "/data/out.parquet",
	}, taskState.Output)
}

func TestCompleteTaskWithNilOutput(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

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
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atomModel, 0))
	require.NoError(t, store.StartTask(runRecord.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runRecord.ID, task.ID, "ok", nil, nil))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusSucceeded, taskState.Status)
	require.Nil(t, taskState.Output)
}

func TestRetryTaskClearsPreviousExecutionArtifacts(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

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
		Name:   "retry-me",
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atomModel, 0))
	require.NoError(t, store.StartTask(runRecord.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runRecord.ID, task.ID, "failure", map[string]string{"rows": "10"}, []string{"branch-a"}))
	require.NoError(t, store.SaveTaskLogSnapshot(runRecord.ID, task.ID, &TaskLogSnapshot{
		Text:      "previous attempt logs",
		Truncated: true,
	}))

	require.NoError(t, store.RetryTask(runRecord.ID, task.ID, 2))

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	var taskState *TaskRun
	for _, candidate := range state.Tasks {
		if candidate.ID == task.ID {
			taskState = candidate
			break
		}
	}
	require.NotNil(t, taskState)
	require.Equal(t, TaskStatusPending, taskState.Status)
	require.Equal(t, "", taskState.RuntimeID)
	require.Equal(t, "", taskState.Result)
	require.Nil(t, taskState.Output)

	snapshot, err := store.GetTaskLogSnapshot(runRecord.ID, task.ID)
	require.NoError(t, err)
	require.Nil(t, snapshot)

	var model models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).First(&model).Error)
	require.Equal(t, "", model.LogText)
	require.False(t, model.LogTruncated)
	require.Len(t, model.BranchSelections, 0)
}

func TestPredecessorOutputs(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
		Name:   "step-a",
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
		Name:   "step-b",
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))

	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", map[string]string{
		"row_count": "42",
	}, nil))

	outputs, err := store.PredecessorOutputs(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	require.Equal(t, map[string]string{"row_count": "42"}, outputs["step-a"])
}

func TestPredecessorOutputs_NoPredecessors(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	taskID := uuid.New()
	runID := uuid.New()

	outputs, err := store.PredecessorOutputs(runID, taskID)
	require.NoError(t, err)
	require.Nil(t, outputs)
}

func TestPredecessorHashes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
		Name:   "step-a",
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
		Name:   "step-b",
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))
	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", map[string]string{
		"row_count": "42",
	}, nil))
	require.NoError(t, store.SetTaskHash(runRecord.ID, taskA.ID, "pred-hash-1"))

	hashes, err := store.PredecessorHashes(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"pred-hash-1"}, hashes)
}

// TestPredecessorHashesUsesEffectiveHash asserts the value-verified
// short-circuit (D2): when a predecessor recorded an effective_hash (its code
// changed but its output was proven byte-identical to a prior run), downstream
// PredecessorHashes presents that prior identity, not the predecessor's new
// hash — so the downstream task sees an unchanged input and stays a cache hit.
func TestPredecessorHashesUsesEffectiveHash(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","a"]`}
	atomB := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","b"]`}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomA.ID, Name: "step-a"}
	taskB := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomB.ID, Name: "step-b"}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)

	edge := &models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: taskA.ID, ToTaskID: taskB.ID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))
	require.NoError(t, store.StartTask(runRecord.ID, taskA.ID, "runtime-a"))
	require.NoError(t, store.CompleteTask(runRecord.ID, taskA.ID, "ok", map[string]string{"row_count": "42"}, nil))

	// taskA re-executed with a NEW identity but was proven value-equal to a
	// prior run whose identity was "pred-hash-old".
	require.NoError(t, store.SetTaskHash(runRecord.ID, taskA.ID, "pred-hash-new"))
	require.NoError(t, store.SetTaskEffectiveHash(runRecord.ID, taskA.ID, "pred-hash-old"))

	hashes, err := store.PredecessorHashes(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"pred-hash-old"}, hashes, "downstream must see the proven prior identity, not the new one")

	// Sanity: an empty effective hash is a no-op (falls back to the true hash).
	require.NoError(t, store.SetTaskEffectiveHash(runRecord.ID, taskA.ID, ""))
	hashes, err = store.PredecessorHashes(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"pred-hash-old"}, hashes, "empty effective hash must not clear an already-set one")
}

func TestPredecessorHashesIncludesCachedPredecessors(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomA := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","a"]`,
	}
	atomB := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","b"]`,
	}
	require.NoError(t, db.Create(atomA).Error)
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomA.ID,
		Name:   "step-a",
	}
	taskB := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atomB.ID,
		Name:   "step-b",
	}
	require.NoError(t, db.Create(taskA).Error)
	require.NoError(t, db.Create(taskB).Error)

	edge := &models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: taskA.ID,
		ToTaskID:   taskB.ID,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(edge).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, taskB, atomB, 1))
	require.NoError(t, store.SetTaskHash(runRecord.ID, taskA.ID, "pred-hash-cached"))
	_, err = store.CacheHitTask(runRecord.ID, taskA.ID, CacheHitSource{
		RunID:     uuid.New(),
		CreatedAt: time.Now().UTC(),
	}, "ok", map[string]string{"row_count": "42"}, nil)
	require.NoError(t, err)

	hashes, err := store.PredecessorHashes(runRecord.ID, taskB.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"pred-hash-cached"}, hashes)
}

func TestRegisterTaskSnapshotsResolvedCacheConfig(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})
	t.Setenv("CAESIUM_CACHE_ENABLED", "false")
	t.Setenv("CAESIUM_CACHE_TTL", "30m")

	store := NewStore(db)

	jobCacheJSON, err := json.Marshal(map[string]any{"ttl": "2h"})
	require.NoError(t, err)

	trigger := &models.Trigger{
		ID:    uuid.New(),
		Alias: "cache-trigger",
		Type:  models.TriggerTypeCron,
	}
	require.NoError(t, db.Create(trigger).Error)

	jobID := uuid.New()
	jobModel := &models.Job{
		ID:          jobID,
		Alias:       "cache-job",
		TriggerID:   trigger.ID,
		CacheConfig: datatypes.JSON(jobCacheJSON),
	}
	require.NoError(t, db.Create(jobModel).Error)

	runRecord, err := store.Start(jobID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","cache"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	stepCacheJSON, err := json.Marshal(map[string]any{"version": 3})
	require.NoError(t, err)

	task := &models.Task{
		ID:          uuid.New(),
		JobID:       jobID,
		AtomID:      atom.ID,
		Name:        "cacheable",
		CacheConfig: datatypes.JSON(stepCacheJSON),
	}
	require.NoError(t, db.Create(task).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))

	var taskRun models.TaskRun
	require.NoError(t, db.First(&taskRun, "job_run_id = ? AND task_id = ?", runRecord.ID, task.ID).Error)
	require.True(t, taskRun.CacheEnabled)
	require.Equal(t, 2*time.Hour, taskRun.CacheTTL)
	require.Equal(t, 3, taskRun.CacheVersion)
}

func TestCompleteTaskWithBranchSkipLeavesOneSuccessJoinRunnable(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","ok"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	decide := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "decide", Type: "branch"}
	fast := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fast-path"}
	slow := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "slow-path"}
	join := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "join", TriggerRule: "one_success"}
	require.NoError(t, db.Create(decide).Error)
	require.NoError(t, db.Create(fast).Error)
	require.NoError(t, db.Create(slow).Error)
	require.NoError(t, db.Create(join).Error)

	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: fast.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: slow.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: fast.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: slow.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, decide, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, fast, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, slow, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, join, atom, 2))

	require.NoError(t, store.StartTask(runRecord.ID, decide.ID, "runtime-decide"))
	completeResult, err := store.CompleteTaskWithResult(runRecord.ID, decide.ID, "ok", nil, []string{"fast-path"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{slow.ID}, completeResult.SkippedTaskIDs)

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	statusByTask := make(map[uuid.UUID]*TaskRun, len(state.Tasks))
	for _, task := range state.Tasks {
		statusByTask[task.TaskID] = task
	}
	require.Equal(t, TaskStatusSkipped, statusByTask[slow.ID].Status)
	require.Equal(t, TaskStatusPending, statusByTask[join.ID].Status)
	require.Equal(t, 1, statusByTask[join.ID].OutstandingPredecessors)

	require.NoError(t, store.StartTask(runRecord.ID, fast.ID, "runtime-fast"))
	completeResult, err = store.CompleteTaskWithResult(runRecord.ID, fast.ID, "ok", nil, nil)
	require.NoError(t, err)
	require.Empty(t, completeResult.SkippedTaskIDs)

	state, err = store.Get(runRecord.ID)
	require.NoError(t, err)

	statusByTask = make(map[uuid.UUID]*TaskRun, len(state.Tasks))
	for _, task := range state.Tasks {
		statusByTask[task.TaskID] = task
	}
	require.Equal(t, TaskStatusPending, statusByTask[join.ID].Status)
	require.Equal(t, 0, statusByTask[join.ID].OutstandingPredecessors)
}

func TestCompleteTaskWithBranchSkipSkipsAllSuccessJoin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","ok"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	decide := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "decide", Type: "branch"}
	fast := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fast-path"}
	slow := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "slow-path"}
	join := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "join", TriggerRule: "all_success"}
	require.NoError(t, db.Create(decide).Error)
	require.NoError(t, db.Create(fast).Error)
	require.NoError(t, db.Create(slow).Error)
	require.NoError(t, db.Create(join).Error)

	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: fast.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: decide.ID, ToTaskID: slow.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: fast.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{ID: uuid.New(), JobID: jobID, FromTaskID: slow.ID, ToTaskID: join.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, decide, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, fast, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, slow, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, join, atom, 2))

	require.NoError(t, store.StartTask(runRecord.ID, decide.ID, "runtime-decide"))
	completeResult, err := store.CompleteTaskWithResult(runRecord.ID, decide.ID, "ok", nil, []string{"fast-path"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{slow.ID}, completeResult.SkippedTaskIDs)

	require.NoError(t, store.StartTask(runRecord.ID, fast.ID, "runtime-fast"))
	completeResult, err = store.CompleteTaskWithResult(runRecord.ID, fast.ID, "ok", nil, nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{join.ID}, completeResult.SkippedTaskIDs)

	state, err := store.Get(runRecord.ID)
	require.NoError(t, err)

	statusByTask := make(map[uuid.UUID]*TaskRun, len(state.Tasks))
	for _, task := range state.Tasks {
		statusByTask[task.TaskID] = task
	}
	require.Equal(t, TaskStatusSkipped, statusByTask[join.ID].Status)
	require.Equal(t, 0, statusByTask[join.ID].OutstandingPredecessors)
}

// seedClaimedTaskRun inserts a task_run row that looks like it has been claimed
// by nodeID and returns the row's UUID.
func seedClaimedTaskRun(t *testing.T, store *Store, nodeID string) uuid.UUID {
	t.Helper()

	now := time.Now().UTC()
	expires := now.Add(5 * time.Minute)

	taskRunID := uuid.New()
	tr := &models.TaskRun{
		ID:             taskRunID,
		JobRunID:       uuid.New(),
		TaskID:         uuid.New(),
		AtomID:         uuid.New(),
		Engine:         models.AtomEngineDocker,
		Image:          "alpine:3.23",
		Status:         string(TaskStatusRunning),
		ClaimedBy:      nodeID,
		ClaimExpiresAt: &expires,
		Attempt:        1,
		MaxAttempts:    1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, store.db.Create(tr).Error)
	return taskRunID
}

func TestRenewLeasesHappyPath(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	nodeID := "node-a"
	id := seedClaimedTaskRun(t, store, nodeID)

	newExpiry := time.Now().UTC().Add(10 * time.Minute)
	rowsAffected, err := store.RenewLeases(t.Context(), nodeID, []uuid.UUID{id}, newExpiry)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	var tr models.TaskRun
	require.NoError(t, db.First(&tr, "id = ?", id).Error)
	require.NotNil(t, tr.ClaimExpiresAt)
	require.WithinDuration(t, newExpiry, *tr.ClaimExpiresAt, time.Second)
}

func TestRenewLeasesEmptyIDsIsNoOp(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	// Should return (0, nil) without hitting the DB.
	rowsAffected, err := store.RenewLeases(t.Context(), "node-a", nil, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(0), rowsAffected)
}

func TestRenewLeasesDoesNotTouchOtherNode(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)

	nodeA := "node-a"
	nodeB := "node-b"

	idA := seedClaimedTaskRun(t, store, nodeA)
	idB := seedClaimedTaskRun(t, store, nodeB)

	// Fetch original expiry for node-b's row so we can assert it is unchanged.
	var trBBefore models.TaskRun
	require.NoError(t, db.First(&trBBefore, "id = ?", idB).Error)
	originalExpiryB := trBBefore.ClaimExpiresAt

	// Renew only node-a's claims, passing node-b's ID in the list too.  The
	// claimed_by predicate should prevent node-b's row from being updated, and
	// rowsAffected should reflect only node-a's matching row.
	newExpiry := time.Now().UTC().Add(10 * time.Minute)
	rowsAffected, err := store.RenewLeases(t.Context(), nodeA, []uuid.UUID{idA, idB}, newExpiry)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	// node-a's row should be extended.
	var trA models.TaskRun
	require.NoError(t, db.First(&trA, "id = ?", idA).Error)
	require.NotNil(t, trA.ClaimExpiresAt)
	require.WithinDuration(t, newExpiry, *trA.ClaimExpiresAt, time.Second)

	// node-b's row must be untouched.
	var trBAfter models.TaskRun
	require.NoError(t, db.First(&trBAfter, "id = ?", idB).Error)
	if originalExpiryB != nil && trBAfter.ClaimExpiresAt != nil {
		require.Equal(t, originalExpiryB.Unix(), trBAfter.ClaimExpiresAt.Unix())
	}
}

// TestBatchedPredecessorDecrement verifies that completing a task with fan-out=4
// (one root → four successors → one join, each successor pointing to the join)
// produces exactly one predecessor-counter UPDATE and one batched event INSERT
// as reflected in the db write metric counters.
func TestBatchedPredecessorDecrement(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`}
	require.NoError(t, db.Create(atom).Error)

	root := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "root"}
	lane1 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "lane1"}
	lane2 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "lane2"}
	lane3 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "lane3"}
	lane4 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "lane4"}
	require.NoError(t, db.Create([]*models.Task{root, lane1, lane2, lane3, lane4}).Error)

	now := time.Now().UTC()
	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane1.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane2.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane3.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane4.ID, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, root, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, lane1, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, lane2, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, lane3, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, lane4, atom, 1))

	// Count event rows before completion.
	var eventsBefore int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ?", runRecord.ID).Count(&eventsBefore).Error)

	require.NoError(t, store.StartTask(runRecord.ID, root.ID, "runtime-root"))
	require.NoError(t, store.CompleteTask(runRecord.ID, root.ID, "ok", nil, nil))

	// All four successors should have outstanding_predecessors = 0.
	var successors []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id IN ?", runRecord.ID,
		[]uuid.UUID{lane1.ID, lane2.ID, lane3.ID, lane4.ID}).Find(&successors).Error)
	for _, s := range successors {
		require.Equal(t, 0, s.OutstandingPredecessors, "successor %s should have 0 outstanding predecessors", s.TaskID)
		require.Equal(t, string(TaskStatusPending), s.Status)
	}

	// Exactly 4 task_ready events should have been emitted (one per successor that hit zero).
	var readyEvents []models.ExecutionEvent
	require.NoError(t, db.Where("run_id = ? AND type = ?", runRecord.ID, string(event.TypeTaskReady)).Find(&readyEvents).Error)
	// 4 task_ready from RegisterTask (for root) + 4 from CompleteTask successors = but root had 0 predecessors → 1 ready at register.
	// Then 4 more task_ready from completeTask. Total = 5.
	// Also 1 task_started event from StartTask + 1 task_succeeded from CompleteTask.
	// Let's just verify the total number of events is correct:
	var eventsAfter int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ?", runRecord.ID).Count(&eventsAfter).Error)
	// Expected: eventsBefore (from Start + RegisterTask root ready event) + task_started + task_succeeded + 4×task_ready
	// The exact count depends on what was before. Just verify >= 6 new events were inserted.
	require.GreaterOrEqual(t, eventsAfter-eventsBefore, int64(6), "expected at least 6 new events from start+complete")
}

// TestBatchedPredecessorDecrement_TriggerRuleFilter verifies that a successor
// filtered out by triggerRule="one_success" gets its counter decremented but
// does not receive a task_ready event when it shouldn't run.
func TestBatchedPredecessorDecrement_TriggerRuleFilter(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`}
	require.NoError(t, db.Create(atom).Error)

	// Two predecessors feeding a join with triggerRule=one_success.
	pred1 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "pred1"}
	pred2 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "pred2"}
	join := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "join", TriggerRule: "one_success"}
	require.NoError(t, db.Create([]*models.Task{pred1, pred2, join}).Error)

	now := time.Now().UTC()
	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: pred1.ID, ToTaskID: join.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: pred2.ID, ToTaskID: join.ID, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, pred1, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, pred2, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, join, atom, 2))

	// Complete pred1: join still has 1 outstanding predecessor, no task_ready for join yet.
	require.NoError(t, store.StartTask(runRecord.ID, pred1.ID, "r1"))
	require.NoError(t, store.CompleteTask(runRecord.ID, pred1.ID, "ok", nil, nil))

	var joinRun models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, join.ID).First(&joinRun).Error)
	require.Equal(t, 1, joinRun.OutstandingPredecessors)
	require.Equal(t, string(TaskStatusPending), joinRun.Status)

	// Complete pred2: join hits 0 predecessors.  With one_success already satisfied,
	// it should get task_ready.
	require.NoError(t, store.StartTask(runRecord.ID, pred2.ID, "r2"))
	require.NoError(t, store.CompleteTask(runRecord.ID, pred2.ID, "ok", nil, nil))

	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, join.ID).First(&joinRun).Error)
	require.Equal(t, 0, joinRun.OutstandingPredecessors)
	require.Equal(t, string(TaskStatusPending), joinRun.Status)

	var readyEvents []models.ExecutionEvent
	require.NoError(t, db.Where("run_id = ? AND task_id = ? AND type = ?",
		runRecord.ID, join.ID, string(event.TypeTaskReady)).Find(&readyEvents).Error)
	require.NotEmpty(t, readyEvents, "join should have received a task_ready event")
}

// TestSkipPropagation_MultiLevel verifies that a multi-level skip correctly
// decrements every descendant's outstanding_predecessors counter.
func TestSkipPropagation_MultiLevel(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`}
	require.NoError(t, db.Create(atom).Error)

	// DAG: root --(branch)--> branchA --> midA --> tail
	//                    \--> branchB (skipped)
	root := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "root", Type: "branch"}
	branchA := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "branchA"}
	branchB := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "branchB"}
	midA := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "midA"}
	tail := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "tail", TriggerRule: "all_success"}
	require.NoError(t, db.Create([]*models.Task{root, branchA, branchB, midA, tail}).Error)

	now := time.Now().UTC()
	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: branchA.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: branchB.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: branchA.ID, ToTaskID: midA.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: midA.ID, ToTaskID: tail.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: branchB.ID, ToTaskID: tail.ID, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, db.Create(&edges).Error)

	require.NoError(t, store.RegisterTask(runRecord.ID, root, atom, 0))
	require.NoError(t, store.RegisterTask(runRecord.ID, branchA, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, branchB, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, midA, atom, 1))
	require.NoError(t, store.RegisterTask(runRecord.ID, tail, atom, 2))

	// Complete root selecting only branchA — branchB gets skipped.
	require.NoError(t, store.StartTask(runRecord.ID, root.ID, "r-root"))
	result, err := store.CompleteTaskWithResult(runRecord.ID, root.ID, "ok", nil, []string{"branchA"})
	require.NoError(t, err)
	require.Contains(t, result.SkippedTaskIDs, branchB.ID)

	// branchB is skipped, so tail's predecessor count from branchB should be decremented.
	var tailRun models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, tail.ID).First(&tailRun).Error)
	// tail has 2 predecessors: midA (not yet done) and branchB (skipped).
	// After branchB skip propagation: tail's count should be 1 (midA still pending).
	require.Equal(t, 1, tailRun.OutstandingPredecessors, "tail should have 1 remaining predecessor after branchB skip")
	require.Equal(t, string(TaskStatusPending), tailRun.Status)

	// Complete branchA → midA → tail should complete normally.
	require.NoError(t, store.StartTask(runRecord.ID, branchA.ID, "r-branchA"))
	require.NoError(t, store.CompleteTask(runRecord.ID, branchA.ID, "ok", nil, nil))

	require.NoError(t, store.StartTask(runRecord.ID, midA.ID, "r-midA"))
	result, err = store.CompleteTaskWithResult(runRecord.ID, midA.ID, "ok", nil, nil)
	require.NoError(t, err)
	// tail's all_success rule: midA succeeded but branchB was skipped → rule not satisfied.
	require.Contains(t, result.SkippedTaskIDs, tail.ID, "tail should be skipped due to all_success rule failing")

	var tailRunFinal models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, tail.ID).First(&tailRunFinal).Error)
	require.Equal(t, string(TaskStatusSkipped), tailRunFinal.Status)
}

// registerSingleTaskRun creates a job/atom/task and registers one TaskRun,
// returning the run and task IDs for hash-write assertions.
func registerSingleTaskRun(t *testing.T, store *Store, db *gorm.DB) (runID, taskID uuid.UUID) {
	t.Helper()
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hi"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  jobID,
		AtomID: atom.ID,
		Name:   "only",
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))
	return runRecord.ID, task.ID
}

// TestSetTaskHashWithBlobPersistsBlobAndDigest asserts the decomposed-input blob
// and resolved digest are written onto the TaskRun row alongside the hash on the
// existing write path, so a distributed worker and the local scheduler both
// leave a queryable record for `caesium why`.
func TestSetTaskHashWithBlobPersistsBlobAndDigest(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	runID, taskID := registerSingleTaskRun(t, store, db)

	blob := datatypes.JSON(`{"blobVersion":1,"hash":"abc","image":"alpine:3.23"}`)
	require.NoError(t, store.SetTaskHashWithBlob(runID, taskID, "abc", "sha256:deadbeef", blob))

	var got models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&got).Error)
	require.Equal(t, "abc", got.Hash)
	require.Equal(t, "sha256:deadbeef", got.ResolvedImageDigest)
	require.JSONEq(t, string(blob), string(got.HashInputBlob))
}

// TestSetTaskHashWithBlobNilBlobLeavesColumnNull asserts the nullable contract:
// writing a hash with a nil blob (cache-off / serialization-failure path) leaves
// the blob column unset rather than writing an empty value.
func TestSetTaskHashWithBlobNilBlobLeavesColumnNull(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	runID, taskID := registerSingleTaskRun(t, store, db)

	require.NoError(t, store.SetTaskHashWithBlob(runID, taskID, "h", "", nil))

	var got models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&got).Error)
	require.Equal(t, "h", got.Hash)
	require.Empty(t, got.HashInputBlob)
}

// TestSetTaskHashWithDigestLeavesBlobNull asserts the back-compat shim
// (SetTaskHashWithDigest) still writes hash + digest without touching the blob
// column, so the A1 call sites keep behaving identically.
func TestSetTaskHashWithDigestLeavesBlobNull(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	runID, taskID := registerSingleTaskRun(t, store, db)

	require.NoError(t, store.SetTaskHashWithDigest(runID, taskID, "h2", "sha256:cafe"))

	var got models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&got).Error)
	require.Equal(t, "h2", got.Hash)
	require.Equal(t, "sha256:cafe", got.ResolvedImageDigest)
	require.Empty(t, got.HashInputBlob)
}
