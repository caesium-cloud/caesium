package job

import (
	"maps"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/ratelimit"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

func (l *localRun) getCacheStore() *cache.Store {
	l.cacheStoreOnce.Do(func() { l.cacheStore = cache.NewStore(l.store.DB()) })
	return l.cacheStore
}

func (l *localRun) resolveTaskCacheIdentity(
	taskID uuid.UUID,
	taskModel *models.Task,
	runner *atomRunner,
	outputEnv map[string]string,
	predOutputs map[string]map[string]string,
) (jobdefschema.CacheConfig, taskHashInputArgs, map[uuid.UUID]string, error) {
	ctx := l.ctx
	j := l.j
	store := l.store
	snapshot := l.snapshot
	predecessors := l.predecessors
	runID := l.runID
	imageIdentityChecksRequired := l.imageIdentityChecksRequired
	taskHashes := l.taskHashes
	// The cache configuration the SCHEDULER resolved onto this run's rows,
	// not a fresh resolution of the live step/job/env config. RegisterTasks
	// calls ResolveCacheConfig once and freezes all seven fields; the
	// distributed worker rebuilds them straight off the row. Re-resolving
	// here made a retried run's cache identity lane-dependent: a `job apply`
	// that bumped `cache.version`, switched `cache.chain`, or toggled
	// `cache.enabled` changed the local key and the local publish decision
	// while the worker kept replaying the registered one.
	cacheCfg := runner.cacheCfg
	predHashByID := make(map[uuid.UUID]string)

	taskName := ""
	if taskModel != nil {
		taskName = taskModel.Name
	}

	// Volatile per-run env (CAESIUM_RUN_ID, the injected partition, …) is
	// deliberately excluded: only the step's declared env (after
	// ${CAESIUM_PARAM_*} substitution) and the resolved predecessor outputs
	// are identity. Interpolation happens here, before Compute, so two runs
	// with different params cannot cache-hit on a shared token.
	interpolatedEnv, err := jobdefruntime.InterpolateParamRefs(runner.spec.Env, snapshot.Params)
	if err != nil {
		return cacheCfg, taskHashInputArgs{}, nil, err
	}
	mergedEnv := make(map[string]string, len(interpolatedEnv)+len(outputEnv))
	maps.Copy(mergedEnv, interpolatedEnv)
	maps.Copy(mergedEnv, outputEnv)

	var predHashes []string
	for _, predID := range predecessors[taskID] {
		if h, ok := taskHashes[predID]; ok {
			predHashes = append(predHashes, h)
			predHashByID[predID] = h
		}
	}

	// When digest pinning is on, fold the resolved content digest (not the
	// mutable tag) into the key. If resolution fails, bypass reuse and
	// publication and carry run-specific uncertainty into downstream hashes.
	//
	// The digest exists only to make a cache key miss on a moved tag, so it
	// is resolved only when caching is actually on: with the cache disabled
	// there is no key to protect and the registry round-trip would be pure
	// cost on every task.
	var resolvedImageDigest, unresolvedImageIdentity string
	if cacheCfg.Enabled && cacheCfg.PinDigests {
		// The engine the ROW froze, matching the distributed lane's
		// imagecheck.Resolve(ctx, taskRun.Engine, taskRun.Image, ...) — the
		// digest is folded into the cache key, so the two lanes must resolve
		// it against the same engine or one unit of work hashes differently
		// depending on which executor ran it.
		engineKind := runner.engineKind
		if engineKind == "" {
			engineKind = models.AtomEngineDocker
		}
		if digest, derr := j.digestResolver().Resolve(ctx, engineKind, runner.image, cacheCfg.DigestTTL); derr == nil {
			resolvedImageDigest = digest
		} else {
			unresolvedImageIdentity = uuid.NewString()
			log.Warn("cache bypassed: requested image digest could not be resolved", "task", taskName, "image", runner.image, "error", derr)
		}
	}
	if imageIdentityChecksRequired && cacheCfg.Chain != cache.ChainValues && unresolvedImageIdentity == "" {
		if unknown, err := store.HasUnresolvedPredecessorImage(runID, taskID); unknown || err != nil {
			unresolvedImageIdentity = uuid.NewString()
			if err != nil {
				log.Warn("cache bypassed: predecessor image identity query failed", "task", taskName, "reason", "identity_query_failed", "error", err)
			} else {
				log.Warn("cache bypassed: transitive predecessor image identity unavailable", "task", taskName, "reason", "unresolved_predecessor")
			}
		}
	}
	runner.resolvedImageDigest = resolvedImageDigest

	return cacheCfg, taskHashInputArgs{
		JobAlias:                j.alias,
		TaskName:                taskName,
		Image:                   runner.image,
		ResolvedImageDigest:     resolvedImageDigest,
		UnresolvedImageIdentity: unresolvedImageIdentity,
		Command:                 runner.command,
		Env:                     mergedEnv,
		WorkDir:                 runner.spec.WorkDir,
		Mounts:                  runner.spec.Mounts,
		ResolvedVolumeMounts:    runner.spec.ResolvedVolumeMounts,
		Kubernetes:              runner.spec.Kubernetes,
		PredecessorHashes:       predHashes,
		PredecessorOutputs:      predOutputs,
		RunParams:               snapshot.Params,
		CacheVersion:            cacheCfg.Version,
		Chain:                   cacheCfg.Chain,
	}, predHashByID, nil
}

func (l *localRun) acquireRateLimitFor(taskID, taskRef uuid.UUID, partition string) (bool, time.Time, error) {
	ctx := l.ctx
	j := l.j
	store := l.store
	runID := l.runID
	rateLimiter := l.rateLimiter
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
