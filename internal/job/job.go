package job

import (
	"context"
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
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	secretLogDrainTimeout = run.SecretLogDrainTimeout
	secretLogAbortTimeout = run.SecretLogAbortTimeout
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

var errUnresolvedIdentityTerminalWrite = errors.New("unresolved image identity failure could not be terminalized")

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
// contentionRetrier retries idempotent work that fails on transient dqlite
// contention. Do is a generic method so callers can return a value from the
// retried function instead of closing over an outer variable.
type contentionRetrier struct {
	backoffs []time.Duration
}

func (r contentionRetrier) Do[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		value, err := fn()
		if err == nil || !dqlite.IsContentionError(err) || attempt >= len(r.backoffs) {
			return value, err
		}
		base := r.backoffs[attempt]
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
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
}

func retryOnContention(ctx context.Context, fn func() error) error {
	_, err := retryOnContentionDo(ctx, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

func retryOnContentionDo[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	return contentionRetrier{backoffs: runStartReadBackoffs}.Do(ctx, fn)
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
	params                 map[string]string
	runStoreFactory        func() *run.Store
	envVariables           func() env.Environment
	taskServiceFactory     func(context.Context) task.Task
	atomServiceFactory     func(context.Context) asvc.Atom
	taskEdgeServiceFactory func(context.Context) taskedge.TaskEdge
	dispatchRunCallbacks   func(context.Context, uuid.UUID, uuid.UUID, error) error
	newDockerEngine        func(context.Context) atom.Engine
	// newKubernetesEngine returns an error (instead of panicking) when the
	// kubernetes engine cannot be constructed — e.g. no reachable kubeconfig —
	// so the local attempt can surface a clean, actionable failure. See #479.
	newKubernetesEngine func(context.Context) (atom.Engine, error)
	newPodmanEngine     func(context.Context) atom.Engine
	atomPollInterval    time.Duration
	secretResolver      secret.Resolver
	// imageResolver is the digest resolver pinDigests uses. Nil falls back to
	// imagecheck.Default(); tests inject a stub so Create can be asserted
	// against a known digest without a registry.
	imageResolver *imagecheck.Resolver
	// beforeComplete is an unexported test seam for the window between an
	// engine deciding to finalize the run and the completion write beginning —
	// the DAG loop's shutdown window, and the aborted-resume finalizer's.
	// Production leaves it nil; keeping the hook before the store transaction
	// lets race regressions interleave RetryPartition deterministically without
	// database locks/sleeps.
	beforeComplete func(uuid.UUID)
	// rateLimitClock is an unexported test seam for the rate limiter's clock.
	// Production leaves it nil and the limiter reads the wall clock.
	//
	// ratelimit.Limiter is a FIXED-window limiter: it buckets on
	// now.Truncate(window), floored at one minute. A test that asserts "a
	// 2-per-minute rule admits exactly two of these four partitions" therefore
	// only holds while the whole dispatch pass stays inside one minute bucket —
	// a pass that straddles :00 gets a fresh bucket and admits two more. Under
	// `-race` that is a real, if rare, source of "the limiter admitted three".
	// Pinning the clock removes the boundary instead of sleeping past it.
	rateLimitClock func() time.Time
	// partitionRetryReplacementFor names the retry-reset instances a
	// replacement engine (startReplacementRun) was started to drive. It bounds
	// the completion fence: a replacement that still leaves THOSE instances
	// pending abandons them explicitly instead of spawning yet another
	// replacement, while a retry that lands in its own shutdown window is a
	// fresh request handed to a further engine. nil for every other engine.
	partitionRetryReplacementFor map[uuid.UUID]struct{}
	// dispatchedInstances records every fan-out instance this engine actually
	// launched a container for. A partition retry reuses the instance row, so
	// row identity alone cannot tell "the retry I was started for and could
	// not dispatch" from "a fresh retry of the same partition after I ran it
	// and it failed again"; whether THIS engine dispatched the row can.
	dispatchedInstancesMu sync.Mutex
	dispatchedInstances   map[uuid.UUID]struct{}
}

// noteInstanceDispatched records that this engine launched the instance.
func (j *job) noteInstanceDispatched(taskRunID uuid.UUID) {
	j.dispatchedInstancesMu.Lock()
	defer j.dispatchedInstancesMu.Unlock()
	if j.dispatchedInstances == nil {
		j.dispatchedInstances = make(map[uuid.UUID]struct{})
	}
	j.dispatchedInstances[taskRunID] = struct{}{}
}

// ownsUndispatchedRetry reports whether a still-pending retry-reset instance
// is one this replacement was started for and never managed to dispatch —
// the only kind it may abandon. Anything else pending is fresh work.
func (j *job) ownsUndispatchedRetry(taskRunID uuid.UUID) bool {
	if _, assigned := j.partitionRetryReplacementFor[taskRunID]; !assigned {
		return false
	}
	j.dispatchedInstancesMu.Lock()
	defer j.dispatchedInstancesMu.Unlock()
	_, dispatched := j.dispatchedInstances[taskRunID]
	return !dispatched
}

// handOffPendingPartitionRetries is the last resort of a bounded completion
// loop: the fence kept refusing while the loop's own scans found nothing to
// classify. Whatever is pending NOW — fresh or not — goes to a replacement
// engine, because returning would leave it pending on a run with no engine.
// Reports whether a replacement was started.
func (j *job) handOffPendingPartitionRetries(store *run.Store, runID uuid.UUID, params map[string]string) bool {
	pending, err := store.PendingPartitionRetries(runID)
	if err != nil {
		log.Error("run completion kept being refused and the pending partition retries could not be read",
			"job_id", j.id, "run_id", runID, "error", err)
		return false
	}
	if len(pending) == 0 {
		log.Error("run completion kept being refused with no pending partition retry visible; leaving the run for an operator",
			"job_id", j.id, "run_id", runID)
		return false
	}
	ids := make([]uuid.UUID, 0, len(pending))
	for i := range pending {
		ids = append(ids, pending[i].ID)
	}
	log.Error("run completion kept being refused; handing every pending partition retry to a replacement engine",
		"job_id", j.id, "run_id", runID, "instances", len(ids))
	j.startReplacementRun(runID, params, ids)
	return true
}

// recoverPendingPartitionRetries is the bounded end of the completion fence,
// shared by the normal completion path and the early-failure finalizer. It
// sorts the run's still-pending retry-reset instances into the ones THIS
// engine was started for and could not dispatch — resolved explicitly
// (skipped, reason on the row), since nothing about a further engine would
// differ — and fresh requests, which are handed to a replacement engine.
// handedOff is true when a replacement now owns the run's finalization; the
// returned error is the run error, possibly replaced by the abandon reason.
func (j *job) recoverPendingPartitionRetries(store *run.Store, runID uuid.UUID, params map[string]string, runErr error) (handedOff bool, updated error, err error) {
	pending, err := store.PendingPartitionRetries(runID)
	if err != nil {
		return false, runErr, err
	}
	var mine, fresh []uuid.UUID
	for i := range pending {
		if j.ownsUndispatchedRetry(pending[i].ID) {
			mine = append(mine, pending[i].ID)
		} else {
			fresh = append(fresh, pending[i].ID)
		}
	}
	// Abandon before handing off: a replacement started for the fresh rows
	// must not inherit rows this engine already failed to dispatch, or they
	// would bounce between engines instead of resolving.
	if len(mine) > 0 {
		reason := "partition retry abandoned: the replacement engine could not dispatch the reset instance; retry the run"
		abandoned, abandonErr := store.AbandonPartitionRetries(runID, mine, reason)
		log.Error("partition retry could not be dispatched by the replacement engine; abandoning it",
			"job_id", j.id, "run_id", runID, "abandoned", abandoned, "error", abandonErr)
		if abandonErr != nil {
			// The rows are still pending and marked. Handing them to yet
			// another engine would only repeat this failure; stop here and
			// let the caller report a run that needs an operator.
			return false, runErr, abandonErr
		}
		if runErr == nil {
			runErr = errors.New(reason)
		}
	}
	if len(fresh) > 0 {
		log.Info("partition retry landed after the DAG finished; starting replacement engine",
			"job_id", j.id, "run_id", runID, "instances", len(fresh))
		j.startReplacementRun(runID, params, fresh)
		return true, runErr, nil
	}
	return false, runErr, nil
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
		runStoreFactory:        run.Default,
		envVariables:           env.Variables,
		taskServiceFactory:     task.Service,
		atomServiceFactory:     asvc.Service,
		taskEdgeServiceFactory: taskedge.Service,
		dispatchRunCallbacks: func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error {
			return callback.Default().Dispatch(ctx, jobID, runID, runErr)
		},
		newDockerEngine:     func(ctx context.Context) atom.Engine { return docker.NewEngine(ctx) },
		newKubernetesEngine: func(ctx context.Context) (atom.Engine, error) { return kubernetes.NewEngine(ctx) },
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

// atomRunner is one local-lane task's execution recipe. Every field the TaskRun
// row freezes is read FROM that row, not from a live catalog read, so a
// re-entered run — `caesium run retry` after a `job apply` — executes what the
// run was registered with, exactly as the distributed worker does. See
// buildLocalRunners for the field-by-field correspondence.
type atomRunner struct {
	engineKind  models.AtomEngine
	image       string
	command     []string
	maxAttempts int
	taskTimeout time.Duration
	// cacheCfg is the cache configuration the SCHEDULER resolved at
	// RegisterTasks, rebuilt from the row's seven cache columns the way
	// runtimeExecutor.Execute rebuilds it. It must not be re-resolved from the
	// live step/job/env config: cacheCfg.Version and cacheCfg.Chain are folded
	// into the identity hash and cacheCfg.Enabled gates publication, so
	// re-resolving would give a retried task a different cache key — and a
	// different publish decision — depending on which lane ran it.
	cacheCfg jobdefschema.CacheConfig
	// outputSchema / schemaValidation are the frozen contract this task's output
	// is judged against, matching runtimeExecutor.runSchemaValidation. Reading
	// them live meant a retry after an apply that edited `outputSchema` or
	// flipped `metadata.schemaValidation` passed on one lane and failed on the
	// other.
	outputSchema     []byte
	schemaValidation string
	spec             container.Spec
	// Each execution attempt needs its own engine bound to that attempt's task
	// context. The runner recipe is shared by retry attempts and fan-out
	// siblings, so never store a context-bound engine on it.
	newEngine func(context.Context) (atom.Engine, error)
	// resolvedImageDigest is the content digest pinDigests resolved for this
	// task. Create pins the runtime image to it so a locally cached tag cannot
	// execute older content than the cache key recorded.
	resolvedImageDigest string
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
// Unlike the Docker/Podman factories, this one can fail (no reachable
// kubeconfig) and returns an error rather than panicking; buildLocalRunners
// surfaces that error as a clean run failure instead of an unrecovered panic.
func WithKubernetesEngineFactory(factory func(context.Context) (atom.Engine, error)) JobOption {
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

func (j *job) digestResolver() *imagecheck.Resolver {
	if j != nil && j.imageResolver != nil {
		return j.imageResolver
	}
	return imagecheck.Default()
}

// withPartitionRetryReplacement flags an engine as the replacement started
// for the given retry-reset instances, which landed in the previous engine's
// shutdown window.
func withPartitionRetryReplacement(taskRunIDs []uuid.UUID) JobOption {
	return func(j *job) {
		j.partitionRetryReplacementFor = make(map[uuid.UUID]struct{}, len(taskRunIDs))
		for _, id := range taskRunIDs {
			j.partitionRetryReplacementFor[id] = struct{}{}
		}
	}
}

// startReplacementRun kicks off a new in-process engine against an existing
// run, matching HTTP partition-retry kickoff: job.New → Run with the run id
// in context so the DAG rehydrates existing TaskRun rows (including a
// partition that RetryPartition reset after this engine left runFannedGroup).
// taskRunIDs are the retry-reset instances the replacement is responsible for.
func (j *job) startReplacementRun(runID uuid.UUID, params map[string]string, taskRunIDs []uuid.UUID) {
	go func() {
		// The replacement engine registers its own cancellable context against
		// the SAME run id: the registry holds a set per run, so cancelling the
		// run reaches this engine and the one that spawned it.
		cancelCtx, release := RegisterRunCancel(context.Background(), runID)
		defer release()
		runCtx := run.WithContext(cancelCtx, runID)
		replacement := New(&models.Job{
			ID:               j.id,
			Alias:            j.alias,
			MaxParallelTasks: j.maxParallelTasks,
			TaskTimeout:      j.taskTimeout,
			RunTimeout:       j.runTimeout,
			Priority:         j.priority,
		},
			WithTriggerID(nil),
			WithParams(params),
			WithPriorityOverride(j.priorityOverride),
			WithRunStoreFactory(j.runStoreFactory),
			WithEnvVariables(j.envVariables),
			WithTaskServiceFactory(j.taskServiceFactory),
			WithAtomServiceFactory(j.atomServiceFactory),
			WithTaskEdgeServiceFactory(j.taskEdgeServiceFactory),
			WithDispatchRunCallbacks(j.dispatchRunCallbacks),
			WithDockerEngineFactory(j.newDockerEngine),
			WithKubernetesEngineFactory(j.newKubernetesEngine),
			WithPodmanEngineFactory(j.newPodmanEngine),
			WithAtomPollInterval(j.atomPollInterval),
			WithSecretResolver(j.secretResolver),
			withPartitionRetryReplacement(taskRunIDs),
		)
		if engine, ok := replacement.(*job); ok {
			// Test seams only; production leaves both nil. Carrying
			// beforeComplete lets a regression interleave a retry with the
			// replacement's own shutdown window; carrying rateLimitClock keeps
			// the replacement's admission decisions in the same fixed window as
			// the engine that spawned it.
			engine.beforeComplete = j.beforeComplete
			engine.rateLimitClock = j.rateLimitClock
		}
		if err := replacement.Run(runCtx); err != nil {
			log.Error("partition retry replacement run failure", "id", j.id, "run_id", runID, "error", err)
		}
	}()
}

// finalizeAbortedResume finalizes a resumed run whose engine failed before its
// completion defer was armed. The retry-reset instances this engine was
// started for are resolved explicitly (skipped, with the reason on the row)
// so Complete's fence does not refuse, any fresh retry is handed to a
// replacement, the run is marked failed with the engine's error, and
// callbacks fire as for any failed run. A run another path already finalized
// is left alone.
func (j *job) finalizeAbortedResume(store *run.Store, runID uuid.UUID, cause error) {
	snapshot, err := store.Get(runID)
	if err != nil {
		log.Error("resumed engine failed before executing and the run could not be read", "job_id", j.id, "run_id", runID, "cause", cause, "error", err)
		return
	}
	switch snapshot.Status {
	case run.StatusSucceeded, run.StatusFailed, run.StatusCancelled:
		return
	}
	// Only the retries this engine was started for may be abandoned; a retry
	// accepted meanwhile is fresh work that gets its own engine (which, if
	// it fails the same way, abandons exactly that set — bounded). A retry
	// can also land between the scan and the completion write; the fence
	// then refuses, and the classification is simply repeated, a bounded
	// number of times, exactly as the normal completion path does.
	var finalized bool
	for attempt := 0; ; attempt++ {
		if attempt >= 2 {
			// Bounded like the normal completion path: the last word is a
			// hand-off (itself retried), never a return that strands a retry.
			for range 3 {
				if j.handOffPendingPartitionRetries(store, runID, j.params) {
					return
				}
				finalized, err = store.CompleteIfActive(runID, cause)
				if !errors.Is(err, run.ErrRunHasPendingWork) {
					break
				}
			}
			if err != nil {
				log.Error("aborted resume could not be finalized or handed off; leaving the run for an operator",
					"job_id", j.id, "run_id", runID, "error", err)
				return
			}
			break
		}
		handedOff, updated, err := j.recoverPendingPartitionRetries(store, runID, j.params, cause)
		if err != nil {
			log.Error("retry-reset instances of an aborted resume could not be resolved; leaving the run for an operator", "job_id", j.id, "run_id", runID, "error", err)
			return
		}
		if handedOff {
			return
		}
		cause = updated
		if j.beforeComplete != nil {
			j.beforeComplete(runID)
		}
		finalized, err = store.CompleteIfActive(runID, cause)
		if errors.Is(err, run.ErrRunHasPendingWork) {
			continue
		}
		if err != nil {
			log.Error("run completion persistence failure after aborted resume", "job_id", j.id, "run_id", runID, "error", err)
			return
		}
		break
	}
	if !finalized {
		// Another path finalized the run between the status read and this
		// write; it owns the callbacks.
		return
	}
	log.Error("resumed engine failed before executing; run finalized as failed", "job_id", j.id, "run_id", runID, "error", cause)
	if snapshot.Quarantine {
		return
	}
	if err := j.dispatchRunCallbacks(context.Background(), j.id, runID, cause); err != nil {
		log.Error("callback dispatch failure", "job_id", j.id, "run_id", runID, "error", err)
	}
}

// pendingGroupIndegree returns the smallest outstanding_predecessors among a
// fanned step's still-pending instances, which is the node's true cross-step
// indegree: every instance shares the step's cross-step predecessors, and a
// pending row whose in-group dependencies are all satisfied — a retried root,
// or the first retried link of a chain — carries exactly that count. ok is
// false when the step has no pending instance to read.
func pendingGroupIndegree(store *run.Store, runID, taskID uuid.UUID) (int, bool) {
	var rows []models.TaskRun
	if err := store.DB().Select("outstanding_predecessors").
		Where("job_run_id = ? AND task_id = ? AND status = ? AND partition_count > 0",
			runID, taskID, string(run.TaskStatusPending)).
		Find(&rows).Error; err != nil {
		log.Warn("failed to read pending fan-out instances for re-entry", "run_id", runID, "task_id", taskID, "error", err)
		return 0, false
	}
	if len(rows) == 0 {
		return 0, false
	}
	minOutstanding := rows[0].OutstandingPredecessors
	for _, row := range rows[1:] {
		if row.OutstandingPredecessors < minOutstanding {
			minOutstanding = row.OutstandingPredecessors
		}
	}
	return minOutstanding, true
}

// buildLocalRunners populates runners (keyed by catalog task ID) from the
// task_runs rows the run was REGISTERED with, so the local lane and the
// distributed worker agree on where a task's execution recipe comes from.
//
// Which fields the row freezes is the whole contract here. RegisterTasks
// (internal/run/store.go) snapshots engine, image, command and max_attempts onto
// the row and never rewrites them — retryFromFailure resets scheduling and
// evidence columns only — so those four come from currentRun.Tasks, which is
// the same set the distributed worker reads off taskRun.
//
// The container spec (env, workDir, mounts, kubernetes) is NOT on the row; the
// distributed worker resolves it live via loadAtomSpec(taskRun.AtomID), and so
// does this, keyed on the row's FROZEN AtomID rather than the live task's.
// atomsByTask supplies the atom already read for RegisterTasks whenever it is
// the same one, so the common path issues no extra query.
//
// currentRun.Tasks is the collapsed view (one entry per catalog task, carrying
// the first instance's frozen columns), which is what the runner map wants:
// fan-out siblings copy the template row's recipe.
func buildLocalRunners(
	ctx context.Context,
	j *job,
	svc asvc.Atom,
	currentRun *run.JobRun,
	defaultTaskTimeout time.Duration,
	atomsByTask map[uuid.UUID]*models.Atom,
	runners map[uuid.UUID]*atomRunner,
) error {
	specByAtom := make(map[uuid.UUID]container.Spec, len(currentRun.Tasks))
	for _, taskState := range currentRun.Tasks {
		if taskState == nil {
			continue
		}
		taskID := taskState.TaskID

		spec, ok := specByAtom[taskState.AtomID]
		if !ok {
			if cached := atomsByTask[taskID]; cached != nil && cached.ID == taskState.AtomID {
				spec = cached.ContainerSpec()
			} else {
				// A row whose atom the catalog no longer resolves — a run being
				// retried after its step was retired, whose atom `job apply`
				// soft-deleted. Leaving the runner unbuilt keeps the failure
				// exactly where it was before this map was built from the rows:
				// scheduling THAT task reports "missing runner", while a retry
				// whose retired task already succeeded still completes.
				frozenAtom, err := retryOnContentionDo(ctx, func() (*models.Atom, error) {
					return svc.Get(taskState.AtomID)
				})
				if err != nil {
					if !errors.Is(err, gorm.ErrRecordNotFound) {
						return err
					}
					log.Warn("no catalog atom for registered task; task is not runnable",
						"job_id", j.id, "task_id", taskID, "atom_id", taskState.AtomID, "error", err)
					continue
				}
				spec = frozenAtom.ContainerSpec()
			}
			specByAtom[taskState.AtomID] = spec
		}

		runner := &atomRunner{
			engineKind:  taskState.Engine,
			image:       taskState.Image,
			command:     slices.Clone(taskState.Command),
			maxAttempts: taskState.MaxAttempts,
			taskTimeout: defaultTaskTimeout,
			// Rebuilt field-for-field from the row, identical to the worker's
			// construction in runtimeExecutor.Execute. An empty CacheChain —
			// every row written before that column existed — means transitive,
			// whose hash is byte-identical to the pre-chain era.
			cacheCfg: jobdefschema.CacheConfig{
				Enabled:    taskState.CacheEnabled,
				TTL:        taskState.CacheTTL,
				Version:    taskState.CacheVersion,
				PinDigests: taskState.CachePinDigests,
				DigestTTL:  taskState.CacheDigestTTL,
				Chain:      taskState.CacheChain,
				TTLNever:   taskState.CacheTTLNever,
			},
			outputSchema:     slices.Clone(taskState.OutputSchema),
			schemaValidation: taskState.SchemaValidation,
			spec:             spec,
		}
		if runner.maxAttempts < 1 {
			runner.maxAttempts = 1
		}

		log.Info("evaluating task atom", "job_id", j.id, "task_id", taskID, "engine", taskState.Engine, "atom_id", taskState.AtomID)

		switch taskState.Engine {
		case models.AtomEngineDocker:
			runner.newEngine = func(ctx context.Context) (atom.Engine, error) {
				return j.newDockerEngine(ctx), nil
			}
		case models.AtomEngineKubernetes:
			runner.newEngine = j.newKubernetesEngine
		case models.AtomEnginePodman:
			runner.newEngine = func(ctx context.Context) (atom.Engine, error) {
				return j.newPodmanEngine(ctx), nil
			}
		default:
			return fmt.Errorf("unable to run atom with engine: %v", taskState.Engine)
		}

		runners[taskID] = runner
	}

	return nil
}

// buildParamEnv returns a map of environment variables derived from params.
// It also injects CAESIUM_RUN_ID and CAESIUM_JOB_ALIAS.
func buildParamEnv(runID uuid.UUID, jobAlias string, params map[string]string) map[string]string {
	return jobdefruntime.BuildRunParamEnv(runID, jobAlias, params)
}

// taskHashInputArgs is the shared cache identity input for local execution.
// Source-specific command and partition decoding stays at each call site.
type taskHashInputArgs = cache.HashInput

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

func unmarshalConcurrency(raw []byte) *jobdefschema.Concurrency {
	if len(raw) == 0 {
		return nil
	}
	v, err := jsonutil.Unmarshal[*jobdefschema.Concurrency](raw)
	if err != nil {
		log.Warn("failed to unmarshal job concurrency metadata", "error", err)
		return nil
	}
	return v
}

func unmarshalRateLimits(raw []byte) []jobdefschema.RateLimit {
	if len(raw) == 0 {
		return nil
	}
	v, err := jsonutil.Unmarshal[[]jobdefschema.RateLimit](raw)
	if err != nil {
		log.Warn("failed to unmarshal job rate limit metadata", "error", err)
		return nil
	}
	return v
}

func (j *job) Run(ctx context.Context) (err error) {
	store := j.runStoreFactory()

	// A resumed run — its id arrives in ctx from partition-retry kickoff, a
	// shutdown-window replacement, whole-run retry, replay, or POST /runs —
	// was reopened (or created) by the caller and has no other engine. The
	// completion defer below is what finalizes it, so a failure before that
	// defer is armed (a secret resolver that cannot be built, a persistent
	// store error) would leave the run running forever with nothing left to
	// execute it. Finalize it here instead.
	completionArmed := false
	if resumeID, resuming := run.FromContext(ctx); resuming {
		defer func() {
			if completionArmed || err == nil {
				return
			}
			j.finalizeAbortedResume(store, resumeID, err)
		}()
	}
	vars := j.envVariables()
	secretResolver := j.secretResolver
	if secretResolver == nil {
		var err error
		secretResolver, err = jobdefruntime.BuildSecretResolver(vars)
		if err != nil {
			return fmt.Errorf("secret resolver configuration failure: %w", err)
		}
	}

	// NOTE: the env cache defaults are deliberately NOT read here any more. The
	// scheduler folds them into each row at RegisterTasks (cache.ConfigFromEnv
	// there), and the executor reads the frozen result off the row so a
	// re-entered run keeps the configuration it was registered with.
	//
	// Lazily built, but fanned instances resolve their cache identity from
	// concurrent goroutines, so the initialization must be once-only rather
	// than a racy nil check.

	executionMode := normalizeExecutionMode(vars.ExecutionMode)
	failurePolicy := normalizeTaskFailurePolicy(vars.TaskFailurePolicy)
	continueOnFailure := failurePolicy == taskFailurePolicyContinue

	// The run-level timeout is applied after resolving the durable run row. A
	// resumed execution must inherit its frozen timeout and current retry-window
	// anchor rather than receive a fresh budget from mutable job metadata.
	runTimeout := j.runTimeout

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

	snapshot, err := retryOnContentionDo(ctx, resolveRun)
	if err != nil {
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

	// THE run-cancel registration, for every path that reaches an engine.
	//
	// The kickoff sites register too, but they can only do so for a run id they
	// already hold — and the trigger paths do not: `caesium` cron
	// (internal/trigger/cron, both the scheduled fire and the catch-up sweep),
	// http, event and webhook triggers all call job.New(...).Run(ctx) and let
	// THIS function resolve the run above. Registering only at the kickoff sites
	// therefore left every trigger-originated run uncancellable, and silently:
	// CancelRunContexts returns 0 and the log line is gated on n > 0, so a
	// cancelled cron run looked identical to a cancelled manual one while its
	// container kept going.
	//
	// Registering here is the fix because this is the single point all eleven
	// paths converge on, immediately after the run id exists. The kickoff-site
	// registrations stay: they close the window between creating the run row and
	// entering Run, and the registry holds a SET per run id, so both entries
	// cancel the same work.
	cancelCtx, releaseCancel := RegisterRunCancel(ctx, runID)
	defer releaseCancel()
	ctx = run.WithContext(cancelCtx, runID)

	var runErr error
	completionArmed = true
	defer func() {
		// The run context is the authority for the whole-run deadline. It must
		// win over an earlier ordinary task failure: with continue-on-failure a
		// later task may consume the remaining run budget, and finalizing with
		// the earlier error would leave that task and its pending siblings
		// non-terminal. Per-task deadlines use child contexts and cannot enter
		// this branch.
		if cause := context.Cause(ctx); runTimeout > 0 && run.IsRunDeadlineError(cause) {
			runErr = cause
			err = cause
		}
		if j.beforeComplete != nil {
			j.beforeComplete(runID)
		}
		completeErr := store.Complete(runID, runErr)
		for attempt := 0; errors.Is(completeErr, run.ErrRunHasPendingWork); attempt++ {
			// A per-partition retry landed after the DAG finished and before
			// this status write. HTTP kickoff only fires when reopened=true;
			// the run was still running so the handler will not start an
			// engine. Fresh retries are handed to a replacement — preserving
			// every undispatched successor, which the replacement must be
			// allowed to release if the retried partition succeeds — and the
			// retries this engine was started for but could not dispatch are
			// resolved explicitly. Completion callbacks fire from whichever
			// engine finalizes the run. The loop is bounded; its last word is
			// a hand-off, never a return that strands a retry.
			if attempt >= 2 {
				// Last resort, itself retried: a hand-off read can fail
				// transiently, and a refusal can be stale by the time it is
				// examined, so alternate hand-off and completion a few times
				// before conceding the run to an operator.
				for range 3 {
					if j.handOffPendingPartitionRetries(store, runID, snapshot.Params) {
						return
					}
					completeErr = store.Complete(runID, runErr)
					if !errors.Is(completeErr, run.ErrRunHasPendingWork) {
						break
					}
				}
				if errors.Is(completeErr, run.ErrRunHasPendingWork) {
					log.Error("run could not be finalized or handed off; leaving the run for an operator",
						"job_id", j.id, "run_id", runID)
					return
				}
				break
			}
			handedOff, updatedErr, recoverErr := j.recoverPendingPartitionRetries(store, runID, snapshot.Params, runErr)
			if recoverErr != nil {
				log.Error("run completion refused for a pending partition retry that could not be resolved; leaving the run for an operator",
					"job_id", j.id, "run_id", runID, "error", recoverErr)
				return
			}
			if handedOff {
				return
			}
			runErr = updatedErr
			completeErr = store.Complete(runID, runErr)
		}
		if completeErr != nil {
			log.Error("run completion persistence failure", "run_id", runID, "error", completeErr)
		}
		if runQuarantined {
			return
		}
		dispatchCtx := context.WithoutCancel(ctx)
		if err := j.dispatchRunCallbacks(dispatchCtx, j.id, runID, runErr); err != nil {
			log.Error("callback dispatch failure", "job_id", j.id, "run_id", runID, "error", err)
		}
	}()
	runTimeout, runTimeoutStartedAt, deadlineErr := store.RunExecutionDeadline(ctx, runID, runTimeout)
	if deadlineErr != nil {
		runErr = fmt.Errorf("resolve run deadline: %w", deadlineErr)
		return runErr
	}
	if runTimeout > 0 {
		var runCancel context.CancelFunc
		ctx, runCancel = context.WithDeadlineCause(ctx, runTimeoutStartedAt.Add(runTimeout), run.NewRunDeadlineError(runTimeout))
		defer runCancel()
	}
	if runQuarantined && executionMode != executionModeDistributed {
		runErr = ErrLocalQuarantinedReplayUnsupported
		return runErr
	}

	tasks, err := retryOnContentionDo(ctx, func() (models.Tasks, error) {
		return j.taskServiceFactory(ctx).List(&task.ListRequest{
			JobID:   j.id.String(),
			OrderBy: []string{"position", "created_at"},
		})
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

		modelAtom, err := retryOnContentionDo(ctx, func() (*models.Atom, error) {
			return svc.Get(t.AtomID)
		})
		if err != nil {
			runErr = err
			return err
		}

		atomsByTask[t.ID] = modelAtom
	}

	edges, err := retryOnContentionDo(ctx, func() (models.TaskEdges, error) {
		return j.taskEdgeServiceFactory(ctx).List(&taskedge.ListRequest{
			JobID:   j.id.String(),
			OrderBy: []string{"created_at"},
		})
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

	currentRun, err := retryOnContentionDo(ctx, func() (*run.JobRun, error) {
		return store.Get(runID)
	})
	if err != nil {
		runErr = err
		return err
	}

	if executionMode == executionModeDistributed {
		runErr = waitForRunCompletion(ctx, store, runID, len(tasks), continueOnFailure, vars.WorkerPollInterval)
		return runErr
	}

	// The local lane executes from the FROZEN task_runs rows, exactly as the
	// distributed worker does. RegisterTasks snapshots engine/image/command onto
	// each row when the run is registered and skips rows that already exist, so
	// on RE-ENTRY (`caesium run retry`, an owner takeover) the recipe is whatever
	// the run was registered with — not whatever the catalog says now. Building
	// the runners from a live `svc.Get(t.AtomID)` here meant a retry after a
	// `job apply` ran the NEW command locally while a distributed worker
	// (internal/worker/runtime_executor.go, which reads taskRun.Engine,
	// taskRun.Image and parseTaskCommand(taskRun.Command)) replayed the OLD one.
	// To pick up a definition change, trigger a new run.
	if err := buildLocalRunners(ctx, j, svc, currentRun, vars.TaskTimeout, atomsByTask, runners); err != nil {
		runErr = err
		return err
	}

	local := &localRun{
		ctx: ctx, j: j, store: store, snapshot: snapshot, currentRun: currentRun,
		tasks: tasks, atomsByTask: atomsByTask, tasksByID: tasksByID, runners: runners,
		taskOrder: taskOrder, triggerRuleByTask: triggerRuleByTask,
		adjacency: adjacency, predecessors: predecessors, indegree: indegree,
		vars: vars, secretResolver: secretResolver, runID: runID,
		runQuarantined: runQuarantined, maxParallel: maxParallel, continueOnFailure: continueOnFailure,
		queue:          make([]uuid.UUID, 0, len(tasks)),
		inQueue:        make(map[uuid.UUID]bool, len(tasks)),
		processed:      make(map[uuid.UUID]bool, len(tasks)),
		taskOutcomes:   make(map[uuid.UUID]run.TaskStatus, len(tasks)),
		taskOutputs:    make(map[uuid.UUID]map[string]string, len(tasks)),
		taskHashes:     make(map[uuid.UUID]string, len(tasks)),
		taskQuarantine: make(map[uuid.UUID]bool, len(tasks)),
		taskAttempts:   make(map[uuid.UUID]int, len(tasks)),
	}
	inQueue := local.inQueue
	processed := local.processed
	taskOutcomes := local.taskOutcomes
	taskOutputs := local.taskOutputs
	taskHashes := local.taskHashes
	taskQuarantine := local.taskQuarantine
	taskAttempts := local.taskAttempts
	imageIdentityChecksRequired := false
	for _, taskState := range currentRun.Tasks {
		imageIdentityChecksRequired = imageIdentityChecksRequired || taskState.CacheEnabled && taskState.CachePinDigests || taskState.HasUnresolvedImageIdentity
	}

	local.imageIdentityChecksRequired = imageIdentityChecksRequired
	for _, taskState := range currentRun.Tasks {
		taskQuarantine[taskState.ID] = taskState.Quarantine || runQuarantined
		taskAttempts[taskState.ID] = max(taskState.Attempt, 1)
		indegree[taskState.ID] = taskState.OutstandingPredecessors
		if taskState.PartitionCount > 0 && !run.IsTerminal(taskState.Status) {
			// The collapsed view carries the FIRST instance's indegree. On
			// re-entry that instance can be a dependent the sweep already
			// skipped with its in-group indegree still recorded, which would
			// park the whole node behind a dependency that no longer matters.
			if minIndegree, ok := pendingGroupIndegree(store, runID, taskState.ID); ok {
				indegree[taskState.ID] = minIndegree
			}
		}
		switch taskState.Status {
		case run.TaskStatusSucceeded, run.TaskStatusCached:
			processed[taskState.ID] = true
			taskOutcomes[taskState.ID] = run.TaskStatusSucceeded
			if len(taskState.Output) > 0 {
				taskOutputs[taskState.ID] = taskState.Output
			}
			local.terminalTasks++
		case run.TaskStatusSkipped:
			processed[taskState.ID] = true
			taskOutcomes[taskState.ID] = run.TaskStatusSkipped
			local.terminalTasks++
		case run.TaskStatusFailed:
			// A failure this re-entry does not reset is a settled outcome, not
			// a reason to abandon the run: whole-run retry never leaves one
			// behind, and a per-partition retry deliberately does — the reset
			// instance (and whatever its success releases) still has to
			// execute, with the run finishing failed on this preserved
			// failure. Bailing here left the accepted retry pending on a
			// terminal run, or — with the completion fence — spawned
			// replacement engines that bailed the same way.
			processed[taskState.ID] = true
			taskOutcomes[taskState.ID] = run.TaskStatusFailed
			local.terminalTasks++
			if runErr == nil {
				runErr = fmt.Errorf("task %s previously failed", taskState.ID)
			}
		}
	}

	push := local.push

	propagateSkipped := local.propagateSkipped

	// A resumed per-partition retry executes the reset instance and whatever
	// its success releases — nothing else. Under the halt failure policy the
	// original engine's first failure resolved every not-yet-dispatched
	// intolerant step as skipped (haltUnstarted), so those cannot come back;
	// but a tolerant root the halt left dispatchable, or a row registered
	// before that sweep existed, can still sit pending with indegree 0, and
	// seeding it here would resurrect work on the back of an unrelated retry.
	// So when retry-reset instances exist, only the nodes that own one enter
	// the initial queue; their successors are released through the ordinary
	// in-loop path.
	retryOwners := make(map[uuid.UUID]struct{})
	if pendingRetries, err := store.PendingPartitionRetries(runID); err != nil {
		log.Warn("failed to read pending partition retries for re-entry", "run_id", runID, "error", err)
	} else {
		for i := range pendingRetries {
			retryOwners[pendingRetries[i].TaskID] = struct{}{}
		}
	}
	for _, taskState := range currentRun.Tasks {
		if processed[taskState.ID] {
			continue
		}
		if len(retryOwners) > 0 {
			if _, owns := retryOwners[taskState.ID]; !owns {
				continue
			}
		}
		if indegree[taskState.ID] == 0 {
			push(taskState.ID)
		}
	}

	if len(local.queue) == 0 && local.terminalTasks < len(tasks) {
		runErr = fmt.Errorf("job %s has no runnable tasks (verify DAG configuration)", j.id)
		return runErr
	}

	paramEnv := buildParamEnv(snapshot.ID, j.alias, snapshot.Params)
	local.paramEnv = paramEnv

	// executeAtom creates, monitors, and stops a container for one execution attempt.
	// It returns the atom result string, any parsed task outputs, any branch
	// selections (for branch-type tasks), a persisted log snapshot, and any error.
	//
	// instanceID identifies the TaskRun row this attempt belongs to. It is
	// uuid.Nil for an unfanned step (whose single row is addressable by taskID)
	// and the instance's TaskRun primary key for a fan-out partition, where N
	// sibling rows share (runID, taskID) and every store write and container name
	// must therefore be keyed on the instance, not the catalog task.
	executeAtom := local.executeAtom

	local.fanOutGroups = make(map[uuid.UUID]run.ExpandedGroup)
	// fanOutGroups is written from whichever worker goroutine completes a
	// producer and read by whichever goroutine next runs a fanned step; the
	// two are only DAG-ordered relative to EACH OTHER, so an unrelated task
	// running concurrently makes the map a shared mutable. Every access after
	// the run loop starts goes through registerExpansion/lookupFanOutGroup.

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
	rehydrateFanOutGroups := local.rehydrateFanOutGroups
	rehydrateFanOutGroups()

	// registerExpansion is the SINGLE place an expansion payload becomes an
	// executable group. Two routes can expand a producer locally — a fresh
	// completion (CompleteTaskWithPartitions) and a cache hit
	// (CacheHitTaskWithPartitions) — and both funnel through here so they cannot
	// drift on what "the group is now runnable" means.
	registerExpansion := local.registerExpansion

	// lookupFanOutGroup is the only read of fanOutGroups once the run loop is
	// dispatching; rehydrateFanOutGroups above runs before any worker starts.
	lookupFanOutGroup := local.lookupFanOutGroup

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
	resolveTaskCacheIdentity := local.resolveTaskCacheIdentity

	var limiterOpts []ratelimit.Option
	if j.rateLimitClock != nil {
		limiterOpts = append(limiterOpts, ratelimit.WithClock(j.rateLimitClock))
	}
	local.rateLimiter = ratelimit.NewLimiter(store.DB(), limiterOpts...)

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
	acquireRateLimitFor := local.acquireRateLimitFor

	// runFannedGroup executes one expanded fan-out group.
	//
	// Readiness is NOT tracked in memory here: it is read from each instance
	// row's outstanding_predecessors column, the same scalar the distributed
	// claimer gates on. The store seeds it at expansion (template value +
	// in-group indegree) and decrements/skips it transitively inside
	// completeTask/failTask, so both lanes share one ordering implementation and
	// a failed dependency skips its dependents instead of hanging the run.
	runFannedGroup := local.runFannedGroup

	runTask := func(taskID uuid.UUID) ([]uuid.UUID, error) {
		failIdentity := func(failure error) ([]uuid.UUID, error) {
			if err := store.FailTask(runID, taskID, failure); err != nil {
				return nil, errors.Join(failure, errUnresolvedIdentityTerminalWrite, fmt.Errorf("persist task identity failure: %w", err))
			}
			return nil, failure
		}
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
		outputEnv, err := pkgtask.BuildOutputEnv(predOutputs)
		if err != nil {
			return nil, err
		}

		taskModel := tasksByID[taskID]
		taskQuarantined := taskQuarantine[taskID] || runQuarantined

		if group, ok := lookupFanOutGroup(taskID); ok && len(group.Instances) > 0 {
			return runFannedGroup(taskID, runner, taskModel, group, outputEnv, predOutputs, predOutputsByID)
		}
		taskTimeout, timingErr := store.LocalTaskExecutionTimeout(ctx, runID, taskID)
		if timingErr != nil {
			return nil, timingErr
		}
		if taskTimeout == 0 {
			taskTimeout = runner.taskTimeout
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
		cacheCfg, hashArgs, predHashByID, err := resolveTaskCacheIdentity(taskID, taskModel, runner, outputEnv, predOutputs)
		if err != nil {
			return nil, err
		}
		// resolvedImageDigest is the content digest folded into inputHash when
		// pinning is on; empty otherwise. Reused when the result is cached so
		// the cache Entry records which image content the hash covers.
		resolvedImageDigest := hashArgs.ResolvedImageDigest

		if cacheCfg.Enabled || hashArgs.UnresolvedImageIdentity != "" {
			cacheStore := local.getCacheStore()
			taskName := ""
			if taskModel != nil {
				taskName = taskModel.Name
			}

			hashInput := hashArgs
			inputHash = hashInput.Compute()
			// Serialize the decomposed input to a canonical, secret-redacted
			// blob so `caesium why` can later diff this run field-by-field. A
			// serialization failure is non-fatal: persist the hash without the
			// blob (a missing blob degrades `why` to digest-only, never wrong).
			blob, blobErr := hashInput.CanonicalJSON(inputHash)
			if blobErr != nil {
				if hashArgs.UnresolvedImageIdentity != "" {
					return failIdentity(fmt.Errorf("serialize unresolved image identity: %w", blobErr))
				}
				log.Warn("failed to serialize hash-input blob", "task", taskName, "error", blobErr)
				blob = nil
			}
			hashInputBlob = blob
			if err := store.SetTaskHashWithBlob(runID, taskID, inputHash, resolvedImageDigest, hashInputBlob); err != nil {
				if hashArgs.UnresolvedImageIdentity != "" {
					return failIdentity(fmt.Errorf("persist unresolved image identity: %w", err))
				}
				log.Warn("failed to persist task hash", "task", taskName, "error", err)
			}
			if err := store.UpdateTaskExecutionDescriptorInputs(runID, taskID, predOutputsByID, predHashByID, inputHash, resolvedImageDigest, hashInputBlob); err != nil {
				if hashArgs.UnresolvedImageIdentity != "" {
					return failIdentity(fmt.Errorf("persist unresolved image execution descriptor: %w", err))
				}
				log.Warn("failed to persist task execution descriptor inputs", "task", taskName, "error", err)
			}

			var entry *cache.Entry
			var found bool
			var err error
			if cacheCfg.Enabled && hashArgs.UnresolvedImageIdentity == "" {
				entry, found, err = cacheStore.Get(inputHash)
			}
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
				if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
					return nil, cause
				}
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
				if cacheCfg.Enabled && !taskQuarantined {
					metrics.TaskCacheMissesTotal.WithLabelValues(j.alias, taskName).Inc()
				}
			}
		}

		// Frozen on the row, exactly as the distributed worker reads it — see
		// the identical note in runFannedGroup.
		maxAttempts := max(runner.maxAttempts, 1)

		var lastErr error
		for attempt := max(taskAttempts[taskID], 1); attempt <= maxAttempts; attempt++ {
			// A cancelled run must not start another attempt. The retry budget
			// is spent on transient failures, and a cancellation is not one:
			// without this the cancel that ended attempt N was itself the
			// trigger for attempt N+1 launching a fresh container on a run the
			// operator had already stopped. The delay-based select below only
			// covers retryDelay > 0, which is the default, so this is the check
			// that holds for a step with no delay configured.
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			taskCtx := ctx
			cancel := func() {}
			if taskTimeout > 0 {
				taskCtx, cancel = context.WithTimeout(ctx, taskTimeout)
			}

			result, output, branchNames, partitions, metricsCapture, logSnapshot, execErr := executeAtom(taskCtx, taskID, uuid.Nil, attempt, taskTimeout, runner, outputEnv)
			cancel()
			if execErr == nil {
				if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
					execErr = cause
				}
			}

			if execErr == nil {
				// Frozen row, not the live catalog - see the fanned twin above.
				// This also removes the nil dereference the row-built runner
				// exposed: taskModel is tasksByID[taskID], previously guaranteed
				// non-nil only because the runner map was itself built from the
				// live catalog. Keying on taskID keeps validation running for a
				// row whose catalog task has vanished, which a nil-taskModel
				// guard would instead silently skip.
				if err := run.ValidateTaskOutputSchema(store, runID, taskID, output, runner.outputSchema, runner.schemaValidation); err != nil {
					if snapshotErr := store.SaveCapturedTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
						log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
					}
					execErr = err
				}
			}

			if execErr == nil {
				// Data-quality seam, beside schema validation. The unfanned
				// path has one row per (run, task), so the catalog task id
				// resolves it unambiguously.
				if err := run.EvaluateDataAssertions(ctx, store, runID, taskID, uuid.Nil, metricsCapture); err != nil {
					if snapshotErr := store.SaveCapturedTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
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
				if snapshotErr := store.SaveCapturedTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
					log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
				}
				if len(output) > 0 {
					taskOutputs[taskID] = output
				}

				// Keep uncertain identity available to downstream tasks even though
				// this execution cannot publish a cache entry or short-circuit.
				if inputHash != "" {
					taskHashes[taskID] = inputHash
				}

				// Store successful result in cache, reusing the hash computed earlier.
				if cacheCfg.Enabled && hashArgs.UnresolvedImageIdentity == "" && inputHash != "" && run.IsSuccessfulTaskResult(result) {
					cacheStore := local.getCacheStore()
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
			if run.IsRunDeadlineError(execErr) {
				// Preserve the typed cause for the run loop. CompleteIfActive in
				// the run completion defer owns the atomic all-task transition.
				return nil, execErr
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
	local.deferred = make(map[uuid.UUID]time.Time)
	deferred := local.deferred
	// dispatched records every node handed to the pool. The halt sweep below
	// must never resolve one of these: its row is still `pending` in SQL until
	// executeAtom's StartTask, so the store cannot tell it from a node this loop
	// has not reached, and skipping it would record a container that ran as
	// skipped.
	local.dispatched = make(map[uuid.UUID]bool)
	dispatched := local.dispatched

	moveDueDeferred := local.moveDueDeferred

	nextDeferredAt := local.nextDeferredAt

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
		dispatched[taskID] = true
		return nil
	}

	// haltUnstarted is the local executor's half of the `halt` failure policy.
	// The store resolves every not-yet-dispatched step whose trigger rule is not
	// failure-tolerant as skipped (run.Store.HaltUnstartedTasks — the same
	// primitive the distributed worker calls, so both lanes stamp the same rows
	// with the same reason), and this brings the in-memory DAG into line: each
	// skipped node is retired and its successors advanced through
	// propagateSkipped, which is what releases an `all_done` cleanup that sits
	// downstream of a halted branch. Nothing here touches `halt`: that flag is
	// the hard stop for dispatch errors and cancellation, whereas a policy halt
	// keeps the loop running for the tolerant successors it deliberately leaves
	// dispatchable.
	haltUnstarted := func(failedID uuid.UUID) error {
		candidates := make([]uuid.UUID, 0, len(tasks))
		for _, t := range tasks {
			if !processed[t.ID] && !dispatched[t.ID] {
				candidates = append(candidates, t.ID)
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		skipped, err := store.HaltUnstartedTasks(runID, failedID, candidates)
		if err != nil {
			return err
		}
		for _, id := range skipped {
			if processed[id] {
				continue
			}
			taskOutcomes[id] = run.TaskStatusSkipped
			processed[id] = true
			local.terminalTasks++
			delete(inQueue, id)
			delete(deferred, id)
			if err := propagateSkipped(id); err != nil {
				return err
			}
		}
		local.queue = slices.DeleteFunc(local.queue, func(id uuid.UUID) bool { return processed[id] })
		return nil
	}

	for (!halt && (len(local.queue) > 0 || len(deferred) > 0)) || active > 0 {
		if !halt && moveDueDeferred() {
			continue
		}

		for !halt && active < maxParallel && len(local.queue) > 0 {
			taskID := local.queue[0]
			local.queue = local.queue[1:]
			delete(inQueue, taskID)

			if processed[taskID] {
				continue
			}

			if err := dispatch(taskID); err != nil {
				if runErr == nil {
					runErr = err
				}
				halt = true
				local.queue = local.queue[:0]
				break
			}
		}

		var result taskResult
		gotResult := false
		if halt && len(local.queue) == 0 && len(deferred) > 0 {
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
		} else if len(local.queue) == 0 && len(deferred) > 0 {
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
					runErr = context.Cause(ctx)
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
				runErr = context.Cause(ctx)
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
		local.terminalTasks++

		if result.err != nil {
			if errors.Is(result.err, errUnresolvedIdentityTerminalWrite) {
				// A storage failure left the predecessor's identity uncertain.
				// Drain admitted work but never dispatch downstream cache checks.
				halt = true
				local.queue = local.queue[:0]
			}
			taskOutcomes[result.id] = run.TaskStatusFailed
			if run.IsRunDeadlineError(result.err) {
				// A genuine run deadline is the final run classification even
				// when another independent task failed earlier under continue.
				runErr = result.err
				// CompleteIfActive below owns the atomic run-timeout transition.
				// Do not apply ordinary task-failure policy here: it would turn
				// unfinished siblings into skips just before that transition.
				halt = true
				local.queue = local.queue[:0]
				continue
			}
			if runErr == nil {
				runErr = result.err
			}
			// Under BOTH failure policies the failed node's descendants resolve
			// the same way: skip only the downstream tasks whose trigger rules
			// require all predecessors to succeed. Tasks with all_done,
			// all_failed, always or one_success rules are left to the normal
			// indegree path so they can still run / evaluate their own rule.
			// The policies differ only in what happens to everything ELSE, and
			// that is haltUnstarted's job at the end of this branch.
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
						local.queue = local.queue[:0]
					}
					taskOutcomes[id] = run.TaskStatusSkipped
					processed[id] = true
					local.terminalTasks++
					delete(inQueue, id)
					if !halt {
						if propErr := propagateSkipped(id); propErr != nil {
							log.Error("failed to propagate skipped task", "run_id", runID, "task_id", id, "error", propErr)
							if runErr == nil {
								runErr = propErr
							}
							halt = true
							local.queue = local.queue[:0]
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
								local.queue = local.queue[:0]
								break
							}
							taskOutcomes[successor] = run.TaskStatusSkipped
							processed[successor] = true
							local.terminalTasks++
							delete(inQueue, successor)
							if err := propagateSkipped(successor); err != nil {
								log.Error("failed to propagate skipped task", "run_id", runID, "task_id", successor, "error", err)
								if runErr == nil {
									runErr = err
								}
								halt = true
								local.queue = local.queue[:0]
								break
							}
						}
					}
				}
			}

			// `halt`: stop admitting new work. Every step not yet dispatched
			// whose rule is not failure-tolerant is resolved skipped, in the
			// store and here; the tolerant ones stay dispatchable and the loop
			// keeps running them (and whatever they release) to completion.
			// Work already dispatched finishes on its own. The run still ends
			// failed on runErr, whatever the cleanup steps then do.
			if !continueOnFailure && !halt {
				if err := haltUnstarted(result.id); err != nil {
					log.Error("failed to halt unstarted tasks", "run_id", runID, "task_id", result.id, "error", err)
					if runErr == nil {
						runErr = err
					}
					halt = true
					local.queue = local.queue[:0]
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
			local.terminalTasks++
			delete(inQueue, skippedID)

			if err := propagateSkipped(skippedID); err != nil {
				log.Error("failed to propagate skipped task", "run_id", runID, "task_id", skippedID, "error", err)
				if runErr == nil {
					runErr = err
				}
				halt = true
				local.queue = local.queue[:0]
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
				// …and successors this loop already retired: a join the halt
				// sweep (or a failed sibling predecessor) skipped while this
				// predecessor was still running. Re-evaluating its rule here
				// would count it terminal a second time.
				if processed[successor] {
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
							local.queue = local.queue[:0]
							break
						}
						taskOutcomes[successor] = run.TaskStatusSkipped
						processed[successor] = true
						local.terminalTasks++
						delete(inQueue, successor)
						if err := propagateSkipped(successor); err != nil {
							log.Error("failed to propagate skipped task", "run_id", runID, "task_id", successor, "error", err)
							if runErr == nil {
								runErr = err
							}
							halt = true
							local.queue = local.queue[:0]
							break
						}
					}
				}
			}
		}
	}

	if local.terminalTasks != liveTaskCount {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("job %s reached terminal state for %d of %d tasks; remaining tasks may be waiting on unresolved dependencies", j.id, local.terminalTasks, liveTaskCount)
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
		// haltedAt is the failed-task count the last halt sweep was issued
		// for; a failure count above it means a new failure to halt on.
		haltedAt = 0
		// stallTicks counts consecutive ticks that found nothing running and
		// nothing releasable; see the stall check for why one is not enough.
		stallTicks = 0
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
			if evt.Type == event.TypeRunFailed || evt.Type == event.TypeRunCancelled {
				snapshot, err := store.Get(runID)
				if err != nil {
					return err
				}
				if snapshot.Status == run.StatusCancelled {
					if snapshot.Error != "" {
						return errors.New(snapshot.Error)
					}
					return fmt.Errorf("run %s cancelled", runID)
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
				if snapshot.Status == run.StatusCancelled {
					if snapshot.Error != "" {
						return errors.New(snapshot.Error)
					}
					return fmt.Errorf("run %s cancelled", runID)
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
			cancelled := 0

			liveCount := max(len(snapshot.Tasks), taskCount)

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

			// The run is complete when the STORE says every row is terminal —
			// the route-completeness contract, not a guess about whether the
			// dispatcher will do anything else.
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

			if failed == 0 {
				continue
			}

			// `halt`: the worker that recorded the failure has already swept
			// the run (runtime_executor.go), so this pass is normally a no-op.
			// It is kept because a failure can be recorded by a path that is
			// not a worker — the owner failing a producer whose expansion could
			// not be planned, say — and because a worker can die between its
			// failure write and its sweep. HaltUnstartedTasks is pending-only,
			// so repeating it is free; it is re-issued whenever the failed
			// count grows so a second failure halts what the first left
			// dispatchable.
			if !continueOnFailure && failed > haltedAt {
				if _, err := store.HaltUnstartedTasks(runID, uuid.Nil, nil); err != nil {
					// Retry on the next tick rather than finalize the run on a
					// transient store error; the worker's own sweep has most
					// likely already done the work.
					log.Warn("failed to halt unstarted tasks; retrying next tick", "run_id", runID, "error", err)
					continue
				}
				haltedAt = failed
				continue
			}

			if running > 0 {
				continue
			}

			// Nothing is running and something has failed. That used to be
			// read as "nothing more will happen", and it fired too early: a
			// failed predecessor releases its rule-tolerant successors in the
			// same transaction that records the failure, and for the moment
			// between that commit and the dispatcher's next tick nothing is
			// running — so the run was finalized, after which the claim
			// predicates' `jr.status = running` refused the released row
			// forever. A pending step whose predecessors are all terminal is
			// exactly what the dispatcher is about to claim (or the cascade
			// about to skip), so it is not a stall; only a run with NO such
			// step is. The store answers that from the run's edges, so the
			// in-memory owner lane, which never decrements the row scalar,
			// gets the same answer as the SQL lanes.
			releasable, err := store.HasReleasablePending(runID)
			if err != nil {
				return err
			}
			if releasable {
				stallTicks = 0
				continue
			}
			// The snapshot and the readiness query are two reads: a pending row
			// can be claimed, run and complete between them, in which case the
			// run is finished, not stalled. Only two consecutive ticks that
			// agree — the second re-reads the snapshot first — count as a stall.
			stallTicks++
			if stallTicks < 2 {
				continue
			}
			return fmt.Errorf("run %s has %d failed task(s) and %d unresolved pending task(s)", runID, failed, liveCount-terminal)
		}
	}
}
