package job

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/ratelimit"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/worker"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// runStartReadBackoffs bounds retries for transient dqlite contention (e.g.
// "checkpoint in progress") on the idempotent reads the run-start /
// DAG-materialization path issues. A contention blip on any of these reads
// would otherwise fail the entire run before a single task row is created.
// ~630ms total across 6 retries — deliberately longer than the per-statement
// connection-pool retry because a WAL checkpoint can outlast a single
// statement's budget, and a stalled run start is far worse than a brief wait.
var runStartReadBackoffs = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
	320 * time.Millisecond,
}

const haltedDispatchWaitInterval = 50 * time.Millisecond

type taskResult struct {
	id              uuid.UUID
	err             error
	skippedByBranch []uuid.UUID
}

// ErrLocalQuarantinedReplayUnsupported is returned when a quarantined replay
// reaches the in-process executor, which is not descriptor-aware.
var ErrLocalQuarantinedReplayUnsupported = errors.New("replay requires the descriptor-aware executor")

// retryOnContention runs fn, retrying only on transient dqlite contention.
//
// The global connection-pool retry (pkg/db) covers a contended statement at
// call time, but dqlite can surface a "checkpoint in progress" error during
// row iteration — after QueryContext has already returned cleanly — which
// escapes that layer and would propagate up as a fatal run-start error. The
// run-start reads guarded here are side-effect-free (or abort without
// committing on contention), so re-running the whole call is safe. A cancelled
// context stops the loop and returns the last error.
func retryOnContention(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !dqlite.IsContentionError(err) || attempt >= len(runStartReadBackoffs) {
			return err
		}
		base := runStartReadBackoffs[attempt]
		d := base
		if maxJitter := int64(base / 5); maxJitter > 0 {
			d = base - time.Duration(rand.Int64N(maxJitter+1))
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Return the cancellation, not the dqlite error, so the run's
			// failure reason is a clear cancellation rather than a misleading
			// "checkpoint in progress".
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForHaltedDispatchResult(results <-chan taskResult, wait time.Duration) (taskResult, bool) {
	if wait <= 0 {
		wait = haltedDispatchWaitInterval
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case result := <-results:
		return result, true
	case <-timer.C:
		return taskResult{}, false
	}
}

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
	priority               string
	concurrency            *jobdefschema.Concurrency
	rateLimits             []jobdefschema.RateLimit
	schemaValidation       string
	jobCacheConfig         interface{}
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
	secretResolver         secret.Resolver
}

// JobOption configures a job before execution.
type JobOption func(*job)

func New(m *models.Job, opts ...JobOption) Job {
	j := &job{
		id:                     m.ID,
		triggerID:              &m.TriggerID,
		maxParallelTasks:       m.MaxParallelTasks,
		taskTimeout:            m.TaskTimeout,
		runTimeout:             m.RunTimeout,
		alias:                  m.Alias,
		priority:               m.Priority,
		concurrency:            unmarshalConcurrency(m.Concurrency),
		rateLimits:             unmarshalRateLimits(m.RateLimits),
		schemaValidation:       m.SchemaValidation,
		jobCacheConfig:         unmarshalCacheConfig(m.CacheConfig),
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

func WithTriggerID(id *uuid.UUID) JobOption {
	return func(j *job) {
		j.triggerID = id
	}
}

// WithRunStoreFactory overrides the run store used for execution state.
func WithRunStoreFactory(factory func() *run.Store) JobOption {
	return func(j *job) {
		if factory != nil {
			j.runStoreFactory = factory
		}
	}
}

// WithEnvVariables overrides the environment configuration.
func WithEnvVariables(variables func() env.Environment) JobOption {
	return func(j *job) {
		if variables != nil {
			j.envVariables = variables
		}
	}
}

// WithTaskServiceFactory overrides the task service used to look up tasks.
func WithTaskServiceFactory(factory func(context.Context) task.Task) JobOption {
	return func(j *job) {
		if factory != nil {
			j.taskServiceFactory = factory
		}
	}
}

// WithAtomServiceFactory overrides the atom service used to look up atoms.
func WithAtomServiceFactory(factory func(context.Context) asvc.Atom) JobOption {
	return func(j *job) {
		if factory != nil {
			j.atomServiceFactory = factory
		}
	}
}

// WithTaskEdgeServiceFactory overrides the task edge service used to look up edges.
func WithTaskEdgeServiceFactory(factory func(context.Context) taskedge.TaskEdge) JobOption {
	return func(j *job) {
		if factory != nil {
			j.taskEdgeServiceFactory = factory
		}
	}
}

// WithDispatchRunCallbacks overrides the callback dispatch function.
func WithDispatchRunCallbacks(dispatch func(context.Context, uuid.UUID, uuid.UUID, error) error) JobOption {
	return func(j *job) {
		if dispatch != nil {
			j.dispatchRunCallbacks = dispatch
		}
	}
}

// WithDockerEngineFactory overrides the Docker engine constructor.
func WithDockerEngineFactory(factory func(context.Context) atom.Engine) JobOption {
	return func(j *job) {
		if factory != nil {
			j.newDockerEngine = factory
		}
	}
}

// WithKubernetesEngineFactory overrides the Kubernetes engine constructor.
func WithKubernetesEngineFactory(factory func(context.Context) atom.Engine) JobOption {
	return func(j *job) {
		if factory != nil {
			j.newKubernetesEngine = factory
		}
	}
}

// WithPodmanEngineFactory overrides the Podman engine constructor.
func WithPodmanEngineFactory(factory func(context.Context) atom.Engine) JobOption {
	return func(j *job) {
		if factory != nil {
			j.newPodmanEngine = factory
		}
	}
}

// WithAtomPollInterval overrides the polling interval for atom completion checks.
func WithAtomPollInterval(interval time.Duration) JobOption {
	return func(j *job) {
		if interval > 0 {
			j.atomPollInterval = interval
		}
	}
}

// WithParams attaches run parameters to the job.
// Parameters are injected into each task's environment as
// CAESIUM_PARAM_<KEY>=<VALUE> (KEY uppercased).
func WithParams(params map[string]string) JobOption {
	return func(j *job) {
		j.params = params
	}
}

// WithSecretResolver configures secret:// resolution for step environment
// values. If omitted, Run builds the resolver from the processed environment.
func WithSecretResolver(resolver secret.Resolver) JobOption {
	return func(j *job) {
		j.secretResolver = resolver
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

// unmarshalCacheConfig decodes a JSON-encoded cache config from a DB column
// back into the interface{} form expected by ResolveCacheConfig.
func unmarshalCacheConfig(raw []byte) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

func unmarshalConcurrency(raw []byte) *jobdefschema.Concurrency {
	if len(raw) == 0 {
		return nil
	}
	var v *jobdefschema.Concurrency
	if err := json.Unmarshal(raw, &v); err != nil {
		log.Warn("failed to unmarshal job concurrency metadata", "error", err)
		return nil
	}
	return v
}

func unmarshalRateLimits(raw []byte) []jobdefschema.RateLimit {
	if len(raw) == 0 {
		return nil
	}
	var v []jobdefschema.RateLimit
	if err := json.Unmarshal(raw, &v); err != nil {
		log.Warn("failed to unmarshal job rate limit metadata", "error", err)
		return nil
	}
	return v
}

func (j *job) Run(ctx context.Context) error {
	store := j.runStoreFactory()
	vars := j.envVariables()
	secretResolver := j.secretResolver
	if secretResolver == nil {
		var err error
		secretResolver, err = jobdefruntime.BuildSecretResolver(vars)
		if err != nil {
			return fmt.Errorf("secret resolver configuration failure: %w", err)
		}
	}

	cacheConfig := cache.ConfigFromEnv()
	var cacheStore *cache.Store
	getCacheStore := func() *cache.Store {
		if cacheStore == nil {
			cacheStore = cache.NewStore(store.DB())
		}
		return cacheStore
	}

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

	var snapshot *run.JobRun
	if err := retryOnContention(ctx, func() error {
		var e error
		snapshot, e = resolveRun()
		return e
	}); err != nil {
		return err
	}

	runID := snapshot.ID
	runQuarantined := snapshot.Quarantine
	ctx = run.WithContext(ctx, runID)

	var runErr error
	defer func() {
		if err := store.Complete(runID, runErr); err != nil {
			log.Error("run completion persistence failure", "run_id", runID, "error", err)
		}
		if runQuarantined {
			return
		}
		dispatchCtx := context.WithoutCancel(ctx)
		if err := j.dispatchRunCallbacks(dispatchCtx, j.id, runID, runErr); err != nil {
			log.Error("callback dispatch failure", "job_id", j.id, "run_id", runID, "error", err)
		}
	}()
	if runQuarantined && executionMode != executionModeDistributed {
		runErr = ErrLocalQuarantinedReplayUnsupported
		return runErr
	}

	var tasks models.Tasks
	if err := retryOnContention(ctx, func() error {
		var e error
		tasks, e = j.taskServiceFactory(ctx).List(&task.ListRequest{
			JobID:   j.id.String(),
			OrderBy: []string{"position", "created_at"},
		})
		return e
	}); err != nil {
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

		var modelAtom *models.Atom
		if err := retryOnContention(ctx, func() error {
			var e error
			modelAtom, e = svc.Get(t.AtomID)
			return e
		}); err != nil {
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

	var edges models.TaskEdges
	if err := retryOnContention(ctx, func() error {
		var e error
		edges, e = j.taskEdgeServiceFactory(ctx).List(&taskedge.ListRequest{
			JobID:   j.id.String(),
			OrderBy: []string{"created_at"},
		})
		return e
	}); err != nil {
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

	registerInputs := make([]run.RegisterTaskInput, 0, len(tasks))
	for _, t := range tasks {
		atomModel := atomsByTask[t.ID]
		registerInputs = append(registerInputs, run.RegisterTaskInput{
			Task:                    t,
			Atom:                    atomModel,
			OutstandingPredecessors: indegree[t.ID],
		})
	}
	if err := store.RegisterTasks(runID, registerInputs); err != nil {
		runErr = err
		return err
	}

	var currentRun *run.JobRun
	if err := retryOnContention(ctx, func() error {
		var e error
		currentRun, e = store.Get(runID)
		return e
	}); err != nil {
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
	taskHashes := make(map[uuid.UUID]string, len(tasks))
	taskQuarantine := make(map[uuid.UUID]bool, len(tasks))
	terminalTasks := 0

	for _, taskState := range currentRun.Tasks {
		taskQuarantine[taskState.ID] = taskState.Quarantine || runQuarantined
		indegree[taskState.ID] = taskState.OutstandingPredecessors
		switch taskState.Status {
		case run.TaskStatusSucceeded, run.TaskStatusCached:
			processed[taskState.ID] = true
			taskOutcomes[taskState.ID] = run.TaskStatusSucceeded
			if len(taskState.Output) > 0 {
				taskOutputs[taskState.ID] = taskState.Output
			}
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

	paramEnv := buildParamEnv(snapshot.ID, j.alias, snapshot.Params)

	// executeAtom creates, monitors, and stops a container for one execution attempt.
	// It returns the atom result string, any parsed task outputs, any branch
	// selections (for branch-type tasks), a persisted log snapshot, and any error.
	executeAtom := func(taskCtx context.Context, taskID uuid.UUID, attempt int, runner *atomRunner, extraEnv map[string]string) (string, map[string]string, []string, *run.TaskLogSnapshot, error) {
		atomName := fmt.Sprintf("%s-%s", taskID, runID)
		if attempt > 1 {
			atomName = fmt.Sprintf("%s-attempt%d", atomName, attempt)
		}

		log.Info("running atom", "job_id", j.id, "task_id", taskID, "image", runner.image, "cmd", runner.command, "attempt", attempt)

		spec := runner.spec
		taskQuarantined := taskQuarantine[taskID] || runQuarantined
		if taskQuarantined {
			return "", nil, nil, nil, ErrLocalQuarantinedReplayUnsupported
		}
		spec, secretIdentities, err := jobdefruntime.ResolveContainerSpecSecretsWithIdentities(taskCtx, secretResolver, spec)
		if err != nil {
			return "", nil, nil, nil, err
		}
		if len(secretIdentities) > 0 {
			refs := make([]models.TaskExecutionSecretRef, 0, len(secretIdentities))
			for _, resolved := range secretIdentities {
				refs = append(refs, run.SecretIdentityDescriptorRef(resolved.EnvKey, resolved.Ref, resolved.Identity))
			}
			if err := store.UpdateTaskExecutionDescriptorSecretRefs(runID, taskID, refs); err != nil {
				log.Warn("failed to persist task execution descriptor secret identity", "task_id", taskID, "error", err)
			}
		}
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
			return "", nil, nil, nil, err
		}

		if err := store.StartTask(runID, taskID, a.ID()); err != nil {
			return "", nil, nil, nil, err
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
					return "", nil, nil, nil, fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskID, taskTimeout, a.ID(), stopErr)
				}
				// Distinguish run-level timeout from task-level timeout.
				if ctx.Err() != nil {
					return "", nil, nil, nil, fmt.Errorf("task %s cancelled: %w", taskID, ctx.Err())
				}
				return "", nil, nil, nil, fmt.Errorf("task %s timed out after %s", taskID, taskTimeout)
			}
			return "", nil, nil, nil, taskCtx.Err()
		case result := <-waitResult:
			if result.err != nil {
				return "", nil, nil, nil, result.err
			}
			a = result.atom
			log.Info("atom finished", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "result", a.Result())

			// Parse both structured outputs and branch markers in a single
			// pass over the log stream (no full buffering).
			var taskOutput map[string]string
			var branchNames []string
			var logSnapshot *run.TaskLogSnapshot
			logStream, logErr := runner.engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
			if logErr == nil {
				markers, parseErr := pkgtask.CaptureMarkersWithRefLimit(logStream, pkgtask.MaxLogSnapshotBytes, vars.OutputRefMaxBytes.Int64())
				if closeErr := logStream.Close(); closeErr != nil {
					log.Warn("failed to close log stream", "task_id", taskID, "error", closeErr)
				}
				if parseErr != nil {
					log.Warn("failed to parse task markers", "task_id", taskID, "error", parseErr)
				} else if markers != nil {
					taskOutput = markers.Output
					branchNames = markers.Branches
					if markers.LogText != "" || markers.LogTruncated {
						logSnapshot = &run.TaskLogSnapshot{
							Text:      markers.LogText,
							Truncated: markers.LogTruncated,
						}
					}
				}
			}

			stopErr := runner.engine.Stop(&atom.EngineStopRequest{
				ID:    a.ID(),
				Force: true,
			})
			return string(a.Result()), taskOutput, branchNames, logSnapshot, stopErr
		}
	}

	runTask := func(taskID uuid.UUID) ([]uuid.UUID, error) {
		runner := runners[taskID]
		if runner == nil {
			return nil, fmt.Errorf("missing runner for task %s", taskID)
		}

		// Build predecessor output env vars for this task.
		predOutputs := make(map[string]map[string]string)
		predOutputsByID := make(map[uuid.UUID]map[string]string)
		for _, predID := range predecessors[taskID] {
			if outputs, ok := taskOutputs[predID]; ok && len(outputs) > 0 {
				predOutputsByID[predID] = outputs
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
		taskQuarantined := taskQuarantine[taskID] || runQuarantined

		// Cache check — attempt to bypass container execution.
		var cacheCfg jobdefschema.CacheConfig
		var inputHash string
		// resolvedImageDigest is the content digest folded into inputHash when
		// pinning is on; empty otherwise. Reused when the result is cached so
		// the cache Entry records which image content the hash covers.
		var resolvedImageDigest string
		// hashInputBlob is the canonical secret-redacted decomposition of the
		// HashInput; declared here (like inputHash) so it survives into the
		// success path where it is also written onto the cache Entry, letting a
		// cache hit be explained as well as a re-run.
		var hashInputBlob []byte
		// Resolve cache config from step-level, job-level, then env defaults.
		var stepCache interface{}
		if taskModel != nil {
			stepCache = unmarshalCacheConfig(taskModel.CacheConfig)
		}
		cacheCfg = jobdefschema.ResolveCacheConfig(stepCache, j.jobCacheConfig, cacheConfig.Enabled, cacheConfig.TTL, cacheConfig.PinDigests, cacheConfig.DigestTTL)

		if cacheCfg.Enabled {
			cacheStore := getCacheStore()
			taskName := ""
			if taskModel != nil {
				taskName = taskModel.Name
			}

			// Build merged env for hashing, excluding volatile per-run vars.
			mergedEnv := make(map[string]string, len(runner.spec.Env)+len(outputEnv))
			for k, v := range runner.spec.Env {
				mergedEnv[k] = v
			}
			for k, v := range outputEnv {
				mergedEnv[k] = v
			}

			// Collect predecessor hashes.
			var predHashes []string
			predHashByID := make(map[uuid.UUID]string)
			for _, predID := range predecessors[taskID] {
				if h, ok := taskHashes[predID]; ok {
					predHashes = append(predHashes, h)
					predHashByID[predID] = h
				}
			}

			// When digest pinning is enabled, resolve the image tag to its
			// content digest and fold the digest (not the mutable tag) into the
			// cache key. Resolution failures fall back to the tag — a cache miss
			// is always safe, so an unresolved digest never serves a stale hit.
			if cacheCfg.PinDigests {
				engineKind := models.AtomEngineDocker
				if a := atomsByTask[taskID]; a != nil {
					engineKind = a.Engine
				}
				if digest, derr := imagecheck.Default().Resolve(ctx, engineKind, runner.image, cacheCfg.DigestTTL); derr == nil {
					resolvedImageDigest = digest
				}
			}

			hashInput := cache.HashInput{
				JobAlias:             j.alias,
				TaskName:             taskName,
				Image:                runner.image,
				ResolvedImageDigest:  resolvedImageDigest,
				Command:              runner.command,
				Env:                  mergedEnv,
				WorkDir:              runner.spec.WorkDir,
				Mounts:               runner.spec.Mounts,
				ResolvedVolumeMounts: runner.spec.ResolvedVolumeMounts,
				Kubernetes:           runner.spec.Kubernetes,
				PredecessorHashes:    predHashes,
				PredecessorOutputs:   predOutputs,
				RunParams:            snapshot.Params,
				CacheVersion:         cacheCfg.Version,
			}
			inputHash = hashInput.Compute()
			// Serialize the decomposed input to a canonical, secret-redacted
			// blob so `caesium why` can later diff this run field-by-field. A
			// serialization failure is non-fatal: persist the hash without the
			// blob (a missing blob degrades `why` to digest-only, never wrong).
			blob, blobErr := hashInput.CanonicalJSON(inputHash)
			if blobErr != nil {
				log.Warn("failed to serialize hash-input blob", "task", taskName, "error", blobErr)
				blob = nil
			}
			hashInputBlob = blob
			if err := store.SetTaskHashWithBlob(runID, taskID, inputHash, resolvedImageDigest, hashInputBlob); err != nil {
				log.Warn("failed to persist task hash", "task", taskName, "error", err)
			}
			if err := store.UpdateTaskExecutionDescriptorInputs(runID, taskID, predOutputsByID, predHashByID, inputHash, resolvedImageDigest, hashInputBlob); err != nil {
				log.Warn("failed to persist task execution descriptor inputs", "task", taskName, "error", err)
			}

			entry, found, err := cacheStore.Get(inputHash)
			switch {
			case err != nil:
				log.Warn("cache lookup failed", "task", taskName, "error", err)
			case found:
				if !taskQuarantined {
					metrics.TaskCacheHitsTotal.WithLabelValues(j.alias, taskName).Inc()
				}
				log.Info("cache hit", "task", taskName, "hash", inputHash[:12])

				cacheResult, cacheErr := store.CacheHitTask(runID, taskID, run.CacheHitSource{
					RunID:     entry.RunID,
					CreatedAt: entry.CreatedAt,
					ExpiresAt: entry.ExpiresAt,
				}, entry.Result, entry.Output, entry.BranchSelections)
				if cacheErr != nil {
					log.Error("failed to apply cache hit", "task", taskName, "error", cacheErr)
					// Fall through to normal execution.
				} else {
					if len(entry.Output) > 0 {
						taskOutputs[taskID] = entry.Output
					}
					taskHashes[taskID] = inputHash
					var skipped []uuid.UUID
					if cacheResult != nil && len(cacheResult.SkippedTaskIDs) > 0 {
						skipped = cacheResult.SkippedTaskIDs
					}
					if !run.IsSuccessfulTaskResult(entry.Result) {
						return skipped, fmt.Errorf("task %s failed with cached result %q", taskID, entry.Result)
					}
					return skipped, nil
				}
			default:
				if !taskQuarantined {
					metrics.TaskCacheMissesTotal.WithLabelValues(j.alias, taskName).Inc()
				}
			}
		}

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

			result, output, branchNames, logSnapshot, execErr := executeAtom(taskCtx, taskID, attempt, runner, outputEnv)
			cancel()

			if execErr == nil {
				if err := run.ValidateTaskOutputSchema(store, runID, taskModel.ID, output, taskModel.OutputSchema, j.schemaValidation); err != nil {
					if snapshotErr := store.SaveTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
						log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
					}
					execErr = err
				}
			}

			if execErr == nil {
				completeResult, completeErr := store.CompleteTaskWithResult(runID, taskID, result, output, branchNames)
				if completeErr != nil {
					return nil, completeErr
				}
				if snapshotErr := store.SaveTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
					log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
				}
				if len(output) > 0 {
					taskOutputs[taskID] = output
				}

				// Store successful result in cache, reusing the hash computed earlier.
				if cacheCfg.Enabled && inputHash != "" && run.IsSuccessfulTaskResult(result) {
					cacheStore := getCacheStore()
					taskName := ""
					if taskModel != nil {
						taskName = taskModel.Name
					}

					if taskQuarantined {
						taskHashes[taskID] = inputHash
						log.Info("quarantined task skipped cache publication", "task", taskName, "hash", inputHash[:12])
					} else {
						// Value-verified short-circuit (D2): this task re-executed
						// because its OWN identity hash (inputHash) changed. If it
						// produced output byte-identical to a prior successful run,
						// present that prior run's identity to downstream consumers so
						// a downstream whose only changed input was this step stays a
						// cache hit instead of re-running. The substitution only
						// happens when content equality is PROVEN (see
						// cache.EquivalentPriorHash); on any uncertainty it returns
						// inputHash unchanged (re-run downstream — always safe). The
						// proof reads priors filtered to exclude inputHash, so the
						// order relative to the Put below does not matter.
						effectiveHash := inputHash
						if priors, priorErr := cacheStore.PriorEntriesByTask(j.id, taskName, inputHash); priorErr != nil {
							log.Warn("short-circuit: failed to load prior entries", "task", taskName, "error", priorErr)
						} else {
							effectiveHash = cache.EquivalentPriorHash(inputHash, output, priors)
						}
						// taskHashes drives the in-memory predHashes a downstream task
						// folds into its own key; storing the effective (possibly
						// prior) identity is what stops the cascade locally.
						taskHashes[taskID] = effectiveHash
						if effectiveHash != inputHash {
							metrics.TaskCacheShortCircuitsTotal.WithLabelValues(j.alias, taskName).Inc()
							log.Info("value-verified short-circuit", "task", taskName, "new_hash", inputHash[:12], "effective_hash", effectiveHash[:12])
							if scErr := store.SetTaskEffectiveHash(runID, taskID, effectiveHash); scErr != nil {
								log.Warn("short-circuit: failed to persist effective hash", "task", taskName, "error", scErr)
							}
						}

						var expiresAt *time.Time
						if cacheCfg.TTL > 0 {
							t := time.Now().Add(cacheCfg.TTL)
							expiresAt = &t
						}
						if putErr := cacheStore.Put(&cache.Entry{
							Hash:                inputHash,
							JobID:               j.id,
							TaskName:            taskName,
							Result:              result,
							Output:              output,
							BranchSelections:    branchNames,
							RunID:               runID,
							TaskRunID:           taskID,
							ResolvedImageDigest: resolvedImageDigest,
							HashInputBlob:       hashInputBlob,
							CreatedAt:           time.Now(),
							ExpiresAt:           expiresAt,
						}); putErr != nil {
							log.Warn("failed to store cache entry", "task", taskName, "error", putErr)
						}
					}
				}

				var skipped []uuid.UUID
				if completeResult != nil && len(completeResult.SkippedTaskIDs) > 0 {
					skipped = completeResult.SkippedTaskIDs
				}
				if !run.IsSuccessfulTaskResult(result) {
					return skipped, fmt.Errorf("task %s failed with result %q", taskID, result)
				}
				return skipped, nil
			}
			lastErr = execErr

			// No more attempts — mark as permanently failed.
			if attempt >= maxAttempts {
				break
			}

			// Compute retry delay.
			delay := computeRetryDelay(taskModel, attempt)

			log.Info("retrying task", "job_id", j.id, "task_id", taskID, "attempt", attempt, "next_attempt", attempt+1, "delay", delay, "error", lastErr)

			if !taskQuarantined {
				metrics.TaskRetriesTotal.WithLabelValues(j.alias, taskID.String(), strconv.Itoa(attempt)).Inc()
			}

			if err := store.RetryTask(runID, taskID, attempt+1); err != nil {
				log.Error("failed to persist task retry state", "run_id", runID, "task_id", taskID, "error", err)
			}

			if delay > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
			}
		}

		if persistErr := store.FailTask(runID, taskID, lastErr); persistErr != nil {
			log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
		}
		return nil, lastErr
	}

	taskPool := worker.NewPool(maxParallel)

	results := make(chan taskResult)
	active := 0
	halt := false
	deferred := make(map[uuid.UUID]time.Time)
	rateLimiter := ratelimit.NewLimiter(store.DB())

	acquireTaskRateLimit := func(taskID uuid.UUID) (bool, time.Time, error) {
		rule, ok, err := ratelimit.RuleForTask(ctx, store.DB(), runID, taskID)
		if err != nil {
			return false, time.Time{}, err
		}
		if !ok {
			return true, time.Time{}, nil
		}
		acquired, err := rateLimiter.Acquire(ctx, rule.Resource, rule.Units, rule.Limit, rule.Window)
		if err != nil {
			return false, time.Time{}, err
		}
		if acquired {
			return true, time.Time{}, nil
		}

		now := time.Now().UTC()
		retryAfter := now.Add(ratelimit.RetryAfter(now, rule.Window))
		if err := store.RateLimitTask(ctx, runID, taskID, retryAfter); err != nil {
			return false, time.Time{}, err
		}
		metrics.RunSkippedTotal.WithLabelValues(j.alias, "rate_limit").Inc()
		log.Info("task delayed by rate limit", "job_id", j.id, "run_id", runID, "task_id", taskID, "resource", rule.Resource, "retry_after", retryAfter)
		return false, retryAfter, nil
	}

	moveDueDeferred := func() bool {
		now := time.Now().UTC()
		moved := false
		for taskID, retryAfter := range deferred {
			if retryAfter.After(now) {
				continue
			}
			delete(deferred, taskID)
			push(taskID)
			moved = true
		}
		return moved
	}

	nextDeferredAt := func() (time.Time, bool) {
		var next time.Time
		for _, retryAfter := range deferred {
			if next.IsZero() || retryAfter.Before(next) {
				next = retryAfter
			}
		}
		return next, !next.IsZero()
	}

	dispatch := func(taskID uuid.UUID) error {
		acquired, retryAfter, err := acquireTaskRateLimit(taskID)
		if err != nil {
			return err
		}
		if !acquired {
			deferred[taskID] = retryAfter
			return nil
		}

		active++
		if err := taskPool.Submit(ctx, func() {
			skipped, err := runTask(taskID)
			results <- taskResult{id: taskID, err: err, skippedByBranch: skipped}
		}); err != nil {
			active--
			return err
		}
		return nil
	}

	for (!halt && (len(queue) > 0 || len(deferred) > 0)) || active > 0 {
		if !halt && moveDueDeferred() {
			continue
		}

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

		var result taskResult
		gotResult := false
		if halt && len(queue) == 0 && len(deferred) > 0 {
			if active == 0 {
				break
			}
			var ok bool
			result, ok = waitForHaltedDispatchResult(results, haltedDispatchWaitInterval)
			if !ok {
				continue
			}
			active--
			gotResult = true
		} else if len(queue) == 0 && len(deferred) > 0 {
			next, ok := nextDeferredAt()
			if !ok {
				continue
			}
			wait := time.Until(next)
			if wait <= 0 {
				continue
			}
			timer := time.NewTimer(wait)
			if active == 0 {
				select {
				case <-ctx.Done():
					timer.Stop()
					runErr = ctx.Err()
					halt = true
					continue
				case <-timer.C:
					continue
				}
			}
			select {
			case result = <-results:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				active--
				gotResult = true
			case <-ctx.Done():
				timer.Stop()
				runErr = ctx.Err()
				halt = true
				continue
			case <-timer.C:
				continue
			}
		}

		if !gotResult {
			if active == 0 {
				break
			}
			result = <-results
			active--
		}

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
					if !halt {
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

		// Update local state for any tasks the run store skipped while
		// resolving branch filtering or trigger-rule evaluation.
		skippedSet := make(map[uuid.UUID]bool, len(result.skippedByBranch))
		for _, skippedID := range result.skippedByBranch {
			if processed[skippedID] {
				skippedSet[skippedID] = true
				continue
			}

			skippedSet[skippedID] = true
			taskOutcomes[skippedID] = run.TaskStatusSkipped
			processed[skippedID] = true
			terminalTasks++
			delete(inQueue, skippedID)

			if err := propagateSkipped(skippedID); err != nil {
				log.Error("failed to propagate skipped task", "run_id", runID, "task_id", skippedID, "error", err)
				if runErr == nil {
					runErr = err
				}
				halt = true
				queue = queue[:0]
				break
			}
		}

		if !halt {
			for _, successor := range adjacency[result.id] {
				if _, ok := indegree[successor]; !ok {
					continue
				}

				// Skip successors already handled by branch filtering in the store.
				if skippedSet[successor] {
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
			cached := 0

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
				case run.TaskStatusCached:
					cached++
				}
			}

			terminal := failed + succeeded + skipped + cached
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
