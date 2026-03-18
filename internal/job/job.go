package job

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
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
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/worker"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
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
	maxParallelTasks       int
	taskTimeout            time.Duration
	runTimeout             time.Duration
	alias                  string
	params                 map[string]string
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
		maxParallelTasks:       m.MaxParallelTasks,
		taskTimeout:            m.TaskTimeout,
		runTimeout:             m.RunTimeout,
		alias:                  m.Alias,
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

// WithParams is an exported option that attaches run parameters to the job.
// Parameters are injected into each task's environment as
// CAESIUM_PARAM_<KEY>=<VALUE> (KEY uppercased).
func WithParams(params map[string]string) jobOption {
	return func(j *job) {
		j.params = params
	}
}

// buildParamEnv returns a map of environment variables derived from params.
// It also injects CAESIUM_RUN_ID and CAESIUM_JOB_ALIAS.
func buildParamEnv(runID uuid.UUID, jobAlias string, params map[string]string) map[string]string {
	env := make(map[string]string, len(params)+2)
	env["CAESIUM_RUN_ID"] = runID.String()
	env["CAESIUM_JOB_ALIAS"] = jobAlias
	for k, v := range params {
		env["CAESIUM_PARAM_"+strings.ToUpper(k)] = v
	}
	return env
}

func (j *job) Run(ctx context.Context) error {
	store := j.runStoreFactory()
	vars := j.envVariables()

	executionMode := normalizeExecutionMode(vars.ExecutionMode)
	failurePolicy := normalizeTaskFailurePolicy(vars.TaskFailurePolicy)
	continueOnFailure := failurePolicy == taskFailurePolicyContinue

	// Use job overrides if specified, otherwise fall back to environment variables
	taskTimeout := j.taskTimeout
	if taskTimeout == 0 {
		taskTimeout = vars.TaskTimeout
	}

	// Apply run-level timeout if configured.
	runTimeout := j.runTimeout
	if runTimeout > 0 {
		var runCancel context.CancelFunc
		ctx, runCancel = context.WithTimeout(ctx, runTimeout)
		defer runCancel()
	}

	maxParallel := j.maxParallelTasks
	if maxParallel <= 0 {
		maxParallel = vars.MaxParallelTasks
	}
	if maxParallel <= 0 {
		maxParallel = runtime.NumCPU()
	}

	resolveRun := func() (*run.JobRun, error) {
		if id, ok := run.FromContext(ctx); ok {
			existing, err := store.Get(id)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return store.Start(j.id, j.triggerID, j.params)
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

		return store.Start(j.id, j.triggerID, j.params)
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
	tasksByID := make(map[uuid.UUID]*models.Task, len(tasks))
	runners := make(map[uuid.UUID]*atomRunner, len(tasks))
	triggerRuleByTask := make(map[uuid.UUID]string, len(tasks))

	for idx, t := range tasks {
		taskOrder[t.ID] = idx
		tasksByID[t.ID] = t

		rule := t.TriggerRule
		if rule == "" {
			rule = jobdefschema.TriggerRuleAllSuccess
		}
		triggerRuleByTask[t.ID] = rule

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
	predecessors := make(map[uuid.UUID][]uuid.UUID, len(tasks))
	indegree := make(map[uuid.UUID]int, len(tasks))
	edgeSet := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(tasks))

	for _, t := range tasks {
		adjacency[t.ID] = []uuid.UUID{}
		predecessors[t.ID] = []uuid.UUID{}
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
		predecessors[to] = append(predecessors[to], from)
		indegree[to]++
		targets[to] = struct{}{}
	}

	addedEdges := 0
	for _, edge := range edges {
		addEdge(edge.FromTaskID, edge.ToTaskID)
		addedEdges++
	}

	if addedEdges == 0 && len(tasks) > 1 {
		// No explicit edges; fall back to sequential creation order.
		for idx := 0; idx < len(tasks)-1; idx++ {
			addEdge(tasks[idx].ID, tasks[idx+1].ID)
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
	taskOutcomes := make(map[uuid.UUID]run.TaskStatus, len(tasks))
	taskOutputs := make(map[uuid.UUID]map[string]string, len(tasks))
	terminalTasks := 0

	for _, taskState := range currentRun.Tasks {
		indegree[taskState.ID] = taskState.OutstandingPredecessors
		switch taskState.Status {
		case run.TaskStatusSucceeded:
			processed[taskState.ID] = true
			taskOutcomes[taskState.ID] = run.TaskStatusSucceeded
			terminalTasks++
		case run.TaskStatusSkipped:
			processed[taskState.ID] = true
			taskOutcomes[taskState.ID] = run.TaskStatusSkipped
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
		slices.SortFunc(queue, func(a, b uuid.UUID) int {
			return cmp.Compare(taskOrder[a], taskOrder[b])
		})
	}

	propagateSkipped := func(start uuid.UUID) error {
		queue := []uuid.UUID{start}
		seen := map[uuid.UUID]struct{}{start: {}}

		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]

			for _, successor := range adjacency[current] {
				if processed[successor] {
					continue
				}
				if _, ok := indegree[successor]; !ok {
					continue
				}
				if indegree[successor] > 0 {
					indegree[successor]--
				}
				if indegree[successor] != 0 {
					continue
				}

				predStatuses := collectPredecessorStatuses(predecessors[successor], taskOutcomes)
				if satisfiesTriggerRule(triggerRuleByTask[successor], predStatuses) {
					push(successor)
					continue
				}

				skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", triggerRuleByTask[successor])
				if err := store.SkipTask(runID, successor, skipRuleReason); err != nil {
					return err
				}

				taskOutcomes[successor] = run.TaskStatusSkipped
				processed[successor] = true
				terminalTasks++
				delete(inQueue, successor)

				if _, ok := seen[successor]; ok {
					continue
				}
				seen[successor] = struct{}{}
				queue = append(queue, successor)
			}
		}

		return nil
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

	paramEnv := buildParamEnv(snapshot.ID, j.alias, snapshot.Params)

	// executeAtom creates, monitors, and stops a container for one execution attempt.
	// It returns the atom result string, any parsed task outputs, and any error.
	executeAtom := func(taskCtx context.Context, taskID uuid.UUID, attempt int, runner *atomRunner, extraEnv map[string]string) (string, map[string]string, error) {
		atomName := taskID.String()
		if attempt > 1 {
			atomName = fmt.Sprintf("%s-attempt%d", taskID, attempt)
		}

		log.Info("running atom", "job_id", j.id, "task_id", taskID, "image", runner.image, "cmd", runner.command, "attempt", attempt)

		spec := runner.spec
		if len(paramEnv) > 0 || len(extraEnv) > 0 {
			merged := make(map[string]string, len(spec.Env)+len(paramEnv)+len(extraEnv))
			for k, v := range spec.Env {
				merged[k] = v
			}
			for k, v := range paramEnv {
				merged[k] = v
			}
			for k, v := range extraEnv {
				merged[k] = v
			}
			spec.Env = merged
		}

		a, err := runner.engine.Create(&atom.EngineCreateRequest{
			Name:    atomName,
			Image:   runner.image,
			Command: runner.command,
			Spec:    spec,
		})
		if err != nil {
			return "", nil, err
		}

		if err := store.StartTask(runID, taskID, a.ID()); err != nil {
			return "", nil, err
		}

		waitResult := make(chan struct {
			atom atom.Atom
			err  error
		}, 1)
		go func() {
			next, waitErr := runner.engine.Wait(&atom.EngineWaitRequest{ID: a.ID(), Context: taskCtx})
			waitResult <- struct {
				atom atom.Atom
				err  error
			}{atom: next, err: waitErr}
		}()

		select {
		case <-taskCtx.Done():
			if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
				if stopErr := runner.engine.Stop(&atom.EngineStopRequest{
					ID:    a.ID(),
					Force: true,
				}); stopErr != nil {
					return "", nil, fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskID, taskTimeout, a.ID(), stopErr)
				}
				return "", nil, fmt.Errorf("task %s timed out after %s", taskID, taskTimeout)
			}
			return "", nil, taskCtx.Err()
		case result := <-waitResult:
			if result.err != nil {
				return "", nil, result.err
			}
			a = result.atom
			log.Info("atom finished", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "result", a.Result())

			// Capture structured task output from container logs before stopping.
			var taskOutput map[string]string
			logs, logErr := runner.engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
			if logErr == nil {
				parsed, parseErr := pkgtask.ParseOutput(logs)
				if err := logs.Close(); err != nil {
					log.Warn("failed to close log stream", "task_id", taskID, "error", err)
				}
				if parseErr != nil {
					log.Warn("failed to parse task output", "task_id", taskID, "error", parseErr)
				} else {
					taskOutput = parsed
				}
			}

			stopErr := runner.engine.Stop(&atom.EngineStopRequest{
				ID:    a.ID(),
				Force: true,
			})
			return string(a.Result()), taskOutput, stopErr
		}
	}

	runTask := func(taskID uuid.UUID) error {
		runner := runners[taskID]
		if runner == nil {
			return fmt.Errorf("missing runner for task %s", taskID)
		}

		// Build predecessor output env vars for this task.
		predOutputs := make(map[string]map[string]string)
		for _, predID := range predecessors[taskID] {
			if outputs, ok := taskOutputs[predID]; ok && len(outputs) > 0 {
				stepName := ""
				if t := tasksByID[predID]; t != nil {
					stepName = t.Name
				}
				if stepName == "" {
					stepName = predID.String()
				}
				predOutputs[stepName] = outputs
			}
		}
		outputEnv := pkgtask.BuildOutputEnv(predOutputs)

		taskModel := tasksByID[taskID]
		maxAttempts := 1
		if taskModel != nil && taskModel.Retries > 0 {
			maxAttempts = taskModel.Retries + 1
		}

		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			taskCtx := ctx
			cancel := func() {}
			if taskTimeout > 0 {
				taskCtx, cancel = context.WithTimeout(ctx, taskTimeout)
			}

			result, output, execErr := executeAtom(taskCtx, taskID, attempt, runner, outputEnv)
			cancel()

			if execErr == nil {
				if err := store.CompleteTask(runID, taskID, result, output); err != nil {
					return err
				}
				if len(output) > 0 {
					taskOutputs[taskID] = output
				}
				if !run.IsSuccessfulTaskResult(result) {
					return fmt.Errorf("task %s failed with result %q", taskID, result)
				}
				return nil
			}
			lastErr = execErr

			// No more attempts — mark as permanently failed.
			if attempt >= maxAttempts {
				break
			}

			// Compute retry delay.
			delay := computeRetryDelay(taskModel, attempt)

			log.Info("retrying task", "job_id", j.id, "task_id", taskID, "attempt", attempt, "next_attempt", attempt+1, "delay", delay, "error", lastErr)

			metrics.TaskRetriesTotal.WithLabelValues(j.alias, taskID.String(), strconv.Itoa(attempt)).Inc()

			if err := store.RetryTask(runID, taskID, attempt+1); err != nil {
				log.Error("failed to persist task retry state", "run_id", runID, "task_id", taskID, "error", err)
			}

			if delay > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
				}
			}
		}

		if persistErr := store.FailTask(runID, taskID, lastErr); persistErr != nil {
			log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
		}
		return lastErr
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
			taskOutcomes[result.id] = run.TaskStatusFailed
			if runErr == nil {
				runErr = result.err
			}
			if !continueOnFailure {
				halt = true
				queue = queue[:0]
				continue
			}

			// With continueOnFailure: skip only downstream tasks whose trigger
			// rules require all predecessors to succeed. Tasks with all_done,
			// all_failed, or always rules are left to the normal indegree path
			// so they can still run / evaluate their own rule.
			skipReason := fmt.Sprintf("skipped due to failed dependency task %s", result.id)
			skipDescendantsFiltered(
				adjacency, predecessors, triggerRuleByTask,
				result.id, processed, inQueue,
				func(id uuid.UUID) {
					if err := store.SkipTask(runID, id, skipReason); err != nil {
						log.Error("failed to persist task skip", "run_id", runID, "task_id", id, "error", err)
						if runErr == nil {
							runErr = err
						}
						halt = true
						queue = queue[:0]
					}
					taskOutcomes[id] = run.TaskStatusSkipped
					processed[id] = true
					terminalTasks++
					delete(inQueue, id)
					if err == nil && !halt {
						if propErr := propagateSkipped(id); propErr != nil {
							log.Error("failed to propagate skipped task", "run_id", runID, "task_id", id, "error", propErr)
							if runErr == nil {
								runErr = propErr
							}
							halt = true
							queue = queue[:0]
						}
					}
				},
			)

			// Decrement indegree for successors that were NOT skipped (they
			// have a failure-tolerant trigger rule). When their indegree
			// reaches 0, evaluate the rule and push or skip accordingly.
			if !halt {
				for _, successor := range adjacency[result.id] {
					if processed[successor] {
						continue
					}
					if _, ok := indegree[successor]; !ok {
						continue
					}
					if indegree[successor] > 0 {
						indegree[successor]--
					}
					if indegree[successor] == 0 {
						predStatuses := collectPredecessorStatuses(predecessors[successor], taskOutcomes)
						if satisfiesTriggerRule(triggerRuleByTask[successor], predStatuses) {
							push(successor)
						} else {
							skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", triggerRuleByTask[successor])
							if err := store.SkipTask(runID, successor, skipRuleReason); err != nil {
								log.Error("failed to persist trigger rule skip", "run_id", runID, "task_id", successor, "error", err)
								if runErr == nil {
									runErr = err
								}
								halt = true
								queue = queue[:0]
								break
							}
							taskOutcomes[successor] = run.TaskStatusSkipped
							processed[successor] = true
							terminalTasks++
							delete(inQueue, successor)
							if err := propagateSkipped(successor); err != nil {
								log.Error("failed to propagate skipped task", "run_id", runID, "task_id", successor, "error", err)
								if runErr == nil {
									runErr = err
								}
								halt = true
								queue = queue[:0]
								break
							}
						}
					}
				}
			}
			continue
		}

		taskOutcomes[result.id] = run.TaskStatusSucceeded

		if !halt {
			for _, successor := range adjacency[result.id] {
				if _, ok := indegree[successor]; !ok {
					continue
				}
				if indegree[successor] > 0 {
					indegree[successor]--
				}
				if indegree[successor] == 0 {
					predStatuses := collectPredecessorStatuses(predecessors[successor], taskOutcomes)
					if satisfiesTriggerRule(triggerRuleByTask[successor], predStatuses) {
						push(successor)
					} else {
						skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", triggerRuleByTask[successor])
						if err := store.SkipTask(runID, successor, skipRuleReason); err != nil {
							log.Error("failed to persist trigger rule skip", "run_id", runID, "task_id", successor, "error", err)
							if runErr == nil {
								runErr = err
							}
							halt = true
							queue = queue[:0]
							break
						}
						taskOutcomes[successor] = run.TaskStatusSkipped
						processed[successor] = true
						terminalTasks++
						delete(inQueue, successor)
						if err := propagateSkipped(successor); err != nil {
							log.Error("failed to propagate skipped task", "run_id", runID, "task_id", successor, "error", err)
							if runErr == nil {
								runErr = err
							}
							halt = true
							queue = queue[:0]
							break
						}
					}
				}
			}
		}
	}

	// Wrap deadline-exceeded errors with a human-readable message.
	if runTimeout > 0 && runErr != nil && errors.Is(runErr, context.DeadlineExceeded) {
		runErr = fmt.Errorf("run timed out after %s", runTimeout)
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

	var (
		ticker = time.NewTicker(pollInterval)
		ch     <-chan event.Event
	)
	defer ticker.Stop()

	if bus := store.Bus(); bus != nil {
		events, err := bus.Subscribe(ctx, event.Filter{
			RunID: runID,
			Types: []event.Type{event.TypeRunTerminal, event.TypeRunCompleted, event.TypeRunFailed},
		})
		if err == nil {
			ch = events
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-ch:
			if !ok {
				ch = nil
				continue
			}
			if evt.Type == event.TypeRunFailed {
				snapshot, err := store.Get(runID)
				if err != nil {
					return err
				}
				if snapshot.Error != "" {
					return errors.New(snapshot.Error)
				}
				return fmt.Errorf("run %s failed", runID)
			}
			if evt.Type == event.TypeRunCompleted || evt.Type == event.TypeRunTerminal {
				snapshot, err := store.Get(runID)
				if err != nil {
					return err
				}
				if snapshot.Status == run.StatusFailed {
					if snapshot.Error != "" {
						return errors.New(snapshot.Error)
					}
					return fmt.Errorf("run %s failed", runID)
				}
				return nil
			}
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
