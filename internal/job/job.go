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
	"sync"
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
	priorityOverride       string
	concurrency            *jobdefschema.Concurrency
	rateLimits             []jobdefschema.RateLimit
	schemaValidation       string
	jobCacheConfig         interface{}
	params                 map[string]string
	expectedStateRevision  int64
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

// fanOutSweepTimeout bounds the fan-out straggler sweep, which runs on a context
// detached from the run's so a CANCELLED run still resolves its instance rows
// (local mode has no recovery owner to revisit them). Detaching removes the
// deadline the run's context supplied, so the sweep carries its own: generous
// enough for a 10k-instance group's reads and writes, short enough that a wedged
// database cannot hold the run loop open indefinitely.
const fanOutSweepTimeout = 10 * time.Second

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

// WithExpectedStateRevision binds a resumed retry runner to the exact durable
// revision committed by its retry transaction. Without this, a delayed rev-N
// goroutine can start after retry N+1 and re-read/bless the newer revision,
// becoming a duplicate waiter and callback sender for N+1.
func WithExpectedStateRevision(stateRevision int64) JobOption {
	return func(j *job) {
		j.expectedStateRevision = stateRevision
	}
}

func WithPriorityOverride(priority string) JobOption {
	return func(j *job) {
		j.priorityOverride = strings.TrimSpace(priority)
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

// taskHashInputArgs is the per-execution input to buildTaskHashInput.
//
// Both the unfanned local path and every fan-out instance construct their cache
// identity through that single function, so the two can never drift on which
// fields are folded into the hash — a drift that would silently give fanned
// steps a different cache identity from every other step. A fan-out instance
// sets the three Partition* fields on top; everything else is identical by
// construction.
type taskHashInputArgs struct {
	JobAlias             string
	TaskName             string
	Image                string
	ResolvedImageDigest  string
	Command              []string
	Env                  map[string]string
	WorkDir              string
	Mounts               []container.Mount
	ResolvedVolumeMounts []container.VolumeMount
	Kubernetes           *container.KubernetesSpec
	PredecessorHashes    []string
	PredecessorOutputs   map[string]map[string]string
	RunParams            map[string]string
	CacheVersion         int
	// Chain is the resolved cache.chain mode. Under CacheChainValues the
	// PredecessorHashes above are carried for provenance but excluded from the
	// key; see cache.HashInput.Chain.
	Chain string

	Partition            string
	PartitionFingerprint string
	PartitionAttributes  map[string]string
}

// buildTaskHashInput is the single construction site for cache.HashInput in the
// local executor. See taskHashInputArgs for why it exists.
func buildTaskHashInput(a taskHashInputArgs) cache.HashInput {
	return cache.HashInput{
		JobAlias:             a.JobAlias,
		TaskName:             a.TaskName,
		Image:                a.Image,
		ResolvedImageDigest:  a.ResolvedImageDigest,
		Command:              a.Command,
		Env:                  a.Env,
		WorkDir:              a.WorkDir,
		Mounts:               a.Mounts,
		ResolvedVolumeMounts: a.ResolvedVolumeMounts,
		Kubernetes:           a.Kubernetes,
		PredecessorHashes:    a.PredecessorHashes,
		PredecessorOutputs:   a.PredecessorOutputs,
		RunParams:            a.RunParams,
		Chain:                a.Chain,
		Partition:            a.Partition,
		PartitionFingerprint: a.PartitionFingerprint,
		PartitionAttributes:  a.PartitionAttributes,
		CacheVersion:         a.CacheVersion,
	}
}

// applyCacheHit marks a task cached, replaying a cached fan-out producer's
// partition list into the same transaction when there is one, so the consumer's
// group expands from cache exactly as it does from a fresh completion.
//
// Losing that list is not a degraded cache hit but a wrong run: the producer
// resolves instantly, the fanned consumer never expands, and the group collapses
// to its unexpanded template row — a green run that did none of the work. The
// call is therefore direct and compile-time checked rather than resolved through
// an optional-capability assertion: dropping CacheHitTaskWithPartitions from the
// store must break the build, not silently reinstate that failure mode on the
// one route (cached producer) nobody varies.
func applyCacheHit(
	store *run.Store,
	runID, taskID uuid.UUID,
	source run.CacheHitSource,
	result string,
	output map[string]string,
	branchSelections []string,
	partitions []pkgtask.Partition,
) (*run.CompleteTaskResult, error) {
	if len(partitions) > 0 {
		return store.CacheHitTaskWithPartitions(runID, taskID, source, result, output, branchSelections, partitions)
	}
	return store.CacheHitTask(runID, taskID, source, result, output, branchSelections)
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
	// Lazily built, but fanned instances resolve their cache identity from
	// concurrent goroutines, so the initialization must be once-only rather
	// than a racy nil check.
	var (
		cacheStore     *cache.Store
		cacheStoreOnce sync.Once
	)
	getCacheStore := func() *cache.Store {
		cacheStoreOnce.Do(func() { cacheStore = cache.NewStore(store.DB()) })
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
		startOpts := []run.StartOption{run.WithStartParams(j.params)}
		startPriority := strings.TrimSpace(j.priorityOverride)
		if startPriority == "" {
			startPriority = strings.TrimSpace(j.priority)
		}
		if startPriority != "" {
			startOpts = append(startOpts, run.WithStartPriority(startPriority))
		}

		if id, ok := run.FromContext(ctx); ok {
			existing, err := store.Get(id)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return store.Start(j.id, j.triggerID, startOpts...)
				}
				return nil, err
			}
			return existing, nil
		}

		if admitted, handled, err := store.AdmitRun(j.id, j.triggerID, startOpts...); handled || err != nil {
			return admitted, err
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

		return store.Start(j.id, j.triggerID, startOpts...)
	}

	var snapshot *run.JobRun
	if err := retryOnContention(ctx, func() error {
		var e error
		snapshot, e = resolveRun()
		return e
	}); err != nil {
		if errors.Is(err, run.ErrRunSkipped) || errors.Is(err, run.ErrRunQueued) {
			return nil
		}
		return err
	}
	if snapshot == nil {
		return nil
	}

	runID := snapshot.ID
	runQuarantined := snapshot.Quarantine
	ctx = run.WithContext(ctx, runID)
	var leaseVersion run.LeaseVersion
	if leases := store.LeaseStore(); leases != nil {
		lease, err := leases.GetLease(ctx, runID)
		if j.expectedStateRevision > 0 {
			if err != nil {
				return fmt.Errorf("resolve expected run lease revision for %s: %w", runID, err)
			}
			if lease.StateRevision != j.expectedStateRevision {
				// The retry transaction already registered this exact execution in
				// process-local active-run bookkeeping. If a later retry won before
				// this goroutine started, it will never reach the deferred finalizer;
				// retire only the superseded epoch here so its gauge cannot leak.
				disposition, retireErr := store.CompleteAtStateRevision(runID, nil, j.expectedStateRevision)
				if retireErr != nil {
					return fmt.Errorf("retire superseded expected run revision for %s: %w", runID, retireErr)
				}
				if disposition != run.CompletionSuperseded {
					return fmt.Errorf("retire expected run revision for %s: got disposition %d: %w",
						runID, disposition, run.ErrOwnerStateChanged)
				}
				return fmt.Errorf("resolve expected run lease revision for %s: expected %d, got %d: %w",
					runID, j.expectedStateRevision, lease.StateRevision, run.ErrOwnerStateChanged)
			}
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("resolve run lease version for %s: %w", runID, err)
		}
		if err == nil {
			leaseVersion = run.LeaseVersion{
				Generation: lease.Generation, StateRevision: lease.StateRevision,
			}
			if leaseVersion.StateRevision <= 0 {
				return fmt.Errorf("resolve run lease version for %s: %w", runID, run.ErrOwnerStateChanged)
			}
		}
	} else if j.expectedStateRevision > 0 {
		return fmt.Errorf("resolve expected run lease revision for %s: lease store unavailable: %w", runID, run.ErrOwnerStateChanged)
	}
	if j.expectedStateRevision > 0 {
		switch snapshot.Status {
		case run.StatusSucceeded, run.StatusFailed, run.StatusCancelled:
			// The bound execution reached a durable terminal state before this
			// resumed goroutine began. This is especially important for a cancelled
			// lease tombstone: its exact revision is retained for callback fencing,
			// but it has no live owner and must never register or execute tasks.
			j.finishRun(ctx, store, runID, runQuarantined, nil, leaseVersion)
			return nil
		}
	}

	var runErr error
	defer func() {
		j.finishRun(ctx, store, runID, runQuarantined, runErr, leaseVersion)
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
	//
	// instanceID identifies the TaskRun row this attempt belongs to. It is
	// uuid.Nil for an unfanned step (whose single row is addressable by taskID)
	// and the instance's TaskRun primary key for a fan-out partition, where N
	// sibling rows share (runID, taskID) and every store write and container name
	// must therefore be keyed on the instance, not the catalog task.
	executeAtom := func(taskCtx context.Context, taskID, instanceID uuid.UUID, attempt int, runner *atomRunner, extraEnv map[string]string) (string, map[string]string, []string, []pkgtask.Partition, *run.TaskLogSnapshot, error) {
		// taskRef is what the run store resolves this execution to; see
		// loadTaskRunByIDOrUnique for the primary-key-or-task-ID contract.
		taskRef := taskID
		atomName := fmt.Sprintf("%s-%s", taskID, runID)
		if instanceID != uuid.Nil {
			taskRef = instanceID
			// Sibling partitions run against the same catalog task in the same
			// run, so the container name must carry the instance identity or
			// Docker rejects every sibling after the first with a name conflict.
			atomName = fmt.Sprintf("%s-%s", atomName, instanceID)
		}
		if attempt > 1 {
			atomName = fmt.Sprintf("%s-attempt%d", atomName, attempt)
		}

		log.Info("running atom", "job_id", j.id, "task_id", taskID, "instance_id", instanceID, "image", runner.image, "cmd", runner.command, "attempt", attempt)

		spec := runner.spec
		taskQuarantined := taskQuarantine[taskID] || runQuarantined
		if taskQuarantined {
			return "", nil, nil, nil, nil, ErrLocalQuarantinedReplayUnsupported
		}
		spec, secretIdentities, err := jobdefruntime.ResolveContainerSpecSecretsWithIdentities(taskCtx, secretResolver, spec)
		if err != nil {
			return "", nil, nil, nil, nil, err
		}
		if len(secretIdentities) > 0 {
			refs := make([]models.TaskExecutionSecretRef, 0, len(secretIdentities))
			for _, resolved := range secretIdentities {
				refs = append(refs, run.SecretIdentityDescriptorRef(resolved.EnvKey, resolved.Ref, resolved.Identity))
			}
			if err := store.UpdateTaskExecutionDescriptorSecretRefs(runID, taskRef, refs); err != nil {
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
			return "", nil, nil, nil, nil, err
		}

		if err := store.StartTask(runID, taskRef, a.ID()); err != nil {
			return "", nil, nil, nil, nil, err
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
					return "", nil, nil, nil, nil, fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskID, taskTimeout, a.ID(), stopErr)
				}
				// Distinguish run-level timeout from task-level timeout.
				if ctx.Err() != nil {
					return "", nil, nil, nil, nil, fmt.Errorf("task %s cancelled: %w", taskID, ctx.Err())
				}
				return "", nil, nil, nil, nil, fmt.Errorf("task %s timed out after %s", taskID, taskTimeout)
			}
			return "", nil, nil, nil, nil, taskCtx.Err()
		case result := <-waitResult:
			if result.err != nil {
				return "", nil, nil, nil, nil, result.err
			}
			a = result.atom
			log.Info("atom finished", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "result", a.Result())

			// Capture the raw exit code before Result() folds it into a coarse
			// status and the incident classifier loses it. Best-effort.
			if exitErr := store.SetTaskExitCode(runID, taskRef, a.ExitCode()); exitErr != nil {
				log.Warn("failed to persist task exit code", "task_id", taskID, "error", exitErr)
			}

			// Parse both structured outputs and branch markers in a single
			// pass over the log stream (no full buffering).
			var taskOutput map[string]string
			var branchNames []string
			var logSnapshot *run.TaskLogSnapshot
			var partitions []pkgtask.Partition
			logStream, logErr := runner.engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
			if logErr == nil {
				maxParts := env.Variables().FanOutMaxPartitions
				markers, parseErr := pkgtask.CaptureMarkersWithLimits(logStream, pkgtask.MaxLogSnapshotBytes, vars.OutputRefMaxBytes.Int64(), maxParts)
				if closeErr := logStream.Close(); closeErr != nil {
					log.Warn("failed to close log stream", "task_id", taskID, "error", closeErr)
				}
				if parseErr != nil {
					var pe *pkgtask.PartitionError
					if errors.As(parseErr, &pe) {
						stopErr := runner.engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true})
						if stopErr != nil {
							return "", nil, nil, nil, nil, fmt.Errorf("%v (also failed to stop atom: %w)", parseErr, stopErr)
						}
						return "", nil, nil, nil, nil, parseErr
					}
					log.Warn("failed to parse task markers", "task_id", taskID, "error", parseErr)
				} else if markers != nil {
					taskOutput = markers.Output
					branchNames = markers.Branches
					partitions = markers.Partitions
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
			return string(a.Result()), taskOutput, branchNames, partitions, logSnapshot, stopErr
		}
	}

	fanOutGroups := make(map[uuid.UUID]run.ExpandedGroup)
	// fanOutGroups is written from whichever worker goroutine completes a
	// producer and read by whichever goroutine next runs a fanned step; the
	// two are only DAG-ordered relative to EACH OTHER, so an unrelated task
	// running concurrently makes the map a shared mutable. Every access after
	// the run loop starts goes through registerExpansion/lookupFanOutGroup.
	var fanOutGroupsMu sync.Mutex

	// rehydrateFanOutGroups reconstructs already-expanded groups from the store.
	//
	// fanOutGroups is normally seeded by the producer's own completion, which
	// returns the expansion payload. A RETRIED or resumed run does not re-execute
	// the producer — RetryFromFailure keeps it terminal-successful — so that
	// payload never arrives, and without this the local loop would treat a fanned
	// step as one ordinary task and every store write keyed on the catalog task
	// id would match N instance rows (ErrAmbiguousTaskRun), failing the retry.
	// Instance rows are the durable record of the group; the payload is only an
	// optimization that saves this read on the first run.
	rehydrateFanOutGroups := func() {
		var rows []models.TaskRun
		if err := store.DB().
			Where("job_run_id = ? AND partition_count > 0", runID).
			Order("task_id ASC, partition_index ASC").
			Find(&rows).Error; err != nil {
			log.Warn("failed to rehydrate fan-out groups", "run_id", runID, "error", err)
			return
		}
		byTask := make(map[uuid.UUID][]models.TaskRun, len(rows))
		order := make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			if _, seen := byTask[row.TaskID]; !seen {
				order = append(order, row.TaskID)
			}
			byTask[row.TaskID] = append(byTask[row.TaskID], row)
		}
		for _, tid := range order {
			if existing, ok := fanOutGroups[tid]; ok && len(existing.Instances) > 0 {
				continue
			}
			instances := byTask[tid]
			g := run.ExpandedGroup{TaskID: tid, Dependents: map[string][]string{}}
			if t := tasksByID[tid]; t != nil {
				g.TaskName = t.Name
			}
			for _, row := range instances {
				var deps []string
				if len(row.PartitionDependsOn) > 0 {
					if err := json.Unmarshal(row.PartitionDependsOn, &deps); err != nil {
						log.Warn("failed to decode partition dependsOn", "task_id", tid, "partition", row.PartitionValue, "error", err)
					}
				}
				var attrs map[string]string
				if len(row.PartitionAttributes) > 0 {
					if err := json.Unmarshal(row.PartitionAttributes, &attrs); err != nil {
						log.Warn("failed to decode partition attributes", "task_id", tid, "partition", row.PartitionValue, "error", err)
					}
				}
				g.Instances = append(g.Instances, run.ExpandedInstance{
					TaskRunID:               row.ID,
					TaskID:                  tid,
					PartitionIndex:          row.PartitionIndex,
					Partition:               pkgtask.Partition{Key: row.PartitionValue, Fingerprint: row.PartitionFingerprint, DependsOn: deps, Attributes: attrs},
					OutstandingPredecessors: row.OutstandingPredecessors,
				})
				for _, d := range deps {
					g.Dependents[d] = append(g.Dependents[d], row.PartitionValue)
				}
			}
			if len(g.Instances) > 0 {
				fanOutGroups[tid] = g
			}
		}
	}
	rehydrateFanOutGroups()

	// registerExpansion is the SINGLE place an expansion payload becomes an
	// executable group. Two routes can expand a producer locally — a fresh
	// completion (CompleteTaskWithPartitions) and a cache hit
	// (CacheHitTaskWithPartitions) — and both funnel through here so they cannot
	// drift on what "the group is now runnable" means.
	registerExpansion := func(res *run.CompleteTaskResult) {
		if res == nil || res.Expansion == nil {
			return
		}
		fanOutGroupsMu.Lock()
		defer fanOutGroupsMu.Unlock()
		for _, g := range res.Expansion.Groups {
			if len(g.Instances) > 0 {
				fanOutGroups[g.TaskID] = g
			}
		}
	}

	// lookupFanOutGroup is the only read of fanOutGroups once the run loop is
	// dispatching; rehydrateFanOutGroups above runs before any worker starts.
	lookupFanOutGroup := func(taskID uuid.UUID) (run.ExpandedGroup, bool) {
		fanOutGroupsMu.Lock()
		defer fanOutGroupsMu.Unlock()
		g, ok := fanOutGroups[taskID]
		return g, ok
	}

	// liveTaskCount is the number of DAG *nodes* the run must resolve, which is
	// the static task count: fan-out changes the TaskRun row count, never the
	// node count. A fanned step stays one node in adjacency/indegree here and is
	// collapsed back to one entry by convertRunModelWithDB, so both this guard
	// and waitForRunCompletion count in the same unit. The instance rows behind a
	// group are accounted for inside runFannedGroup, which does not return until
	// every one of them is terminal.
	liveTaskCount := len(tasks)

	// resolveTaskCacheIdentity computes the cache config plus the partition-free
	// hash-input args for a task. Shared by the unfanned path and every instance
	// of a fanned group so both fold in exactly the same fields.
	resolveTaskCacheIdentity := func(
		taskID uuid.UUID,
		taskModel *models.Task,
		runner *atomRunner,
		outputEnv map[string]string,
		predOutputs map[string]map[string]string,
	) (jobdefschema.CacheConfig, taskHashInputArgs, map[uuid.UUID]string) {
		var stepCache interface{}
		if taskModel != nil {
			stepCache = unmarshalCacheConfig(taskModel.CacheConfig)
		}
		cacheCfg := jobdefschema.ResolveCacheConfig(
			stepCache, j.jobCacheConfig,
			cacheConfig.Enabled, cacheConfig.TTL, cacheConfig.PinDigests, cacheConfig.DigestTTL,
		)
		predHashByID := make(map[uuid.UUID]string)

		taskName := ""
		if taskModel != nil {
			taskName = taskModel.Name
		}

		// Volatile per-run env (CAESIUM_RUN_ID, the injected partition, …) is
		// deliberately excluded: only the step's declared env and the resolved
		// predecessor outputs are identity.
		mergedEnv := make(map[string]string, len(runner.spec.Env)+len(outputEnv))
		for k, v := range runner.spec.Env {
			mergedEnv[k] = v
		}
		for k, v := range outputEnv {
			mergedEnv[k] = v
		}

		var predHashes []string
		for _, predID := range predecessors[taskID] {
			if h, ok := taskHashes[predID]; ok {
				predHashes = append(predHashes, h)
				predHashByID[predID] = h
			}
		}

		// When digest pinning is on, fold the resolved content digest (not the
		// mutable tag) into the key. Resolution failures fall back to the tag —
		// a cache miss is always safe, so an unresolved digest never serves a
		// stale hit.
		//
		// The digest exists only to make a cache key miss on a moved tag, so it
		// is resolved only when caching is actually on: with the cache disabled
		// there is no key to protect and the registry round-trip would be pure
		// cost on every task.
		var resolvedImageDigest string
		if cacheCfg.Enabled && cacheCfg.PinDigests {
			engineKind := models.AtomEngineDocker
			if a := atomsByTask[taskID]; a != nil {
				engineKind = a.Engine
			}
			if digest, derr := imagecheck.Default().Resolve(ctx, engineKind, runner.image, cacheCfg.DigestTTL); derr == nil {
				resolvedImageDigest = digest
			}
		}

		return cacheCfg, taskHashInputArgs{
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
			Chain:                cacheCfg.Chain,
		}, predHashByID
	}

	rateLimiter := ratelimit.NewLimiter(store.DB())

	// acquireRateLimitFor consumes one rate-limit token for ONE UNIT OF WORK.
	//
	// taskID selects the RULE — declarations are per step, so every instance of a
	// fanned step shares one. taskRef is the row a rejection parks, and it is a
	// different thing: the catalog task id for an unfanned step, the instance's
	// own TaskRun id for a fan-out instance. RateLimitTask refuses a catalog id
	// that names N siblings (ErrAmbiguousTaskRun), so conflating the two both
	// halted the run and, before that, let a whole group through on one token.
	//
	// One token per instance is what makes the local lane agree with the
	// distributed ones: the claimer and the owner dispatcher each acquire per
	// TaskRun row against the same catalog rule, so a `2 per minute` rule means
	// two PARTITIONS a minute wherever the step runs. Acquiring once for the
	// group meant a 1000-partition step consumed a single token locally and a
	// thousand under a worker.
	acquireRateLimitFor := func(taskID, taskRef uuid.UUID, partition string) (bool, time.Time, error) {
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
		if err := store.RateLimitTask(ctx, runID, taskRef, retryAfter); err != nil {
			return false, time.Time{}, err
		}
		metrics.RunSkippedTotal.WithLabelValues(j.alias, "rate_limit").Inc()
		logArgs := []any{"job_id", j.id, "run_id", runID, "task_id", taskID, "resource", rule.Resource, "retry_after", retryAfter}
		if partition != "" {
			logArgs = append(logArgs, "partition", partition)
		}
		log.Info("task delayed by rate limit", logArgs...)
		return false, retryAfter, nil
	}

	// runFannedGroup executes one expanded fan-out group.
	//
	// Readiness is NOT tracked in memory here: it is read from each instance
	// row's outstanding_predecessors column, the same scalar the distributed
	// claimer gates on. The store seeds it at expansion (template value +
	// in-group indegree) and decrements/skips it transitively inside
	// completeTask/failTask, so both lanes share one ordering implementation and
	// a failed dependency skips its dependents instead of hanging the run.
	runFannedGroup := func(
		taskID uuid.UUID,
		runner *atomRunner,
		taskModel *models.Task,
		group run.ExpandedGroup,
		outputEnv map[string]string,
		predOutputs map[string]map[string]string,
		predOutputsByID map[uuid.UUID]map[string]string,
	) ([]uuid.UUID, error) {
		if len(group.Instances) == 0 {
			return nil, nil
		}

		fo, decodeErr := jobdefruntime.DecodeFanOutConfig(nil)
		if taskModel != nil {
			if decoded, err := jobdefruntime.DecodeFanOutConfig(taskModel.FanOutConfig); err == nil {
				fo = decoded
			} else {
				decodeErr = err
			}
		}
		if decodeErr != nil {
			log.Warn("failed to decode fanOut config", "job_id", j.id, "task_id", taskID, "error", decodeErr)
		}

		envName := jobdefschema.DefaultFanOutEnv
		// An omitted failurePolicy is fail_fast, NOT continue. The schema
		// validator stamps that default onto the stored config
		// (pkg/jobdef/definition.go validateSteps) and the run owner normalizes
		// identically (run.normalizeFanOutFailurePolicy): only an explicit
		// "continue" continues, and anything else — "" or a value this build
		// does not recognize — fails the group fast. The three lanes must agree
		// here or a job that omits the key runs every sibling locally and
		// cancels them under the owner, which is the mode-dependent divergence
		// the plan's route-completeness contract exists to prevent.
		failurePolicy := jobdefschema.FanOutFailureFailFast
		groupParallel := maxParallel
		if fo != nil {
			if fo.Env != "" {
				envName = fo.Env
			}
			if fo.FailurePolicy == jobdefschema.FanOutFailureContinue {
				failurePolicy = jobdefschema.FanOutFailureContinue
			}
			// maxParallel caps the group; the job-level pool still bounds the
			// total, so the effective cap is the smaller of the two. Unset (0)
			// means "bounded only by the job".
			if fo.MaxParallel > 0 && fo.MaxParallel < groupParallel {
				groupParallel = fo.MaxParallel
			}
		}
		if groupParallel < 1 {
			groupParallel = 1
		}
		failFast := failurePolicy == jobdefschema.FanOutFailureFailFast

		// Static per-instance facts, keyed by TaskRun id. Statuses and readiness
		// come from the store, never from this map.
		type instanceMeta struct {
			partition  pkgtask.Partition
			maxAttempt int
		}
		maxAttempts := 1
		if taskModel != nil && taskModel.Retries > 0 {
			maxAttempts = taskModel.Retries + 1
		}
		meta := make(map[uuid.UUID]instanceMeta, len(group.Instances))
		for _, inst := range group.Instances {
			meta[inst.TaskRunID] = instanceMeta{partition: inst.Partition, maxAttempt: maxAttempts}
		}

		cacheCfg, hashArgs, predHashByID := resolveTaskCacheIdentity(taskID, taskModel, runner, outputEnv, predOutputs)
		taskQuarantined := taskQuarantine[taskID] || runQuarantined
		taskName := ""
		if taskModel != nil {
			taskName = taskModel.Name
		}

		type instanceResult struct {
			taskRunID uuid.UUID
			partition string
			output    map[string]string
			// skippedTasks are CATALOG task ids the store skipped downstream when
			// this instance resolved the group. They are what the run loop wants
			// back — never instance primary keys, which it would miscount as DAG
			// nodes in terminalTasks.
			skippedTasks []uuid.UUID
			// identityHash is this instance's own cache identity, collected so the
			// group can fold one aggregate hash for downstream steps.
			identityHash string
			// retry is set when the attempt failed but the instance has attempts
			// left; the row has already been reset to pending.
			retry bool
			err   error
		}

		var (
			byPartition = make(map[string]map[string]string)
			// skippedTaskIDs are catalog task ids to hand back to the run loop.
			skippedTaskIDs []uuid.UUID
			seenSkipped    = make(map[uuid.UUID]bool)
			hashByInstance = make(map[uuid.UUID]string, len(group.Instances))
			inFlight       int
			sawFailure     bool
			// rateLimitFailed records that acquiring a rate-limit token itself
			// errored (a store/limiter fault, not a rejection). It is separate
			// from sawFailure because a rejected acquisition is normal and a
			// broken one must not leave the group parked for a whole window
			// waiting on a decision nothing is going to make.
			rateLimitFailed bool
			firstErr        error
			results         = make(chan instanceResult, len(group.Instances))
			attempts        = make(map[uuid.UUID]int, len(group.Instances))
			running         = make(map[uuid.UUID]bool, len(group.Instances))
		)

		// dispatch runs one attempt of one instance. It owns every terminal write
		// for that instance.
		dispatch := func(taskRunID uuid.UUID, m instanceMeta, attempt int) {
			partEnv := map[string]string{
				envName: m.partition.Key,
			}
			if raw, err := m.partition.CanonicalJSON(); err == nil {
				partEnv[jobdefschema.FanOutPartitionJSONEnv] = string(raw)
			}
			extra := make(map[string]string, len(outputEnv)+len(partEnv))
			for k, v := range outputEnv {
				extra[k] = v
			}
			for k, v := range partEnv {
				extra[k] = v
			}

			// Per-partition identity: the shared args plus this instance's
			// partition fields. The partition env above is deliberately NOT part
			// of the hash — dependsOn rides inside CAESIUM_PARTITION_JSON and is
			// a scheduling instruction, not a data input.
			//
			// The identity is computed and persisted whether or not caching is
			// enabled: it is what makes a partition addressable to `caesium
			// receipt get`, `caesium why --partition` and `run retry
			// --partition`. Caching is only one consumer of it, and gates just
			// the lookup and the publish below.
			args := hashArgs
			args.Partition = m.partition.Key
			args.PartitionFingerprint = m.partition.Fingerprint
			args.PartitionAttributes = m.partition.Attributes
			hashInput := buildTaskHashInput(args)
			inputHash := hashInput.Compute()
			hashInputBlob, blobErr := hashInput.CanonicalJSON(inputHash)
			if blobErr != nil {
				log.Warn("failed to serialize hash-input blob", "task", taskName, "partition", m.partition.Key, "error", blobErr)
				hashInputBlob = nil
			}
			// SetTaskHashWithBlob resolves its second argument through
			// loadTaskRunByIDOrUnique, so the instance's TaskRun id addresses
			// exactly this row — the same primary-key-or-task-id contract
			// StartTask/SetTaskExitCode already take.
			if err := store.SetTaskHashWithBlob(runID, taskRunID, inputHash, args.ResolvedImageDigest, hashInputBlob); err != nil {
				log.Warn("failed to persist partition hash", "task", taskName, "partition", m.partition.Key, "error", err)
			}
			if err := store.UpdateTaskExecutionDescriptorInputs(runID, taskRunID, predOutputsByID, predHashByID, inputHash, args.ResolvedImageDigest, hashInputBlob); err != nil {
				log.Warn("failed to persist partition descriptor inputs", "task", taskName, "partition", m.partition.Key, "error", err)
			}

			// This cache-hit site — a FANNED INSTANCE resolving its OWN
			// per-partition result — is deliberately NOT gated like the two
			// producer-facing cache-hit sites in this file and in
			// internal/worker/runtime_executor.go (see F7,
			// run.Store.HasFanOutSuccessor): a fanned instance can never itself
			// be a fan-out PRODUCER, because pkg/jobdef/definition.go's step
			// validation rejects chained fan-out ("fanOut.from %q is itself a
			// fanOut step") for every job accepted through
			// internal/jobdef/importer.go, which is the only writer of
			// Task.FanOutConfig. So entry.Partitions is never consulted here,
			// and there is no downstream group this hit could silently collapse.
			// If chained fan-out is ever allowed, this invariant breaks and this
			// site needs the same gate.
			if cacheCfg.Enabled {
				if attempt == 1 {
					if entry, found, err := getCacheStore().Get(inputHash); err != nil {
						log.Warn("cache lookup failed", "task", taskName, "partition", m.partition.Key, "error", err)
					} else if found {
						if !taskQuarantined {
							metrics.TaskCacheHitsTotal.WithLabelValues(j.alias, taskName).Inc()
						}
						cacheRes, cacheErr := store.CacheHitTask(runID, taskRunID, run.CacheHitSource{
							RunID:     entry.RunID,
							CreatedAt: entry.CreatedAt,
							ExpiresAt: entry.ExpiresAt,
						}, entry.Result, entry.Output, entry.BranchSelections)
						if cacheErr != nil {
							log.Error("failed to apply partition cache hit", "task", taskName, "partition", m.partition.Key, "error", cacheErr)
							// Fall through to normal execution.
						} else {
							var hitErr error
							if !run.IsSuccessfulTaskResult(entry.Result) {
								hitErr = fmt.Errorf("partition %q failed with cached result %q", m.partition.Key, entry.Result)
							}
							var hitSkipped []uuid.UUID
							if cacheRes != nil {
								hitSkipped = cacheRes.SkippedTaskIDs
							}
							results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, output: entry.Output, skippedTasks: hitSkipped, identityHash: inputHash, err: hitErr}
							return
						}
					} else if !taskQuarantined {
						metrics.TaskCacheMissesTotal.WithLabelValues(j.alias, taskName).Inc()
					}
				}
			}

			taskCtx := ctx
			cancel := func() {}
			if taskTimeout > 0 {
				taskCtx, cancel = context.WithTimeout(ctx, taskTimeout)
			}
			result, output, branches, _, logSnapshot, execErr := executeAtom(taskCtx, taskID, taskRunID, attempt, runner, extra)
			cancel()

			if execErr == nil && taskModel != nil {
				// Record violations on THIS INSTANCE's row. SaveSchemaViolations
				// refuses a catalog task id that resolves to N siblings and only
				// logs the refusal, so keying on taskModel.ID meant a fanned step
				// recorded nothing: fail mode lost the evidence for the failure
				// it was reporting, and warn mode opened an incident with no row.
				if err := run.ValidateTaskOutputSchemaInstance(store, runID, taskModel.ID, taskRunID, output, taskModel.OutputSchema, j.schemaValidation); err != nil {
					execErr = err
				}
			}

			if execErr != nil {
				_ = store.SaveTaskLogSnapshot(runID, taskRunID, logSnapshot)
				// Retries cover execution errors only, matching the unfanned
				// local path: a container that ran and exited non-zero is a
				// terminal result, not a transient fault.
				if attempt < m.maxAttempt {
					if !taskQuarantined {
						metrics.TaskRetriesTotal.WithLabelValues(j.alias, taskID.String(), strconv.Itoa(attempt)).Inc()
					}
					if retryErr := store.RetryTaskInstance(runID, taskRunID, attempt+1); retryErr != nil {
						log.Error("failed to persist partition retry state", "run_id", runID, "partition", m.partition.Key, "error", retryErr)
					} else {
						log.Info("retrying partition", "job_id", j.id, "task_id", taskID, "partition", m.partition.Key, "attempt", attempt, "next_attempt", attempt+1, "error", execErr)
						results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, retry: true, err: execErr}
						return
					}
				}
				// FailTask persists the real cause on this instance's row and
				// runs the transitive in-group skip cascade. CompleteTaskInstance
				// would stamp the canned "command exited with non-zero status".
				if persistErr := store.FailTaskInstance(runID, taskRunID, execErr); persistErr != nil {
					log.Error("failed to persist partition failure", "run_id", runID, "partition", m.partition.Key, "error", persistErr)
				}
				log.Error("partition execution failed", "job_id", j.id, "task_id", taskID, "partition", m.partition.Key, "error", execErr)
				results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: execErr}
				return
			}

			completeRes, completeErr := store.CompleteTaskInstance(taskRunID, result, output, branches, nil)
			if completeErr != nil {
				results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: completeErr}
				return
			}
			_ = store.SaveTaskLogSnapshot(runID, taskRunID, logSnapshot)
			var completeSkipped []uuid.UUID
			if completeRes != nil {
				completeSkipped = completeRes.SkippedTaskIDs
			}

			if !run.IsSuccessfulTaskResult(result) {
				results <- instanceResult{
					taskRunID:    taskRunID,
					partition:    m.partition.Key,
					skippedTasks: completeSkipped,
					err:          fmt.Errorf("partition %q failed with result %q", m.partition.Key, result),
				}
				return
			}

			// Publish the successful result so a later run of the same partition
			// set is a hit. Quarantined replays never publish.
			if cacheCfg.Enabled && inputHash != "" {
				if taskQuarantined {
					log.Info("quarantined partition skipped cache publication", "task", taskName, "partition", m.partition.Key)
				} else {
					expiresAt := cache.EntryExpiry(time.Now(), cacheCfg.TTL, cacheCfg.TTLNever)
					if putErr := getCacheStore().Put(&cache.Entry{
						Hash:                inputHash,
						JobID:               j.id,
						TaskName:            taskName,
						Result:              result,
						Output:              output,
						BranchSelections:    branches,
						RunID:               runID,
						TaskRunID:           taskRunID,
						ResolvedImageDigest: hashArgs.ResolvedImageDigest,
						HashInputBlob:       hashInputBlob,
						CreatedAt:           time.Now(),
						ExpiresAt:           expiresAt,
					}); putErr != nil {
						log.Warn("failed to store partition cache entry", "task", taskName, "partition", m.partition.Key, "error", putErr)
					}
				}
			}

			results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, output: output, skippedTasks: completeSkipped, identityHash: inputHash}
		}

		// absorb folds one reported instance result into the group's bookkeeping.
		// Shared by the loop and by the cancellation drain below so a cancelled
		// group still collects the identities and outputs of instances that DID
		// finish — the fan-in aggregate is rebuilt from them.
		absorb := func(res instanceResult) {
			inFlight--
			delete(running, res.taskRunID)
			for _, id := range res.skippedTasks {
				if !seenSkipped[id] {
					seenSkipped[id] = true
					skippedTaskIDs = append(skippedTaskIDs, id)
				}
			}
			if res.identityHash != "" {
				hashByInstance[res.taskRunID] = res.identityHash
			}
		}

		for {
			rows, err := store.TaskRunInstances(ctx, runID, taskID)
			if err != nil {
				if ctx.Err() == nil {
					return skippedTaskIDs, err
				}
				// The run was cancelled out from under the loop, and this read
				// carries the run's context, so it fails before a single row has
				// been examined. Returning here is what left a cancelled run's
				// instances stranded even after the SWEEP was detached: the sweep
				// was never reached. Drain what is still in flight — those
				// containers' contexts are cancelled too, so they resolve
				// promptly — and fall through to the sweep, which runs detached
				// and is the only thing that will resolve what was never
				// dispatched.
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				sawFailure = true
				for inFlight > 0 {
					absorb(<-results)
				}
				break
			}

			terminal := 0
			var ready []*run.TaskRun
			// rateLimitedUntil is the earliest moment a parked instance becomes
			// dispatchable again. RateLimitTask persists the deadline on the row,
			// so a parked instance is recognizable across loop passes without any
			// in-memory bookkeeping — the same way readiness is read, not tracked.
			var rateLimitedUntil time.Time
			noteRateLimited := func(at time.Time) {
				if at.IsZero() {
					return
				}
				if rateLimitedUntil.IsZero() || at.Before(rateLimitedUntil) {
					rateLimitedUntil = at
				}
			}
			now := time.Now().UTC()
			for _, row := range rows {
				if row == nil {
					continue
				}
				if run.IsTerminal(row.Status) {
					terminal++
					continue
				}
				if running[row.ID] {
					continue
				}
				// Readiness is the store's scalar, seeded at expansion and
				// decremented by each terminal sibling.
				if row.Status == run.TaskStatusPending && row.OutstandingPredecessors == 0 {
					if row.RateLimitRetryAfter != nil && row.RateLimitRetryAfter.After(now) {
						noteRateLimited(*row.RateLimitRetryAfter)
						continue
					}
					ready = append(ready, row)
				}
			}

			if terminal == len(rows) && inFlight == 0 {
				break
			}

			if !failFast || !sawFailure {
				for _, row := range ready {
					if inFlight >= groupParallel {
						break
					}
					m, ok := meta[row.ID]
					if !ok {
						// An instance row the expansion payload did not describe
						// cannot be executed safely; leave it to the store.
						continue
					}
					// One token per INSTANCE, acquired before the container is
					// launched and against the step's own rule. The token is not
					// taken for the group at dispatch time, so a `2 per minute`
					// rule admits two partitions a minute here exactly as it does
					// under the claimer and the owner dispatcher.
					acquired, retryAfter, rlErr := acquireRateLimitFor(taskID, row.ID, m.partition.Key)
					if rlErr != nil {
						if firstErr == nil {
							firstErr = rlErr
						}
						sawFailure = true
						rateLimitFailed = true
						break
					}
					if !acquired {
						noteRateLimited(retryAfter)
						continue
					}
					attempts[row.ID]++
					running[row.ID] = true
					inFlight++
					go dispatch(row.ID, m, attempts[row.ID])
				}
			}

			if inFlight == 0 {
				if !rateLimitedUntil.IsZero() && !rateLimitFailed && (!failFast || !sawFailure) {
					// Every dispatchable instance is parked behind the rate-limit
					// window. Waiting here is what keeps the group alive: breaking
					// out would hand still-runnable partitions to the straggler
					// sweep, which resolves them as "never dispatched" and fails a
					// run whose only problem was that it was going too fast.
					wait := time.Until(rateLimitedUntil)
					if wait <= 0 {
						continue
					}
					timer := time.NewTimer(wait)
					select {
					case <-ctx.Done():
						timer.Stop()
						if firstErr == nil {
							firstErr = ctx.Err()
						}
					case <-timer.C:
						timer.Stop()
						continue
					}
				}
				// Nothing running and nothing dispatchable. Either fail_fast has
				// tripped, or the remaining instances are blocked behind siblings
				// the store has already resolved. Either way the straggler sweep
				// after the loop resolves whatever is left.
				break
			}

			res := <-results
			absorb(res)

			if res.retry {
				// The row is pending again; the next loop pass re-reads it.
				continue
			}
			if res.err != nil {
				sawFailure = true
				if firstErr == nil {
					firstErr = res.err
				}
				continue
			}
			if len(res.output) > 0 {
				byPartition[res.partition] = res.output
			}
		}

		// The group node is about to be reported terminal to the run loop, so no
		// instance row may be left non-terminal: an orphan pending row keeps the
		// run's own accounting (and any downstream group-status read) waiting on
		// something nothing will ever dispatch. Resolve stragglers explicitly and
		// say so, rather than leaving the run to time out. In local mode there is
		// no recovery owner to revisit the row later, so "leave it and let
		// recovery sort it out" is not an available option here.
		//
		// DO NOT "harmonize" the store primitive this calls with the one the
		// fail-fast cascade calls. SkipTaskInstance resolves any non-terminal row
		// (internal/run/store_instance.go); markInstanceSkippedTx, used by
		// failFastSkipSiblingsTx, is deliberately pending-only. They look like an
		// inconsistency and are not one: the guard belongs to the caller's
		// knowledge, not to the primitive. This sweep has drained every in-flight
		// instance and therefore KNOWS the containers are over; the SQL cascade
		// does not, because a distributed worker may still POST a completion, and
		// resolving a live row there would invite that worker to contradict it.
		// Same operation, opposite epistemic position, correctly different guards.
		//
		// The sweep runs on a DETACHED context, and that is the whole reason it
		// still works when it is needed most. It used to read through the run's
		// own ctx, so a cancelled run made the very first query return
		// context.Canceled and the sweep returned before resolving anything —
		// exactly the "stranded for good" outcome the paragraph above says is not
		// an available option in local mode. Cancellation is not a reason to skip
		// the cleanup; it is the most common reason to need it. The timeout keeps
		// a detached context from becoming an unbounded one if the DB is wedged.
		sweepCtx, cancelSweep := context.WithTimeout(context.WithoutCancel(ctx), fanOutSweepTimeout)
		defer cancelSweep()

		// Captured once, before the writes below, so every row in this sweep gets
		// the same explanation. ctx here is the RUN's context, not sweepCtx.
		runCancelled := ctx.Err() != nil

		rows, err := store.TaskRunInstances(sweepCtx, runID, taskID)
		if err != nil {
			return skippedTaskIDs, err
		}
		var stranded []string
		var unrecorded []string
		cancelledUnresolved := 0
		for _, row := range rows {
			if row == nil || run.IsTerminal(row.Status) {
				continue
			}
			// A row still RUNNING here is a different animal from a pending one.
			// Both loop exits above are gated on inFlight == 0 and the only path
			// past the bottom of the loop is `res := <-results`, so every
			// instance this group dispatched has already reported: the container
			// is provably over. Still-running therefore means the completion
			// WRITE failed, not that the work was cancelled — and the container
			// may well have SUCCEEDED. Blaming that row on the group's failure
			// policy puts a confident, wrong explanation on the one instance
			// whose outcome is genuinely unknown, and that string is what
			// `caesium run partitions` and `caesium why --partition` display.
			//
			// It is still RESOLVED rather than left alone: local mode has no
			// recovery owner to revisit it, so an unresolved row is stranded for
			// good and hangs the accounting this sweep exists to protect. Say the
			// true thing about it instead of leaving it.
			wasRunning := row.Status == run.TaskStatusRunning
			// A row still parked behind its rate-limit window is a third animal
			// again: it was dispatchable, it was deliberately held back, and the
			// run ended (cancelled, timed out) before the window rolled. Calling
			// that "never dispatched (unresolved in-group dependency)" sends the
			// reader hunting a dependsOn bug that does not exist.
			rateLimited := !wasRunning && row.RateLimitRetryAfter != nil && row.RateLimitRetryAfter.After(time.Now().UTC())
			// The status stays SKIPPED even for a cancelled run: these instances
			// never ran, so `failed` — what the unfanned local path stamps on the
			// task it was actually executing — would be a lie about work that
			// never started, and would drag the failure accounting and the
			// in-group cascade along with it. Only the REASON mirrors that path,
			// so both lanes read the same way in `caesium run partitions`.
			reason := "fan-out instance was never dispatched (unresolved in-group dependency)"
			switch {
			case wasRunning:
				reason = "fan-out instance outcome unrecorded: completion write failed"
			case failFast && sawFailure:
				reason = "fan-out group failed fast"
			case runCancelled && rateLimited:
				reason = fmt.Sprintf("fan-out instance was parked by the step's rate limit when the run was cancelled: %v", ctx.Err())
			case runCancelled:
				reason = fmt.Sprintf("fan-out instance cancelled before dispatch: %v", ctx.Err())
			case rateLimited:
				reason = "fan-out instance still parked by the step's rate limit when the run ended"
			}
			// Note this resolves an INSTANCE row; its id is deliberately not
			// added to skippedTaskIDs, which the run loop reads as catalog task
			// ids and counts against the DAG's node total.
			if skipErr := store.SkipTaskInstance(runID, row.ID, reason); skipErr != nil {
				return skippedTaskIDs, skipErr
			}
			switch {
			case wasRunning:
				unrecorded = append(unrecorded, row.PartitionValue)
			case runCancelled:
				// Counted, not "stranded": the run was cancelled, and reporting
				// these as unresolved partitions would bury the one fact that
				// explains all of them under a list of symptoms.
				cancelledUnresolved++
			case !failFast || !sawFailure:
				stranded = append(stranded, row.PartitionValue)
			}
		}
		// Logged unconditionally, unlike the stranded case below: a lost outcome
		// is worth surfacing whatever the failure policy, and gating it on
		// !failFast is how it stayed invisible.
		if len(unrecorded) > 0 {
			log.Error("fan-out instances completed without a recorded outcome", "job_id", j.id, "task_id", taskID, "partitions", unrecorded)
			if firstErr == nil {
				firstErr = fmt.Errorf("fan-out step %s left %d partition(s) with an unrecorded outcome: %v", taskID, len(unrecorded), unrecorded)
			}
		}
		if len(stranded) > 0 {
			log.Error("fan-out instances were never dispatched", "job_id", j.id, "task_id", taskID, "partitions", stranded)
			if firstErr == nil {
				firstErr = fmt.Errorf("fan-out step %s left %d partition(s) unresolved: %v", taskID, len(stranded), stranded)
			}
		}
		// Only when the cancellation actually left work unresolved. A group whose
		// every instance had already finished when the run was cancelled did not
		// fail, and manufacturing an error for it would turn a clean group into a
		// failed one on the way out.
		if cancelledUnresolved > 0 && firstErr == nil {
			firstErr = fmt.Errorf("fan-out step %s left %d partition(s) unresolved when the run was cancelled: %w",
				taskID, cancelledUnresolved, ctx.Err())
		}

		// Aggregate from the store so cache hits, skips and failures are all
		// reflected, not just what this loop executed.
		//
		// The instance ROWS are the aggregate's source of truth, not the
		// byPartition/hashByInstance maps above: those only ever describe the
		// instances THIS invocation dispatched. After a manual partition retry
		// (`caesium run retry --partition`) or a RetryFromFailure that preserved
		// the succeeded siblings, that is a single instance — so the rebuilt
		// fan-in aggregate reported PARTITION_COUNT=1 for an N-partition group
		// and the group identity hash folded one instance instead of N, re-keying
		// every downstream step purely because someone retried a partition.
		// Hydrating from the rows makes a retried run's aggregate and group hash
		// identical to a fresh run's.
		// sweepCtx, not ctx: this rebuild is the other half of the cleanup above
		// and is just as necessary on a cancelled run — a downstream step's
		// predecessor hashes and fan-in aggregate must not silently vanish
		// because the run's context died between the last instance and here.
		identities, err := store.FanOutInstanceIdentities(sweepCtx, runID, taskID)
		if err != nil {
			return skippedTaskIDs, err
		}
		succeeded, failed := 0, 0
		// groupHashes are the terminal-success instances' identities in
		// partition-index order (FanOutInstanceIdentities orders by
		// partition_index), the order run.GroupIdentityHash requires.
		var groupHashes []string
		for _, inst := range identities {
			switch inst.Status {
			case run.TaskStatusSucceeded, run.TaskStatusCached:
				succeeded++
				// The persisted (effective) identity is preferred over this
				// invocation's computed one so the value folded here is
				// byte-identical to what the SQL read path
				// (store.PredecessorHashes) folds for the same group. The
				// in-memory value is the fallback for the narrow case where
				// persisting the hash failed.
				h := inst.IdentityHash
				if h == "" {
					h = hashByInstance[inst.TaskRunID]
				}
				if h != "" {
					groupHashes = append(groupHashes, h)
				}
				if _, seen := byPartition[inst.PartitionValue]; !seen && len(inst.Output) > 0 {
					byPartition[inst.PartitionValue] = inst.Output
				}
			case run.TaskStatusFailed:
				failed++
			}
		}
		// An aggregate that does not fit MaxOutputBytes fails the GROUP rather
		// than publishing a partial contract: silently collapsing to the three
		// counters would drop every user key a downstream step reads.
		aggregate, aggErr := pkgtask.AggregateFanInOutputs(taskName, byPartition, succeeded, failed)
		if aggErr != nil {
			log.Error("failed to aggregate fan-in outputs", "job_id", j.id, "task_id", taskID, "error", aggErr)
			if firstErr == nil {
				firstErr = aggErr
			}
		} else {
			taskOutputs[taskID] = aggregate
		}

		// Publish ONE aggregate identity for the group so a downstream step folds
		// the fanned predecessor into its own cache key as a single
		// PredecessorHashes entry — the same value the SQL read path
		// (store.PredecessorHashes) computes for the distributed lane. Without
		// this the local lane contributed nothing for a fanned predecessor, so a
		// downstream step's identity was blind to its input changing.
		if h := run.GroupIdentityHash(groupHashes); h != "" {
			taskHashes[taskID] = h
		}

		return skippedTaskIDs, firstErr
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

		if group, ok := lookupFanOutGroup(taskID); ok && len(group.Instances) > 0 {
			return runFannedGroup(taskID, runner, taskModel, group, outputEnv, predOutputs, predOutputsByID)
		}

		// Cache check — attempt to bypass container execution.
		var inputHash string
		// hashInputBlob is the canonical secret-redacted decomposition of the
		// HashInput; declared here (like inputHash) so it survives into the
		// success path where it is also written onto the cache Entry, letting a
		// cache hit be explained as well as a re-run.
		var hashInputBlob []byte

		// The same resolver every fan-out instance uses, so the two paths cannot
		// drift on which fields are folded into the cache key.
		cacheCfg, hashArgs, predHashByID := resolveTaskCacheIdentity(taskID, taskModel, runner, outputEnv, predOutputs)
		// resolvedImageDigest is the content digest folded into inputHash when
		// pinning is on; empty otherwise. Reused when the result is cached so
		// the cache Entry records which image content the hash covers.
		resolvedImageDigest := hashArgs.ResolvedImageDigest

		if cacheCfg.Enabled {
			cacheStore := getCacheStore()
			taskName := ""
			if taskModel != nil {
				taskName = taskModel.Name
			}

			hashInput := buildTaskHashInput(hashArgs)
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
			// A cache entry with no recorded partition list (nil, not merely
			// empty — see cache.Entry.Partitions) is ambiguous for a task with a
			// downstream fan-out consumer: it might be a pre-fan-out entry (or
			// one written before the parser learned to record an explicit `[]`)
			// that never had the chance to record one. Treat that combination as
			// a MISS, exactly like `found` were false, so the producer runs once
			// more and backfills a real (possibly still-empty) list. Ordinary
			// tasks are unaffected: HasAnyFanOutConsumerForRun/HasFanOutSuccessor
			// are false for them, so a legitimately partition-less entry keeps
			// hitting as before.
			//
			// BOTH lookup errors below fail CLOSED — treat the hit as a MISS, exactly
			// as a cacheStore.Get error already does in this same block. An
			// inconclusive answer from either query would otherwise let an
			// unrecorded-partitions entry resolve "cached" on a run that DOES use
			// fan-out: the group expands to nothing and the consumer is silently
			// skipped via onEmpty — the exact collapse this gate exists to prevent.
			// The cost of failing closed is one extra execution of this task on a
			// rare transient read error; the cost of failing open is a silently
			// empty group. (The per-run pre-filter is still what keeps ordinary
			// hits cheap — it only changes what happens when that query ERRORS.)
			if found && entry.Partitions == nil {
				hasAnyFanOut, hafErr := store.HasAnyFanOutConsumerForRun(runID)
				switch {
				case hafErr != nil:
					log.Warn("cache: failed to check whether this run uses fan-out; treating the hit as unusable (fail closed)",
						"task", taskName, "hash", inputHash[:12], "error", hafErr)
					found = false
				case hasAnyFanOut:
					hasConsumer, hcErr := store.HasFanOutSuccessor(runID, taskID)
					switch {
					case hcErr != nil:
						log.Warn("cache: failed to check for a fan-out consumer; treating the hit as unusable (fail closed)",
							"task", taskName, "hash", inputHash[:12], "error", hcErr)
						found = false
					case hasConsumer:
						log.Info("cache: no partition list recorded on the cache entry; re-running producer to record one",
							"task", taskName, "hash", inputHash[:12])
						found = false
					}
				}
			}
			switch {
			case err != nil:
				log.Warn("cache lookup failed", "task", taskName, "error", err)
			case found:
				if !taskQuarantined {
					metrics.TaskCacheHitsTotal.WithLabelValues(j.alias, taskName).Inc()
				}
				log.Info("cache hit", "task", taskName, "hash", inputHash[:12])

				// A fan-out producer's partition list is part of what its
				// execution produced, so it rides the cache entry and must be
				// replayed into the cache-hit transaction: without it the
				// producer resolves, the consumer's group never expands, and the
				// fanned step silently collapses to its unexpanded template row.
				cacheResult, cacheErr := applyCacheHit(store, runID, taskID, run.CacheHitSource{
					RunID:     entry.RunID,
					CreatedAt: entry.CreatedAt,
					ExpiresAt: entry.ExpiresAt,
				}, entry.Result, entry.Output, entry.BranchSelections, entry.Partitions)
				if cacheErr != nil {
					log.Error("failed to apply cache hit", "task", taskName, "error", cacheErr)
					// Fall through to normal execution.
				} else {
					registerExpansion(cacheResult)
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

			result, output, branchNames, partitions, logSnapshot, execErr := executeAtom(taskCtx, taskID, uuid.Nil, attempt, runner, outputEnv)
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
				completeResult, completeErr := store.CompleteTaskWithPartitions(runID, taskID, result, output, branchNames, partitions)
				if completeErr != nil {
					return nil, completeErr
				}
				registerExpansion(completeResult)
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

						expiresAt := cache.EntryExpiry(time.Now(), cacheCfg.TTL, cacheCfg.TTLNever)
						if putErr := cacheStore.Put(&cache.Entry{
							Hash:             inputHash,
							JobID:            j.id,
							TaskName:         taskName,
							Result:           result,
							Output:           output,
							BranchSelections: branchNames,
							// A fan-out producer's emitted partition list is part
							// of what its execution produced: without it a cache
							// hit on the producer would resolve the step without
							// ever expanding its consumer's group.
							Partitions:          partitions,
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
		// An EXPANDED fan-out step acquires its tokens per instance inside
		// runFannedGroup, not once here for the whole group. Acquiring here would
		// be wrong twice over: one token would admit all N partitions, and the
		// rejection path would park the row by catalog task id — which names N
		// rows, so RateLimitTask returns ErrAmbiguousTaskRun and this dispatch
		// error halts the entire run.
		if group, ok := lookupFanOutGroup(taskID); !ok || len(group.Instances) == 0 {
			acquired, retryAfter, err := acquireRateLimitFor(taskID, taskID, "")
			if err != nil {
				return err
			}
			if !acquired {
				deferred[taskID] = retryAfter
				return nil
			}
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

	if terminalTasks != liveTaskCount {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("job %s reached terminal state for %d of %d tasks; remaining tasks may be waiting on unresolved dependencies", j.id, terminalTasks, liveTaskCount)
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

func (j *job) finishRun(
	ctx context.Context,
	store *run.Store,
	runID uuid.UUID,
	quarantined bool,
	runErr error,
	leaseVersion run.LeaseVersion,
) {
	resolvedErr, dispatchCallbacks, err := finalizeRunForCallbacks(store, runID, runErr, leaseVersion)
	if err != nil {
		log.Error("run completion persistence failure", "run_id", runID, "error", err)
		return
	}
	if !dispatchCallbacks {
		log.Warn("run completion superseded by owner state refresh", "run_id", runID)
		return
	}
	if quarantined {
		return
	}
	dispatchCtx := context.WithoutCancel(ctx)
	if err := j.dispatchRunCallbacks(dispatchCtx, j.id, runID, resolvedErr); err != nil {
		log.Error("callback dispatch failure", "job_id", j.id, "run_id", runID, "error", err)
	}
}

// finalizeRunForCallbacks closes the durable run and decides whether this job
// invocation still owns callback delivery for the logical execution it
// observed. Ownership generation can change during a SQL-owner takeover without
// changing that execution, so the waiter finalizes against StateRevision alone
// under the lease row lock. A retry changes StateRevision; waiters from the
// previous revision must never deliver the retried execution's callback.
func finalizeRunForCallbacks(
	store *run.Store,
	runID uuid.UUID,
	runErr error,
	observed run.LeaseVersion,
) (resolvedErr error, dispatchCallbacks bool, persistErr error) {
	if observed.StateRevision <= 0 {
		if err := store.Complete(runID, runErr); err != nil {
			return runErr, false, err
		}
		return runErr, true, nil
	}

	disposition, err := store.CompleteAtStateRevision(runID, runErr, observed.StateRevision)
	if err != nil {
		return runErr, false, err
	}
	switch disposition {
	case run.CompletionFinalized:
		return runErr, true, nil
	case run.CompletionAlreadyTerminal:
		return durableTerminalRunCallbackOutcome(store, runID, runErr)
	case run.CompletionSuperseded:
		return runErr, false, nil
	case run.CompletionLeaseMissing:
		return terminalRunCallbackOutcome(store, runID, runErr)
	default:
		return runErr, false, fmt.Errorf("finalize run %s: unknown completion disposition %d", runID, disposition)
	}
}

// durableTerminalRunCallbackOutcome derives callback content from the winning
// durable terminal row. This matters when another owner finalized first: the
// losing waiter must not publish its stale local result (for example, success
// after the durable winner recorded failure).
func durableTerminalRunCallbackOutcome(store *run.Store, runID uuid.UUID, fallback error) (error, bool, error) {
	current, err := store.Get(runID)
	if err != nil {
		return fallback, false, err
	}
	switch current.Status {
	case run.StatusSucceeded:
		return nil, true, nil
	case run.StatusFailed, run.StatusCancelled:
		if current.Error != "" {
			return errors.New(current.Error), true, nil
		}
		return fmt.Errorf("run %s %s", runID, current.Status), true, nil
	default:
		return fallback, false, nil
	}
}

// terminalRunCallbackOutcome handles cancellation, whose transaction removes
// the lease after persisting the terminal JobRun. With no lease there is no
// revision to retry against, so the durable terminal row supplies the callback
// result. A nonterminal row remains suppressed.
func terminalRunCallbackOutcome(store *run.Store, runID uuid.UUID, fallback error) (error, bool, error) {
	current, err := store.Get(runID)
	if err != nil {
		return fallback, false, err
	}
	if current.Status != run.StatusCancelled {
		// Cancellation is the only sanctioned terminal path that deletes the
		// lease. Treating an unversioned succeeded/failed row as callback-eligible
		// could let an old waiter claim a newer no-lease execution's outcome.
		return fallback, false, nil
	}
	if current.Error != "" {
		return errors.New(current.Error), true, nil
	}
	return fmt.Errorf("run %s %s", runID, current.Status), true, nil
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
			Types: []event.Type{event.TypeRunTerminal, event.TypeRunCompleted, event.TypeRunFailed, event.TypeRunCancelled},
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
			if evt.Type == event.TypeRunFailed || evt.Type == event.TypeRunCancelled ||
				evt.Type == event.TypeRunCompleted || evt.Type == event.TypeRunTerminal {
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
				if snapshot.Status == run.StatusCancelled {
					if snapshot.Error != "" {
						return errors.New(snapshot.Error)
					}
					return fmt.Errorf("run %s cancelled", runID)
				}
				if snapshot.Status == run.StatusSucceeded {
					return nil
				}
				// Events are at-least-once and may have been queued before a retry
				// transaction reopened the run. A currently nonterminal snapshot is
				// authoritative: ignore the stale terminal event and keep waiting.
				continue
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
			cancelled := 0

			liveCount := len(snapshot.Tasks)
			if liveCount < taskCount {
				liveCount = taskCount
			}

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
				case run.TaskStatusCancelled:
					cancelled++
				}
			}

			if snapshot.Status == run.StatusCancelled {
				if snapshot.Error != "" {
					return errors.New(snapshot.Error)
				}
				return fmt.Errorf("run %s cancelled", runID)
			}

			terminal := failed + succeeded + skipped + cached + cancelled
			if terminal == liveCount {
				if cancelled > 0 {
					return fmt.Errorf("run %s cancelled", runID)
				}
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
