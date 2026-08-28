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
	"github.com/caesium-cloud/caesium/internal/replay"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
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

	var descriptor *models.TaskExecutionDescriptor
	if taskRun.Quarantine {
		if !taskRun.ReplaySafe {
			err := fmt.Errorf("quarantined replay task is not replay safe: task %s", taskRun.TaskID)
			log.Error("refusing unsafe quarantined worker task", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID, "error", err)
			if persistErr := sink.Failed(ctx, taskRun, err); persistErr != nil && !errors.Is(persistErr, run.ErrTaskClaimMismatch) {
				log.Error("failed to persist replay-safe guard failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
			}
			return
		}
		loaded, descErr := e.store.TaskExecutionDescriptor(ctx, taskRun.JobRunID, taskRun.TaskID)
		if descErr != nil {
			err := fmt.Errorf("replay descriptor unavailable for quarantined task: %w", descErr)
			log.Error("failed to load replay descriptor for worker task", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID, "error", err)
			if persistErr := sink.Failed(ctx, taskRun, err); persistErr != nil && !errors.Is(persistErr, run.ErrTaskClaimMismatch) {
				log.Error("failed to persist replay descriptor failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
			}
			return
		}
		descriptor = loaded
		if descriptor.Baseline.JobAlias != "" {
			jobAlias = descriptor.Baseline.JobAlias
		}
	}

	// Load the task model to get retry configuration.
	var taskModel models.Task
	hasTaskModel := e.store.DB().First(&taskModel, "id = ?", taskRun.TaskID).Error == nil
	taskName := taskRun.TaskID.String()
	if hasTaskModel && taskModel.Name != "" {
		taskName = taskModel.Name
	}
	if descriptor != nil && descriptor.Baseline.TaskName != "" {
		taskName = descriptor.Baseline.TaskName
	}

	// fanOut is the step's fan-out scheduling metadata. It is NOT part of the
	// cache identity (only the per-instance partition is); it is read here for
	// the injected env var's name, which fanOut.env may rename and which the
	// local lane already honours.
	var fanOut *jobdefschema.FanOut
	if hasTaskModel {
		decoded, foErr := jobdefruntime.DecodeFanOutConfig(taskModel.FanOutConfig)
		if foErr != nil {
			log.Warn("failed to decode fanOut config for worker task", "task_id", taskRun.TaskID, "error", foErr)
		} else {
			fanOut = decoded
		}
	}

	var atomSpec container.Spec
	if descriptor != nil {
		atomSpec = descriptor.ContainerSpec
		if atomSpec.Kubernetes == nil && descriptor.KubernetesSpec != nil {
			atomSpec.Kubernetes = descriptor.KubernetesSpec
		}
	} else {
		var err error
		atomSpec, err = e.loadAtomSpec(taskRun.AtomID)
		if err != nil {
			log.Error("failed to load atom spec for worker task", "task_id", taskRun.TaskID, "atom_id", taskRun.AtomID, "error", err)
			if persistErr := sink.Failed(ctx, taskRun, err); persistErr != nil && !errors.Is(persistErr, run.ErrTaskClaimMismatch) {
				log.Error("failed to persist atom spec load failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
			}
			return
		}
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
		// Scheduler-set on the row (internal/run/store.go) so a worker builds
		// the SAME identity key as local execution. An empty CacheChain — every
		// row written before the column existed — means transitive, whose hash
		// is byte-identical to the pre-chain era.
		Chain:    taskRun.CacheChain,
		TTLNever: taskRun.CacheTTLNever,
	}
	// A fanned instance's identity hash is persisted whether or not caching is
	// on, exactly as the local lane does (internal/job/job.go's per-instance
	// dispatch computes and writes it outside its `if cacheCfg.Enabled` block).
	// The hash is what makes ONE PARTITION addressable — `caesium receipt get`,
	// `caesium why --partition`, `run retry --partition` and the receipt's
	// per-partition attestation all match on it — and caching is only one of its
	// consumers. Gating the write on caching meant a distributed run with the
	// cache off left every instance row's hash empty, so the partitions of a
	// group were no longer distinguishable units of work.
	//
	// Caching still gates what caching owns: the lookup below and the publish
	// after a successful run.
	needsIdentityHash := cacheCfg.Enabled || run.IsFanOutInstance(taskRun)
	if needsIdentityHash {
		if cacheCfg.Enabled {
			cacheStore = cache.NewStore(e.store.DB())
		}

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
		//
		// Gated on cacheCfg.Enabled, matching the local lane's
		// resolveTaskCacheIdentity (`if cacheCfg.Enabled && cacheCfg.PinDigests`):
		// the digest exists only to make a cache key miss on a moved tag, so with
		// the cache off there is no key to protect. It must also stay empty here
		// or the two lanes would fold DIFFERENT fields into one partition's
		// identity and the same unit of work would hash differently depending on
		// which executor ran it.
		if cacheCfg.Enabled {
			if descriptor != nil && descriptor.Runtime.ResolvedImageDigest != "" {
				resolvedImageDigest = descriptor.Runtime.ResolvedImageDigest
			} else if cacheCfg.PinDigests {
				if digest, derr := imagecheck.Default().Resolve(ctx, taskRun.Engine, taskRun.Image, cacheCfg.DigestTTL); derr == nil {
					resolvedImageDigest = digest
				}
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
			Partition:            taskRun.PartitionValue,
			PartitionFingerprint: taskRun.PartitionFingerprint,
			// All three partition fields, exactly as the local lane sets them
			// (internal/job/job.go). Dropping the attributes here would let the
			// two lanes disagree about one instance's cache identity, so the same
			// unit of work would hash differently depending on which executor ran
			// it — a permanent cache miss that looks like cache flakiness.
			PartitionAttributes: decodePartitionAttributes(taskRun.PartitionAttributes),
			// The chain mode, exactly as the local lane sets it. Dropping it
			// here would let the two lanes fold DIFFERENT fields into one task's
			// identity, so the same unit of work would hash differently
			// depending on which executor ran it.
			Chain:        cacheCfg.Chain,
			CacheVersion: cacheCfg.Version,
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
		// Address the INSTANCE, not the catalog task: SetTaskHashWithBlob resolves
		// its reference through loadTaskRunByIDOrUnique, so a fanned step's
		// catalog id names N rows and the write is refused — the instance's own
		// hash and hash-input blob would never land, leaving `caesium why
		// --partition`, `receipt get` and `run retry --partition` with no identity
		// to match. The descriptor write below already passes taskRun.ID.
		if err := e.store.SetTaskHashWithBlob(taskRun.JobRunID, taskRun.ID, cacheHash, resolvedImageDigest, hashInputBlob); err != nil {
			log.Warn("cache: failed to persist task hash", "task_id", taskRun.TaskID, "hash", cacheHash, "error", err)
		}
		if err := e.store.UpdateTaskExecutionDescriptorInputs(taskRun.JobRunID, taskRun.ID, descriptorPredOutputs, descriptorPredHashes, cacheHash, resolvedImageDigest, hashInputBlob); err != nil {
			log.Warn("cache: failed to persist task execution descriptor inputs", "task_id", taskRun.TaskID, "error", err)
		}

		if cacheStore != nil {
			entry, found, getErr := cacheStore.Get(cacheHash)
			if getErr != nil {
				log.Warn("cache: lookup failed", "task_id", taskRun.TaskID, "hash", cacheHash, "error", getErr)
			} else if found {
				log.Info("cache hit for worker task", "task_id", taskRun.TaskID, "hash", cacheHash, "cached_run_id", entry.RunID)
				source := run.CacheHitSource{
					RunID:     entry.RunID,
					CreatedAt: entry.CreatedAt,
					ExpiresAt: entry.ExpiresAt,
				}
				// A cache hit is a completion, and for a fan-out producer it is
				// the completion that must still expand the group: the cached
				// partition list stands in for the container output nobody ran.
				var cacheErr error
				if withParts, ok := sink.(cachedPartitionSink); ok && len(entry.Partitions) > 0 {
					cacheErr = withParts.CachedWithPartitions(ctx, taskRun, source, entry.Result, entry.Output, entry.BranchSelections, entry.Partitions)
				} else {
					cacheErr = sink.Cached(ctx, taskRun, source, entry.Result, entry.Output, entry.BranchSelections)
				}
				if err := cacheErr; err != nil {
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
		emitted, execErr := e.executeTask(ctx, taskRun, sink, atomSpec, runParams, resolveJobAlias(), descriptor, fanOut, attempt >= maxAttempts)
		if execErr == nil {
			// Store successful result in cache, including any partition list this
			// producer emitted: a later hit replays the result without running
			// the container that printed the markers, so the entry is the only
			// place the group's shape can come from.
			if cacheStore != nil && cacheHash != "" && !taskRun.Quarantine {
				e.storeCacheEntry(cacheStore, cacheCfg, cacheHash, resolvedImageDigest, hashInputBlob, taskRun, resolveJobAlias(), emitted)
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
		if descriptor != nil && descriptor.Runtime.RetryDelay > 0 {
			delay = descriptor.Runtime.RetryDelay
			if descriptor.Runtime.RetryBackoff {
				delay = descriptor.Runtime.RetryDelay * (1 << uint(attempt-1))
			}
		} else if hasTaskModel && taskModel.RetryDelay > 0 {
			delay = taskModel.RetryDelay
			if taskModel.RetryBackoff {
				delay = taskModel.RetryDelay * (1 << uint(attempt-1))
			}
		}

		log.Info("retrying worker task", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "attempt", attempt, "next_attempt", attempt+1, "delay", delay, "error", lastErr)

		if !taskRun.Quarantine {
			metrics.TaskRetriesTotal.WithLabelValues(resolveJobAlias(), taskRun.TaskID.String(), strconv.Itoa(attempt)).Inc()
		}

		// Address the INSTANCE, not the catalog task: RetryTaskClaimed resolved
		// its reference through loadTaskRunByIDOrUnique, so a fanned step's
		// catalog id named N rows and the reset was refused (ErrAmbiguousTaskRun)
		// — silently, because the failure is only logged.
		//
		// It is also the CLAIM-HOLDING reset. RetryTaskClaimed re-pends the row,
		// but StartTaskClaimed only starts a row that is `running`, so the very
		// next attempt tore its container down with ErrTaskClaimMismatch and
		// abandoned the task — the worker's in-process retry budget was
		// unreachable on both lanes. This worker never released the claim and is
		// about to launch the next container itself, so the row stays running and
		// claimed between attempts.
		if retryErr := e.store.RetryTaskClaimedInstance(taskRun.JobRunID, taskRun.ID, attempt+1, taskRun.ClaimedBy); retryErr != nil {
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

// decodePartitionAttributes decodes the scalar attributes persisted on a fan-out
// instance row.  A decode failure yields nil rather than an error: the caller is
// building a cache identity, and a nil map simply means "no attributes" — the
// same value an unfanned or bare-string partition carries.
func decodePartitionAttributes(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var attrs map[string]string
	if err := json.Unmarshal(raw, &attrs); err != nil {
		log.Warn("failed to decode partition attributes", "error", err)
		return nil
	}
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

// partitionFromTaskRun rebuilds the normalized partition object from the columns
// the expansion transaction persisted.  All four fields round-trip, so
// CAESIUM_PARTITION_JSON in a distributed container is byte-identical to the
// local lane's — a producer's `dependsOn` and attributes are visible to the
// container that consumes the partition, not silently dropped on one lane.
func partitionFromTaskRun(taskRun *models.TaskRun) pkgtask.Partition {
	part := pkgtask.Partition{
		Key:         taskRun.PartitionValue,
		Fingerprint: taskRun.PartitionFingerprint,
		Attributes:  decodePartitionAttributes(taskRun.PartitionAttributes),
	}
	if len(taskRun.PartitionDependsOn) > 0 {
		var deps []string
		if err := json.Unmarshal(taskRun.PartitionDependsOn, &deps); err != nil {
			log.Warn("failed to decode partition dependsOn", "task_id", taskRun.TaskID, "error", err)
		} else if len(deps) > 0 {
			part.DependsOn = deps
		}
	}
	return part
}

// partitionEnv builds the fan-out env vars injected into a fanned instance's
// container: the partition key under fanOut.env (default CAESIUM_PARTITION) and
// the normalized object under the fixed CAESIUM_PARTITION_JSON, which validation
// forbids renaming.  Returns nil for an unfanned task.
func partitionEnv(taskRun *models.TaskRun, fanOut *jobdefschema.FanOut) map[string]string {
	if taskRun == nil || taskRun.PartitionValue == "" {
		return nil
	}
	envName := jobdefschema.DefaultFanOutEnv
	if fanOut != nil && fanOut.Env != "" {
		envName = fanOut.Env
	}
	out := map[string]string{envName: taskRun.PartitionValue}
	if raw, err := partitionFromTaskRun(taskRun).CanonicalJSON(); err == nil {
		out[jobdefschema.FanOutPartitionJSONEnv] = string(raw)
	} else {
		log.Warn("failed to encode partition object", "task_id", taskRun.TaskID, "error", err)
	}
	return out
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

// executeTask runs one attempt and returns the partition list the container
// emitted (nil for a non-producer), which the caller folds into the task's cache
// entry so a later hit can still expand the group.
//
// finalAttempt tells the attempt whether it is allowed to write a terminal
// outcome for a container that reported failure. On any earlier attempt the
// failure is returned to the retry loop and NOTHING is persisted, so the row
// stays this worker's to reset. A successful result is always reported: success
// ends the task whatever the attempt budget said.
func (e *runtimeExecutor) executeTask(ctx context.Context, taskRun *models.TaskRun, sink CompletionSink, atomSpec container.Spec, runParams map[string]string, jobAlias string, descriptor *models.TaskExecutionDescriptor, fanOut *jobdefschema.FanOut, finalAttempt bool) ([]pkgtask.Partition, error) {
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
		return nil, err
	}

	command := parseTaskCommand(taskRun.Command)
	atomName := fmt.Sprintf("%s-%s", taskRun.TaskID, taskRun.JobRunID)
	// Fan-out siblings share (TaskID, JobRunID); without the instance identity
	// every sibling after the first collides on the container name. Mirrors
	// run.isFanOutInstance.
	if taskRun.PartitionCount > 1 || (taskRun.PartitionCount > 0 && taskRun.PartitionValue != "") {
		atomName = fmt.Sprintf("%s-%s", atomName, taskRun.ID)
	}
	if taskRun.ClaimAttempt > 0 {
		atomName = fmt.Sprintf("%s-attempt%d", atomName, taskRun.ClaimAttempt)
	}

	spec, secretIdentities, err := jobdefruntime.ResolveContainerSpecSecretsWithIdentities(taskCtx, e.secretResolver, atomSpec)
	if err != nil {
		return nil, err
	}
	if descriptor != nil && taskRun.Quarantine {
		if err := replay.VerifyReplaySecretIdentities(taskCtx, e.secretResolver, descriptor.SecretRefs, secretIdentities, spec.Env); err != nil {
			return nil, err
		}
	}
	if len(secretIdentities) > 0 && !taskRun.Quarantine {
		refs := make([]models.TaskExecutionSecretRef, 0, len(secretIdentities))
		for _, resolved := range secretIdentities {
			refs = append(refs, run.SecretIdentityDescriptorRef(resolved.EnvKey, resolved.Ref, resolved.Identity))
		}
		if err := e.store.UpdateTaskExecutionDescriptorSecretRefs(taskRun.JobRunID, taskRun.ID, refs); err != nil {
			log.Warn("failed to persist worker task execution descriptor secret identity", "task_id", taskRun.TaskID, "error", err)
		}
	}

	predOutputs, predErr := e.store.PredecessorOutputs(taskRun.JobRunID, taskRun.TaskID)
	if predErr != nil {
		log.Warn("failed to query predecessor outputs", "task_id", taskRun.TaskID, "error", predErr)
	}
	paramEnv := buildRunParamEnv(taskRun.JobRunID, jobAlias, runParams)
	outputEnv := pkgtask.BuildOutputEnv(predOutputs)
	if len(spec.Env) > 0 || len(paramEnv) > 0 || len(outputEnv) > 0 || taskRun.PartitionValue != "" {
		merged := make(map[string]string, len(spec.Env)+len(paramEnv)+len(outputEnv)+2)
		for k, v := range spec.Env {
			merged[k] = v
		}
		for k, v := range paramEnv {
			merged[k] = v
		}
		for k, v := range outputEnv {
			merged[k] = v
		}
		for k, v := range partitionEnv(taskRun, fanOut) {
			merged[k] = v
		}
		spec.Env = merged
	}

	// engine.Create both creates AND starts the container, so this is the last
	// moment a task that was resolved out from under this worker can still be
	// prevented from running. The pool may have held the claimed task for a long
	// time — one free slot and a queue of fan-out siblings is all it takes — and
	// fail_fast cancelling a sibling of an already-failed group revokes its claim
	// (internal/run: markInstanceCancelledBeforeStartTx). Without this check the
	// cancelled instance's container starts, and only the StartTaskClaimed below
	// notices, after the work has begun.
	if err := e.store.EnsureTaskRunStartable(taskRun.JobRunID, taskRun.ID, taskRun.ClaimedBy); err != nil {
		return nil, err
	}

	a, err := engine.Create(&atom.EngineCreateRequest{
		Name:    atomName,
		Image:   taskRun.Image,
		Command: command,
		Spec:    spec,
	})
	if err != nil {
		return nil, err
	}

	if err := e.store.StartTaskClaimed(taskRun.JobRunID, taskRun.ID, a.ID(), taskRun.ClaimedBy); err != nil {
		// The authoritative fence: the row is no longer running-and-claimed by
		// this worker, so the task was resolved while Create was in flight. The
		// container is already up, so tear it down rather than leaving it to run
		// to completion against a terminal row — the orphaned-container shape.
		// Execute treats ErrTaskClaimMismatch as "abandon quietly": no
		// completion is posted and no failure is recorded.
		if errors.Is(err, run.ErrTaskClaimMismatch) {
			log.Info("worker task resolved before start; stopping container",
				"run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "task_run_id", taskRun.ID, "atom_id", a.ID())
			if stopErr := engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true}); stopErr != nil {
				log.Warn("failed to stop container for a task resolved before start",
					"run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "atom_id", a.ID(), "error", stopErr)
			}
		}
		return nil, err
	}

	finalAtom, monitorErr := e.monitorTask(taskCtx, taskRun, engine, a)
	if monitorErr != nil {
		return nil, monitorErr
	}
	// monitorTask returns the post-Wait atom snapshot whose Result/State
	// reflect actual execution. The original `a` from Create() is pre-execution
	// state and would report Result=Unknown for the kubernetes engine.
	a = finalAtom

	// Capture the raw exit code before Result() folds it into a coarse status and
	// the incident classifier loses it. Best-effort: a persistence failure must
	// not fail an otherwise-complete task.
	if err := e.store.SetTaskExitCode(taskRun.JobRunID, taskRun.ID, a.ExitCode()); err != nil {
		log.Warn("failed to persist task exit code", "task_id", taskRun.TaskID, "error", err)
	}

	// Parse structured task output and branch markers in a single pass
	// over the log stream (no full buffering). Logs must be fetched before
	// engine.Stop runs, because Stop tears down the underlying container/pod.
	var taskOutput map[string]string
	var branchSelections []string
	var partitions []pkgtask.Partition
	var logSnapshot *run.TaskLogSnapshot
	logs, logErr := engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
	if logErr == nil {
		markers, parseErr := pkgtask.CaptureMarkersWithLimits(logs, pkgtask.MaxLogSnapshotBytes, 0, env.Variables().FanOutMaxPartitions)
		if closeErr := logs.Close(); closeErr != nil {
			log.Warn("failed to close log stream", "task_id", taskRun.TaskID, "error", closeErr)
		}
		if parseErr != nil {
			var pe *pkgtask.PartitionError
			if errors.As(parseErr, &pe) {
				return nil, parseErr
			}
			log.Warn("failed to parse task markers", "task_id", taskRun.TaskID, "error", parseErr)
		} else if markers != nil {
			taskOutput = markers.Output
			partitions = markers.Partitions
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

	// Persist the captured log BEFORE any path that can return early. The Stop
	// above is stop-AND-remove on every engine, so this snapshot is now the only
	// copy of the container's output — and a task that fails its declared
	// outputSchema below is precisely the one whose log someone will open.
	// Keyed on the TaskRun primary key so a fan-out instance records its own log
	// instead of broadcasting across (job_run_id, task_id) siblings.
	if err := e.store.SaveTaskLogSnapshot(taskRun.JobRunID, taskRun.ID, logSnapshot); err != nil {
		log.Warn("failed to persist task log snapshot", "task_id", taskRun.TaskID, "error", err)
	}

	// Runtime schema validation: if the task declares an outputSchema and the job has
	// schemaValidation enabled, validate the actual output against the schema.
	if err := e.runSchemaValidation(taskRun, taskOutput); err != nil {
		return nil, err
	}

	// Decide the attempt's outcome BEFORE any terminal write.
	//
	// A non-success engine result on a NON-FINAL attempt is a failed ATTEMPT, not
	// a failed task. Routing it through the completion sink anyway durably
	// terminalizes the row — and for a fan-out producer expands its successors,
	// and for a group member runs the failurePolicy cascade — after which the
	// retry loop is resetting a row that is already terminal. For a fanned
	// instance that reset used to be addressed by the catalog task id, which
	// names N rows and is refused, so the next attempt ran against the terminal
	// row and its completion was claim-rejected: the instance ended failed with
	// its remaining attempts silently discarded. Only the final attempt may
	// terminalize, which is also what makes a retried instance record exactly one
	// terminal write instead of one per attempt.
	result := string(a.Result())
	if !run.IsSuccessfulTaskResult(result) {
		failure := fmt.Errorf("task %s failed with result %q", taskRun.TaskID, result)
		if !finalAttempt {
			return partitions, failure
		}
		// Final attempt: the container ran and reported its own result, so the
		// COMPLETION route owns the full set of failure consequences (the
		// group's failurePolicy, the in-group skip cascade, the successor
		// advance). That is deliberately the success sink carrying a failure
		// result — see the comment on run.completeTask's TaskStatusFailed branch.
		if err := e.reportCompletion(ctx, sink, taskRun, result, taskOutput, branchSelections, partitions); err != nil {
			return nil, err
		}
		return partitions, failure
	}

	if err := e.reportCompletion(ctx, sink, taskRun, result, taskOutput, branchSelections, partitions); err != nil {
		return nil, err
	}

	return partitions, nil
}

// reportCompletion routes a finished attempt's terminal outcome through the
// completion sink, preferring the partition-carrying route when the sink
// implements it and this container emitted a partition list.
func (e *runtimeExecutor) reportCompletion(
	ctx context.Context,
	sink CompletionSink,
	taskRun *models.TaskRun,
	result string,
	taskOutput map[string]string,
	branchSelections []string,
	partitions []pkgtask.Partition,
) error {
	if withParts, ok := sink.(interface {
		SucceededWithPartitions(context.Context, *models.TaskRun, string, map[string]string, []string, []pkgtask.Partition) error
	}); ok && len(partitions) > 0 {
		return withParts.SucceededWithPartitions(ctx, taskRun, result, taskOutput, branchSelections, partitions)
	}
	return sink.Succeeded(ctx, taskRun, result, taskOutput, branchSelections)
}

// runSchemaValidation records any output-schema violations on THIS INSTANCE's
// row. The instance TaskRun id is load-bearing: SaveSchemaViolations refuses a
// catalog task id that resolves to N sibling rows, and the refusal is only
// logged — so keying on taskRun.TaskID meant a fanned step recorded nothing at
// all. In fail mode that discards the evidence for the very failure being
// reported; in warn mode it opens a schema_violation incident with no row
// behind it.
func (e *runtimeExecutor) runSchemaValidation(taskRun *models.TaskRun, output map[string]string) error {
	if taskRun == nil {
		return nil
	}
	return run.ValidateTaskOutputSchemaInstance(
		e.store, taskRun.JobRunID, taskRun.TaskID, taskRun.ID,
		output, taskRun.OutputSchema, taskRun.SchemaValidation,
	)
}

// storeCacheEntry reads back the completed task run and stores the result in the cache.
func (e *runtimeExecutor) storeCacheEntry(cacheStore *cache.Store, cacheCfg jobdefschema.CacheConfig, hash, resolvedImageDigest string, hashInputBlob []byte, taskRun *models.TaskRun, jobAlias string, partitions []pkgtask.Partition) {
	if taskRun.Quarantine {
		log.Info("quarantined worker task suppressed cache store entry", "task_id", taskRun.TaskID, "hash", hash)
		return
	}

	// Read back the completed task run to get output and result.
	// Keyed on the TaskRun primary key: (job_run_id, task_id) names N rows for a
	// fanned step, so this read would cache an arbitrary sibling's result under
	// this instance's hash.
	var completed models.TaskRun
	if err := e.store.DB().Where("id = ?", taskRun.ID).First(&completed).Error; err != nil {
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
		// Address the INSTANCE, not the catalog task. SetTaskEffectiveHash
		// resolves its second argument through loadTaskRunByIDOrUnique, so a
		// catalog task ID naming N fan-out siblings returns ErrAmbiguousTaskRun
		// and the short-circuit hash is never persisted — silently, because the
		// failure is only logged. Matches the SetTaskHashWithBlob call above.
		if scErr := e.store.SetTaskEffectiveHash(taskRun.JobRunID, taskRun.ID, effectiveHash); scErr != nil {
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
		Partitions:          partitions,
		CreatedAt:           time.Now().UTC(),
	}

	entry.ExpiresAt = cache.EntryExpiry(entry.CreatedAt, cacheCfg.TTL, cacheCfg.TTLNever)

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
