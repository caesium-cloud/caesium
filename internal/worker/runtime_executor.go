package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	taskFailurePolicyHalt     = "halt"
	taskFailurePolicyContinue = "continue"
)

type runtimeExecutor struct {
	store             *run.Store
	taskTimeout       time.Duration
	continueOnFailure bool
	engineFactory     func(context.Context, models.AtomEngine) (atom.Engine, error)

	// localSink finalizes ClaimNext'd tasks against the local DB (unchanged from
	// Phase 1).  Dispatched tasks build an owner-routed sink per task instead.
	localSink CompletionSink
	// completePost is the seam the owner sink uses to POST to /internal/complete.
	// nil in production → defaults to dispatch.PostComplete; tests inject a fake.
	completePost   completePoster
	secretResolver secret.Resolver
}

func NewRuntimeExecutor(store *run.Store, taskTimeout time.Duration, failurePolicy string, resolvers ...secret.Resolver) TaskExecutor {
	if store == nil {
		panic("runtime executor requires run store")
	}
	var resolver secret.Resolver
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}

	return (&runtimeExecutor{
		store:             store,
		taskTimeout:       taskTimeout,
		continueOnFailure: normalizeTaskFailurePolicy(failurePolicy) == taskFailurePolicyContinue,
		engineFactory:     defaultNewEngine,
		localSink:         NewLocalSink(store),
		secretResolver:    resolver,
	}).Execute
}

// sinkFor selects the completion sink for a task.  Dispatched tasks (carrying
// owner metadata in their context) route their terminal outcome back to the
// owner via /internal/complete; ClaimNext'd tasks complete locally exactly as
// in Phase 1.  When run-owner mode is off there is never any dispatchMeta, so
// the local sink is always selected and behavior is byte-identical.
func (e *runtimeExecutor) sinkFor(ctx context.Context) CompletionSink {
	if meta, ok := dispatchMetaFrom(ctx); ok {
		return newOwnerSink(meta, e.completePost)
	}
	return e.localSink
}

