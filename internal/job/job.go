package job

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	asvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/worker"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Job
type Job interface {
	Run(ctx context.Context) error
}

type job struct {
	id                     uuid.UUID
	triggerID              *uuid.UUID
	runStoreFactory        func() *run.Store
	envVariables           func() env.Environment
	taskServiceFactory     func(context.Context) task.Task
	atomServiceFactory     func(context.Context) asvc.Atom
	taskEdgeServiceFactory func(context.Context) taskedge.TaskEdge
	dispatchRunCallbacks   func(context.Context, uuid.UUID, uuid.UUID, error) error
	newDockerEngine        func(context.Context) atom.Engine
	newKubernetesEngine    func(context.Context) atom.Engine
	newPodmanEngine        func(context.Context) atom.Engine
	atomPollInterval       time.Duration
}

type jobOption func(*job)

func New(m *models.Job, opts ...jobOption) Job {
	j := &job{
		id:                     m.ID,
		triggerID:              &m.TriggerID,
		runStoreFactory:        run.Default,
		envVariables:           env.Variables,
		taskServiceFactory:     task.Service,
		atomServiceFactory:     asvc.Service,
		taskEdgeServiceFactory: taskedge.Service,
		dispatchRunCallbacks: func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error {
			return callback.Default().Dispatch(ctx, jobID, runID, runErr)
		},
		newDockerEngine:     func(ctx context.Context) atom.Engine { return docker.NewEngine(ctx) },
		newKubernetesEngine: func(ctx context.Context) atom.Engine { return kubernetes.NewEngine(ctx) },
		newPodmanEngine:     func(ctx context.Context) atom.Engine { return podman.NewEngine(ctx) },
		atomPollInterval:    env.Variables().AtomPollInterval,
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(j)
	}

	return j
}

type atomRunner struct {
	image   string
	command []string
	spec    container.Spec
	engine  atom.Engine
}

const (
	executionModeLocal       = "local"
	executionModeDistributed = "distributed"
)

func WithTriggerID(id *uuid.UUID) jobOption {
	return func(j *job) {
		j.triggerID = id
	}
}

func withRunStoreFactory(factory func() *run.Store) jobOption {
	return func(j *job) {
		if factory != nil {
			j.runStoreFactory = factory
		}
	}
}

func withEnvVariables(variables func() env.Environment) jobOption {
	return func(j *job) {
		if variables != nil {
			j.envVariables = variables
		}
	}
}

func withTaskServiceFactory(factory func(context.Context) task.Task) jobOption {
	return func(j *job) {
		if factory != nil {
			j.taskServiceFactory = factory
		}
	}
}

func withAtomServiceFactory(factory func(context.Context) asvc.Atom) jobOption {
	return func(j *job) {
		if factory != nil {
			j.atomServiceFactory = factory
		}
	}
}

func withTaskEdgeServiceFactory(factory func(context.Context) taskedge.TaskEdge) jobOption {
	return func(j *job) {
		if factory != nil {
			j.taskEdgeServiceFactory = factory
		}
	}
}

func withDispatchRunCallbacks(dispatch func(context.Context, uuid.UUID, uuid.UUID, error) error) jobOption {
	return func(j *job) {
		if dispatch != nil {
			j.dispatchRunCallbacks = dispatch
		}
	}
}

func withDockerEngineFactory(factory func(context.Context) atom.Engine) jobOption {
	return func(j *job) {
		if factory != nil {
			j.newDockerEngine = factory
		}
	}
}

func withKubernetesEngineFactory(factory func(context.Context) atom.Engine) jobOption {
	return func(j *job) {
		if factory != nil {
			j.newKubernetesEngine = factory
		}
	}
}

func withPodmanEngineFactory(factory func(context.Context) atom.Engine) jobOption {
	return func(j *job) {
		if factory != nil {
			j.newPodmanEngine = factory
		}
	}
}

func withAtomPollInterval(interval time.Duration) jobOption {
	return func(j *job) {
		if interval > 0 {
			j.atomPollInterval = interval
		}
	}
}

