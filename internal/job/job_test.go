package job

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	asvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRunLocalExecutesParallelBranches(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskA := uuid.New()
	taskB := uuid.New()
	taskC := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskA, JobID: jobID, AtomID: uuid.New()},
		{ID: taskB, JobID: jobID, AtomID: uuid.New()},
		{ID: taskC, JobID: jobID, AtomID: uuid.New()},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskA, ToTaskID: taskB},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.runDurationByName[taskA.String()] = 70 * time.Millisecond
	engine.runDurationByName[taskB.String()] = 20 * time.Millisecond
	engine.runDurationByName[taskC.String()] = 20 * time.Millisecond

	withTestDeps(t, store, env.Environment{
		MaxParallelTasks:  2,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
		TaskTimeout:       0,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}).Run(context.Background())
	require.NoError(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskA])
	require.Equal(t, run.TaskStatusSucceeded, status[taskB])
	require.Equal(t, run.TaskStatusSucceeded, status[taskC])
	require.GreaterOrEqual(t, engine.maxConcurrent(), 2)
}

func TestRunLocalContinuePolicySkipsFailedDescendants(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskFailed := uuid.New()
	taskSkipped := uuid.New()
	taskIndependent := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskFailed, JobID: jobID, AtomID: uuid.New()},
		{ID: taskSkipped, JobID: jobID, AtomID: uuid.New()},
		{ID: taskIndependent, JobID: jobID, AtomID: uuid.New()},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		taskSvc.tasks[0].AtomID: fakeModelAtom(taskSvc.tasks[0].AtomID),
		taskSvc.tasks[1].AtomID: fakeModelAtom(taskSvc.tasks[1].AtomID),
		taskSvc.tasks[2].AtomID: fakeModelAtom(taskSvc.tasks[2].AtomID),
	}}
	edgeSvc := &fakeTaskEdgeService{edges: models.TaskEdges{
		{ID: uuid.New(), JobID: jobID, FromTaskID: taskFailed, ToTaskID: taskSkipped},
	}}
	persistGraph(t, db, taskSvc.tasks, edgeSvc.edges)

	engine.createErrByName[taskFailed.String()] = errors.New("create failed")
	engine.runDurationByName[taskIndependent.String()] = 25 * time.Millisecond

	withTestDeps(t, store, env.Environment{
		MaxParallelTasks:  2,
		TaskFailurePolicy: taskFailurePolicyContinue,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, edgeSvc, engine)

	err := New(&models.Job{ID: jobID}).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "create failed")

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskFailed])
	require.Equal(t, run.TaskStatusSkipped, status[taskSkipped])
	require.Equal(t, run.TaskStatusSucceeded, status[taskIndependent])

	runTask := taskRunByID(snapshot, taskSkipped)
	require.NotNil(t, runTask)
	require.Contains(t, runTask.Error, taskFailed.String())
}

func TestRunLocalTaskTimeoutFailsTaskAndStopsAtom(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: taskID, JobID: jobID, AtomID: atomID},
	}}
	persistGraph(t, db, taskSvc.tasks, nil)
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtom(atomID),
	}}

	engine.runDurationByName[taskID.String()] = 10 * time.Second

	withTestDeps(t, store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
		TaskTimeout:       40 * time.Millisecond,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	err := New(&models.Job{ID: jobID}).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskID])
	require.True(t, engine.wasForceStopped(taskID.String()))
}

func withTestDeps(
	t *testing.T,
	store *run.Store,
	vars env.Environment,
	taskSvc task.Task,
	atomSvc asvc.Atom,
	edgeSvc taskedge.TaskEdge,
	engine atom.Engine,
) {
	t.Helper()

	origStoreFactory := runStoreFactory
	origEnvVariables := envVariables
	origTaskServiceFactory := taskServiceFactory
	origAtomServiceFactory := atomServiceFactory
	origTaskEdgeServiceFactory := taskEdgeServiceFactory
	origDispatchRunCallbacks := dispatchRunCallbacks
	origNewDockerEngine := newDockerEngine
	origAtomPollInterval := atomPollInterval

	runStoreFactory = func() *run.Store { return store }
	envVariables = func() env.Environment { return vars }
	taskServiceFactory = func(context.Context) task.Task { return taskSvc }
	atomServiceFactory = func(context.Context) asvc.Atom { return atomSvc }
	taskEdgeServiceFactory = func(context.Context) taskedge.TaskEdge { return edgeSvc }
	dispatchRunCallbacks = func(context.Context, uuid.UUID, uuid.UUID, error) error { return nil }
	newDockerEngine = func(context.Context) atom.Engine { return engine }
	atomPollInterval = 5 * time.Millisecond

	t.Cleanup(func() {
		runStoreFactory = origStoreFactory
		envVariables = origEnvVariables
		taskServiceFactory = origTaskServiceFactory
		atomServiceFactory = origAtomServiceFactory
		taskEdgeServiceFactory = origTaskEdgeServiceFactory
		dispatchRunCallbacks = origDispatchRunCallbacks
		newDockerEngine = origNewDockerEngine
		atomPollInterval = origAtomPollInterval
	})
}