func (e *runtimeExecutor) Execute(ctx context.Context, taskRun *models.TaskRun) {
	if taskRun == nil {
		return
	}

	// Select the completion sink once per task: owner-routed for dispatched
	// tasks, local DB writes for ClaimNext'd tasks.
	sink := e.sinkFor(ctx)

	jobAlias := ""
	resolveJobAlias := func() string {
		if jobAlias != "" {
			return jobAlias
		}

		var result struct {
			Alias string
		}
		if err := e.store.DB().
			Table("job_runs").
			Select("jobs.alias AS alias").
			Joins("join jobs on jobs.id = job_runs.job_id").
			Where("job_runs.id = ?", taskRun.JobRunID).
			Take(&result).Error; err == nil && strings.TrimSpace(result.Alias) != "" {
			jobAlias = result.Alias
			return jobAlias
		}

		jobAlias = "unknown"
		return jobAlias
	}

	// Load the task model to get retry configuration.
	var taskModel models.Task
	hasTaskModel := e.store.DB().First(&taskModel, "id = ?", taskRun.TaskID).Error == nil
	taskName := taskRun.TaskID.String()
	if hasTaskModel && taskModel.Name != "" {
		taskName = taskModel.Name
	}

	atomSpec, err := e.loadAtomSpec(taskRun.AtomID)
	if err != nil {
		log.Error("failed to load atom spec for worker task", "task_id", taskRun.TaskID, "atom_id", taskRun.AtomID, "error", err)
		if persistErr := sink.Failed(ctx, taskRun, err); persistErr != nil && !errors.Is(persistErr, run.ErrTaskClaimMismatch) {
			log.Error("failed to persist atom spec load failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
		}
		return
	}
	runParams, err := e.loadRunParams(taskRun.JobRunID)
	if err != nil {
		log.Error("failed to load run params for worker task", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID, "error", err)
		if persistErr := sink.Failed(ctx, taskRun, err); persistErr != nil && !errors.Is(persistErr, run.ErrTaskClaimMismatch) {
			log.Error("failed to persist run param load failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
		}
		return
	}

	// Cache check: attempt to satisfy the task from cache before container execution.
	var cacheStore *cache.Store
	var cacheHash string
	// resolvedImageDigest is the content digest folded into cacheHash when
	// pinning is on; empty otherwise. Reused when caching the result.
	var resolvedImageDigest string
	// hashInputBlob is the canonical secret-redacted decomposition of the
	// HashInput; reused when caching the result so a cache hit can be explained.
	var hashInputBlob []byte
	cacheCfg := jobdefschema.CacheConfig{
		Enabled:    taskRun.CacheEnabled,
		TTL:        taskRun.CacheTTL,
		Version:    taskRun.CacheVersion,
		PinDigests: taskRun.CachePinDigests,
		DigestTTL:  taskRun.CacheDigestTTL,
	}
	if cacheCfg.Enabled {
		cacheStore = cache.NewStore(e.store.DB())

		// Look up job alias for hash computation.
		cacheJobAlias := resolveJobAlias()

		// Fetch predecessor outputs for hash input.
		predOutputs, predErr := e.store.PredecessorOutputs(taskRun.JobRunID, taskRun.TaskID)
		if predErr != nil {
			log.Warn("cache: failed to query predecessor outputs", "task_id", taskRun.TaskID, "error", predErr)
		}
		predHashes, predHashErr := e.store.PredecessorHashes(taskRun.JobRunID, taskRun.TaskID)
		if predHashErr != nil {
			log.Warn("cache: failed to query predecessor hashes", "task_id", taskRun.TaskID, "error", predHashErr)
		}
		descriptorPredOutputs, descriptorPredHashes, descriptorErr := e.store.PredecessorDescriptorInputs(taskRun.JobRunID, taskRun.TaskID)
		if descriptorErr != nil {
			log.Warn("cache: failed to query predecessor descriptor inputs", "task_id", taskRun.TaskID, "error", descriptorErr)
		}

		// Build merged env for hashing, excluding volatile per-run vars.
		mergedEnv := make(map[string]string, len(atomSpec.Env))
		for k, v := range atomSpec.Env {
			mergedEnv[k] = v
		}
		if outputEnv := pkgtask.BuildOutputEnv(predOutputs); len(outputEnv) > 0 {
			for k, v := range outputEnv {
				mergedEnv[k] = v
			}
		}

		// When digest pinning is on, resolve the image tag to its content
		// digest and fold the digest into the cache key. A resolution failure
		// falls back to the literal tag — a cache miss is always safe.
		if cacheCfg.PinDigests {
			if digest, derr := imagecheck.Default().Resolve(ctx, taskRun.Engine, taskRun.Image, cacheCfg.DigestTTL); derr == nil {
				resolvedImageDigest = digest
			}
		}

		hashInput := cache.HashInput{
			JobAlias:             cacheJobAlias,
			TaskName:             taskName,
			Image:                taskRun.Image,
			ResolvedImageDigest:  resolvedImageDigest,
			Command:              parseTaskCommand(taskRun.Command),
			Env:                  mergedEnv,
			WorkDir:              atomSpec.WorkDir,
			Mounts:               atomSpec.Mounts,
			ResolvedVolumeMounts: atomSpec.ResolvedVolumeMounts,
			Kubernetes:           atomSpec.Kubernetes,
			PredecessorHashes:    predHashes,
			PredecessorOutputs:   predOutputs,
			RunParams:            runParams,
			CacheVersion:         cacheCfg.Version,
		}
		cacheHash = hashInput.Compute()
		// Serialize the decomposed input to a canonical, secret-redacted blob so
		// a distributed worker persists the same field-by-field record the local
		// path does (the worker rebuilds the identical HashInput from the
		// scheduler-propagated TaskRun + predecessor data). A serialization
		// failure is non-fatal — the hash is still written without the blob.
		blob, blobErr := hashInput.CanonicalJSON(cacheHash)
		if blobErr != nil {
			log.Warn("cache: failed to serialize hash-input blob", "task_id", taskRun.TaskID, "error", blobErr)
			blob = nil
		}
		hashInputBlob = blob
		if err := e.store.SetTaskHashWithBlob(taskRun.JobRunID, taskRun.TaskID, cacheHash, resolvedImageDigest, hashInputBlob); err != nil {
			log.Warn("cache: failed to persist task hash", "task_id", taskRun.TaskID, "hash", cacheHash, "error", err)
		}
		if err := e.store.UpdateTaskExecutionDescriptorInputs(taskRun.JobRunID, taskRun.TaskID, descriptorPredOutputs, descriptorPredHashes, cacheHash, resolvedImageDigest, hashInputBlob); err != nil {
			log.Warn("cache: failed to persist task execution descriptor inputs", "task_id", taskRun.TaskID, "error", err)
		}

		if cacheStore != nil {
			entry, found, getErr := cacheStore.Get(cacheHash)
			if getErr != nil {
				log.Warn("cache: lookup failed", "task_id", taskRun.TaskID, "hash", cacheHash, "error", getErr)
			} else if found {
				log.Info("cache hit for worker task", "task_id", taskRun.TaskID, "hash", cacheHash, "cached_run_id", entry.RunID)
				if err := sink.Cached(ctx, taskRun, run.CacheHitSource{
					RunID:     entry.RunID,
					CreatedAt: entry.CreatedAt,
					ExpiresAt: entry.ExpiresAt,
				}, entry.Result, entry.Output, entry.BranchSelections); err != nil {
					if errors.Is(err, run.ErrTaskClaimMismatch) {
						log.Info("worker task claim changed during cache hit", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
						return
					}
					log.Error("cache: failed to persist cache hit", "task_id", taskRun.TaskID, "error", err)
					// Fall through to normal execution on persistence failure.
				} else {
					if !taskRun.Quarantine {
						metrics.TaskCacheHitsTotal.WithLabelValues(cacheJobAlias, taskName).Inc()
					}
					return
				}
			}
		}
	}

	maxAttempts := taskRun.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	currentAttempt := taskRun.Attempt
	if currentAttempt < 1 {
		currentAttempt = 1
	}

	var lastErr error
	for attempt := currentAttempt; attempt <= maxAttempts; attempt++ {
		execErr := e.executeTask(ctx, taskRun, sink, atomSpec, runParams, resolveJobAlias())
		if execErr == nil {
			// Store successful result in cache.
			if cacheStore != nil && cacheHash != "" && !taskRun.Quarantine {
				e.storeCacheEntry(cacheStore, cacheCfg, cacheHash, resolvedImageDigest, hashInputBlob, taskRun, resolveJobAlias())
			} else if taskRun.Quarantine && cacheStore != nil && cacheHash != "" {
				log.Info("quarantined worker task skipped cache publication", "task_id", taskRun.TaskID, "hash", cacheHash)
			}
			return
		}

		if errors.Is(execErr, run.ErrTaskClaimMismatch) {
			log.Info("worker task claim changed; skipping execution result", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
			return
		}

		if errors.Is(execErr, context.Canceled) {
			log.Info("worker task canceled", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
			return
		}

		lastErr = execErr

		// No more attempts — break to failure handling.
		if attempt >= maxAttempts {
			break
		}

		// Compute retry delay (retryDelay * 2^(attempt-1) if backoff, else retryDelay).
		var delay time.Duration
		if hasTaskModel && taskModel.RetryDelay > 0 {
			delay = taskModel.RetryDelay
			if taskModel.RetryBackoff {
				delay = taskModel.RetryDelay * (1 << uint(attempt-1))
			}
		}

		log.Info("retrying worker task", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "attempt", attempt, "next_attempt", attempt+1, "delay", delay, "error", lastErr)

		if !taskRun.Quarantine {
			metrics.TaskRetriesTotal.WithLabelValues(resolveJobAlias(), taskRun.TaskID.String(), strconv.Itoa(attempt)).Inc()
		}

		if retryErr := e.store.RetryTaskClaimed(taskRun.JobRunID, taskRun.TaskID, attempt+1, taskRun.ClaimedBy); retryErr != nil {
			if errors.Is(retryErr, run.ErrTaskClaimMismatch) {
				log.Info("worker task claim changed before retry persistence", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
				return
			}
			log.Error("failed to persist worker task retry state", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", retryErr)
		}

		// Update local attempt counter and sleep before the next attempt.
		// Lease renewal during the delay is handled by the per-node batched renewal
		// ticker on the Worker.
		taskRun.Attempt = attempt + 1
		if delay > 0 {
			e.sleepRetryDelay(ctx, delay)
		}

		if ctx.Err() != nil {
			return
		}
	}

	if persistErr := sink.Failed(ctx, taskRun, lastErr); persistErr != nil {
		if errors.Is(persistErr, run.ErrTaskClaimMismatch) {
			log.Info("worker task claim changed before failure persistence", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
			return
		}
		log.Error("failed to persist worker task failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
	}

	if !e.continueOnFailure {
		return
	}

	descendants, descErr := collectDescendantsFromEdges(e.store.DB(), taskRun.TaskID)
	if descErr != nil {
		log.Error("failed to collect descendant tasks", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", descErr)
		return
	}

	reason := fmt.Sprintf("skipped due to failed dependency task %s", taskRun.TaskID)
	for _, taskID := range descendants {
		if skipErr := e.store.SkipTask(taskRun.JobRunID, taskID, reason); skipErr != nil {
			log.Error("failed to persist skipped descendant task", "run_id", taskRun.JobRunID, "task_id", taskID, "error", skipErr)
		}
	}
}

// sleepRetryDelay sleeps for the given duration, respecting context cancellation.
// Lease renewal during retry delays is handled by the per-node batched renewal
// ticker on the Worker (see Worker.runLeaseRenewal).
func (e *runtimeExecutor) sleepRetryDelay(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (e *runtimeExecutor) loadAtomSpec(atomID uuid.UUID) (container.Spec, error) {
	var atomModel models.Atom
	if err := e.store.DB().Select("spec").First(&atomModel, "id = ?", atomID).Error; err != nil {
		return container.Spec{}, err
	}
	if len(atomModel.Spec) == 0 {
		return container.Spec{}, nil
	}
	var spec container.Spec
	if err := json.Unmarshal(atomModel.Spec, &spec); err != nil {
		return container.Spec{}, fmt.Errorf("decode atom spec: %w", err)
	}
	return spec, nil
}

func (e *runtimeExecutor) loadRunParams(runID uuid.UUID) (map[string]string, error) {
	var jobRun models.JobRun
	if err := e.store.DB().Select("params").First(&jobRun, "id = ?", runID).Error; err != nil {
		return nil, err
	}
	if len(jobRun.Params) == 0 {
		return nil, nil
	}
	var params map[string]string
	if err := json.Unmarshal(jobRun.Params, &params); err != nil {
		return nil, fmt.Errorf("decode run params: %w", err)
	}
	return params, nil
}

func buildRunParamEnv(runID uuid.UUID, jobAlias string, params map[string]string) map[string]string {
	env := make(map[string]string, len(params)+2)
	env["CAESIUM_RUN_ID"] = runID.String()
	env["CAESIUM_JOB_ALIAS"] = jobAlias
	for k, v := range params {
		env["CAESIUM_PARAM_"+strings.ToUpper(k)] = v
	}
	return env
}

func (e *runtimeExecutor) executeTask(ctx context.Context, taskRun *models.TaskRun, sink CompletionSink, atomSpec container.Spec, runParams map[string]string, jobAlias string) error {
	taskCtx := ctx
	cancel := func() {}
	if e.taskTimeout > 0 {
		taskCtx, cancel = context.WithTimeout(ctx, e.taskTimeout)
	}
	defer cancel()

	engineFactory := e.engineFactory
	if engineFactory == nil {
		engineFactory = defaultNewEngine
	}
	engine, err := engineFactory(taskCtx, taskRun.Engine)
	if err != nil {
		return err
	}

	command := parseTaskCommand(taskRun.Command)
	atomName := fmt.Sprintf("%s-%s", taskRun.TaskID, taskRun.JobRunID)
	if taskRun.ClaimAttempt > 0 {
		atomName = fmt.Sprintf("%s-attempt%d", atomName, taskRun.ClaimAttempt)
	}

	spec, secretIdentities, err := jobdefruntime.ResolveContainerSpecSecretsWithIdentities(taskCtx, e.secretResolver, atomSpec)
	if err != nil {
		return err
	}
	if len(secretIdentities) > 0 {
		refs := make([]models.TaskExecutionSecretRef, 0, len(secretIdentities))
		for _, resolved := range secretIdentities {
			refs = append(refs, run.SecretIdentityDescriptorRef(resolved.EnvKey, resolved.Ref, resolved.Identity))
		}
		if err := e.store.UpdateTaskExecutionDescriptorSecretRefs(taskRun.JobRunID, taskRun.TaskID, refs); err != nil {
			log.Warn("failed to persist worker task execution descriptor secret identity", "task_id", taskRun.TaskID, "error", err)
		}
	}

	predOutputs, predErr := e.store.PredecessorOutputs(taskRun.JobRunID, taskRun.TaskID)
	if predErr != nil {
		log.Warn("failed to query predecessor outputs", "task_id", taskRun.TaskID, "error", predErr)
	}
	paramEnv := buildRunParamEnv(taskRun.JobRunID, jobAlias, runParams)
	outputEnv := pkgtask.BuildOutputEnv(predOutputs)
	if len(spec.Env) > 0 || len(paramEnv) > 0 || len(outputEnv) > 0 {
		merged := make(map[string]string, len(spec.Env)+len(paramEnv)+len(outputEnv))
		for k, v := range spec.Env {
			merged[k] = v
		}
		for k, v := range paramEnv {
			merged[k] = v
		}
		for k, v := range outputEnv {
			merged[k] = v
		}
		spec.Env = merged
	}

	a, err := engine.Create(&atom.EngineCreateRequest{
		Name:    atomName,
		Image:   taskRun.Image,
		Command: command,
		Spec:    spec,
	})
	if err != nil {
		return err
	}

	if err := e.store.StartTaskClaimed(taskRun.JobRunID, taskRun.TaskID, a.ID(), taskRun.ClaimedBy); err != nil {
		return err
	}

	finalAtom, monitorErr := e.monitorTask(taskCtx, taskRun, engine, a)
	if monitorErr != nil {
		return monitorErr
	}
	// monitorTask returns the post-Wait atom snapshot whose Result/State
	// reflect actual execution. The original `a` from Create() is pre-execution
	// state and would report Result=Unknown for the kubernetes engine.
	a = finalAtom

	// Parse structured task output and branch markers in a single pass
	// over the log stream (no full buffering). Logs must be fetched before
	// engine.Stop runs, because Stop tears down the underlying container/pod.
	var taskOutput map[string]string
	var branchSelections []string
	var logSnapshot *run.TaskLogSnapshot
	logs, logErr := engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
	if logErr == nil {
		markers, parseErr := pkgtask.CaptureMarkers(logs, pkgtask.MaxLogSnapshotBytes)
		if closeErr := logs.Close(); closeErr != nil {
			log.Warn("failed to close log stream", "task_id", taskRun.TaskID, "error", closeErr)
		}
		if parseErr != nil {
			log.Warn("failed to parse task markers", "task_id", taskRun.TaskID, "error", parseErr)
		} else if markers != nil {
			taskOutput = markers.Output
			if len(markers.Branches) > 0 {
				branchSelections = markers.Branches
			}
			if markers.LogText != "" || markers.LogTruncated {
				logSnapshot = &run.TaskLogSnapshot{
					Text:      markers.LogText,
					Truncated: markers.LogTruncated,
				}
			}
		}
	}

	if stopErr := engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true}); stopErr != nil {
		log.Warn("failed to stop atom after task completion", "task_id", taskRun.TaskID, "atom_id", a.ID(), "error", stopErr)
	}

	// Runtime schema validation: if the task declares an outputSchema and the job has
	// schemaValidation enabled, validate the actual output against the schema.
	if err := e.runSchemaValidation(taskRun, taskOutput); err != nil {
		return err
	}

	if err := sink.Succeeded(ctx, taskRun, string(a.Result()), taskOutput, branchSelections); err != nil {
		return err
	}
	if err := e.store.SaveTaskLogSnapshot(taskRun.JobRunID, taskRun.TaskID, logSnapshot); err != nil {
		log.Warn("failed to persist task log snapshot", "task_id", taskRun.TaskID, "error", err)
	}
	if !run.IsSuccessfulTaskResult(string(a.Result())) {
		return fmt.Errorf("task %s failed with result %q", taskRun.TaskID, a.Result())
	}

	return nil
}

func (e *runtimeExecutor) runSchemaValidation(taskRun *models.TaskRun, output map[string]string) error {
	if taskRun == nil {
		return nil
	}
	return run.ValidateTaskOutputSchema(e.store, taskRun.JobRunID, taskRun.TaskID, output, taskRun.OutputSchema, taskRun.SchemaValidation)
}

// storeCacheEntry reads back the completed task run and stores the result in the cache.
func (e *runtimeExecutor) storeCacheEntry(cacheStore *cache.Store, cacheCfg jobdefschema.CacheConfig, hash, resolvedImageDigest string, hashInputBlob []byte, taskRun *models.TaskRun, jobAlias string) {
	if taskRun.Quarantine {
		log.Info("quarantined worker task suppressed cache store entry", "task_id", taskRun.TaskID, "hash", hash)
		return
	}

	// Read back the completed task run to get output and result.
	var completed models.TaskRun
	if err := e.store.DB().Where("job_run_id = ? AND task_id = ?", taskRun.JobRunID, taskRun.TaskID).First(&completed).Error; err != nil {
		log.Warn("cache: failed to read completed task run for caching", "task_id", taskRun.TaskID, "error", err)
		return
	}

	// Only cache successful results.
	if !run.IsSuccessfulTaskResult(completed.Result) {
		return
	}

	// Resolve the job ID from the job run.
	var jobRun models.JobRun
	if err := e.store.DB().Select("job_id").First(&jobRun, "id = ?", taskRun.JobRunID).Error; err != nil {
		log.Warn("cache: failed to look up job ID for caching", "run_id", taskRun.JobRunID, "error", err)
		return
	}

	// Resolve task name.
	var taskModel models.Task
	if err := e.store.DB().Select("name").First(&taskModel, "id = ?", taskRun.TaskID).Error; err != nil {
		log.Warn("cache: failed to look up task name for caching", "task_id", taskRun.TaskID, "error", err)
		return
	}

	// Decode output and branch selections from JSON.
	var output map[string]string
	if len(completed.Output) > 0 {
		_ = json.Unmarshal(completed.Output, &output)
	}
	var branchSelections []string
	if len(completed.BranchSelections) > 0 {
		_ = json.Unmarshal(completed.BranchSelections, &branchSelections)
	}

	// Value-verified short-circuit (D2): this worker task re-executed because
	// its own identity hash changed (a cache miss). If it produced output
	// byte-identical to a prior successful run, persist that prior run's
	// identity as the effective hash so downstream tasks — which read
	// PredecessorHashes (COALESCE(effective_hash, hash)) from the DB — see an
	// unchanged predecessor and cache-hit instead of re-running. The
	// substitution only happens when content equality is PROVEN; otherwise
	// EquivalentPriorHash returns hash and no effective_hash is written
	// (re-run downstream — always safe). Excluding hash from the prior query
	// makes the order relative to the Put below irrelevant.
	//
	// Ordering note: this task is already marked Succeeded (sink.Succeeded ran
	// before storeCacheEntry) when effective_hash is written here. A downstream
	// claimed in that narrow window would read the producer's true hash without
	// the effective_hash and therefore re-run — a missed optimization, NEVER a
	// stale result. The invariant (a miss is always safe) is preserved; we
	// optimize the common case and never risk a false short-circuit.
	if priors, priorErr := cacheStore.PriorEntriesByTask(jobRun.JobID, taskModel.Name, hash); priorErr != nil {
		log.Warn("short-circuit: failed to load prior entries", "task_id", taskRun.TaskID, "error", priorErr)
	} else if effectiveHash := cache.EquivalentPriorHash(hash, output, priors); effectiveHash != hash {
		metrics.TaskCacheShortCircuitsTotal.WithLabelValues(jobAlias, taskModel.Name).Inc()
		log.Info("value-verified short-circuit for worker task", "task_id", taskRun.TaskID, "new_hash", hash, "effective_hash", effectiveHash)
		if scErr := e.store.SetTaskEffectiveHash(taskRun.JobRunID, taskRun.TaskID, effectiveHash); scErr != nil {
			log.Warn("short-circuit: failed to persist effective hash", "task_id", taskRun.TaskID, "error", scErr)
		}
	}

	entry := &cache.Entry{
		Hash:                hash,
		JobID:               jobRun.JobID,
		TaskName:            taskModel.Name,
		Result:              completed.Result,
		Output:              output,
		BranchSelections:    branchSelections,
		RunID:               taskRun.JobRunID,
		TaskRunID:           completed.ID,
		ResolvedImageDigest: resolvedImageDigest,
		HashInputBlob:       hashInputBlob,
		CreatedAt:           time.Now().UTC(),
	}

	if cacheCfg.TTL > 0 {
		expiresAt := entry.CreatedAt.Add(cacheCfg.TTL)
		entry.ExpiresAt = &expiresAt
	}

	if err := cacheStore.Put(entry); err != nil {
		log.Warn("cache: failed to store entry", "task_id", taskRun.TaskID, "hash", hash, "error", err)
	} else {
		log.Info("cache: stored entry for worker task", "task_id", taskRun.TaskID, "hash", hash)
	}
}

// monitorTask blocks until the engine reports the atom has terminated and
// returns the post-Wait atom snapshot.
//
// On the success path the caller is responsible for stopping/cleaning up the
// atom — monitorTask intentionally leaves it running so the caller can read
// logs from the live container/pod before teardown.
//
// On any error path (deadline exceeded, parent cancellation, engine.Wait
// failure) monitorTask makes a best-effort engine.Stop before returning, so
// failures don't leak orphaned containers/pods. The Stop uses a detached
// context inside each engine implementation so cleanup still runs even when
// the parent context has been cancelled.
//
// Lease renewal is no longer done per-task inside monitorTask. The Worker
// issues a single batched UPDATE for all in-flight claims via its per-node
// renewal ticker (see Worker.runLeaseRenewal).
func (e *runtimeExecutor) monitorTask(ctx context.Context, taskRun *models.TaskRun, engine atom.Engine, a atom.Atom) (atom.Atom, error) {
	waitResult := make(chan struct {
		atom atom.Atom
		err  error
	}, 1)
	go func() {
		next, err := engine.Wait(&atom.EngineWaitRequest{ID: a.ID(), Context: ctx})
		waitResult <- struct {
			atom atom.Atom
			err  error
		}{atom: next, err: err}
	}()

	stopAtom := func() error {
		return engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true})
	}

	for {
		select {
		case <-ctx.Done():
			stopErr := stopAtom()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if stopErr != nil {
					return a, fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskRun.TaskID, e.taskTimeout, a.ID(), stopErr)
				}
				return a, fmt.Errorf("task %s timed out after %s", taskRun.TaskID, e.taskTimeout)
			}
			if stopErr != nil {
				log.Warn("failed to stop atom after task cancellation", "task_id", taskRun.TaskID, "atom_id", a.ID(), "error", stopErr)
			}
			return a, ctx.Err()
		case result := <-waitResult:
			if result.err != nil {
				if stopErr := stopAtom(); stopErr != nil {
					log.Warn("failed to stop atom after engine wait error", "task_id", taskRun.TaskID, "atom_id", a.ID(), "error", stopErr)
				}
				return a, result.err
			}
			return result.atom, nil
		}
	}
}

func defaultNewEngine(ctx context.Context, engineType models.AtomEngine) (atom.Engine, error) {
	switch engineType {
	case models.AtomEngineDocker:
		return docker.NewEngine(ctx), nil
	case models.AtomEngineKubernetes:
		return kubernetes.NewEngine(ctx), nil
	case models.AtomEnginePodman:
		return podman.NewEngine(ctx), nil
	default:
		return nil, fmt.Errorf("unsupported engine type: %v", engineType)
	}
}

func parseTaskCommand(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		return parsed
	}

	return []string{raw}
}

func normalizeTaskFailurePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case taskFailurePolicyContinue:
		return taskFailurePolicyContinue
	default:
		return taskFailurePolicyHalt
	}
}

func collectDescendantsFromEdges(db *gorm.DB, start uuid.UUID) ([]uuid.UUID, error) {
	queue := []uuid.UUID{start}
	seen := map[uuid.UUID]struct{}{}
	descendants := make([]uuid.UUID, 0)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		var edges []models.TaskEdge
		if err := db.Where("from_task_id = ?", current).Find(&edges).Error; err != nil {
			return nil, err
		}

		for _, edge := range edges {
			if _, ok := seen[edge.ToTaskID]; ok {
				continue
			}
			seen[edge.ToTaskID] = struct{}{}
			descendants = append(descendants, edge.ToTaskID)
			queue = append(queue, edge.ToTaskID)
		}
	}

	return descendants, nil
}