func (j *job) Run(ctx context.Context) error {
	store := j.runStoreFactory()
	vars := j.envVariables()
	executionMode := normalizeExecutionMode(vars.ExecutionMode)
	failurePolicy := normalizeTaskFailurePolicy(vars.TaskFailurePolicy)
	continueOnFailure := failurePolicy == taskFailurePolicyContinue
	taskTimeout := vars.TaskTimeout

	resolveRun := func() (*run.JobRun, error) {
		if id, ok := run.FromContext(ctx); ok {
			existing, err := store.Get(id)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return store.Start(j.id)
				}
				return nil, err
			}
			return existing, nil
		}

		running, err := store.FindRunning(j.id)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}

		if running != nil {
			if executionMode == executionModeDistributed {
				return store.Get(running.ID)
			}

			if err := store.ResetInFlightTasks(running.ID); err != nil {
				return nil, err
			}
			return store.Get(running.ID)
		}

		return store.Start(j.id, j.triggerID)
	}

	snapshot, err := resolveRun()
	if err != nil {
		return err
	}

	runID := snapshot.ID
	ctx = run.WithContext(ctx, runID)

	var runErr error
	defer func() {
		if err := store.Complete(runID, runErr); err != nil {
			log.Error("run completion persistence failure", "run_id", runID, "error", err)
		}
		dispatchCtx := context.WithoutCancel(ctx)
		if err := j.dispatchRunCallbacks(dispatchCtx, j.id, runID, runErr); err != nil {
			log.Error("callback dispatch failure", "job_id", j.id, "run_id", runID, "error", err)
		}
	}()

	tasks, err := j.taskServiceFactory(ctx).List(&task.ListRequest{
		JobID:   j.id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		runErr = err
		return err
	}

	if len(tasks) == 0 {
		runErr = fmt.Errorf("job %s has no tasks", j.id)
		return runErr
	}

	log.Info("running job tasks", "job_id", j.id, "count", len(tasks))

	svc := j.atomServiceFactory(ctx)

	taskOrder := make(map[uuid.UUID]int, len(tasks))
	atomsByTask := make(map[uuid.UUID]*models.Atom, len(tasks))
	runners := make(map[uuid.UUID]*atomRunner, len(tasks))

	for idx, t := range tasks {
		taskOrder[t.ID] = idx

		modelAtom, err := svc.Get(t.AtomID)
		if err != nil {
			runErr = err
			return err
		}

		atomsByTask[t.ID] = modelAtom

		if executionMode == executionModeDistributed {
			continue
		}

		runner := &atomRunner{
			image:   modelAtom.Image,
			command: modelAtom.Cmd(),
			spec:    modelAtom.ContainerSpec(),
		}

		log.Info("evaluating task atom", "job_id", j.id, "task_id", t.ID, "engine", modelAtom.Engine, "atom_id", modelAtom.ID)

		switch modelAtom.Engine {
		case models.AtomEngineDocker:
			runner.engine = j.newDockerEngine(ctx)
		case models.AtomEngineKubernetes:
			runner.engine = j.newKubernetesEngine(ctx)
		case models.AtomEnginePodman:
			runner.engine = j.newPodmanEngine(ctx)
		default:
			runErr = fmt.Errorf("unable to run atom with engine: %v", modelAtom.Engine)
			return runErr
		}

		runners[t.ID] = runner
	}

	edges, err := j.taskEdgeServiceFactory(ctx).List(&taskedge.ListRequest{
		JobID:   j.id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		runErr = err
		return err
	}

	adjacency := make(map[uuid.UUID][]uuid.UUID, len(tasks))
	indegree := make(map[uuid.UUID]int, len(tasks))
	edgeSet := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(tasks))

	for _, t := range tasks {
		adjacency[t.ID] = []uuid.UUID{}
		indegree[t.ID] = 0
	}

	addEdge := func(from, to uuid.UUID) {
		if _, ok := adjacency[from]; !ok {
			return
		}
		if _, ok := adjacency[to]; !ok {
			return
		}
		targets, ok := edgeSet[from]
		if !ok {
			targets = make(map[uuid.UUID]struct{})
			edgeSet[from] = targets
		}
		if _, exists := targets[to]; exists {
			return
		}
		adjacency[from] = append(adjacency[from], to)
		indegree[to]++
		targets[to] = struct{}{}
	}

	addedEdges := 0
	for _, edge := range edges {
		addEdge(edge.FromTaskID, edge.ToTaskID)
		addedEdges++
	}

	if addedEdges == 0 {
		for _, t := range tasks {
			if t.NextID == nil {
				continue
			}
			addEdge(t.ID, *t.NextID)
			addedEdges++
		}

		if addedEdges == 0 && len(tasks) > 1 {
			for idx := 0; idx < len(tasks)-1; idx++ {
				addEdge(tasks[idx].ID, tasks[idx+1].ID)
			}
		}
	}

	for _, t := range tasks {
		atomModel := atomsByTask[t.ID]
		if err := store.RegisterTask(runID, t, atomModel, indegree[t.ID]); err != nil {
			runErr = err
			return err
		}
	}

	currentRun, err := store.Get(runID)
	if err != nil {
		runErr = err
		return err
	}

	if executionMode == executionModeDistributed {
		runErr = waitForRunCompletion(ctx, store, runID, len(tasks), continueOnFailure, vars.WorkerPollInterval)
		return runErr
	}

	queue := make([]uuid.UUID, 0, len(tasks))
	inQueue := make(map[uuid.UUID]bool, len(tasks))
	processed := make(map[uuid.UUID]bool, len(tasks))
	terminalTasks := 0

	for _, taskState := range currentRun.Tasks {
		indegree[taskState.ID] = taskState.OutstandingPredecessors
		switch taskState.Status {
		case run.TaskStatusSucceeded:
			processed[taskState.ID] = true
			terminalTasks++
		case run.TaskStatusSkipped:
			processed[taskState.ID] = true
			terminalTasks++
		case run.TaskStatusFailed:
			runErr = fmt.Errorf("task %s previously failed", taskState.ID)
			return runErr
		}
	}

	push := func(id uuid.UUID) {
		if processed[id] || inQueue[id] {
			return
		}
		queue = append(queue, id)
		inQueue[id] = true
		sort.Slice(queue, func(i, j int) bool {
			return taskOrder[queue[i]] < taskOrder[queue[j]]
		})
	}

	for _, taskState := range currentRun.Tasks {
		if processed[taskState.ID] {
			continue
		}
		if indegree[taskState.ID] == 0 {
			push(taskState.ID)
		}
	}

	if len(queue) == 0 && terminalTasks < len(tasks) {
		runErr = fmt.Errorf("job %s has no runnable tasks (verify DAG configuration)", j.id)
		return runErr
	}

	type taskResult struct {
		id  uuid.UUID
		err error
	}

	runTask := func(taskID uuid.UUID) error {
		runner := runners[taskID]
		if runner == nil {
			return fmt.Errorf("missing runner for task %s", taskID)
		}

		taskCtx := ctx
		cancel := func() {}
		if taskTimeout > 0 {
			taskCtx, cancel = context.WithTimeout(ctx, taskTimeout)
		}
		defer cancel()

		log.Info("running atom", "job_id", j.id, "task_id", taskID, "image", runner.image, "cmd", runner.command)

		a, err := runner.engine.Create(&atom.EngineCreateRequest{
			Name:    fmt.Sprintf("%s-%s", runID.String()[:8], taskID.String()),
			Image:   runner.image,
			Command: runner.command,
			Spec:    runner.spec,
		})
		if err != nil {
			if persistErr := store.FailTask(runID, taskID, err); persistErr != nil {
				log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
			}
			return err
		}

		if err := store.StartTask(runID, taskID, a.ID()); err != nil {
			return err
		}

		monitor := func() error {
			ticker := time.NewTicker(j.atomPollInterval)
			defer ticker.Stop()

			for {
				select {
				case <-taskCtx.Done():
					if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
						if stopErr := runner.engine.Stop(&atom.EngineStopRequest{
							ID:    a.ID(),
							Force: true,
						}); stopErr != nil {
							return fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskID, taskTimeout, a.ID(), stopErr)
						}
						return fmt.Errorf("task %s timed out after %s", taskID, taskTimeout)
					}
					return taskCtx.Err()
				case <-ticker.C:
					var fetchErr error
					a, fetchErr = runner.engine.Get(&atom.EngineGetRequest{ID: a.ID()})
					if fetchErr != nil {
						return fetchErr
					}

					if !a.StoppedAt().IsZero() {
						log.Info("atom finished", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "result", a.Result())

						return runner.engine.Stop(&atom.EngineStopRequest{
							ID:    a.ID(),
							Force: true,
						})
					}

					log.Info("atom running", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "state", a.State())
				}
			}
		}

		if err = monitor(); err != nil {
			if persistErr := store.FailTask(runID, taskID, err); persistErr != nil {
				log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
			}
			return err
		}

		atomResult := a.Result()
		if atomResult != atom.Success {
			err := fmt.Errorf("atom failed with result: %s", atomResult)
			if persistErr := store.FailTask(runID, taskID, err); persistErr != nil {
				log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
			}
			return err
		}

		if err := store.CompleteTask(runID, taskID, string(atomResult)); err != nil {
			return err
		}

		return nil
	}

	maxParallel := vars.MaxParallelTasks
	if maxParallel <= 0 {
		maxParallel = runtime.NumCPU()
	}

	taskPool := worker.NewPool(maxParallel)

	results := make(chan taskResult)
	active := 0
	halt := false

	dispatch := func(taskID uuid.UUID) error {
		active++
		if err := taskPool.Submit(ctx, func() {
			results <- taskResult{id: taskID, err: runTask(taskID)}
		}); err != nil {
			active--
			return err
		}
		return nil
	}

	for len(queue) > 0 || active > 0 {
		for !halt && active < maxParallel && len(queue) > 0 {
			taskID := queue[0]
			queue = queue[1:]
			delete(inQueue, taskID)

			if processed[taskID] {
				continue
			}

			if err := dispatch(taskID); err != nil {
				if runErr == nil {
					runErr = err
				}
				halt = true
				queue = queue[:0]
				break
			}
		}

		if active == 0 {
			break
		}

		result := <-results
		active--

		if processed[result.id] {
			continue
		}

		processed[result.id] = true
		terminalTasks++

		if result.err != nil {
			if runErr == nil {
				runErr = result.err
			}
			if !continueOnFailure {
				halt = true
				queue = queue[:0]
				continue
			}

			descendants := collectDescendants(adjacency, result.id)
			skipReason := fmt.Sprintf("skipped due to failed dependency task %s", result.id)
			for _, id := range descendants {
				if processed[id] {
					continue
				}
				if err := store.SkipTask(runID, id, skipReason); err != nil {
					log.Error("failed to persist task skip", "run_id", runID, "task_id", id, "error", err)
					if runErr == nil {
						runErr = err
					}
					halt = true
					queue = queue[:0]
					break
				}
				processed[id] = true
				terminalTasks++
				delete(inQueue, id)
			}
			continue
		}

		if !halt {
			for _, successor := range adjacency[result.id] {
				if _, ok := indegree[successor]; !ok {
					continue
				}
				if indegree[successor] > 0 {
					indegree[successor]--
				}
				if indegree[successor] == 0 {
					push(successor)
				}
			}
		}
	}

	if terminalTasks != len(tasks) {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("job %s reached terminal state for %d of %d tasks; remaining tasks may be waiting on unresolved dependencies", j.id, terminalTasks, len(tasks))
	}

	if runErr != nil {
		return runErr
	}

	return nil
}

func normalizeExecutionMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case executionModeDistributed:
		return executionModeDistributed
	default:
		return executionModeLocal
	}
}

func waitForRunCompletion(ctx context.Context, store *run.Store, runID uuid.UUID, taskCount int, continueOnFailure bool, pollInterval time.Duration) error {
	if taskCount <= 0 {
		return nil
	}

	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			snapshot, err := store.Get(runID)
			if err != nil {
				return err
			}

			failed := 0
			running := 0
			succeeded := 0
			skipped := 0

			for _, taskState := range snapshot.Tasks {
				switch taskState.Status {
				case run.TaskStatusFailed:
					failed++
				case run.TaskStatusRunning:
					running++
				case run.TaskStatusSucceeded:
					succeeded++
				case run.TaskStatusSkipped:
					skipped++
				}
			}

			terminal := failed + succeeded + skipped
			if terminal == taskCount {
				if failed > 0 {
					return fmt.Errorf("run %s completed with %d failed task(s)", runID, failed)
				}
				return nil
			}

			if failed > 0 && running == 0 {
				if continueOnFailure {
					return fmt.Errorf("run %s has %d failed task(s) and %d unresolved pending task(s)", runID, failed, taskCount-terminal)
				}
				return fmt.Errorf("run %s halted after %d failed task(s)", runID, failed)
			}
		}
	}
}