func latestRunSnapshot(t *testing.T, store *run.Store, jobID uuid.UUID) *run.JobRun {
	t.Helper()

	var model models.JobRun
	err := store.DB().
		Where("job_id = ?", jobID).
		Order("created_at DESC").
		First(&model).Error
	require.NoError(t, err)

	snapshot, err := store.Get(model.ID)
	require.NoError(t, err)
	return snapshot
}

func taskStatusByID(snapshot *run.JobRun) map[uuid.UUID]run.TaskStatus {
	out := make(map[uuid.UUID]run.TaskStatus, len(snapshot.Tasks))
	for _, taskState := range snapshot.Tasks {
		out[taskState.ID] = taskState.Status
	}
	return out
}

func taskRunByID(snapshot *run.JobRun, taskID uuid.UUID) *run.TaskRun {
	for _, taskState := range snapshot.Tasks {
		if taskState.ID == taskID {
			return taskState
		}
	}
	return nil
}

func fakeModelAtom(id uuid.UUID) *models.Atom {
	return &models.Atom{
		ID:      id,
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.20",
		Command: `["echo","ok"]`,
	}
}

func persistGraph(t *testing.T, db *gorm.DB, tasks models.Tasks, edges models.TaskEdges) {
	t.Helper()

	if len(tasks) > 0 {
		require.NoError(t, db.Create(&tasks).Error)
	}
	if len(edges) > 0 {
		require.NoError(t, db.Create(&edges).Error)
	}
}

type fakeTaskService struct {
	tasks models.Tasks
}

func (s *fakeTaskService) WithDatabase(*gorm.DB) task.Task { return s }

func (s *fakeTaskService) List(req *task.ListRequest) (models.Tasks, error) {
	if req != nil && req.JobID != "" {
		filtered := make(models.Tasks, 0, len(s.tasks))
		for _, taskModel := range s.tasks {
			if taskModel.JobID.String() == req.JobID {
				filtered = append(filtered, taskModel)
			}
		}
		return filtered, nil
	}
	return append(models.Tasks(nil), s.tasks...), nil
}

func (s *fakeTaskService) Get(id uuid.UUID) (*models.Task, error) {
	for _, taskModel := range s.tasks {
		if taskModel.ID == id {
			return taskModel, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *fakeTaskService) Create(*task.CreateRequest) (*models.Task, error) {
	return nil, errors.New("not implemented")
}

func (s *fakeTaskService) Delete(uuid.UUID) error { return nil }

type fakeAtomService struct {
	atoms map[uuid.UUID]*models.Atom
}

func (s *fakeAtomService) WithDatabase(*gorm.DB) asvc.Atom { return s }

func (s *fakeAtomService) List(*asvc.ListRequest) (models.Atoms, error) {
	out := make(models.Atoms, 0, len(s.atoms))
	for _, atomModel := range s.atoms {
		out = append(out, atomModel)
	}
	return out, nil
}

func (s *fakeAtomService) Get(id uuid.UUID) (*models.Atom, error) {
	atomModel, ok := s.atoms[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return atomModel, nil
}

func (s *fakeAtomService) Create(*asvc.CreateRequest) (*models.Atom, error) {
	return nil, errors.New("not implemented")
}

func (s *fakeAtomService) Delete(uuid.UUID) error { return nil }

type fakeTaskEdgeService struct {
	edges models.TaskEdges
}

func (s *fakeTaskEdgeService) WithDatabase(*gorm.DB) taskedge.TaskEdge { return s }

func (s *fakeTaskEdgeService) List(req *taskedge.ListRequest) (models.TaskEdges, error) {
	if req != nil && req.JobID != "" {
		filtered := make(models.TaskEdges, 0, len(s.edges))
		for _, edge := range s.edges {
			if edge.JobID.String() == req.JobID {
				filtered = append(filtered, edge)
			}
		}
		return filtered, nil
	}
	return append(models.TaskEdges(nil), s.edges...), nil
}

func (s *fakeTaskEdgeService) Create(*taskedge.CreateRequest) (*models.TaskEdge, error) {
	return nil, errors.New("not implemented")
}

type fakeEngine struct {
	mu sync.Mutex

	createErrByName   map[string]error
	runDurationByName map[string]time.Duration
	resultByName      map[string]atom.Result

	atoms map[string]*fakeEngineAtomState

	inFlight      int
	maxInFlight   int
	stopForceByID map[string]bool
}

type fakeEngineAtomState struct {
	id       string
	name     string
	result   atom.Result
	duration time.Duration

	createdAt time.Time
	startedAt time.Time
	stoppedAt time.Time

	active bool
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		createErrByName:   map[string]error{},
		runDurationByName: map[string]time.Duration{},
		resultByName:      map[string]atom.Result{},
		atoms:             map[string]*fakeEngineAtomState{},
		stopForceByID:     map[string]bool{},
	}
}

func (e *fakeEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.atoms[req.ID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}

	if state.stoppedAt.IsZero() && state.duration >= 0 && time.Since(state.startedAt) >= state.duration {
		state.stoppedAt = time.Now().UTC()
	}

	return state.snapshot(), nil
}

func (e *fakeEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) {
	return nil, nil
}

func (e *fakeEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err, ok := e.createErrByName[req.Name]; ok {
		return nil, err
	}

	duration := 10 * time.Millisecond
	if configured, ok := e.runDurationByName[req.Name]; ok {
		duration = configured
	}

	result := atom.Success
	if configured, ok := e.resultByName[req.Name]; ok {
		result = configured
	}

	now := time.Now().UTC()
	state := &fakeEngineAtomState{
		id:        req.Name,
		name:      req.Name,
		result:    result,
		duration:  duration,
		createdAt: now,
		startedAt: now,
		active:    true,
	}
	e.atoms[state.id] = state
	e.inFlight++
	if e.inFlight > e.maxInFlight {
		e.maxInFlight = e.inFlight
	}

	return state.snapshot(), nil
}

func (e *fakeEngine) Stop(req *atom.EngineStopRequest) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.atoms[req.ID]
	if !ok {
		return nil
	}

	if req.Force {
		e.stopForceByID[req.ID] = true
	}

	if state.stoppedAt.IsZero() {
		state.stoppedAt = time.Now().UTC()
	}
	if state.active {
		state.active = false
		if e.inFlight > 0 {
			e.inFlight--
		}
	}

	return nil
}

func (e *fakeEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (e *fakeEngine) maxConcurrent() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxInFlight
}

func (e *fakeEngine) wasForceStopped(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopForceByID[id]
}

type fakeAtom struct {
	id        string
	engine    atom.Engine
	state     atom.State
	result    atom.Result
	createdAt time.Time
	startedAt time.Time
	stoppedAt time.Time
}

func (a *fakeAtom) ID() string           { return a.id }
func (a *fakeAtom) State() atom.State    { return a.state }
func (a *fakeAtom) Result() atom.Result  { return a.result }
func (a *fakeAtom) CreatedAt() time.Time { return a.createdAt }
func (a *fakeAtom) StartedAt() time.Time { return a.startedAt }
func (a *fakeAtom) StoppedAt() time.Time { return a.stoppedAt }
func (a *fakeAtom) Engine() atom.Engine  { return a.engine }

func (s *fakeEngineAtomState) snapshot() atom.Atom {
	state := atom.Running
	if !s.stoppedAt.IsZero() {
		state = atom.Stopped
	}

	return &fakeAtom{
		id:        s.id,
		state:     state,
		result:    s.result,
		createdAt: s.createdAt,
		startedAt: s.startedAt,
		stoppedAt: s.stoppedAt,
	}
}

var _ task.Task = (*fakeTaskService)(nil)
var _ asvc.Atom = (*fakeAtomService)(nil)
var _ taskedge.TaskEdge = (*fakeTaskEdgeService)(nil)
var _ atom.Engine = (*fakeEngine)(nil)
var _ atom.Atom = (*fakeAtom)(nil)
